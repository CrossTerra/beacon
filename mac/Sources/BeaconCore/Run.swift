import Combine
import Darwin
import Darwin
import Foundation

/// Why a run is not offered, or was stopped before launch. Lowercase codes, as Signal records them.
public enum RunRefusal: String, Error, Sendable {
    /// No checkout of that name in Settings.
    case unknownCheckout = "unknown_checkout"
    case checkoutMissing = "checkout_missing"
    /// The directory at the checkout's path is not the one approved.
    case checkoutChanged = "checkout_changed"
    /// Another run holds the checkout (13.00-D37).
    case checkoutBusy = "checkout_busy"
    case modelUnavailable = "model_unavailable"
    case opencodeMissing = "opencode_missing"
    /// The person's login shell did not give an environment.
    case environmentUnavailable = "environment_unavailable"
    /// tether is not running, or the request could not be sent.
    case supervisorUnavailable = "supervisor_unavailable"
    /// tether is not up yet (starting, or sweeping what a crashed session left): the proposal stays pending.
    case supervisorNotReady = "supervisor_not_ready"
    case launchFailed = "launch_failed"

    static let runServicesProblem = "Not approvable yet: Beacon's run supervisor is not ready. This proposal stays pending."
}

/// Starts and stops runs. The app's is `tether`; tests use a stand-in that keeps the same contract: a launch reads
/// `runs/<id>/launch` and `prompt`, and the answers come back through `RunHandler.started`, `notStarted`, `exited`
/// and `gone`.
@MainActor
public protocol RunLauncher: AnyObject {
    /// False if the request could not be made: nothing was started.
    func launch(_ runnerID: String) -> Bool
    /// False if the request could not be made (the supervisor is gone): nothing was asked to stop.
    @discardableResult
    func stop(_ runnerID: String) -> Bool
}

/// The `agent.run` handler (plan 13.00, _Agent runs_). Before approval it pins the named checkout (its directory's
/// identity and branch) and checks the model against opencode's configured models. At approval the checkout is
/// claimed: one run per checkout, any number in parallel across checkouts (D37). After approval it records the
/// durable start, checks everything again, and asks the launcher for one `opencode run` in that checkout, with the
/// prompt on standard input, the chosen model, `--auto` and the person's login environment. Launch is
/// `app.action.executed` with the runner id; the run's life follows as runner events, its final message the report.
@MainActor
public final class RunHandler: ObservableObject, ActionResolver {
    /// How Beacon finds what a run needs. Each may be slow (a login shell, `opencode models`); none runs on the
    /// main thread.
    public struct Tools: Sendable {
        public var environment: @Sendable () -> [String: String]?
        public var models: @Sendable (_ opencode: String, _ environment: [String: String]) -> [String]?

        public init(environment: @escaping @Sendable () -> [String: String]?,
                    models: @escaping @Sendable (String, [String: String]) -> [String]?) {
            self.environment = environment
            self.models = models
        }

        public static let standard = Tools(environment: { LoginEnvironment.read() },
                                           models: { OpencodeModels.list(opencode: $0, environment: $1) })
    }

    struct Context {
        let environment: [String: String]
        let opencode: String
        let models: [String]
        let at: Date
    }

    struct Run {
        let proposal: String
        let checkout: String
        var exited = false
        var stopReason: String?
    }

    /// The models opencode offers, for the approval window's picker; empty until first read.
    @Published public private(set) var models: [String] = []
    /// Runs launched in this session and not yet gone, by proposal id.
    @Published public private(set) var running: Set<String> = []
    public var isReady: Bool { !quitting && launcher != nil }
    /// Called synchronously when the desk must drop or re-resolve a run snapshot.
    public var onAvailabilityChanged: (() -> Void)?
    /// A run whose output grows past this is stopped (`output_too_large`).
    public var outputLimit: UInt64 = 64 << 20

