import BeaconSecurity
import Darwin
import Foundation
import Testing
@testable import BeaconCore

/// The test process connects to its own listener, so the kernel's peer check runs for real: the admitted
/// requirement is this process's own cdhash.
func selfRequirement() -> String {
    var buffer = [CChar](repeating: 0, count: 256)
    precondition(beacon_self_cdhash_requirement(&buffer, buffer.count))
    return String(decoding: buffer.prefix { $0 != 0 }.map { UInt8(bitPattern: $0) }, as: UTF8.self)
}

/// Vary the ancestry returned for the signed test client without moving the entire test process between groups.
private struct TestProcesses {
    let attribution: RunAttribution
    private let table: Locked<[pid_t: RunAttribution.ProcessInfo]>

    init() {
        let table = Locked([getpid(): RunAttribution.ProcessInfo(group: 10, parent: 1)])
        self.table = table
        attribution = RunAttribution(processInfo: { pid in table.withLock { $0[pid] } })
    }

    func set(group: pid_t, parent: pid_t = 1, for pid: pid_t = getpid()) {
        table.withLock { $0[pid] = .init(group: group, parent: parent) }
    }
}

final class Clock: @unchecked Sendable {
    private let lock = NSLock()
    private var date = Date(timeIntervalSince1970: 1_800_000_000)

    var now: Date {
        lock.lock()
        defer { lock.unlock() }
        return date
    }

    func advance(_ seconds: TimeInterval) {
        lock.lock()
        date += seconds
        lock.unlock()
    }
}

final class Harness: @unchecked Sendable {
    let directory: String
    let path: String
    let queue = DispatchQueue(label: "test.proposals")
    let book = MemoryBook()
    let clock = Clock()
    let server: ProposalServer

    init(clients: [AdmittedClient] = [AdmittedClient(name: "beacon-act", requirement: selfRequirement())],
         enabled: [ActionType] = ActionType.allCases, attribution: RunAttribution? = nil,
         limits: ServerLimits = ServerLimits(), slackLookup: ProposalServer.SlackLookup? = nil) throws {
        directory = FileManager.default.temporaryDirectory.appendingPathComponent("sock-" + randomHex(4)).path
        path = directory + "/run/act.sock"
        try FileManager.default.createDirectory(atPath: directory, withIntermediateDirectories: true)
        let clock = self.clock
        server = ProposalServer(path: path, clients: clients, book: book, enabled: enabled, attribution: attribution, limits: limits,
                                now: { clock.now }, queue: queue, slackLookup: slackLookup)
        try server.start()
    }

    deinit {
        server.stop()
        try? FileManager.default.removeItem(atPath: directory)
    }

    func connect() throws -> Client { try Client(path: path) }
}

/// A plain blocking client. It does no peer check of its own: the tests here are about the server's.
final class Client {
    let fd: Int32

    init(path: String) throws {
        var address = try ProposalServer.address(path)
        let socket = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
        guard socket >= 0 else { throw ServerError.system(errno) }
        var timeout = timeval(tv_sec: 30, tv_usec: 0), one: Int32 = 1
        let configured = setsockopt(socket, SOL_SOCKET, SO_RCVTIMEO, &timeout, socklen_t(MemoryLayout<timeval>.size)) == 0
            && setsockopt(socket, SOL_SOCKET, SO_SNDTIMEO, &timeout, socklen_t(MemoryLayout<timeval>.size)) == 0
            && setsockopt(socket, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size)) == 0
        let connected = withUnsafePointer(to: &address) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                configured && Darwin.connect(socket, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) == 0
            }
        }
        guard connected else {
            let code = errno
            close(socket)
            throw ServerError.system(code)
        }
        fd = socket
    }

    deinit { close(fd) }

    func send(_ bytes: Data) {
        _ = bytes.withUnsafeBytes { write(fd, $0.baseAddress!, $0.count) }
    }

    func send(_ value: JSON) {
        var data = value.canonicalData
        data.append(UInt8(ascii: "\n"))
        send(data)
    }

    /// One reply line, or nil at EOF.
    func receive() -> JSON? {
        var line = Data()
        var byte: UInt8 = 0
        while read(fd, &byte, 1) == 1 {
            if byte == UInt8(ascii: "\n") { return try? JSON.parse(line) }
            line.append(byte)
        }
        return nil
    }

    func call(_ value: JSON) -> JSON? {
        send(value)
        return receive()
    }

    /// Whether the server closed without sending anything.
    func closedSilently() -> Bool {
        var byte: UInt8 = 0
        return read(fd, &byte, 1) == 0
    }
}

