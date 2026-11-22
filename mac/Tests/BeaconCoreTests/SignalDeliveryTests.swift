import Foundation
import Testing
@testable import BeaconCore

/// A fake Signal behind a URL protocol, keeping app records by the contract's rules (docs/approval-app.md): the
/// same bytes under a stored key replay with `200`, different bytes are `409 idempotency_conflict`, a key out of
/// journal order is `409 conflict` with `journal_order`, and a new journal is accepted only at sequence 1 with a
/// later id. It can refuse one key as a `transition`, lose an answer after storing, or be down.
final class FakeSignalServer: @unchecked Sendable {
    private static let lock = NSLock()
    nonisolated(unsafe) private static var byHost: [String: FakeSignalServer] = [:]

    let host = "signal-\(randomHex(4)).test"
    var origin: String { "https://\(host)" }
    let key = "sig_app_fixture_key"
    private let lock = NSLock()
    private(set) var stored: [(key: String, bytes: Data)] = []
    private(set) var authorizations: [String?] = []
    var transitionAt: String?
    var loseAnswerAt: String?
    var unavailable = false
    var me: [String: Any] = ["actor": ["id": "act_01m6a6ft00e81840g2081040g3", "type": "integration", "name": "Beacon",
                                       "handle": "beacon-winton"], "scopes": ["read", "write"]]

    init() {
        FakeSignalServer.lock.withLock { FakeSignalServer.byHost[host] = self }
    }

    deinit {
        _ = FakeSignalServer.lock.withLock { FakeSignalServer.byHost.removeValue(forKey: host) }
    }

    static var configuration: URLSessionConfiguration {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [FakeSignalProtocol.self]
        return configuration
    }

    static func handler(for host: String) -> FakeSignalServer? {
        lock.withLock { byHost[host] }
    }

    func update(_ body: (FakeSignalServer) -> Void) {
        lock.withLock { body(self) }
    }

    enum Answer {
        case json(Int, Any)
        case lost
    }

    func handle(_ request: URLRequest, body: Data) -> Answer {
        lock.withLock {
            let authorization = request.value(forHTTPHeaderField: "Authorization")
            authorizations.append(authorization)
            guard authorization == "Bearer " + key else { return .json(401, problem("unauthorized")) }
            switch (request.httpMethod, request.url?.path) {
            case ("GET", "/api/me"):
                return .json(200, me)
            case ("POST", "/api/app-records"):
                if unavailable { return .json(503, problem("unavailable")) }
                guard let record = try? JSON.parse(body), let key = record["key"]?.stringValue else {
                    return .json(400, problem("invalid"))
                }
                if let existing = stored.first(where: { $0.key == key }) {
                    return existing.bytes == body ? .json(200, ["event_id": "evt_\(key)", "replayed": true])
                        : .json(409, problem("idempotency_conflict"))
                }
                if transitionAt == key { return .json(409, problem("conflict", reason: "transition")) }
                if let last = stored.last {
                    let parts = key.split(separator: ":"), lastParts = last.key.split(separator: ":")
                    let ok = parts[1] == lastParts[1] ? Int(parts[2])! > Int(lastParts[2])!
                        : (parts[2] == "1" && parts[1] > lastParts[1])
                    if !ok { return .json(409, problem("conflict", reason: "journal_order")) }
                }
                stored.append((key, body))
                if loseAnswerAt == key {
                    loseAnswerAt = nil
                    return .lost
                }
                return .json(201, ["event_id": "evt_\(key)", "replayed": false])
            default:
                return .json(404, problem("not_found"))
            }
        }
    }

    private func problem(_ code: String, reason: String? = nil) -> [String: Any] {
        var error: [String: Any] = ["code": code, "message": "fake"]
        if let reason { error["details"] = ["reason": reason] }
        return ["error": error]
    }
}

final class FakeSignalProtocol: URLProtocol {
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        guard let host = request.url?.host, let fake = FakeSignalServer.handler(for: host) else {
            client?.urlProtocol(self, didFailWithError: URLError(.cannotFindHost))
            return
        }
        var body = request.httpBody ?? Data()
        if request.httpBody == nil, let stream = request.httpBodyStream {
            stream.open()
            var buffer = [UInt8](repeating: 0, count: 65536)
            while stream.hasBytesAvailable {
                let n = stream.read(&buffer, maxLength: buffer.count)
                if n <= 0 { break }
                body.append(buffer, count: n)
            }
            stream.close()
        }
        switch fake.handle(request, body: body) {
        case .json(let status, let value):
            let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1",
                                           headerFields: ["Content-Type": "application/json"])!
            client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: try! JSONSerialization.data(withJSONObject: value))
            client?.urlProtocolDidFinishLoading(self)
        case .lost:
            client?.urlProtocol(self, didFailWithError: URLError(.networkConnectionLost))
        }
    }

    override func stopLoading() {}
}

struct SignalDeliveryTests {
    func transport(_ signal: FakeSignalServer) throws -> SignalRecords {
        SignalRecords(client: try ServiceClient(origin: signal.origin, authorization: Secret(signal.key),
                                                configuration: FakeSignalServer.configuration))
    }