    private let journal: Journal
    private let checkouts: () -> [CheckoutSettings]
    private let runsDirectory: URL
    private let account: String
    private let tools: Tools
    private let now: () -> Date
    private var context: Context?
    /// Checkout claims: checkout id to proposal id.
    private var claims: [String: String] = [:]
    /// Launch requests sent, waiting for tether's answer: runner id to proposal id.
    private var pending: [String: String] = [:]
    private var runs: [String: Run] = [:]
    private var quitting = false
    private weak var launcher: RunLauncher? {
        didSet { onAvailabilityChanged?() }
    }
    /// A new helper that has swept for the old marker, but is not yet clear of every known run group.
    private weak var sweptLauncher: RunLauncher?
    private var watch: Timer?
    /// Read by the proposal socket, from its own queue.
    public nonisolated let attribution = RunAttribution()

    public init(journal: Journal, checkouts: @escaping () -> [CheckoutSettings], runs: URL,
                account: String = "macos:user:\(NSUserName())", tools: Tools = .standard,
                now: @escaping () -> Date = { Date() }) {
        self.journal = journal
        self.checkouts = checkouts
        runsDirectory = runs
        self.account = account
        self.tools = tools
        self.now = now
    }

    // MARK: Before approval

    public func resolve(_ proposal: Proposal, model: String?) async throws -> ResolvedAction {
        guard proposal.type == .agentRun else { throw NoResolver.Unsupported() }
        // Approve stays off until tether is up, so an approval is never consumed for a run that cannot start.
        guard isReady else { throw RunRefusal.supervisorNotReady }
        let p = proposal.params
        guard let name = p["checkout"]?.stringValue, let prompt = p["prompt"]?.stringValue,
              let proposed = p["model"]?.stringValue else { throw Invalid(field: "/params") }
        guard let checkout = checkouts().first(where: { $0.name == name }) else { throw RunRefusal.unknownCheckout }
        guard let identity = CheckoutIdentity.read(checkout.path) else { throw RunRefusal.checkoutMissing }
        let context = try await self.context(refresh: false)
        guard isReady else { throw RunRefusal.supervisorNotReady }
        let chosen = model ?? proposed
        guard context.models.contains(chosen) else { throw RunRefusal.modelUnavailable }
        var facts = [
            "Checkout": checkout.path, "Branch": identity.branch,
            "Model": chosen == proposed ? chosen : "\(chosen) (proposed \(proposed))",
            "Prompt": "\(prompt.unicodeScalars.count) characters",
            "Runs as": "opencode run --auto: permissions this checkout's opencode config does not deny are approved",
        ]
        if claims[identity.id] != nil { facts["Busy"] = "Another run holds this checkout; approve once it ends" }
        if identity.ownRunner { facts["Note"] = "This checkout has its own bin/run; Beacon runs bare opencode without it" }
        return ResolvedAction(
            origin: nil, account: account,
            destination: .object(["checkout_id": .string(identity.id), "branch": .string(identity.branch)]),
            facts: facts, readAt: now(), accountLabel: NSUserName(),
            destinationLabel: "\(name) · \(identity.branch) · \(checkout.path)")
    }

    /// Claims the approved checkout, or says why not. Called by the window before the journal consumes the
    /// approval; a refused claim leaves the proposal pending.
    public func claim(_ approval: FrozenApproval) -> String? {
        guard approval.type == .agentRun else { return nil }
        guard isReady else { return RunRefusal.runServicesProblem }
        guard let checkout = approval.destination["checkout_id"]?.stringValue else { return nil }
        if let holder = claims[checkout], holder != approval.id {
            return "Not approved: another run is using this checkout. It stays pending; approve it once that run ends."
        }
        claims[checkout] = approval.id
        return nil
    }

    /// Frees the claim a proposal holds, if any.
    public func release(_ proposal: String) {
        claims = claims.filter { $0.value != proposal }
    }

    /// opencode, the login environment and the models, read at most once a minute unless `refresh`.
    func context(refresh: Bool) async throws -> Context {
        if !refresh, let context, now().timeIntervalSince(context.at) < 60 { return context }
        let tools = self.tools
        let read = await Task.detached { () -> Result<(String, [String: String], [String]), RunRefusal> in
            guard let environment = tools.environment() else { return .failure(.environmentUnavailable) }
            guard let opencode = LoginEnvironment.which("opencode", in: environment) else { return .failure(.opencodeMissing) }
            guard let models = tools.models(opencode, environment) else { return .failure(.modelUnavailable) }
            return .success((opencode, environment, models))
        }.value
        let (opencode, environment, models) = try read.get()
        let context = Context(environment: environment, opencode: opencode, models: models, at: now())
        self.context = context
        self.models = models
        return context
    }