func mergeProposal(number: Int64 = 140, extra: [String: JSON] = [:]) -> JSON {
    var request: [String: JSON] = [
        "op": "propose", "type": "github.merge", "reason": "The review is approved.",
        "params": .object(["repo": "CrossTerra/signal", "number": .int(number), "base": "main",
                           "head": .string(String(repeating: "a", count: 40)), "method": "squash"]),
        "agent": .object(["name": "bosun", "session": "ses_1"]),
        "context": .object(["subject": "github:pr:crossterra/signal#140"]),
    ]
    extra.forEach { request[$0] = $1 }
    return .object(request)
}

struct ProposalServerTests {
    @Test func proposeGetWithdrawForTheAdmittedPeer() throws {
        let h = try Harness(enabled: [.githubMerge])
        let client = try h.connect()
        let types = client.call(.object(["op": "types"]))
        #expect(array(types?["types"]).compactMap { $0["type"]?.stringValue } == ["github.merge"])

        let reply = try #require(client.call(mergeProposal()))
        let id = try #require(reply["id"]?.stringValue)
        #expect(PublicID.isValid(id, prefix: "apr") && reply["status"] == "pending" && reply["version"] == .int(1))
        let stored = try #require(h.queue.sync { h.book.proposal(id, at: h.clock.now) })
        #expect(reply["params_hash"]?.stringValue == ApprovalHash.params(type: "github.merge", params: stored.params, salt: stored.salt))
        #expect(stored.salt.count == 32 && stored.client == "beacon-act" && stored.agent == "bosun")

        #expect(client.call(.object(["op": "get", "id": .string(id)]))?["status"] == "pending")
        #expect(client.call(.object(["op": "withdraw", "id": .string(id)]))?["status"] == "withdrawn")
        #expect(client.call(.object(["op": "withdraw", "id": .string(id)]))?["error"] == "not_pending")
        #expect(client.call(.object(["op": "get", "id": .string(id)]))?["version"] == .int(2))
        #expect(reply["runner_id"] == nil, "only a run has a runner id")

        // A run's reply carries the runner id Beacon allocated, by which its reports are routed.
        var run = mergeProposal().objectValue!
        run["type"] = "agent.run"
        run["params"] = runParams
        run["context"] = nil
        #expect(client.call(.object(run))?["error"] == "type_disabled")
        let runs = try Harness(enabled: [.agentRun])
        let launched = try #require(try runs.connect().call(.object(run)))
        let runner = try #require(launched["runner_id"]?.stringValue)
        #expect(PublicID.isValid(runner, prefix: "run"))
        #expect(runs.queue.sync { runs.book.runnerID(launched["id"]!.stringValue!) } == runner)
    }