    /// Signal's fixture journal, replayed through Beacon's journal and delivered over HTTP: Signal stores the
    /// fixture's bodies, in order, each once, with the app's key.
    @Test func deliversTheFixtureJournalInOrder() async throws {
        let signal = FakeSignalServer()
        let (journal, entries) = try replaySignalFixture()
        let sender = OutboxSender(journal: journal, transport: try transport(signal), person: person)
        #expect(try await sender.drain() == .done(sent: entries.count))
        #expect(signal.stored.map(\.key) == (1...entries.count).map { "beacon:\(journal.id):\($0)" })
        for (entry, stored) in zip(entries, signal.stored) {
            var expected = entry["body"]!.objectValue!
            if expected["metadata"]!["action"] != "github.merge" { expected["subject"] = .string("signal:actor:\(person)") }
            expected["key"] = .string(stored.key)
            #expect(stored.bytes == JSON.object(expected).canonicalData, "\(entry["note"]!.stringValue!)")
        }
        #expect(signal.authorizations.allSatisfy { $0 == "Bearer " + signal.key })
        #expect(try await sender.drain() == .done(sent: 0))
        #expect(signal.stored.count == entries.count)
    }

    @Test func aLostAnswerIsReplayedAndStoredOnce() async throws {
        let signal = FakeSignalServer()
        let (journal, _) = try filledJournal(scratchPath(), at: Date(timeIntervalSince1970: 1_800_000_000))
        signal.loseAnswerAt = "beacon:\(journal.id):2"
        let sender = OutboxSender(journal: journal, transport: try transport(signal), person: person)
        #expect(try await sender.drain() == .retry(sent: 1))
        #expect(try await sender.drain() == .done(sent: 2))
        #expect(signal.stored.map(\.key) == (1...3).map { "beacon:\(journal.id):\($0)" })
        signal.unavailable = true
        try journal.decline(journal.pending(at: Date(timeIntervalSince1970: 1_800_000_010)).first!.id, note: nil,
                            at: Date(timeIntervalSince1970: 1_800_000_010))
        #expect(try await sender.drain() == .retry(sent: 0))
        signal.unavailable = false
        #expect(try await sender.drain() == .done(sent: 1))
    }

    /// Signal refuses a record as an invalid transition: delivery stops there, and nothing after it is sent.
    @Test func aTransitionConflictStopsDelivery() async throws {
        let signal = FakeSignalServer()
        let (journal, _) = try filledJournal(scratchPath(), at: Date(timeIntervalSince1970: 1_800_000_000))
        signal.transitionAt = "beacon:\(journal.id):2"
        let sender = OutboxSender(journal: journal, transport: try transport(signal), person: person)
        #expect(try await sender.drain() == .stopped(sent: 1, reason: "conflict:transition"))
        #expect(try await sender.drain() == .stopped(sent: 0, reason: "conflict:transition"))
        #expect(signal.stored.count == 1)
        #expect(try journal.deliveryStatus().stopped == "conflict:transition")
    }

    /// A reset store starts a new journal: Signal takes it from sequence 1, and from then on refuses the old one.
    @Test func aNewJournalReplacesTheOld() async throws {
        let signal = FakeSignalServer()
        let t0 = Date(timeIntervalSince1970: 1_800_000_000)
        let (old, oldIDs) = try filledJournal(scratchPath(), at: t0, count: 2)
        _ = try await OutboxSender(journal: old, transport: try transport(signal), person: person).drain()
        try await Task.sleep(nanoseconds: 2_000_000)
        let (fresh, _) = try filledJournal(scratchPath(), at: t0, count: 1)
        #expect(try await OutboxSender(journal: fresh, transport: try transport(signal), person: person).drain() == .done(sent: 1))
        try old.decline(oldIDs[0], note: nil, at: t0 + 5)
        #expect(try await OutboxSender(journal: old, transport: try transport(signal), person: person).drain()
                == .stopped(sent: 0, reason: "conflict:journal_order"))
    }

    @Test func settingsAcceptOnlyTheAppActorsKey() async throws {
        let signal = FakeSignalServer()
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent("signal-" + randomHex(6))
        let vault = Vault(url: directory.appendingPathComponent("app/vault"), key: FixtureVaultKey())
        let url = directory.appendingPathComponent("app/settings.json")
        func register(_ key: String = "sig_app_fixture_key", person: String = person) async throws -> SignalSettings {
            try await SignalSetup.register(key: Secret(key), origin: signal.origin, person: person, vault: vault,
                                           settings: url, configuration: FakeSignalServer.configuration)
        }
        await #expect(throws: SignalSetup.Refusal.person) { try await register(person: "@winton") }
        await #expect(throws: SignalSetup.Refusal.rejected) { try await register("wrong") }
        signal.me["scopes"] = ["read", "write", "relay"]
        await #expect(throws: SignalSetup.Refusal.scopes) { try await register() }
        signal.me["scopes"] = ["read", "write"]
        signal.me["actor"] = ["id": "act_01m6a6ft00e81840g2081040g3", "type": "human", "handle": "winton"]
        await #expect(throws: SignalSetup.Refusal.notAnApp) { try await register() }
        signal.me["actor"] = ["id": "act_01m6a6ft00e81840g2081040g3", "type": "integration", "handle": "beacon-winton"]
        let settings = try await register()
        #expect(settings.actor == "act_01m6a6ft00e81840g2081040g3" && settings.person == person)
        #expect(try vault.credential(for: try settings.binding).value == signal.key)
        #expect(try String(contentsOf: url, encoding: .utf8).contains(signal.key) == false)

        // The registered key reaches Signal through the vault, bound to that origin and actor.
        let (journal, _) = try filledJournal(scratchPath(), at: Date(timeIntervalSince1970: 1_800_000_000), count: 1)
        let transport = try SignalRecords.connect(settings, vault: vault, configuration: FakeSignalServer.configuration)
        #expect(try await OutboxSender(journal: journal, transport: transport, person: settings.person).drain() == .done(sent: 1))
        try SignalSetup.remove(vault: vault, settings: url)
        #expect(throws: VaultError.notFound) { try vault.credential(for: try settings.binding) }
    }
}