    // MARK: After approval

    /// Starts one approved run. The start is durable before anything else; a refusal from here on is a failed
    /// `pre_dispatch` with nothing launched, and frees the checkout. The checkout's identity is pinned in the launch:
    /// tether opens the directory, compares it and starts the run in it by descriptor, so a directory swapped in at
    /// the path while Beacon reads the login environment and the models is refused (`checkout_changed`).
    public func execute(_ id: String) async throws {
        let approval = try journal.start(id, at: now())
        guard let runner = journal.runnerID(id), let name = approval.params["checkout"]?.stringValue,
              let prompt = approval.params["prompt"]?.stringValue, let model = approval.params["model"]?.stringValue,
              let pinned = approval.destination["checkout_id"]?.stringValue, let pin = CheckoutIdentity.pin(pinned) else {
            return try fail(id, .launchFailed, by: "pre_dispatch")
        }
        guard !quitting, let launcher else { return try fail(id, .supervisorUnavailable, by: "pre_dispatch") }
        guard let checkout = checkouts().first(where: { $0.name == name }),
              let identity = CheckoutIdentity.read(checkout.path) else { return try fail(id, .checkoutMissing, by: "pre_dispatch") }
        guard identity.id == pinned else { return try fail(id, .checkoutChanged, by: "pre_dispatch") }
        let context: Context
        do {
            context = try await self.context(refresh: true)
        } catch let refusal as RunRefusal {
            return try fail(id, refusal, by: "pre_dispatch")
        }
        guard !quitting, self.launcher === launcher else { return try fail(id, .supervisorUnavailable, by: "pre_dispatch") }
        guard context.models.contains(model) else { return try fail(id, .modelUnavailable, by: "pre_dispatch") }
        do {
            try prepare(runner, in: checkout.path, pin: pin, prompt: prompt, model: model, context: context)
        } catch {
            forgetLaunch(runner)
            return try fail(id, .launchFailed, by: "pre_dispatch")
        }
        pending[runner] = id
        runs[runner] = Run(proposal: id, checkout: pinned)
        guard launcher.launch(runner) else {
            pending[runner] = nil
            runs[runner] = nil
            forgetLaunch(runner)
            return try fail(id, .supervisorUnavailable, by: "pre_dispatch")
        }
    }