    /// A proposal from a process in a run's group is that run's: verified, with its runner id, and only that run
    /// reads it. Bosun's proposals are not the run's to read or withdraw, nor another run's.
    @Test func aRunsProposalsAreItsOwn() throws {
        let processes = TestProcesses(), attribution = processes.attribution
        try #require(attribution.completeSweep())
        let h = try Harness(enabled: [.githubMerge], attribution: attribution)
        let bosun = try h.connect()
        let bosunID = try #require(bosun.call(mergeProposal())?["id"]?.stringValue)

        let runner = "run_01m7x1v080ec1r60r30c1g60r3"
        // The launch handshake records the group before the run can connect.
        attribution.add(leader: 11, runner: runner)
        processes.set(group: 11)
        let run = try h.connect()
        #expect(run.call(.object(["op": "get", "id": .string(bosunID)]))?["error"] == "not_found")
        #expect(run.call(.object(["op": "withdraw", "id": .string(bosunID)]))?["error"] == "not_found")
        let runID = try #require(run.call(mergeProposal(number: 141))?["id"]?.stringValue)
        let stored = try #require(h.queue.sync { h.book.proposal(runID, at: h.clock.now) })
        #expect(stored.runner == runner)
        #expect(run.call(.object(["op": "get", "id": .string(runID)]))?["status"] == "pending")

        attribution.add(leader: 12, runner: "run_01m7x1v080ec1r60r30c1g60r4")
        processes.set(group: 12)
        let other = try h.connect()
        #expect(other.call(.object(["op": "get", "id": .string(runID)]))?["error"] == "not_found")
        processes.set(group: 10)
        #expect(bosun.call(.object(["op": "get", "id": .string(runID)]))?["error"] == "not_found")
        #expect(bosun.call(.object(["op": "get", "id": .string(bosunID)]))?["status"] == "pending")

        // A fresh peer from a known group keeps that ownership while the sweep is pending.
        attribution.sessionEnded(verified: false)
        processes.set(group: 11)
        let duringSweep = try h.connect()
        #expect(duringSweep.call(.object(["op": "get", "id": .string(runID)]))?["status"] == "pending")
        #expect(duringSweep.call(.object(["op": "withdraw", "id": .string(bosunID)]))?["error"] == "not_found")
        let anotherID = try #require(duringSweep.call(mergeProposal(number: 142))?["id"]?.stringValue)
        #expect(h.queue.sync { h.book.proposal(anotherID, at: h.clock.now)?.runner } == runner)
    }

    /// A Slack proposal's intake lookup is asynchronous. If recovery begins while it is in flight, the connection
    /// is no longer attributable and nothing is stored when the lookup returns.
    @Test func recoveryDuringASlackLookupStoresNothing() throws {
        let attribution = TestProcesses().attribution
        #expect(attribution.completeSweep())
        let entered = DispatchSemaphore(value: 0), gate = DispatchSemaphore(value: 0)
        let h = try Harness(enabled: [.slackPost], attribution: attribution, slackLookup: { _, _ in
            entered.signal()
            let released = await withCheckedContinuation { done in
                DispatchQueue.global().async { done.resume(returning: gate.wait(timeout: .now() + 30) == .success) }
            }
            try #require(released, "the Slack lookup fixture's gate was never released")
            return SlackPlacement(account: "slack:user:U0PERSON", visibility: .public)
        })
        let client = try h.connect()
        do {
            defer { gate.signal() }
            client.send(SlackHarness.frame())
            try #require(entered.wait(timeout: .now() + 30) == .success)
            attribution.sessionEnded(verified: false)
        }
        #expect(client.receive() == .object(["error": "busy"]))
        #expect(h.queue.sync { h.book.pendingCount(client: nil, at: h.clock.now) } == 0)
    }

    @Test func unknownPeersCannotUseProposalOwnershipUntilTheSweep() throws {
        let attribution = TestProcesses().attribution
        let h = try Harness(attribution: attribution)
        let beforeFirstSweep = try h.connect()
        #expect(beforeFirstSweep.call(mergeProposal())?["error"] == "busy")
        #expect(attribution.completeSweep())

        let bosun = try h.connect()
        let id = try #require(bosun.call(mergeProposal())?["id"])
        let get: JSON = .object(["op": "get", "id": id])
        let withdraw: JSON = .object(["op": "withdraw", "id": id])
        let wait: JSON = .object(["op": "wait", "id": id, "after": .int(1), "timeout": .int(30)])
        let waiter = try h.connect()
        #expect(waiter.call(get)?["status"] == "pending")
        waiter.send(wait)

        attribution.sessionEnded(verified: false)
        let unclassified = try h.connect()
        for peer in [unclassified, bosun] {
            #expect(peer.call(.object(["op": "types"]))?["types"] != nil)
            for request in [get, withdraw, wait, mergeProposal(number: 141)] {
                #expect(peer.call(request) == .object(["error": "busy"]))
            }
        }
        #expect(waiter.receive()?["error"] == "busy", "a wait in flight cannot see the result during recovery")
        #expect(h.queue.sync { h.book.pendingCount(client: nil, at: h.clock.now) } == 1)

        #expect(attribution.completeSweep())
        // None of the old sockets can acquire Bosun's identity by outlasting the sweep.
        for peer in [beforeFirstSweep, unclassified, bosun] {
            #expect(peer.call(get)?["error"] == "busy")
        }
        let reconnected = try h.connect()
        #expect(reconnected.call(get)?["status"] == "pending")
        #expect(reconnected.call(withdraw)?["status"] == "withdrawn")
    }

