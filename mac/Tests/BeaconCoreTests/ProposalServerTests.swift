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
         enabled: [ActionType] = ActionType.allCases, limits: ServerLimits = ServerLimits()) throws {
        directory = FileManager.default.temporaryDirectory.appendingPathComponent("sock-" + randomHex(4)).path
        path = directory + "/run/act.sock"
        try FileManager.default.createDirectory(atPath: directory, withIntermediateDirectories: true)
        let clock = self.clock
        server = ProposalServer(path: path, clients: clients, book: book, enabled: enabled, limits: limits,
                                now: { clock.now }, queue: queue)
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
        fd = socket(AF_UNIX, SOCK_STREAM, 0)
        var address = try ProposalServer.address(path)
        let connected = withUnsafePointer(to: &address) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { Darwin.connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
        }
        guard connected == 0 else { throw ServerError.system(errno) }
        var timeout = timeval(tv_sec: 8, tv_usec: 0)
        setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, socklen_t(MemoryLayout<timeval>.size))
        var one: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))
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
