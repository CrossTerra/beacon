import Darwin
import Foundation
import Testing
import TetherSupport
@testable import BeaconCore

/// A fake `opencode`: `models` lists two models and a line that is not one; `run` records its arguments, directory,
/// prompt and environment, then acts on the prompt's first word, writing `--format json` events as opencode does.
let fakeOpencode = #"""
#!/usr/bin/env python3
import json, os, signal, sys, time
if sys.argv[1:2] == ["models"]:
    print("anthropic/claude-opus-5\nopenai/gpt-6\nnot a model id")
    sys.exit(0)
prompt = sys.stdin.read()
with open(os.environ["FAKE_LOG"] + "/" + str(os.getpid()), "w") as log:
    json.dump({"argv": sys.argv[1:], "cwd": os.getcwd(), "prompt": prompt,
               "env": {k: v for k, v in os.environ.items()}}, log)
def emit(kind, **data):
    sys.stdout.write(json.dumps(dict(type=kind, timestamp=0, sessionID="ses_1", **data)) + "\n")
    sys.stdout.flush()
def text(message, part, words):
    emit("text", part={"id": part, "messageID": message, "type": "text", "text": words})
word = prompt.split()[0]
if word == "succeed":
    emit("step_start", part={"type": "step-start"})
    text("msg_1", "prt_1", "Looking at the plan.")
    text("msg_2", "prt_2", "Two gaps: the journal view")
    text("msg_2", "prt_msg_2_text-0", " and the Bosun watch.")
    sys.exit(0)
if word == "fail":
    emit("error", error={"name": "APIError", "data": {"message": "The model refused the request."}})
    sys.stderr.write("stderr detail\n")
    sys.exit(1)
if word == "crash":
    sys.stderr.write("panic: something broke\n")
    sys.exit(3)
if word == "long":
    text("msg_1", "prt_1", "start " + "x" * 40000 + " end")
    sys.exit(0)
if word == "spamerr":
    while True:
        sys.stderr.write("z" * 10000 + "\n")
if word == "spam":
    while True:
        text("msg_1", "prt_1", "y" * 10000)
if word == "hang":
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    if os.fork() == 0:
        while True: time.sleep(1)
    while True: time.sleep(1)