    /// The proposed record says who proposed: a run, verified, with its id; anyone else unverified, without one.
    @Test func theRecordNamesTheRunThatProposed() throws {
        let path = scratchPath()
        let journal = try Journal(path: path, at: Date(timeIntervalSince1970: 1_800_000_000))
        let t = Date(timeIntervalSince1970: 1_800_000_000)
        let fromRun = Proposal(request: request("github.merge", mergeParams), client: "beacon-act", at: t,
                               runner: "run_01m7x1v080ec1r60r30c1g60r3")
        let fromBosun = Proposal(request: request("github.merge", mergeParams), client: "beacon-act", at: t)
        try journal.add(fromRun)
        try journal.add(fromBosun)
        let records = try deliverAll(journal)
        #expect(records[0]["metadata"]?["proposer"]?["verified"] == .bool(true))
        #expect(records[0]["metadata"]?["proposer"]?["runner_id"] == "run_01m7x1v080ec1r60r30c1g60r3")
        #expect(records[1]["metadata"]?["proposer"]?["verified"] == .bool(false))
        #expect(records[1]["metadata"]?["proposer"]?["runner_id"] == nil)
        // The journal keeps it across a relaunch.
        let reopened = try Journal(path: path, at: t)
        #expect(reopened.proposal(fromRun.id, at: t)?.runner == "run_01m7x1v080ec1r60r30c1g60r3")
    }

    @Test func quitRequiresTheVerifiedInstallerCLIIdentity() throws {
        let attribution = TestProcesses().attribution
        try #require(attribution.completeSweep())
        let clientIdentity = AdmittedClient(name: "beacon-act", requirement: selfRequirement())
        let h = try Harness(clients: [clientIdentity], attribution: attribution)
        let client = try h.connect()
        #expect(client.call(.object(["op": "quit"])) == .object(["error": "invalid", "field": "/op"]))
        let quits = Locked(0)
        // The name alone is not the identity.
        h.server.quit = (client: .init(name: "beacon-act", requirement: "identifier \"dev.beacon.other\""),
                         executable: "", action: { quits.withLock { $0 += 1 } })
        #expect(client.call(.object(["op": "quit"])) == .object(["error": "invalid", "field": "/op"]))
        h.server.quit = (client: clientIdentity, executable: "", action: { quits.withLock { $0 += 1 } })
        #expect(client.call(.object(["op": "quit", "now": .bool(true)]))?["error"] == "invalid")
        #expect(quits.withLock { $0 } == 0)
        #expect(client.call(.object(["op": "quit"])) == .object(["status": "quitting"]))
        // Quit replies before asking the app to terminate.
        #expect(h.queue.sync { quits.withLock { $0 } } == 1)
    }

