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
    /// Answers given, in order, for a key before the usual rules: `(status, error code, field, Retry-After)`.
    var scripted: [String: [(Int, String, String?, String?)]] = [:]
    /// Refuse a record whose person is not this, as Signal does.
    var requiredPerson: String?
    private(set) var posts: [(key: String, bytes: Data)] = []
    var loseAnswerAt: String?
    var unavailable = false
    var me: [String: Any] = ["actor": ["id": "act_01m6a6ft00e81840g2081040g3", "type": "integration", "name": "Beacon",
                                       "handle": "beacon-winton"], "scopes": ["read", "write"]]
    /// `GET /api/actors/{id}`: the app actor, working for the person, and the person.
    var actors: [String: [String: Any]] = [
        "act_01m6a6ft00e81840g2081040g3": ["id": "act_01m6a6ft00e81840g2081040g3", "type": "integration",
                                           "handle": "beacon-winton", "approval_app": true, "works_for": person,
                                           "deactivated_at": NSNull()],
        person: ["id": person, "type": "human", "name": "Winton", "handle": "winton", "approval_app": false, "works_for": NSNull(),
                 "deactivated_at": NSNull()],
    ]

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
        case json(Int, Any, [String: String])
        case lost
    }

    func handle(_ request: URLRequest, body: Data) -> Answer {
        lock.withLock {
            let authorization = request.value(forHTTPHeaderField: "Authorization")
            authorizations.append(authorization)
            guard authorization == "Bearer " + key else { return .json(401, problem("unauthorized"), [:]) }
            switch (request.httpMethod, request.url?.path) {
            case ("GET", "/api/me"):
                return .json(200, me, [:])
            case ("GET", let path?) where path.hasPrefix("/api/actors/"):
                let id = String(path.dropFirst("/api/actors/".count))
                return actors[id].map { .json(200, $0, [:]) } ?? .json(404, problem("not_found"), [:])
            case ("POST", "/api/app-records"):
                if unavailable { return .json(503, problem("unavailable"), [:]) }
                guard let record = try? JSON.parse(body), let key = record["key"]?.stringValue else {
                    return .json(400, problem("invalid"), [:])
                }
                posts.append((key, body))
                if var queue = scripted[key], !queue.isEmpty {
                    let (status, code, field, retryAfter) = queue.removeFirst()
                    scripted[key] = queue
                    return .json(status, problem(code, field: field), retryAfter.map { ["Retry-After": $0] } ?? [:])
                }
                if let requiredPerson, record["metadata"]?["person"]?.stringValue != requiredPerson {
                    return .json(400, problem("invalid", field: "/metadata/person"), [:])
                }
                if let existing = stored.first(where: { $0.key == key }) {
                    return existing.bytes == body ? .json(200, ["event_id": "evt_\(key)", "replayed": true], [:])
                        : .json(409, problem("idempotency_conflict"), [:])
                }
                if transitionAt == key { return .json(409, problem("conflict", reason: "transition"), [:]) }
                if let last = stored.last {
                    let parts = key.split(separator: ":"), lastParts = last.key.split(separator: ":")
                    let ok = parts[1] == lastParts[1] ? Int(parts[2])! > Int(lastParts[2])!
                        : (parts[2] == "1" && parts[1] > lastParts[1])
                    if !ok { return .json(409, problem("conflict", reason: "journal_order"), [:]) }
                }
                stored.append((key, body))
                if loseAnswerAt == key {
                    loseAnswerAt = nil
                    return .lost
                }
                return .json(201, ["event_id": "evt_\(key)", "replayed": false], [:])
            default:
                return .json(404, problem("not_found"), [:])
            }
        }
    }

    private func problem(_ code: String, reason: String? = nil, field: String? = nil) -> [String: Any] {
        var error: [String: Any] = ["code": code, "message": "fake"]
        var details: [String: Any] = [:]
        details["reason"] = reason
        details["field"] = field
        if !details.isEmpty { error["details"] = details }
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
        case .json(let status, let value, let headers):
            let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1",
                                           headerFields: headers.merging(["Content-Type": "application/json"]) { a, _ in a })!
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
            // The repository as spelled (see reproducesSignalsFixtureJournal).
            else { expected["subject"] = "github:pr:CrossTerra/signal#140" }
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
        #expect(try await sender.drain() == .retry(sent: 1, after: nil))
        #expect(try await sender.drain() == .done(sent: 2))
        #expect(signal.stored.map(\.key) == (1...3).map { "beacon:\(journal.id):\($0)" })
        signal.unavailable = true
        try journal.decline(journal.pending(at: Date(timeIntervalSince1970: 1_800_000_010)).first!.id, note: nil,
                            at: Date(timeIntervalSince1970: 1_800_000_010))
        #expect(try await sender.drain() == .retry(sent: 0, after: nil))
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

    @Test func classifiesSignalsAnswers() {
        func answer(_ status: Int, _ code: String? = nil, reason: String? = nil, field: String? = nil,
                    retryAfter: String? = nil) -> RecordAnswer {
            SignalRecords.classify(status: status, code: code, reason: reason, field: field, retryAfter: retryAfter,
                                   eventID: "evt_1", replayed: nil)
        }
        #expect(answer(201) == .stored(eventID: "evt_1", replayed: false))
        #expect(answer(200) == .stored(eventID: "evt_1", replayed: true))
        #expect(answer(401) == .retry(after: nil, ambiguous: false))
        #expect(answer(429, retryAfter: "120") == .retry(after: 120, ambiguous: false))
        #expect(answer(403, "forbidden") == .retry(after: nil, ambiguous: false))
        #expect(answer(403, "forbidden", field: "/subject") == .stop(reason: "refused:403:/subject"))
        #expect(answer(400, "invalid", field: "/occurred_at") == .retry(after: nil, ambiguous: false))
        #expect(answer(400, "invalid", field: "/metadata/person") == .stop(reason: "refused:400:/metadata/person"))
        #expect(answer(413) == .stop(reason: "refused:413"))
        #expect(answer(409, "conflict", reason: "transition") == .stop(reason: "conflict:transition"))
        #expect(answer(409, "idempotency_conflict") == .stop(reason: "conflict:idempotency_conflict"))
        #expect(answer(503, "unavailable") == .retry(after: nil, ambiguous: true))
        #expect(answer(500) == .retry(after: nil, ambiguous: true))
    }

    @Test func retriesWaitAndRefusalsWaitForResume() async throws {
        let signal = FakeSignalServer()
        let (journal, _) = try filledJournal(scratchPath(), at: Date(timeIntervalSince1970: 1_800_000_000), count: 2)
        let first = "beacon:\(journal.id):1"
        signal.scripted[first] = [(429, "rate_limited", nil, "120"), (401, "unauthorized", nil, nil),
                                  (400, "invalid", "/occurred_at", nil), (403, "forbidden", nil, nil)]
        let sender = OutboxSender(journal: journal, transport: try transport(signal), person: person)
        #expect(try await sender.drain() == .retry(sent: 0, after: 120))
        for _ in 0..<3 { #expect(try await sender.drain() == .retry(sent: 0, after: nil)) }
        #expect(try await sender.drain() == .done(sent: 2))
        #expect(Set(signal.posts.filter { $0.key == first }.map(\.bytes)).count == 1)
    }

    /// A record Signal refused because of the person id is rebuilt after the person is corrected and delivery
    /// resumed.
    @Test func resumeRebuildsARecordSignalNeverStored() async throws {
        let signal = FakeSignalServer()
        let corrected = "act_01m6a6ft00e40r2081040g2082"
        signal.requiredPerson = corrected
        let (journal, _) = try filledJournal(scratchPath(), at: Date(timeIntervalSince1970: 1_800_000_000), count: 2)
        #expect(try await OutboxSender(journal: journal, transport: try transport(signal), person: person).drain()
                == .stopped(sent: 0, reason: "refused:400:/metadata/person"))
        #expect(try await OutboxSender(journal: journal, transport: try transport(signal), person: corrected).drain()
                == .stopped(sent: 0, reason: "refused:400:/metadata/person"))
        try journal.resumeDelivery()
        #expect(try await OutboxSender(journal: journal, transport: try transport(signal), person: corrected).drain() == .done(sent: 2))
        #expect(signal.stored.allSatisfy { (try? JSON.parse($0.bytes))?["metadata"]?["person"] == .string(corrected) })
    }

    /// A record that ever went without a definite answer keeps its exact bytes, even after a resume.
    @Test func resumeNeverRebuildsARecordSignalMayHold() async throws {
        let signal = FakeSignalServer()
        let (journal, _) = try filledJournal(scratchPath(), at: Date(timeIntervalSince1970: 1_800_000_000), count: 1)
        let key = "beacon:\(journal.id):1"
        signal.scripted[key] = [(503, "unavailable", nil, nil), (400, "invalid", "/metadata/person", nil)]
        let sender = OutboxSender(journal: journal, transport: try transport(signal), person: person)
        #expect(try await sender.drain() == .retry(sent: 0, after: nil))
        #expect(try await sender.drain() == .stopped(sent: 0, reason: "refused:400:/metadata/person"))
        try journal.resumeDelivery()
        let corrected = OutboxSender(journal: journal, transport: try transport(signal),
                                     person: "act_01m6a6ft00e40r2081040g2082")
        #expect(try await corrected.drain() == .done(sent: 1))
        #expect(Set(signal.posts.map(\.bytes)).count == 1)
    }

    /// A record that cannot be built stops delivery at its sequence with the reason, and resumes once it can.
    @Test func anUnbuildableRecordStopsAndResumes() async throws {
        let signal = FakeSignalServer()
        let (journal, _) = try filledJournal(scratchPath(), at: Date(timeIntervalSince1970: 1_800_000_000), count: 2)
        #expect(try await OutboxSender(journal: journal, transport: try transport(signal), person: "@winton").drain()
                == .stopped(sent: 0, reason: "record:/metadata/person"))
        #expect(signal.posts.isEmpty)
        try journal.resumeDelivery()
        #expect(try await OutboxSender(journal: journal, transport: try transport(signal), person: person).drain() == .done(sent: 2))
    }

    /// A setup code as Signal's /beacon page writes it.
    static func setupCode(_ fields: [String: Any], prefix: String = SignalSetupCode.prefix) -> String {
        let data = try! JSONSerialization.data(withJSONObject: fields)
        let base64url = data.base64EncodedString().replacingOccurrences(of: "+", with: "-")
            .replacingOccurrences(of: "/", with: "_").replacingOccurrences(of: "=", with: "")
        return prefix + base64url
    }

    @Test func readsTheSetupCode() throws {
        let decoded = try SignalSetupCode.decode("  " + Self.setupCode(["origin": "https://Signal.Example.com/", "key": "sig_k"]) + "\n")
        #expect(decoded.origin == "https://signal.example.com" && decoded.key.value == "sig_k")
        // Padded base64url is read too.
        let padded = SignalSetupCode.prefix + Data(#"{"origin":"https://s.test","key":"sig_k1"}"#.utf8).base64EncodedString()
        #expect(try SignalSetupCode.decode(padded).key.value == "sig_k1")

        let problems: [(SignalSetupCode.Problem, String)] = [
            (.prefix, ""),
            (.prefix, "sig_k"),
            (.prefix, Self.setupCode(["origin": "https://s.test", "key": "k"], prefix: "beacon-setup:v2:")),
            (.prefix, Self.setupCode(["origin": "https://s.test", "key": "k"], prefix: "Beacon-Setup:v1:")),
            (.encoding, SignalSetupCode.prefix),
            (.encoding, SignalSetupCode.prefix + "not base64!"),
            (.encoding, SignalSetupCode.prefix + "a"),
            (.encoding, SignalSetupCode.prefix + Data("[1]".utf8).base64EncodedString()),
            (.encoding, SignalSetupCode.prefix + Data("origin=https://s.test".utf8).base64EncodedString()),
            (.fields, Self.setupCode(["key": "sig_k"])),
            (.fields, Self.setupCode(["origin": "https://s.test"])),
            (.fields, Self.setupCode(["origin": "https://s.test", "key": ""])),
            (.fields, Self.setupCode(["origin": "https://s.test", "key": 7])),
            (.fields, Self.setupCode(["origin": "https://s.test", "key": "sig k"])),
            (.fields, Self.setupCode(["origin": "http://s.test", "key": "sig_k"])),
            (.fields, Self.setupCode(["origin": "https://s.test/api", "key": "sig_k"])),
        ]
        for (problem, code) in problems {
            #expect(throws: problem, "\(problem)") { try SignalSetupCode.decode(code) }
        }
        // A refusal never carries the key.
        do {
            _ = try SignalSetupCode.decode(Self.setupCode(["origin": "http://s.test", "key": "sig_secret"]))
        } catch {
            #expect(!"\(error)".contains("sig_secret"))
        }
    }

    @Test func settingsAcceptOnlyTheAppActorsKey() async throws {
        let signal = FakeSignalServer()
        let app = "act_01m6a6ft00e81840g2081040g3"
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent("signal-" + randomHex(6))
        let vault = Vault(url: directory.appendingPathComponent("app/vault"), key: FixtureVaultKey())
        let url = directory.appendingPathComponent("app/settings.json")
        func register(_ key: String = "sig_app_fixture_key") async throws -> SignalSettings {
            try await SignalSetup.register(code: Self.setupCode(["origin": signal.origin, "key": key]), vault: vault,
                                           settings: url, configuration: FakeSignalServer.configuration)
        }
        await #expect(throws: SignalSetup.Refusal.code(.prefix)) {
            try await SignalSetup.register(code: "sig_app_fixture_key", vault: vault, settings: url,
                                           configuration: FakeSignalServer.configuration)
        }
        await #expect(throws: SignalSetup.Refusal.code(.encoding)) {
            try await SignalSetup.register(code: SignalSetupCode.prefix + "%%%", vault: vault, settings: url,
                                           configuration: FakeSignalServer.configuration)
        }
        await #expect(throws: SignalSetup.Refusal.code(.fields)) {
            try await SignalSetup.register(code: Self.setupCode(["origin": signal.origin]), vault: vault, settings: url,
                                           configuration: FakeSignalServer.configuration)
        }
        #expect(signal.authorizations.isEmpty)
        await #expect(throws: SignalSetup.Refusal.rejected) { try await register("wrong") }
        signal.me["scopes"] = ["read", "write", "relay"]
        await #expect(throws: SignalSetup.Refusal.scopes) { try await register() }
        signal.me["scopes"] = ["read", "write"]
        signal.me["actor"] = ["id": app, "type": "human", "handle": "winton"]
        await #expect(throws: SignalSetup.Refusal.notAnApp) { try await register() }
        signal.me["actor"] = ["id": app, "type": "integration", "handle": "beacon-winton"]

        // The app actor as Signal shows it: active, marked approval_app, working for an active person.
        let good = signal.actors[app]!
        let cases: [(SignalSetup.Refusal, [String: Any])] = [
            (.notAnApp, ["deactivated_at": "2026-11-20T00:00:00Z"]),
            (.notApprovalApp, ["approval_app": false]),
            (.notApprovalApp, ["approval_app": NSNull()]),
            (.noPerson, ["works_for": NSNull()]),
            (.noPerson, ["works_for": app]),
            (.noPerson, ["works_for": "act_01m6a6ft00e81840g2081040g9"]),
        ]
        for (refusal, change) in cases {
            signal.actors[app] = good.merging(change) { $1 }
            await #expect(throws: refusal, "\(change)") { try await register() }
        }
        signal.actors[app] = good.filter { $0.key != "works_for" }
        await #expect(throws: SignalSetup.Refusal.noPerson) { try await register() }
        signal.actors[app] = good
        signal.actors[person]!["type"] = "agent"
        await #expect(throws: SignalSetup.Refusal.noPerson) { try await register() }
        signal.actors[person]!["type"] = "human"
        signal.actors[person]!["deactivated_at"] = "2026-11-20T00:00:00Z"
        await #expect(throws: SignalSetup.Refusal.noPerson) { try await register() }
        signal.actors[person]!["deactivated_at"] = NSNull()
        #expect(try AppSettings.load(url).signal == nil)

        // The actor and the person come from Signal, never from anything typed.
        let settings = try await register()
        #expect(settings.actor == app && settings.person == person && settings.usable)
        #expect(settings.origin == signal.origin)
        #expect(settings.connected == "Connected to \(signal.origin) as Winton (@winton)")
        #expect(try AppSettings.load(url).signal == settings)
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

