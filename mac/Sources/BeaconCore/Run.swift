import Combine
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
    case launchFailed = "launch_failed"
}

/// Starts and stops runs. The app's is `tether`; tests use a stand-in that keeps the same contract: a launch reads
/// `runs/<id>/launch` and `prompt`, and the answers come back through `RunHandler.started`, `notStarted`, `exited`
/// and `gone`.
@MainActor
public protocol RunLauncher: AnyObject {
    /// False if the request could not be made: nothing was started.
    func launch(_ runnerID: String) -> Bool
    func stop(_ runnerID: String)
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
    /// Set once tether is up; nil while it is not.
    public weak var launcher: RunLauncher?
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
    private var watch: Timer?

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
        let p = proposal.params
        guard let name = p["checkout"]?.stringValue, let prompt = p["prompt"]?.stringValue,
              let proposed = p["model"]?.stringValue else { throw Invalid(field: "/params") }
        guard let checkout = checkouts().first(where: { $0.name == name }) else { throw RunRefusal.unknownCheckout }
        guard let identity = CheckoutIdentity.read(checkout.path) else { throw RunRefusal.checkoutMissing }
        let context = try await self.context(refresh: false)
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
        guard approval.type == .agentRun, let checkout = approval.destination["checkout_id"]?.stringValue else { return nil }
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
    /// `pre_dispatch` with nothing launched, and frees the checkout.
    public func execute(_ id: String) async throws {
        let approval = try journal.start(id, at: now())
        guard let runner = journal.runnerID(id), let name = approval.params["checkout"]?.stringValue,
              let prompt = approval.params["prompt"]?.stringValue, let model = approval.params["model"]?.stringValue,
              let pinned = approval.destination["checkout_id"]?.stringValue else {
            return try fail(id, .launchFailed, by: "pre_dispatch")
        }
        guard let launcher else { return try fail(id, .supervisorUnavailable, by: "pre_dispatch") }
        guard let checkout = checkouts().first(where: { $0.name == name }),
              let identity = CheckoutIdentity.read(checkout.path) else { return try fail(id, .checkoutMissing, by: "pre_dispatch") }
        guard identity.id == pinned else { return try fail(id, .checkoutChanged, by: "pre_dispatch") }
        let context: Context
        do {
            context = try await self.context(refresh: true)
        } catch let refusal as RunRefusal {
            return try fail(id, refusal, by: "pre_dispatch")
        }
        guard context.models.contains(model) else { return try fail(id, .modelUnavailable, by: "pre_dispatch") }
        do {
            try prepare(runner, in: checkout.path, prompt: prompt, model: model, context: context)
        } catch {
            return try fail(id, .launchFailed, by: "pre_dispatch")
        }
        pending[runner] = id
        runs[runner] = Run(proposal: id, checkout: pinned)
        guard launcher.launch(runner) else {
            pending[runner] = nil
            runs[runner] = nil
            return try fail(id, .supervisorUnavailable, by: "pre_dispatch")
        }
    }