    /// `runs/<runner id>/`: the prompt, exactly as approved, and the launch: the checkout and its pinned identity
    /// (`device:inode`), `opencode run` with the approved model, and the login environment. Both `0600` in a `0700`
    /// directory, and both deleted once tether answers, or when the launch fails or is found at recovery.
    func prepare(_ runner: String, in path: String, pin: String, prompt: String, model: String, context: Context) throws {
        let directory = runsDirectory.appendingPathComponent(runner)
        try FileManager.default.createDirectory(at: runsDirectory, withIntermediateDirectories: true,
                                                attributes: [.posixPermissions: 0o700])
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false,
                                                attributes: [.posixPermissions: 0o700])
        let argv = [context.opencode, "run", "--standalone", "--format", "json", "--auto", "--model", model]
        let environment = context.environment.filter { !LoginEnvironment.excluded($0.key) && !$0.key.contains("\0") }
            .map { "\($0.key)=\($0.value)" }.sorted()
        let fields = [path, pin, String(argv.count)] + argv + environment
        guard fields.allSatisfy({ !$0.contains("\0") }) else { throw Invalid(field: "/launch") }
        var launch = Data()
        for field in fields { launch.append(Data(field.utf8)); launch.append(0) }
        try PrivateFile.replace(directory.appendingPathComponent("prompt"), with: Data(prompt.utf8))
        try PrivateFile.replace(directory.appendingPathComponent("launch"), with: launch)
    }

    // MARK: The launcher's answers

    /// The run started as the leader of process group `pid`. Its `beacon act` calls are attributed to it from now
    /// until it is gone (tether keeps the leader unreaped until then, so the group id is not reused).
    public func started(_ runner: String, pid: pid_t) {
        guard let id = pending.removeValue(forKey: runner) else { return }
        forgetLaunch(runner)
        // Before anything else: the run's group is the leader's pid, and a connection from it is already a run's.
        attribution.add(leader: pid, runner: runner)
        record { try journal.result(id, executed: true, confirmedBy: "reply", fields: launched(id), at: now()) }
        guard !quitting else {
            // The launch was answered after Quit began: it started, and tether is stopping it with everything else.
            stoppedAtQuit(id)
            return
        }
        record { try journal.runner(id, .working, at: now()) }
        running.insert(id)
        startWatching()
    }

    public func notStarted(_ runner: String, errno code: Int32) {
        guard let id = pending.removeValue(forKey: runner) else { return }
        forgetLaunch(runner)
        runs[runner] = nil
        // ESTALE: the directory at the checkout's path is not the one approved.
        let refusal: RunRefusal = code == ESTALE ? .checkoutChanged : .launchFailed
        record {
            try journal.result(id, executed: false, confirmedBy: "reply",
                               fields: ["error": .string(refusal.rawValue),
                                        "result": .string("tether did not start opencode (error \(code)).")], at: now())
        }
        release(id)
    }

    /// The run's process exited. Its report is read off the main thread; its record comes before `closed`.
    public func exited(_ runner: String, status: Int32) {
        guard var run = runs[runner], !run.exited else { return }
        run.exited = true
        runs[runner] = run
        guard !quitting else { return }
        let directory = runsDirectory.appendingPathComponent(runner)
        let proposal = run.proposal, reason = run.stopReason
        chain(runner) { [weak self] in
            let (report, errors) = await Task.detached {
                (RunReport.read(directory.appendingPathComponent("output")),
                 RunReport.tail(directory.appendingPathComponent("errors")))
            }.value
            let event: RunnerEvent
            if let reason {
                event = .failed(error: reason, detail: report.report ?? report.error)
            } else if status == 0 {
                event = .finished(report: report.report ?? "The run ended without a final message.")
            } else {
                let code = status == 130 ? "interrupted" : status > 128 ? "signal_\(status - 128)" : "exit_\(status)"
                event = .failed(error: code, detail: report.error ?? errors ?? report.report)
            }
            guard let self else { return }
            self.record { try self.journal.runner(proposal, event, at: self.now()) }
        }
    }

    /// Every process of the run is gone: it is closed, after its report, also during Quit, and its checkout is free.
    public func gone(_ runner: String) {
        guard let run = runs.removeValue(forKey: runner) else { return }
        attribution.remove(runner: runner)
        let quitting = self.quitting
        after(runner) { [weak self] in
            guard let self else { return }
            if !run.exited, !quitting {
                self.record { try self.journal.runner(run.proposal, .failed(error: "ended", detail: nil), at: self.now()) }
            }
            self.close(run.proposal)
            self.running.remove(run.proposal)
            self.release(run.proposal)
        }
    }

    /// The person's Stop: the run's whole process group is stopped; it is recorded as failed, `stopped`. False when
    /// the request could not reach tether: the run is not marked as stopped, and tether's end settles it.
    @discardableResult
    public func stop(_ proposal: String) -> Bool {
        guard let entry = runs.first(where: { $0.value.proposal == proposal }), !entry.value.exited else { return false }
        let previous = entry.value.stopReason
        runs[entry.key]?.stopReason = previous ?? "stopped"
        guard launcher?.stop(entry.key) == true else {
            runs[entry.key]?.stopReason = previous
            return false
        }
        return true
    }

    /// Whether a proposal's run is still going.
    public func isRunning(_ proposal: String) -> Bool { running.contains(proposal) }

    // MARK: Quit and relaunch

    /// Quit: every run still working is recorded as stopped at Quit; tether then stops them all. A launch with no
    /// answer yet is recorded the same way if tether answers `started` before the session ends, and otherwise is
    /// left to the next launch's recovery.
    public func beginShutdown() {
        quitting = true
        watch?.invalidate()
        for (_, run) in runs where !run.exited && journal.proposal(run.proposal, at: now())?.status == .executed {
            stoppedAtQuit(run.proposal)
        }
    }

    private func stoppedAtQuit(_ id: String) {
        record {
            try journal.runner(id, .failed(error: "stopped_at_quit", detail: "Beacon quit while the run was working."), at: now())
        }
    }

    /// `closed` comes only after an outcome: a run with none yet (the Quit races it lost) is recorded as stopped at
    /// Quit, or as ended, first.
    private func close(_ id: String) {
        let state = journal.runnerState(id)
        guard state != "runner_closed" else { return }
        if state != "runner_finished" && state != "runner_failed" {
            if quitting {
                stoppedAtQuit(id)
            } else {
                record { try journal.runner(id, .failed(error: "ended", detail: nil), at: now()) }
            }
        }
        record { try journal.runner(id, .closed, at: now()) }
    }

    /// tether's session ended: at Quit, or because tether itself stopped. A run still working when tether ended on
    /// its own failed (`supervisor_ended`). If tether confirmed every process gone, each run is closed; if not, the
    /// next session closes them after its sweep. Keep their attribution until then, and don't let an unknown peer
    /// act as Bosun in the meantime.
    public func servicesStopped(verified: Bool) {
        // A Quit-only session may have swept while an old group was still exiting: check once more before it ends.
        if verified, quitting { finishRecovery() }
        attribution.sessionEnded(verified: verified)
        sweptLauncher = nil
        launcher = nil
        watch?.invalidate()
        watch = nil
        supervisorEnded()
        let quitting = self.quitting
        for (runner, run) in runs where journal.proposal(run.proposal, at: now())?.status == .executed {
            after(runner) { [weak self] in
                guard let self else { return }
                if !run.exited, !quitting {
                    self.record {
                        try self.journal.runner(run.proposal, .failed(error: "supervisor_ended",
                                                                      detail: "tether ended while the run was working."),
                                                at: self.now())
                    }
                }
                if verified { self.close(run.proposal) }
            }
        }
        runs.removeAll()
        running.removeAll()
        claims.removeAll()
    }

    /// The new helper reports ready only after its startup sweep. Don't resume runs or external proposal clients until
    /// the old process groups are gone too: the marker sweep alone may not see every member of a group.
    public func servicesReady(_ launcher: RunLauncher) {
        guard self.launcher == nil else { return }
        sweptLauncher = launcher
        finishRecovery()
    }

    private func finishRecovery() {
        guard let sweptLauncher else { return }
        guard attribution.completeSweep() else {
            if !quitting { startWatching() }
            return
        }
        self.sweptLauncher = nil
        // A session started just to sweep at Quit can close old runs, but never re-enable approvals.
        recover()
        if !quitting { launcher = sweptLauncher }
    }

    /// After a relaunch, once tether has stopped anything a crashed session left. Every run directory loses its
    /// `launch` and `prompt` (a crash can leave them). A launched run that was not closed failed as `interrupted`
    /// and is closed now. A start whose launch is unknown is read back: tether writes `started`, with the pid, only
    /// after it started the run, so with it the run started, and without it nothing did.
    func recover() {
        let directories = (try? FileManager.default.contentsOfDirectory(atPath: runsDirectory.path)) ?? []
        for runner in directories where pending[runner] == nil { forgetLaunch(runner) }
        for proposal in journal.all() where proposal.type == .agentRun && !isRunning(proposal.id) {
            let id = proposal.id
            guard let runner = journal.runnerID(id), runs[runner] == nil, pending[runner] == nil else { continue }
            let state = journal.runnerState(id)
            switch proposal.status {
            case .executed where state != "runner_closed":
                if state != "runner_finished" && state != "runner_failed" {
                    record {
                        try journal.runner(id, .failed(error: "interrupted",
                                                       detail: "Beacon stopped before the run ended."), at: now())
                    }
                }
                record { try journal.runner(id, .closed, at: now()) }
            case .outcomeUnknown:
                let started = runsDirectory.appendingPathComponent(runner).appendingPathComponent("started")
                if FileManager.default.fileExists(atPath: started.path) {
                    record {
                        try journal.result(id, executed: true, confirmedBy: "read_back",
                                           fields: ["evidence": "Read back: tether recorded that it started the run."],
                                           at: now())
                    }
                    record {
                        try journal.runner(id, .failed(error: "interrupted",
                                                       detail: "Beacon stopped before the run ended."), at: now())
                    }
                    record { try journal.runner(id, .closed, at: now()) }
                } else {
                    record {
                        try journal.result(id, executed: false, confirmedBy: "read_back",
                                           fields: ["error": "not_launched",
                                                    "evidence": "Read back: tether never recorded starting the run."],
                                           at: now())
                    }
                }
            default:
                break
            }
        }
    }

    /// Launched runs not yet closed: after tether ended without confirming, their processes may still run until a
    /// new session's sweep stops them.
    public var hasUnclosedRuns: Bool {
        journal.all().contains { $0.type == .agentRun && $0.status == .executed && journal.runnerState($0.id) != "runner_closed" }
    }

    /// tether ended while launches waited for its answer: whether they started is unknown until the next session
    /// reads back tether's `started` mark.
    func supervisorEnded() {
        for (runner, id) in pending {
            record { try journal.outcomeUnknown(id, reason: "supervisor_ended", at: now()) }
            runs[runner] = nil
            forgetLaunch(runner)
        }
        pending.removeAll()
    }

    // MARK: Internals

    /// `launch` holds the environment (which may hold the person's tokens) and `prompt` the prompt: neither is kept
    /// once tether has answered, nor after a failed launch or a crash. `output`, `errors` and `started` stay.
    private func forgetLaunch(_ runner: String) {
        guard !runner.contains("/") else { return }
        let directory = runsDirectory.appendingPathComponent(runner)
        for name in ["launch", "prompt"] { try? FileManager.default.removeItem(at: directory.appendingPathComponent(name)) }
    }

    private func launched(_ id: String) -> [String: JSON] {
        let p = journal.approval(id)?.params
        let text = "Launched opencode in \(p?["checkout"]?.stringValue ?? "the checkout") with \(p?["model"]?.stringValue ?? "the model")."
        return ["result": .string(text)]
    }

    private func fail(_ id: String, _ refusal: RunRefusal, by confirmedBy: String) throws {
        release(id)
        try journal.result(id, executed: false, confirmedBy: confirmedBy,
                           fields: ["error": .string(refusal.rawValue)], at: now())
    }

    /// A journal write about a run's life. One that the journal refuses (a transition already recorded) is skipped:
    /// the run is not affected by it.
    private func record(_ write: () throws -> Void) {
        try? write()
    }

    /// A run's journal writes, in order: one that may wait (reading the report) is queued, and later ones wait for
    /// it. With nothing queued a write happens at once, so Quit does not lose it.
    private var tails: [String: Task<Void, Never>] = [:]

    private func chain(_ runner: String, _ work: @escaping @MainActor () async -> Void) {
        let previous = tails[runner]
        let next = Task { @MainActor in
            await previous?.value
            await work()
        }
        tails[runner] = next
        Task { @MainActor [weak self] in
            await next.value
            if self?.tails[runner] == next { self?.tails[runner] = nil }
        }
    }

    private func after(_ runner: String, _ work: @escaping @MainActor () -> Void) {
        if tails[runner] == nil { work() } else { chain(runner) { work() } }
    }

    /// Every two seconds: a run whose output and errors together passed the limit is stopped.
    private func startWatching() {
        guard watch == nil else { return }
        watch = Timer.scheduledTimer(withTimeInterval: 2, repeats: true) { [weak self] _ in
            MainActor.assumeIsolated { self?.checkOutput() }
        }
    }

    func checkOutput() {
        // A group's last member might still have been present when the marker sweep completed.
        finishRecovery()
        func size(_ url: URL) -> UInt64 {
            (try? FileManager.default.attributesOfItem(atPath: url.path)[.size] as? NSNumber)?.uint64Value ?? 0
        }
        for (runner, run) in runs where !run.exited && run.stopReason == nil {
            let directory = runsDirectory.appendingPathComponent(runner)
            if size(directory.appendingPathComponent("output")) + size(directory.appendingPathComponent("errors")) > outputLimit {
                runs[runner]?.stopReason = "output_too_large"
                // Not sent: tether is gone, and its end records the run; the next check tries again if it is not.
                if launcher?.stop(runner) != true { runs[runner]?.stopReason = nil }
            }
        }
    }
}