    /// Only an explicitly external peer may quit, even when all peers satisfy the installer's signature.
    @Test func runsEscapedDescendantsAndUnknownPeersCannotQuit() throws {
        let processes = TestProcesses(), attribution = processes.attribution
        let h = try Harness(attribution: attribution)
        let quits = Locked(0)
        h.server.quit = (client: .init(name: "beacon-act", requirement: selfRequirement()), executable: "",
                         action: { quits.withLock { $0 += 1 } })
        let quit: JSON = .object(["op": "quit"])
        let denied: JSON = .object(["error": "invalid", "field": "/op"])
        let beforeSweep = try h.connect()
        #expect(beforeSweep.call(quit) == denied)
        try #require(attribution.completeSweep())
        #expect(beforeSweep.call(quit) == denied, "an unknown connection must reconnect after the sweep")
        let external = try h.connect()
        #expect(external.call(quit) == .object(["status": "quitting"]))

        attribution.add(leader: 11, runner: "run_01m7x1v080ec1r60r30c1g60r3")
        processes.set(group: 11)
        let run = try h.connect()
        #expect(run.call(quit) == denied)
        let parent = getpid() == 11 ? pid_t(12) : pid_t(11)
        processes.set(group: 11, for: parent)
        processes.set(group: 13, parent: parent)
        #expect(attribution.peer(of: getpid()) == .unverifiedRun)
        let escaped = try h.connect()
        #expect(escaped.call(quit) == denied)
        #expect(escaped.call(mergeProposal())?["error"] == "busy")
        #expect(external.call(quit) == denied, "ancestry is checked again on an already-admitted socket")

        // A failed ancestry probe is not proof that a process is external.
        processes.set(group: 13, parent: getpid())
        #expect(attribution.peer(of: getpid()) == .unknown)
        #expect(try h.connect().call(quit) == denied)
        #expect(h.queue.sync { quits.withLock { $0 } } == 1)
    }

    /// The refusal the installer used to discard. A caller that is not external gets `invalid /op` and nothing quits,
    /// including before the sweep. The running app's own CLI is the exception: its kernel path may quit anyway.
    @Test func theRunningAppsCLICanQuitWhenTheCallerIsNotExternal() throws {
        let admitted = TestProcesses()
        let found = try Harness(enabled: [.githubMerge], attribution: admitted.attribution)
        try #require(admitted.attribution.completeSweep())
        let stored = try #require(try found.connect().call(mergeProposal()))
        let id = try #require(stored["id"]?.stringValue)
        let installer = try #require(found.queue.sync { found.book.proposal(id, at: found.clock.now)?.clientPath })

        let blocked = TestProcesses()
        let h = try Harness(attribution: blocked.attribution)
        let quits = Locked(0)
        let identity = AdmittedClient(name: "beacon-act", requirement: selfRequirement())
        let denied: JSON = .object(["error": "invalid", "field": "/op"])
        h.server.quit = (client: identity, executable: installer + ".new", action: { quits.withLock { $0 += 1 } })
        #expect(try h.connect().call(.object(["op": "quit"])) == denied, "a staged CLI is not the running app's CLI")
        h.server.quit = (client: identity, executable: installer, action: { quits.withLock { $0 += 1 } })
        #expect(try h.connect().call(.object(["op": "quit"])) == .object(["status": "quitting"]))
        #expect(h.queue.sync { quits.withLock { $0 } } == 1)

        blocked.attribution.add(leader: 11, runner: "run_01m7x1v080ec1r60r30c1g60r3")
        blocked.set(group: 11)
        #expect(blocked.attribution.peer(of: getpid()) == .run("run_01m7x1v080ec1r60r30c1g60r3"))
        #expect(try h.connect().call(.object(["op": "quit"])) == .object(["status": "quitting"]))
        h.server.quit = (client: identity, executable: installer + ".new", action: { quits.withLock { $0 += 1 } })
        #expect(try h.connect().call(.object(["op": "quit"])) == denied, "a run using another path still cannot quit")
        #expect(h.queue.sync { quits.withLock { $0 } } == 2)
    }

    @Test func refusesAnUnadmittedPeerWithoutReading() throws {
        let h = try Harness(clients: [AdmittedClient(name: "other", requirement: "identifier \"dev.beacon.nobody\"")])
        let client = try h.connect()
        client.send(mergeProposal())
        #expect(client.closedSilently())
        #expect(h.queue.sync { h.book.pendingCount(client: nil, at: h.clock.now) } == 0)
    }