    /// `runs/<runner id>/`: the prompt, exactly as approved, and the launch: the checkout, `opencode run` with the
    /// approved model, and the login environment. Both `0600` in a `0700` directory.
    func prepare(_ runner: String, in path: String, prompt: String, model: String, context: Context) throws {
        let directory = runsDirectory.appendingPathComponent(runner)
        try FileManager.default.createDirectory(at: runsDirectory, withIntermediateDirectories: true,
                                                attributes: [.posixPermissions: 0o700])
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false,
                                                attributes: [.posixPermissions: 0o700])
        let argv = [context.opencode, "run", "--standalone", "--format", "json", "--auto", "--model", model]
        let environment = context.environment.filter { !LoginEnvironment.excluded($0.key) && !$0.key.contains("\0") }
            .map { "\($0.key)=\($0.value)" }.sorted()
        let fields = [path, String(argv.count)] + argv + environment
        guard fields.allSatisfy({ !$0.contains("\0") }) else { throw Invalid(field: "/launch") }
        var launch = Data()
        for field in fields { launch.append(Data(field.utf8)); launch.append(0) }
        try PrivateFile.replace(directory.appendingPathComponent("prompt"), with: Data(prompt.utf8))
        try PrivateFile.replace(directory.appendingPathComponent("launch"), with: launch)
    }

    // MARK: The launcher's answers

    public func started(_ runner: String) {
        guard let id = pending.removeValue(forKey: runner) else { return }
        forgetLaunch(runner)
        record { try journal.result(id, executed: true, confirmedBy: "reply", fields: launched(id), at: now()) }
        record { try journal.runner(id, .working, at: now()) }
        running.insert(id)
        startWatching()
    }

    public func notStarted(_ runner: String, errno code: Int32) {
        guard let id = pending.removeValue(forKey: runner) else { return }
        forgetLaunch(runner)
        runs[runner] = nil
        record {
            try journal.result(id, executed: false, confirmedBy: "reply",
                               fields: ["error": .string(RunRefusal.launchFailed.rawValue),
                                        "result": .string("tether could not start opencode (error \(code)).")], at: now())
        }
        release(id)
    }

    public func exited(_ runner: String, status: Int32) {
        guard var run = runs[runner], !run.exited else { return }
        run.exited = true
        runs[runner] = run
        guard !quitting else { return }
        let directory = runsDirectory.appendingPathComponent(runner)
        let report = RunReport.read(directory.appendingPathComponent("output"))
        let event: RunnerEvent
        if let reason = run.stopReason {
            event = .failed(error: reason, detail: report.report ?? report.error)
        } else if status == 0 {
            event = .finished(report: report.report ?? "The run ended without a final message.")
        } else {
            let code = status == 130 ? "interrupted" : status > 128 ? "signal_\(status - 128)" : "exit_\(status)"
            event = .failed(error: code, detail: report.error
                ?? RunReport.tail(directory.appendingPathComponent("errors")) ?? report.report)
        }
        record { try journal.runner(run.proposal, event, at: now()) }
    }

    /// Every process of the run is gone: it is closed, also during Quit, and its checkout is free.
    public func gone(_ runner: String) {
        guard let run = runs.removeValue(forKey: runner) else { return }
        if !run.exited, !quitting {
            record { try journal.runner(run.proposal, .failed(error: "ended", detail: nil), at: now()) }
        }
        record { try journal.runner(run.proposal, .closed, at: now()) }
        running.remove(run.proposal)
        release(run.proposal)
    }

    /// The person's Stop: the run's whole process group is stopped; it is recorded as failed, `stopped`.
    public func stop(_ proposal: String) {
        guard let entry = runs.first(where: { $0.value.proposal == proposal }), !entry.value.exited else { return }
        runs[entry.key]?.stopReason = entry.value.stopReason ?? "stopped"
        launcher?.stop(entry.key)
    }

    /// Whether a proposal's run is still going.
    public func isRunning(_ proposal: String) -> Bool { running.contains(proposal) }

    // MARK: Quit and relaunch

    /// Quit: every run still working is recorded as stopped at Quit; tether then stops them all. A launch with no
    /// answer yet is left to the next launch's recovery.
    public func beginShutdown() {
        quitting = true
        watch?.invalidate()
        for (_, run) in runs where !run.exited && journal.proposal(run.proposal, at: now())?.status == .executed {
            record {
                try journal.runner(run.proposal, .failed(error: "stopped_at_quit",
                                                         detail: "Beacon quit while the run was working."), at: now())
            }
        }
    }

    /// tether confirmed every process is gone: each run is closed. Unconfirmed, they are closed at the next launch.
    public func servicesStopped(verified: Bool) {
        guard verified else { return }
        for (_, run) in runs where journal.proposal(run.proposal, at: now())?.status == .executed {
            record { try journal.runner(run.proposal, .closed, at: now()) }
        }
        runs.removeAll()
        running.removeAll()
    }

    /// After a relaunch, once tether has stopped anything a crashed session left: a launched run that was not
    /// closed failed as `interrupted` and is closed now; a start whose launch is unknown is read back from its run
    /// directory: tether creates the run's output just before it starts the run, so without it nothing started.
    public func recover() {
        for proposal in journal.all() where proposal.type == .agentRun && !isRunning(proposal.id) {
            let id = proposal.id
            guard let runner = journal.runnerID(id), runs[runner] == nil else { continue }
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
                let output = runsDirectory.appendingPathComponent(runner).appendingPathComponent("output")
                if FileManager.default.fileExists(atPath: output.path) {
                    record {
                        try journal.result(id, executed: true, confirmedBy: "read_back",
                                           fields: ["evidence": "Read back: tether had created the run's output, so it started."],
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
                                                    "evidence": "Read back: the run has no output, so it never started."],
                                           at: now())
                    }
                }
            default:
                break
            }
        }
    }

    /// tether ended while launches waited for its answer: whether they started is unknown until the next launch.
    public func supervisorEnded() {
        for (runner, id) in pending {
            record { try journal.outcomeUnknown(id, reason: "reply_lost", at: now()) }
            runs[runner] = nil
        }
        pending.removeAll()
    }

    // MARK: Internals

    /// Once tether has answered, it has read `launch` and holds `prompt` open: both go, so the environment (which
    /// may hold the person's tokens) and the prompt are not kept on disk. `output` and `errors` stay.
    private func forgetLaunch(_ runner: String) {
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

    /// Every two seconds: a run whose output passed the limit is stopped.
    private func startWatching() {
        guard watch == nil else { return }
        watch = Timer.scheduledTimer(withTimeInterval: 2, repeats: true) { [weak self] _ in
            MainActor.assumeIsolated { self?.checkOutput() }
        }
    }

    func checkOutput() {
        for (runner, run) in runs where !run.exited && run.stopReason == nil {
            let output = runsDirectory.appendingPathComponent(runner).appendingPathComponent("output")
            let size = (try? FileManager.default.attributesOfItem(atPath: output.path)[.size] as? NSNumber)?.uint64Value ?? 0
            if size > outputLimit {
                runs[runner]?.stopReason = "output_too_large"
                launcher?.stop(runner)
            }
        }
    }
}