/// Which run a connecting process belongs to: the process group of a run Beacon launched. During a live session,
/// tether keeps each run's leader unreaped until its whole group is gone. Before a successful sweep, a peer not in
/// a known group cannot be assumed to be external (e.g. Bosun).
public final class RunAttribution: @unchecked Sendable {
    enum Peer: Equatable {
        case run(String)
        case external(UInt64)
        case unknown
        /// In a process group Beacon did not record, with an ancestor that is a run it launched. Not the person's
        /// client: it can neither act as Bosun nor as that run.
        case unverifiedRun

        var runner: String? {
            if case .run(let id) = self { return id }
            return nil
        }
    }

    private let lock = NSLock()
    private var leaders: [pid_t: String] = [:]
    private var awaitingSweep = true
    // A connection admitted as external before an unconfirmed stop must reconnect after the sweep.
    private var epoch: UInt64 = 0

    public init() {}

    func add(leader: pid_t, runner: String) {
        guard leader > 0 else { return }
        lock.withLock { leaders[leader] = runner }
    }
    func remove(runner: String) { lock.withLock { leaders = leaders.filter { $0.value != runner } } }

    func sessionEnded(verified: Bool) {
        lock.withLock {
            if !verified {
                awaitingSweep = true
                epoch &+= 1
            } else if !awaitingSweep {
                // An ack from a replacement helper need not account for the old retained groups.
                leaders.removeAll()
            }
        }
    }