    @Test func noOperationApprovesOrLists() throws {
        let h = try Harness()
        let client = try h.connect()
        for op in ["approve", "list", "history", "runners", "decide"] {
            #expect(client.call(.object(["op": .string(op)])) == .object(["error": "invalid", "field": "/op"]))
        }
        #expect(client.call(.object(["op": "types", "x": .int(1)]))?["error"] == "invalid")
    }

    @Test func refusesInvalidRequestsBeforeStoringAnything() throws {
        let h = try Harness(enabled: [.githubMerge])
        let client = try h.connect()
        client.send(Data("{\"op\":\"propose\",\"n\":1.0}\n".utf8))
        #expect(client.receive() == .object(["error": "invalid_frame"]))
        #expect(client.call(mergeProposal(extra: ["extra": .bool(true)])) == .object(["error": "invalid", "field": "/extra"]))
        #expect(client.call(mergeProposal(extra: ["reason": ""]))?["field"] == "/reason")
        #expect(client.call(mergeProposal(extra: ["agent": .object(["name": "bosun", "pid": .int(1)])]))?["field"] == "/agent/pid")
        var slack = mergeProposal().objectValue!
        slack["type"] = "slack.post"
        slack["params"] = .object(["workspace": "T1", "channel": "C1", "thread_ts": .null, "text": "hi"])
        #expect(client.call(.object(slack)) == .object(["error": "type_disabled"]))
        #expect(h.queue.sync { h.book.pendingCount(client: nil, at: h.clock.now) } == 0)
    }

    @Test func otherClientsProposalsReadAsNotFound() throws {
        let h = try Harness()
        let foreign = Proposal(request: try ProposalRequest(mergeProposal().objectValue!), client: "runner", at: h.clock.now)
        try h.queue.sync { try h.book.add(foreign) }
        let client = try h.connect()
        for op in ["get", "withdraw"] {
            #expect(client.call(.object(["op": .string(op), "id": .string(foreign.id)])) == .object(["error": "not_found"]))
        }
        #expect(client.call(.object(["op": "wait", "id": .string(foreign.id), "after": .int(0), "timeout": .int(1)]))?["error"] == "not_found")
        #expect(h.queue.sync { h.book.proposal(foreign.id, at: h.clock.now)?.status } == .pending)
    }

    @Test func waitAnswersOnChangeAndOnTimeout() throws {
        let h = try Harness()
        let client = try h.connect()
        let id = try #require(client.call(mergeProposal())?["id"]?.stringValue)
        // Already past `after`: an immediate answer.
        #expect(client.call(.object(["op": "wait", "id": .string(id), "after": .int(0), "timeout": .int(5)]))?["version"] == .int(1))
        client.send(.object(["op": "wait", "id": .string(id), "after": .int(1), "timeout": .int(300)]))
        h.queue.asyncAfter(deadline: .now() + 0.2) { h.book.set(id, .approved) }
        let changed = client.receive()
        #expect(changed?["status"] == "approved" && changed?["version"] == .int(2))

        client.send(.object(["op": "wait", "id": .string(id), "after": .int(2), "timeout": .int(30)]))
        Thread.sleep(forTimeInterval: 0.2) // Let the server register the wait before the clock moves.
        h.clock.advance(31)
        #expect(client.receive()?["version"] == .int(2))
        #expect(client.call(.object(["op": "wait", "id": .string(id), "after": .int(2), "timeout": .int(301)]))?["field"] == "/timeout")
    }