sys.exit(9)
"""#

/// tether's side of a run, in-process: reads `runs/<id>/launch` and `prompt`, opens the pinned directory and refuses
/// it with ESTALE unless its device and inode match, and prepares the suspended group leader. Once acknowledged it
/// resumes, writes `started`, and reports its exit and cleanup. `stop` sends TERM, then KILL after half a second.
@MainActor
final class FakeLauncher: RunLauncher {
    let runs: URL
    weak var handler: RunHandler?
    var refuse = false
    var silent = false
    var onAcknowledge: ((String, pid_t) -> Void)?
    private(set) var acknowledgements: [String] = []
    private(set) var pids: [String: pid_t] = [:]
    private var suspended: Set<String> = []

    init(runs: URL) { self.runs = runs }

    func launch(_ runner: String) -> Bool {
        guard !refuse else { return false }
        if silent { return true }
        let directory = runs.appendingPathComponent(runner)
        guard let launch = try? Data(contentsOf: directory.appendingPathComponent("launch")) else {
            Task { @MainActor in self.handler?.notStarted(runner, errno: ENOENT) }
            return true
        }
        let fields = launch.split(separator: 0, omittingEmptySubsequences: false).dropLast().map { String(decoding: $0, as: UTF8.self) }
        let argc = Int(fields[2])!
        let argv = Array(fields[3..<(3 + argc)]), env = Array(fields[(3 + argc)...])
        let cwd = open(fields[0], O_RDONLY | O_DIRECTORY)
        var info = stat()
        let identity = fields[1].split(separator: ":", omittingEmptySubsequences: false)
        guard identity.count == 2, let device = UInt64(identity[0]), let inode = UInt64(identity[1]),
              cwd >= 0, fstat(cwd, &info) == 0,
              tether_same_identity(info.st_dev, info.st_ino, device, inode) != 0 else {
            if cwd >= 0 { close(cwd) }
            Task { @MainActor in self.handler?.notStarted(runner, errno: ESTALE) }
            return true
        }
        let input = open(directory.appendingPathComponent("prompt").path, O_RDONLY)
        let output = open(directory.appendingPathComponent("output").path, O_WRONLY | O_CREAT | O_EXCL, 0o600)
        let errors = open(directory.appendingPathComponent("errors").path, O_WRONLY | O_CREAT | O_EXCL, 0o600)
        let cArgs = argv.map { strdup($0) } + [nil], cEnv = env.map { strdup($0) } + [nil]
        let pid = tether_spawn_suspended(argv[0], cArgs, cEnv, input, output, errors, cwd)
        let code = errno
        cArgs.forEach { free($0) }
        cEnv.forEach { free($0) }
        [input, output, errors, cwd].forEach { close($0) }
        guard pid > 0 else {
            Task { @MainActor in self.handler?.notStarted(runner, errno: code) }
            return true
        }
        pids[runner] = pid
        suspended.insert(runner)
        Task { @MainActor in self.handler?.prepared(runner, pid: pid) }
        return true
    }

    func acknowledgeLaunch(_ runner: String) -> Bool {
        acknowledgements.append(runner)
        if silent { return true }
        guard let pid = pids[runner], suspended.remove(runner) != nil else { return false }
        onAcknowledge?(runner, pid)
        if tether_resume(pid) != 0 {
            let code = errno
            stopSuspended(pid)
            pids[runner] = nil
            Task { @MainActor in self.handler?.notStarted(runner, errno: code) }
            return true
        }
        try? Data("\(pid)\n".utf8).write(to: runs.appendingPathComponent(runner).appendingPathComponent("started"))
        Task { @MainActor in self.handler?.started(runner, pid: pid) }
        Thread.detachNewThread { [weak self] in
            var status: Int32 = 0
            while waitpid(pid, &status, 0) < 0 && errno == EINTR {}
            let code = status & 0x7f == 0 ? (status >> 8) & 0xff : 128 + (status & 0x7f)
            killpg(pid, SIGKILL)
            Task { @MainActor in
                self?.handler?.exited(runner, status: code)
                self?.handler?.gone(runner)
            }
        }
        return true
    }

    /// False, as a lease tether has closed: nothing is asked to stop.
    var refuseStop = false

    @discardableResult
    func stop(_ runner: String) -> Bool {
        guard !refuseStop else { return false }
        guard let pid = pids[runner] else { return true }
        if suspended.remove(runner) != nil {
            stopSuspended(pid)
            pids[runner] = nil
            handler?.notStarted(runner, errno: ECANCELED)
            return true
        }
        killpg(pid, SIGTERM)
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.5) { killpg(pid, SIGKILL) }
        return true
    }

    /// Quit: every group is killed, as tether's cleanup does.
    func stopAll() {
        for (runner, pid) in pids {
            if suspended.remove(runner) != nil {
                stopSuspended(pid)
                pids[runner] = nil
                handler?.notStarted(runner, errno: ECANCELED)
            } else {
                killpg(pid, SIGKILL)
            }
        }
    }

    private func stopSuspended(_ pid: pid_t) {
        kill(pid, SIGKILL)
        while waitpid(pid, nil, 0) < 0 && errno == EINTR {}
    }
}

final class SwapBox: @unchecked Sendable {
    var checkout: URL?
    private var done = false

    /// Moves the checkout away and puts a new directory at its path, once.
    func swap() {
        guard let checkout, !done else { return }
        done = true
        let away = checkout.deletingLastPathComponent().appendingPathComponent("moved-" + randomHex(4))
        try? FileManager.default.moveItem(at: checkout, to: away)
        try? FileManager.default.createDirectory(at: checkout, withIntermediateDirectories: true)
    }
}

@MainActor
final class RunHarness {
    let root: URL
    let checkout: URL
    let journal: Journal
    let handler: RunHandler
    let launcher: FakeLauncher
    let log: URL
    var checkouts: [CheckoutSettings]
    let environment: [String: String]
    /// Every record delivered so far, so each test can read them more than once.
    private var delivered: [JSON] = []

    init(models: [String]? = nil, duringRefresh: (@Sendable () -> Void)? = nil) throws {
        root = URL(fileURLWithPath: realpath(FileManager.default.temporaryDirectory.path, nil).map { p in
            defer { free(p) }
            return String(cString: p)
        }!).appendingPathComponent("run-" + randomHex(6))
        checkout = root.appendingPathComponent("signal")
        log = root.appendingPathComponent("log")
        let bin = root.appendingPathComponent("bin")
        for directory in [checkout.appendingPathComponent(".git"), log, bin] {
            try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        }
        try Data("ref: refs/heads/main\n".utf8).write(to: checkout.appendingPathComponent(".git/HEAD"))
        let opencode = bin.appendingPathComponent("opencode")
        try Data(fakeOpencode.utf8).write(to: opencode)
        chmod(opencode.path, 0o755)
        journal = try Journal(path: root.appendingPathComponent("journal.sqlite").path, at: Date())
        checkouts = [CheckoutSettings(name: "signal", path: checkout.path)]
        environment = ["PATH": bin.path + ":/usr/bin:/bin", "HOME": root.path, "FAKE_LOG": log.path,
                       "SECRET_LOOKING": "from-the-profile", "BEACON_SUPERVISOR": "leaked"]
        let runs = root.appendingPathComponent("runs")
        launcher = FakeLauncher(runs: runs)
        var holder: RunHarness?
        let models = models, env = environment
        handler = RunHandler(
            journal: journal, checkouts: { holder?.checkouts ?? [] }, runs: runs, account: "macos:user:winton",
            tools: RunHandler.Tools(
                environment: { env },
                models: { opencode, environment in
                    duringRefresh?()
                    return models ?? OpencodeModels.list(opencode: opencode, environment: environment, timeout: 20) }))
        holder = self
        launcher.handler = handler
        handler.servicesReady(launcher)
    }

    func propose(_ prompt: String, checkout: String = "signal", model: String = "anthropic/claude-opus-5") throws -> String {
        let params: JSON = .object(["checkout": .string(checkout), "prompt": .string(prompt), "model": .string(model)])
        let proposal = Proposal(request: request("agent.run", params), client: "beacon-act", at: Date())
        try journal.add(proposal)
        return proposal.id
    }

    /// Resolves, claims and approves, as the window does. Returns the claim's refusal, if any.
    @discardableResult
    func approve(_ id: String, model: String? = nil) async throws -> String? {
        let proposal = journal.proposal(id, at: Date())!
        let resolved = try await handler.resolve(proposal, model: model)
        let approval = try FrozenApproval(proposal, origin: resolved.origin, account: resolved.account,
                                          destination: resolved.destination, model: model)
        if let refusal = handler.claim(approval) { return refusal }
        try journal.approve(approval, at: Date())
        return nil
    }

    func run(_ prompt: String) async throws -> String {
        let id = try propose(prompt)
        try await approve(id)
        try await handler.execute(id)
        return id
    }

    func until(_ what: String, timeout: TimeInterval = 20, _ check: () -> Bool) async throws {
        let deadline = Date().addingTimeInterval(timeout)
        while !check() {
            guard Date() < deadline else { throw Timeout(what: what) }
            try await Task.sleep(nanoseconds: 20_000_000)
        }
    }

    struct Timeout: Error { let what: String }

    func records(_ id: String) throws -> [JSON] {
        delivered += try deliverAll(journal)
        return delivered.filter { $0["metadata"]?["proposal"]?.stringValue == id }
    }

    var calls: Int { (try? FileManager.default.contentsOfDirectory(atPath: log.path).count) ?? 0 }

    /// What the fake opencode recorded for its one run.
    func invocation() throws -> [String: Any] {
        let files = try FileManager.default.contentsOfDirectory(at: log, includingPropertiesForKeys: nil)
        #expect(files.count == 1)
        return try JSONSerialization.jsonObject(with: Data(contentsOf: files[0])) as! [String: Any]
    }

    func living(_ pid: pid_t) -> Bool { kill(pid, 0) == 0 }
}

@MainActor
struct RunTests {
    @Test func aRunLaunchesReportsAndCloses() async throws {
        let h = try RunHarness()
        let id = try h.propose("succeed\nReview the plan, exactly as written: \"quotes\" and -flags.")
        let resolved = try await h.handler.resolve(h.journal.proposal(id, at: Date())!, model: nil)
        #expect(h.handler.models == ["anthropic/claude-opus-5", "openai/gpt-6"])
        #expect(resolved.origin == nil && resolved.account == "macos:user:winton")
        #expect(resolved.destination["branch"] == "main")
        #expect(resolved.destination["checkout_id"]?.stringValue?.hasPrefix("vol:") == true)
        #expect(resolved.destinationLabel == "signal · main · \(h.checkout.path)")
        #expect(resolved.facts["Runs as"]?.contains("--auto") == true)
        h.launcher.onAcknowledge = { [weak h] runner, pid in
            #expect(h?.handler.attribution.peer(of: pid) == .run(runner))
            #expect(h?.journal.proposal(id, at: Date())?.status == .started)
            #expect(h?.calls == 0, "the runner has not executed yet")
        }
        try await h.approve(id)
        try await h.handler.execute(id)
        try await h.until("the run to end") { !h.handler.isRunning(id) && h.journal.runnerState(id) == "runner_closed" }

        let runner = h.journal.runnerID(id)!
        #expect(h.launcher.acknowledgements == [runner])
        let records = try h.records(id)
        #expect(records.map { $0["type"]!.stringValue! } == [
            "app.action.proposed", "app.action.approved", "app.action.started", "app.action.executed",
            "app.runner.working", "app.runner.finished", "app.runner.closed",
        ])
        #expect(records[3]["metadata"]?["runner_id"]?.stringValue == runner)
        #expect(records[3]["metadata"]?["confirmed_by"] == "reply")
        #expect(records[5]["sealed"]?["metadata"]?["report"] == "Two gaps: the journal view and the Bosun watch.")
        #expect(records.allSatisfy { $0["subject"]?.stringValue == "agent-run:winton:" + runner.dropFirst(4) })

        let call = try h.invocation()
        #expect(call["argv"] as? [String] == ["run", "--standalone", "--format", "json", "--auto", "--model",
                                              "anthropic/claude-opus-5"])
        #expect(call["cwd"] as? String == h.checkout.path)
        #expect(call["prompt"] as? String == "succeed\nReview the plan, exactly as written: \"quotes\" and -flags.")
        let env = call["env"] as! [String: String]
        #expect(env["SECRET_LOOKING"] == "from-the-profile", "the login environment reaches the run")
        #expect(env["BEACON_SUPERVISOR"] == nil, "Beacon's own variables never do")
        #expect(env["PATH"] == h.environment["PATH"])
        // Once started, the launch (the environment) and the prompt are gone; the output stays, the person's alone.
        let directory = h.root.appendingPathComponent("runs/\(runner)")
        #expect(!FileManager.default.fileExists(atPath: directory.appendingPathComponent("launch").path))
        #expect(!FileManager.default.fileExists(atPath: directory.appendingPathComponent("prompt").path))
        let mode = try FileManager.default.attributesOfItem(atPath: directory.appendingPathComponent("output").path)[.posixPermissions]
        #expect((mode as? NSNumber)?.intValue == 0o600)
        let folder = try FileManager.default.attributesOfItem(atPath: directory.path)[.posixPermissions]
        #expect((folder as? NSNumber)?.intValue == 0o700)
        #expect(h.handler.claim(try FrozenApproval(h.journal.proposal(try h.propose("x"), at: Date())!, origin: nil,
                                                   account: "macos:user:winton", destination: resolved.destination)) == nil,
                "the checkout is free once the run is gone")
    }

    @Test func aTimedOutAcknowledgementFailsTheLaunchWithoutARunnerOutcome() async throws {
        let h = try RunHarness()
        h.launcher.silent = true
        let id = try await h.run("succeed")
        let runner = try #require(h.journal.runnerID(id))
        h.handler.prepared(runner, pid: 99_999)
        #expect(h.launcher.acknowledgements == [runner])
        #expect(h.journal.proposal(id, at: Date())?.status == .started)
        h.handler.notStarted(runner, errno: ETIMEDOUT)
        let records = try h.records(id)
        #expect(records.map { $0["type"] } == ["app.action.proposed", "app.action.approved", "app.action.started", "app.action.failed"])
        #expect(records.last?["metadata"]?["error"] == "launch_failed")
        #expect(records.last?["metadata"]?["confirmed_by"] == "reply")
        #expect(h.journal.runnerState(id) == nil && !h.handler.isRunning(id))
        #expect(try await h.approve(try h.propose("succeed")) == nil, "the checkout is free again")
    }

    @Test func quitDoesNotAcknowledgeAPreparedLaunch() async throws {
        let h = try RunHarness()
        h.launcher.silent = true
        let id = try await h.run("succeed")
        let runner = try #require(h.journal.runnerID(id))
        h.handler.beginShutdown()
        h.handler.prepared(runner, pid: 99_999)
        #expect(h.launcher.acknowledgements.isEmpty)
        h.handler.notStarted(runner, errno: ECANCELED)
        #expect(h.journal.outcome(id)?["error"] == "launch_failed")
        #expect(h.journal.runnerState(id) == nil)
    }

    @Test func aFailedRunSaysWhy() async throws {
        let h = try RunHarness()
        let id = try await h.run("fail")
        try await h.until("the run to end") { h.journal.runnerState(id) == "runner_closed" }
        let failed = try h.records(id).first { $0["type"] == "app.runner.failed" }!
        #expect(failed["metadata"]?["error"] == "exit_1")
        #expect(failed["metadata"]?["from"] == "runner")
        #expect(failed["sealed"]?["metadata"]?["detail"] == "The model refused the request.")

        // Without an error event, the detail is the end of what it wrote to standard error.
        let h2 = try RunHarness()
        let crashed = try await h2.run("crash")
        try await h2.until("the run to end") { h2.journal.runnerState(crashed) == "runner_closed" }
        let detail = try h2.records(crashed).first { $0["type"] == "app.runner.failed" }!
        #expect(detail["metadata"]?["error"] == "exit_3")
        #expect(detail["sealed"]?["metadata"]?["detail"] == "panic: something broke")
    }

    @Test func aLongReportKeepsItsEnd() async throws {
        let h = try RunHarness()
        let id = try await h.run("long")
        try await h.until("the run to end") { h.journal.runnerState(id) == "runner_closed" }
        let report = try h.records(id).first { $0["type"] == "app.runner.finished" }!["sealed"]!["metadata"]!["report"]!.stringValue!
        #expect(report.unicodeScalars.count == Limits.text)
        #expect(report.hasPrefix("[Beacon kept the last part of a longer report: 40010 characters in all.]"))
        #expect(report.hasSuffix("x end"))
    }

    @Test func outputPastTheLimitStopsTheRun() async throws {
        let h = try RunHarness()
        h.handler.outputLimit = 200_000
        let id = try await h.run("spam")
        try await h.until("the run to start") { h.handler.isRunning(id) }
        h.handler.checkOutput()
        try await h.until("the output to pass the limit and the run to stop") {
            h.handler.checkOutput()
            return h.journal.runnerState(id) == "runner_closed"
        }
        #expect(try h.records(id).first { $0["type"] == "app.runner.failed" }?["metadata"]?["error"] == "output_too_large")
    }

    @Test func stopAndQuitStopARunThatHangs() async throws {
        // The person's Stop.
        let h = try RunHarness()
        let id = try await h.run("hang")
        try await h.until("the run to start") { h.launcher.pids.count == 1 }
        let pid = h.launcher.pids.values.first!
        try await h.until("the fake to record itself") { h.calls == 1 }
        h.handler.stop(id)
        try await h.until("the run to stop") { h.journal.runnerState(id) == "runner_closed" }
        #expect(!h.living(pid))
        #expect(try h.records(id).first { $0["type"] == "app.runner.failed" }?["metadata"]?["error"] == "stopped")

        // Quit: the run is recorded as stopped at Quit, then closed once the supervisor confirms.
        let q = try RunHarness()
        let quit = try await q.run("hang")
        try await q.until("the run to start") { q.handler.isRunning(quit) }
        q.handler.beginShutdown()
        #expect(q.journal.runnerState(quit) == "runner_failed")
        q.launcher.stopAll()
        try await Task.sleep(nanoseconds: 300_000_000)
        q.handler.servicesStopped(verified: true)
        try await q.until("the run to close") { q.journal.runnerState(quit) == "runner_closed" }
        let failed = try q.records(quit).first { $0["type"] == "app.runner.failed" }!
        #expect(failed["metadata"]?["error"] == "stopped_at_quit")
        #expect(try q.records(quit).filter { $0["type"] == "app.runner.failed" }.count == 1)
    }

    /// Bugbot on #38: a launch whose `started` arrives after Quit began is recorded as stopped at Quit, never as
    /// working, and is never closed without that outcome, whether its end comes as `gone` or as tether's end.
    @Test func aStartAnsweredAfterQuitIsStoppedAtQuitBeforeClosed() async throws {
        for viaGone in [true, false] {
            let h = try RunHarness()
            h.launcher.silent = true // tether has not answered yet.
            let id = try await h.run("hang")
            #expect(h.journal.proposal(id, at: Date())?.status == .started)
            let runner = try #require(h.journal.runnerID(id))
            h.handler.prepared(runner, pid: 99_999)
            #expect(h.launcher.acknowledgements == [runner])
            h.handler.beginShutdown()
            h.handler.started(runner, pid: 99_999)
            #expect(h.journal.runnerState(id) == "runner_failed")
            #expect(!h.handler.isRunning(id))
            if viaGone {
                h.handler.gone(runner)
            } else {
                h.handler.servicesStopped(verified: true)
            }
            try await h.until("the run to close") { h.journal.runnerState(id) == "runner_closed" }
            let types = try h.records(id).compactMap { $0["type"]?.stringValue }
            #expect(types.suffix(3) == ["app.action.executed", "app.runner.failed", "app.runner.closed"], "\(types)")
            #expect(!types.contains("app.runner.working"))
            #expect(try h.records(id).first { $0["type"] == "app.runner.failed" }?["metadata"]?["error"] == "stopped_at_quit")
        }
    }

    /// A Stop that cannot reach tether (its end of the lease is closed) does not mark the run as stopped.
    @Test func aStopThatCannotBeSentChangesNothing() async throws {
        let h = try RunHarness()
        let id = try await h.run("hang")
        try await h.until("the run to start") { h.handler.isRunning(id) }
        h.launcher.refuseStop = true
        #expect(!h.handler.stop(id))
        h.launcher.refuseStop = false
        #expect(h.handler.stop(id))
        try await h.until("the run to stop") { h.journal.runnerState(id) == "runner_closed" }
        #expect(try h.records(id).first { $0["type"] == "app.runner.failed" }?["metadata"]?["error"] == "stopped")
    }

    @Test func aMissingOrReplacedCheckoutIsRefused() async throws {
        let h = try RunHarness()
        // Before approval: an unknown name, then a missing directory.
        let unknown = try h.propose("succeed", checkout: "other")
        await #expect(throws: RunRefusal.unknownCheckout) { try await h.approve(unknown) }

        // After approval, the directory is replaced: the start is refused, nothing launched, the checkout freed.
        let replaced = try h.propose("succeed")
        try await h.approve(replaced)
        try FileManager.default.removeItem(at: h.checkout)
        try FileManager.default.createDirectory(at: h.checkout, withIntermediateDirectories: true)
        try await h.handler.execute(replaced)
        #expect(h.journal.outcome(replaced)?["error"] == "checkout_changed")
        #expect(h.journal.outcome(replaced)?["confirmed_by"] == "pre_dispatch")

        let missing = try h.propose("succeed")
        try await h.approve(missing)
        try FileManager.default.removeItem(at: h.checkout)
        try await h.handler.execute(missing)
        #expect(h.journal.outcome(missing)?["error"] == "checkout_missing")
        await #expect(throws: RunRefusal.checkoutMissing) { try await h.approve(try h.propose("succeed")) }
        #expect(h.launcher.pids.isEmpty)
    }

    /// One run per checkout, even under another name; other checkouts are not held up.
    @Test func aBusyCheckoutStaysPending() async throws {
        let h = try RunHarness()
        let other = h.root.appendingPathComponent("beacon")
        try FileManager.default.createDirectory(at: other, withIntermediateDirectories: true)
        h.checkouts += [CheckoutSettings(name: "signal-too", path: h.checkout.path),
                        CheckoutSettings(name: "beacon", path: other.path)]
        let first = try await h.run("hang")
        try await h.until("the first run") { h.handler.isRunning(first) }

        let second = try h.propose("succeed", checkout: "signal-too")
        let refusal = try await h.approve(second)
        #expect(refusal?.contains("another run is using this checkout") == true)
        #expect(h.journal.proposal(second, at: Date())?.status == .pending)
        let resolved = try await h.handler.resolve(h.journal.proposal(second, at: Date())!, model: nil)
        #expect(resolved.facts["Busy"] != nil)

        let elsewhere = try h.propose("succeed", checkout: "beacon")
        #expect(try await h.approve(elsewhere) == nil, "another checkout is free")

        h.handler.stop(first)
        try await h.until("the first run to end") { h.journal.runnerState(first) == "runner_closed" }
        #expect(try await h.approve(second) == nil, "the checkout is free once the run is gone")
        #expect(h.journal.proposal(second, at: Date())?.status == .approved)
    }

    /// The model is the one field the person may change (D7); the run uses the approved one.
    @Test func theApprovedModelIsTheOneLaunched() async throws {
        let h = try RunHarness()
        let id = try h.propose("succeed")
        await #expect(throws: RunRefusal.modelUnavailable) {
            try await h.handler.resolve(h.journal.proposal(id, at: Date())!, model: "openai/gpt-0")
        }
        let resolved = try await h.handler.resolve(h.journal.proposal(id, at: Date())!, model: "openai/gpt-6")
        #expect(resolved.facts["Model"] == "openai/gpt-6 (proposed anthropic/claude-opus-5)")
        try await h.approve(id, model: "openai/gpt-6")
        try await h.handler.execute(id)
        try await h.until("the run to end") { h.journal.runnerState(id) == "runner_closed" }
        #expect((try h.invocation()["argv"] as? [String])?.last == "openai/gpt-6")
        let approved = try h.records(id).first { $0["type"] == "app.action.approved" }!
        #expect(approved["metadata"]?["edited"] == .array([.string("model")]))
        #expect(approved["sealed"]?["metadata"]?["original_model"] == "anthropic/claude-opus-5")
    }

    @Test func aModelNoLongerConfiguredStopsTheLaunch() async throws {
        let h = try RunHarness(models: ["anthropic/claude-opus-5"])
        let id = try h.propose("succeed")
        try await h.approve(id)
        // The same journal and checkout, but opencode's models changed before launch.
        let late = RunHandler(journal: h.journal, checkouts: { h.checkouts },
                              runs: h.root.appendingPathComponent("runs"), account: "macos:user:winton",
                              tools: .init(environment: { [env = h.environment] in env }, models: { _, _ in ["openai/gpt-6"] }))
        late.servicesReady(h.launcher)
        try await late.execute(id)
        #expect(h.journal.outcome(id)?["error"] == "model_unavailable")
        #expect(h.launcher.pids.isEmpty)
    }

    @Test func aLaunchTetherRefusesIsRecorded() async throws {
        let h = try RunHarness()
        h.launcher.refuse = true
        let id = try await h.run("succeed")
        #expect(h.journal.outcome(id)?["error"] == "supervisor_unavailable")
        let directory = h.root.appendingPathComponent("runs/\(h.journal.runnerID(id)!)")
        #expect(!FileManager.default.fileExists(atPath: directory.appendingPathComponent("launch").path))
        #expect(!FileManager.default.fileExists(atPath: directory.appendingPathComponent("prompt").path))
        // Freed: a later run in the checkout may be approved.
        h.launcher.refuse = false
        #expect(try await h.approve(try h.propose("succeed")) == nil)
    }

    /// A directory swapped in at the checkout's path after the check, while Beacon reads the models, is refused by
    /// the launcher's pinned identity: nothing runs, and the launch and prompt are gone.
    @Test func aCheckoutSwappedDuringTheLaunchIsRefused() async throws {
        let box = SwapBox()
        let h = try RunHarness(duringRefresh: { box.swap() })
        let id = try h.propose("succeed")
        try await h.approve(id)
        box.checkout = h.checkout
        try await h.handler.execute(id)
        try await h.until("tether's answer") { h.journal.proposal(id, at: Date())?.status == .failed }
        #expect(h.journal.outcome(id)?["error"] == "checkout_changed")
        #expect(h.journal.outcome(id)?["confirmed_by"] == "reply")
        #expect(h.launcher.pids.isEmpty && h.calls == 0)
        let directory = h.root.appendingPathComponent("runs/\(h.journal.runnerID(id)!)")
        #expect(!FileManager.default.fileExists(atPath: directory.appendingPathComponent("launch").path))
        #expect(!FileManager.default.fileExists(atPath: directory.appendingPathComponent("started").path))
        #expect(try await h.approve(try h.propose("succeed")) == nil, "the checkout is free again")
    }

    /// tether ending while a run works: it failed (`supervisor_ended`), then closed once tether confirmed; without
    /// confirmation it is not closed. A confirmed stop leaves no run ownership; an unconfirmed one keeps it until the
    /// next session's sweep confirms the old groups are gone.
    @Test func tetherEndingWhileARunWorks() async throws {
        for verified in [true, false] {
            let h = try RunHarness()
            let id = try await h.run("hang")
            try await h.until("the run to start") { h.handler.isRunning(id) }
            let runner = try #require(h.journal.runnerID(id))
            let pid = try #require(h.launcher.pids[runner])
            h.handler.servicesStopped(verified: verified)
            #expect(h.journal.runnerWords(id)?["error"] == "supervisor_ended")
            #expect(h.journal.runnerState(id) == (verified ? "runner_closed" : "runner_failed"))
            #expect(!h.handler.isRunning(id))
            #expect(!h.handler.isReady)
            #expect(h.handler.attribution.peer(of: pid).runner == (verified ? nil : runner))
            h.launcher.stopAll()
        }
    }

    /// Before tether is up (starting, or sweeping after a crash) a run cannot be approved: it stays pending with the
    /// reason, and nothing is consumed or recorded as failed.
    @Test func noApprovalBeforeTetherIsUp() async throws {
        let h = try RunHarness()
        h.handler.servicesStopped(verified: true)
        let id = try h.propose("succeed")
        await #expect(throws: RunRefusal.supervisorNotReady) { try await h.approve(id) }
        #expect(h.journal.proposal(id, at: Date())?.status == .pending)
        h.handler.servicesReady(h.launcher)
        #expect(try await h.approve(id) == nil)
    }

    @Test func tetherStoppingDiscardsTheFrozenRunAndReadyResolvesItAgain() async throws {
        let h = try RunHarness(models: ["anthropic/claude-opus-5", "openai/gpt-6"])
        let desk = ApprovalDesk(journal: h.journal, resolver: h.handler, runServicesReady: { h.handler.isReady })
        h.handler.onAvailabilityChanged = { [weak desk] in desk?.runAvailabilityChanged() }
        desk.claim = { h.handler.claim($0) }
        var approved: [String] = []
        desk.onApproved = { approved.append($0) }
        let id = try h.propose("succeed")
        desk.select(id)
        await desk.freezing?.value
        let original = try #require(desk.frozen)

        h.handler.servicesStopped(verified: false)
        #expect(desk.frozen == nil)
        #expect(desk.problem?.contains("supervisor is not ready") == true)
        #expect(!desk.approve(original.token))
        desk.readAgain()
        desk.choose(model: "openai/gpt-6")
        #expect(desk.freezing == nil && desk.frozen == nil)
        #expect(desk.pending.map(\.id) == [id])
        #expect(h.journal.proposal(id, at: Date())?.status == .pending && approved.isEmpty)
        #expect(try h.records(id).map { $0["type"] } == ["app.action.proposed"])

        h.handler.servicesReady(h.launcher)
        await desk.freezing?.value
        let refreshed = try #require(desk.frozen)
        #expect(refreshed.approval.params["model"] == "openai/gpt-6")
        #expect(desk.problem == nil && refreshed.token != original.token)
        #expect(!desk.approve(original.token))
        #expect(desk.approve(refreshed.token) && approved == [id])
    }

    /// tether ending without confirming: an unanswered launch is unknown (`supervisor_ended`); a launched run is
    /// left unclosed, so Quit or the next launch sweeps it, and the next session's recovery closes it.
    @Test func anUnconfirmedEndLeavesRunsForTheSweep() async throws {
        let h = try RunHarness()
        let working = try await h.run("hang")
        var swept = false
        defer { if !swept { h.launcher.stopAll() } }
        try await h.until("the run to start") { h.handler.isRunning(working) }
        let runner = try #require(h.journal.runnerID(working))
        let pid = try #require(h.launcher.pids[runner])
        h.launcher.silent = true
        h.checkouts.append(CheckoutSettings(name: "other", path: h.root.appendingPathComponent("log").path))
        let unanswered = try h.propose("succeed", checkout: "other")
        try await h.approve(unanswered)
        try await h.handler.execute(unanswered)
        h.handler.servicesStopped(verified: false)
        #expect(h.journal.proposal(unanswered, at: Date())?.status == .outcomeUnknown)
        #expect(try h.records(unanswered).last?["metadata"]?["reason"] == "supervisor_ended")
        #expect(h.handler.hasUnclosedRuns)
        // The old groups stay recorded through the sweep, so a survivor is still that run's until it confirms.
        #expect(h.handler.attribution.peer(of: pid) == .run(runner))
        #expect(h.handler.attribution.peer(of: getpid()) == .unknown)

        // A ready signal alone isn't proof that the old group is gone: its members may not have had the marker.
        h.handler.servicesReady(h.launcher)
        #expect(!h.handler.isReady && h.handler.hasUnclosedRuns)
        #expect(h.handler.attribution.peer(of: pid) == .run(runner))
        // The sweep only finishes once the old group is gone, so it must still be there.
        #expect(kill(-pid, 0) == 0)
        h.launcher.stopAll()
        swept = true
        try await h.until("the old process group to be gone") { kill(-pid, 0) == -1 && errno == ESRCH }
        h.handler.checkOutput() // the handler retries this check until recovery can finish
        #expect(h.journal.runnerState(working) == "runner_closed")
        #expect(!h.handler.hasUnclosedRuns)
        #expect(h.handler.isReady)
        let external = h.handler.attribution.peer(of: getpid())
        #expect(external.runner == nil && h.handler.attribution.canAccessProposals(external))
    }

    /// Quit after an unconfirmed stop opens a session only to sweep: its ready signal closes the old runs, but
    /// approvals stay off.
    @Test func aQuitOnlySweepClosesOldRunsWithoutReenablingApproval() async throws {
        let h = try RunHarness()
        let working = try await h.run("hang")
        try await h.until("the run to start") { h.handler.isRunning(working) }
        let runner = try #require(h.journal.runnerID(working))
        let pid = try #require(h.launcher.pids[runner])
        h.handler.servicesStopped(verified: false)
        h.launcher.stopAll()
        try await h.until("the old process group to be gone") { kill(-pid, 0) == -1 && errno == ESRCH }

        h.handler.beginShutdown()
        h.handler.servicesReady(h.launcher)
        #expect(h.journal.runnerState(working) == "runner_closed")
        #expect(!h.handler.hasUnclosedRuns)
        #expect(!h.handler.isReady)
        let id = try h.propose("succeed")
        await #expect(throws: RunRefusal.supervisorNotReady) { try await h.approve(id) }
        #expect(h.journal.proposal(id, at: Date())?.status == .pending)
    }

    @Test func errorsCountTowardTheOutputLimit() async throws {
        let h = try RunHarness()
        h.handler.outputLimit = 200_000
        let id = try await h.run("spamerr")
        try await h.until("the run to start") { h.handler.isRunning(id) }
        try await h.until("errors past the limit to stop the run") {
            h.handler.checkOutput()
            return h.journal.runnerState(id) == "runner_closed"
        }
        #expect(h.journal.runnerWords(id)?["error"] == "output_too_large")
    }

    /// A process in a run's group is that run's; anything else, or a run that is gone, is nobody's.
    @Test func aRunsProcessesAreAttributedToIt() async throws {
        let h = try RunHarness()
        let id = try await h.run("hang")
        try await h.until("the run and its child") { h.calls == 1 }
        let runner = h.journal.runnerID(id)!
        let leader = h.launcher.pids[runner]!
        #expect(h.handler.attribution.peer(of: leader) == .run(runner))
        #expect(h.handler.attribution.peer(of: getpid()).runner == nil)
        h.handler.stop(id)
        try await h.until("the run to end") { h.journal.runnerState(id) == "runner_closed" }
        #expect(h.handler.attribution.peer(of: leader).runner == nil)
    }

    /// After a crash: a launched run not closed failed as interrupted; a start with no answer is read back.
    @Test func recoveryAfterARelaunch() async throws {
        let h = try RunHarness()
        h.launcher.silent = true
        let lost = try await h.run("succeed")
        let lostLaunch = h.root.appendingPathComponent("runs/\(h.journal.runnerID(lost)!)/launch")
        #expect(FileManager.default.fileExists(atPath: lostLaunch.path))
        h.handler.supervisorEnded()
        #expect(!FileManager.default.fileExists(atPath: lostLaunch.path), "the environment is not left behind")
        #expect(h.journal.proposal(lost, at: Date())?.status == .outcomeUnknown)
        // Whether it started is unknown, so its checkout stays claimed in this session.
        #expect(try await h.approve(try h.propose("succeed"))?.contains("another run") == true)

        // Journal states a crash leaves behind, written directly.
        func approveDirectly(_ id: String) throws {
            let proposal = h.journal.proposal(id, at: Date())!
            try h.journal.approve(try FrozenApproval(proposal, origin: nil, account: "macos:user:winton",
                                                     destination: .object(["checkout_id": "vol:1/inode:1", "branch": "main"])),
                                  at: Date())
        }
        let launched = try h.propose("succeed")
        try approveDirectly(launched)
        _ = try h.journal.start(launched, at: Date())
        try h.journal.result(launched, executed: true, confirmedBy: "reply", fields: [:], at: Date())
        try h.journal.runner(launched, .working, at: Date())
        let begun = try h.propose("succeed")
        try approveDirectly(begun)
        _ = try h.journal.start(begun, at: Date())
        try h.journal.outcomeUnknown(begun, reason: "app_restarted", at: Date())
        let directory = h.root.appendingPathComponent("runs/\(h.journal.runnerID(begun)!)")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        // tether's mark that it started the run. An output file alone proves nothing: tether creates it first.
        FileManager.default.createFile(atPath: directory.appendingPathComponent("started").path, contents: Data("42\n".utf8))
        let lostDirectory = h.root.appendingPathComponent("runs/\(h.journal.runnerID(lost)!)")
        FileManager.default.createFile(atPath: lostDirectory.appendingPathComponent("output").path, contents: Data())
        // What a crash during a launch leaves: the launch (the environment) and the prompt.
        let crashed = h.root.appendingPathComponent("runs/run_crashed")
        try FileManager.default.createDirectory(at: crashed, withIntermediateDirectories: true)
        for name in ["launch", "prompt"] {
            FileManager.default.createFile(atPath: crashed.appendingPathComponent(name).path, contents: Data("TOKEN=x".utf8))
        }

        let relaunched = RunHandler(journal: h.journal, checkouts: { [] }, runs: h.root.appendingPathComponent("runs"),
                                    tools: .init(environment: { nil }, models: { _, _ in nil }))
        relaunched.recover()
        #expect(!FileManager.default.fileExists(atPath: crashed.appendingPathComponent("launch").path))
        #expect(!FileManager.default.fileExists(atPath: crashed.appendingPathComponent("prompt").path))
        #expect(h.journal.outcome(lost)?["error"] == "not_launched", "an output file without tether's mark")
        #expect(h.journal.outcome(lost)?["confirmed_by"] == "read_back")
        #expect(h.journal.runnerState(launched) == "runner_closed")
        #expect(h.journal.runnerWords(launched)?["error"] == "interrupted")
        #expect(h.journal.proposal(begun, at: Date())?.status == .executed)
        #expect(h.journal.outcome(begun)?["confirmed_by"] == "read_back")
        #expect(h.journal.runnerState(begun) == "runner_closed")
        // Nothing is recorded twice.
        let count = try deliverAll(h.journal).count
        relaunched.recover()
        #expect(try deliverAll(h.journal).count == 0)
        #expect(count > 0)
    }
}

struct RunSupportTests {
    @Test func theReportIsTheLastMessage() throws {
        let url = FileManager.default.temporaryDirectory.appendingPathComponent("report-" + randomHex(6))
        let lines = [
            #"{"type":"text","part":{"id":"prt_1","messageID":"msg_1","text":"first"}}"#,
            "not json",
            #"{"type":"text","part":{"id":"prt_2","messageID":"msg_2","text":"Part one."}}"#,
            #"{"type":"text","part":{"id":"prt_3","messageID":"msg_2","text":"Part two"}}"#,
            #"{"type":"text","part":{"id":"prt_msg_2_text-1","messageID":"msg_2","text":", continued."}}"#,
            #"{"type":"error","error":{"name":"X","data":{"message":"late error"}}}"#,
        ]
        try Data((lines.joined(separator: "\n")).utf8).write(to: url)
        #expect(RunReport.read(url) == RunReport(report: "Part one.\n\nPart two, continued.", error: "late error"))
        try Data().write(to: url)
        #expect(RunReport.read(url) == RunReport(report: nil, error: nil))
    }

    @Test func theLoginEnvironmentIsParsedAfterItsMarker() {
        let raw = Data("profile noise\0__M__\0PATH=/a:/b\0EMPTY=\0BEACON_TETHERED=1\0=bad\0X=1=2\0".utf8)
        #expect(LoginEnvironment.parse(raw, marker: "__M__") == ["PATH": "/a:/b", "EMPTY": "", "X": "1=2"])
        #expect(LoginEnvironment.parse(Data("PATH=/x\0".utf8), marker: "__M__") == nil)
    }

    @Test func checkoutIdentityReadsTheBranch() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent("ident-" + randomHex(6))
        let main = root.appendingPathComponent("main"), worktree = root.appendingPathComponent("wt")
        try FileManager.default.createDirectory(at: main.appendingPathComponent(".git/worktrees/wt"), withIntermediateDirectories: true)
        try FileManager.default.createDirectory(at: worktree.appendingPathComponent("bin"), withIntermediateDirectories: true)
        try Data("ref: refs/heads/plan-13\n".utf8).write(to: main.appendingPathComponent(".git/HEAD"))
        try Data((String(repeating: "a", count: 40) + "\n").utf8).write(to: main.appendingPathComponent(".git/worktrees/wt/HEAD"))
        try Data("gitdir: \(main.path)/.git/worktrees/wt\n".utf8).write(to: worktree.appendingPathComponent(".git"))
        FileManager.default.createFile(atPath: worktree.appendingPathComponent("bin/run").path, contents: Data(),
                                       attributes: [.posixPermissions: 0o755])
        #expect(CheckoutIdentity.read(main.path)?.branch == "plan-13")
        #expect(CheckoutIdentity.read(main.path)?.ownRunner == false)
        let wt = CheckoutIdentity.read(worktree.path)
        #expect(wt?.branch == "detached:aaaaaaa" && wt?.ownRunner == true)
        #expect(CheckoutIdentity.read(root.path)?.branch == "none")
        #expect(CheckoutIdentity.read(root.appendingPathComponent("missing").path) == nil)
        #expect(CheckoutIdentity.read(main.path)?.id != CheckoutIdentity.read(worktree.path)?.id)
        var info = stat()
        #expect(lstat(main.path, &info) == 0)
        let read = try #require(CheckoutIdentity.read(main.path))
        #expect(read.id == CheckoutIdentity.id(device: info.st_dev, inode: info.st_ino))
        #expect(CheckoutIdentity.pin(read.id) != nil)
    }

    /// Darwin's device number is signed. The id, the launch pin and tether's check use its bits, so a negative
    /// device still launches. The old signed decimal made `pin` nil and the approved run `launch_failed`.
    @Test func aNegativeDeviceIsTheSameVolumeAtThePinAndTheCheck() throws {
        let device = dev_t(-1)
        #expect(device < 0)
        let inode: ino_t = 9
        let id = CheckoutIdentity.id(device: device, inode: inode)
        #expect(id == "vol:4294967295/inode:9")
        let pin = try #require(CheckoutIdentity.pin(id))
        #expect(pin == "4294967295:9")
        let parts = pin.split(separator: ":")
        #expect(tether_same_identity(device, inode, UInt64(parts[0])!, UInt64(parts[1])!) == 1)
        #expect(CheckoutIdentity.pin("vol:-1/inode:9") == nil)
        // strtoull of the signed decimal is not this device, and a value above 32 bits must not alias to it.
        #expect(tether_same_identity(device, inode, UInt64.max, inode) == 0)
        #expect(tether_same_identity(device, inode, 4294967296, inode) == 0)
        let minimum = dev_t(Int32.min)
        #expect(minimum < 0)
        #expect(CheckoutIdentity.pin(CheckoutIdentity.id(device: minimum, inode: 1)) == "2147483648:1")
        #expect(tether_same_identity(minimum, 1, 2147483648, 1) == 1)
    }
}