struct OutboxMigrationTests {
    /// A journal from before the `ambiguous` column (#34) opens, gains it, and keeps the bytes-once rule.
    @Test func anOlderOutboxGainsTheColumn() throws {
        let path = scratchPath()
        let directory = realpath((path as NSString).deletingLastPathComponent, nil)!
        defer { free(directory) }
        let resolved = String(cString: directory) + "/journal.sqlite"
        do {
            let old = try SQLite(path: resolved)
            try old.execute("""
                CREATE TABLE meta (name TEXT PRIMARY KEY, value TEXT NOT NULL);
                CREATE TABLE entries (seq INTEGER PRIMARY KEY, proposal TEXT NOT NULL, kind TEXT NOT NULL,
                    at INTEGER NOT NULL, body TEXT NOT NULL);
                CREATE TABLE outbox (seq INTEGER PRIMARY KEY REFERENCES entries(seq), key TEXT NOT NULL UNIQUE,
                    nonce TEXT NOT NULL, bytes BLOB, state TEXT NOT NULL DEFAULT 'pending', event_id TEXT, stopped TEXT);
                CREATE TRIGGER outbox_bytes_once BEFORE UPDATE OF bytes, key, nonce ON outbox
                    WHEN OLD.bytes IS NOT NULL OR OLD.key IS NOT NEW.key OR OLD.nonce IS NOT NEW.nonce
                    BEGIN SELECT RAISE(ABORT, 'outbox bytes are written once'); END;
                """)
        }
        let journal = try Journal(path: path, at: Date(timeIntervalSince1970: 1_800_000_000))
        try journal.add(Proposal(request: request("github.merge", mergeParams), client: "c", at: Date(timeIntervalSince1970: 1_800_000_000)))
        let (seq, _) = try #require(try journal.nextDelivery(person: person))
        try journal.markAmbiguous(seq)
        try journal.stopDelivery(seq, reason: "refused:400")
        try journal.resumeDelivery()
        #expect(try journal.db.query("SELECT bytes IS NOT NULL, ambiguous FROM outbox").first == [.int(1), .int(1)])
        #expect(throws: SQLiteError.self) { try journal.db.execute("UPDATE outbox SET bytes = NULL") }
        #expect(throws: SQLiteError.self) { try journal.db.execute("UPDATE outbox SET ambiguous = 0") }
    }
}