    /// Call only after tether's ready signal. Probe group existence without sending a signal; a leader can be
    /// gone while its group still has members. Anything but ESRCH leaves ownership restricted.
    func completeSweep() -> Bool {
        lock.withLock {
            if !awaitingSweep { return true }
            leaders = leaders.filter { group, _ in kill(-group, 0) == 0 || errno != ESRCH }
            guard leaders.isEmpty else { return false }
            awaitingSweep = false
            return true
        }
    }

    func peer(of pid: pid_t) -> Peer {
        lock.withLock {
            guard pid > 0 else { return .unknown }
            let group = getpgid(pid)
            guard group > 0 else { return .unknown }
            if let runner = leaders[group] { return .run(runner) }
            if launchedAncestor(of: pid) { return .unverifiedRun }
            return awaitingSweep ? .unknown : .external(epoch)
        }
    }

    /// Whether a Beacon-launched process is an ancestor of `pid`: a run's descendant that left its group before the
    /// group was recorded. Bounded, and it never follows a process that has exited.
    private func launchedAncestor(of pid: pid_t) -> Bool {
        var seen = Set<pid_t>()
        var current = pid
        for _ in 0..<16 {
            guard current > 1, seen.insert(current).inserted else { return false }
            let parent = ppid(of: current)
            guard parent > 1 else { return false }
            if let runner = leaders[getpgid(parent)], leaders[parent] == nil || leaders[parent] == runner { return true }
            current = parent
        }
        return false
    }

    private func ppid(of pid: pid_t) -> pid_t {
        var info = proc_bsdinfo()
        let size = proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &info, Int32(MemoryLayout<proc_bsdinfo>.size))
        guard size == MemoryLayout<proc_bsdinfo>.size else { return 0 }
        return pid_t(info.pbi_ppid)
    }

    func canAccessProposals(_ peer: Peer) -> Bool {
        switch peer {
        case .run: return true
        case .external(let connected): return lock.withLock { !awaitingSweep && connected == epoch }
        case .unknown, .unverifiedRun: return false
        }
    }
}