    @Test func boundsPendingProposalsAndWaits() throws {
        var limits = ServerLimits()
        limits.pendingPerClient = 2
        limits.waits = 1
        let h = try Harness(limits: limits)
        let client = try h.connect()
        let first = try #require(client.call(mergeProposal(number: 1))?["id"]?.stringValue)
        _ = client.call(mergeProposal(number: 2))
        #expect(client.call(mergeProposal(number: 3)) == .object(["error": "busy"]))

        client.send(.object(["op": "wait", "id": .string(first), "after": .int(1), "timeout": .int(300)]))
        let other = try h.connect()
        #expect(other.call(.object(["op": "wait", "id": .string(first), "after": .int(1), "timeout": .int(300)])) == .object(["error": "busy"]))
        // A disconnected waiter frees its slot.
        shutdown(client.fd, SHUT_RDWR) // Not close: deinit closes the descriptor once.
        Thread.sleep(forTimeInterval: 0.2)
        other.send(.object(["op": "wait", "id": .string(first), "after": .int(1), "timeout": .int(300)]))
        h.queue.asyncAfter(deadline: .now() + 0.1) { h.book.set(first, .declined) }
        #expect(other.receive()?["status"] == "declined")
    }

    @Test func boundsProposalsPerHour() throws {
        var limits = ServerLimits()
        limits.proposalsPerClientPerHour = 2
        let h = try Harness(limits: limits)
        let client = try h.connect()
        for number in 1...2 {
            let id = try #require(client.call(mergeProposal(number: Int64(number)))?["id"]?.stringValue)
            _ = client.call(.object(["op": "withdraw", "id": .string(id)]))
        }
        #expect(client.call(mergeProposal(number: 3)) == .object(["error": "busy"]))
        h.clock.advance(3601)
        #expect(client.call(mergeProposal(number: 3))?["status"] == "pending")
    }

    @Test func boundsFrames() throws {
        var limits = ServerLimits()
        limits.frameBytes = 4096
        limits.frameTimeout = 2
        let h = try Harness(limits: limits)
        let large = try h.connect()
        large.send(Data(repeating: UInt8(ascii: " "), count: 4096))
        #expect(large.receive() == .object(["error": "frame_too_large"]))
        #expect(large.receive() == nil)

        let partial = try h.connect()
        partial.send(Data("{\"op\":".utf8))
        Thread.sleep(forTimeInterval: 0.1)
        h.clock.advance(3)
        #expect(partial.receive() == nil)

        // Exactly the limit, newline included, is one frame.
        let exact = try h.connect()
        var frame = Data("{\"op\":\"types\"}".utf8)
        frame.append(Data(repeating: UInt8(ascii: " "), count: 4096 - frame.count - 1))
        frame.append(UInt8(ascii: "\n"))
        exact.send(frame)
        #expect(exact.receive()?["types"] != nil)
    }

    /// Paused in a wait, a connection cannot buffer more than one frame and a small margin.
    @Test func boundsBufferedBytesDuringAWait() throws {
        var limits = ServerLimits()
        limits.frameBytes = 4096
        let h = try Harness(limits: limits)
        let client = try h.connect()
        let id = try #require(client.call(mergeProposal())?["id"]?.stringValue)
        client.send(.object(["op": "wait", "id": .string(id), "after": .int(1), "timeout": .int(300)]))
        Thread.sleep(forTimeInterval: 0.1)
        var flood = Data()
        while flood.count <= 4096 + Connection.margin { flood.append(Data("{\"op\":\"types\"}\n".utf8)) }
        client.send(flood)
        #expect(client.receive() == .object(["error": "frame_too_large"]))
        #expect(client.receive() == nil)
        // Its wait slot is free again.
        #expect(try h.connect().call(.object(["op": "wait", "id": .string(id), "after": .int(0), "timeout": .int(1)]))?["version"] == .int(1))
    }

    @Test func boundsConnectionsAndRefusesASecondListener() throws {
        var limits = ServerLimits()
        limits.connections = 2
        let h = try Harness(limits: limits)
        let a = try h.connect(), b = try h.connect()
        #expect(a.call(.object(["op": "types"])) != nil && b.call(.object(["op": "types"])) != nil)
        #expect(try h.connect().closedSilently())

        let second = ProposalServer(path: h.path, clients: [], book: MemoryBook(), enabled: [])
        #expect(throws: ServerError.inUse) { try second.start() }
        #expect(a.call(.object(["op": "types"])) != nil)
    }
}
