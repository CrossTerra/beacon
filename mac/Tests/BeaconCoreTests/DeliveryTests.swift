import Foundation
import Testing
@testable import BeaconCore

/// A Signal that keeps app records by the contract's rules: the same bytes under a stored key replay, different
/// bytes conflict, and a new key must come after the last stored one in the same journal. A new journal is
/// accepted only at sequence 1 with an id that sorts after the stored one.
final class FakeSignal: RecordTransport, @unchecked Sendable {
    private let lock = NSLock()
    private(set) var stored: [(key: String, bytes: Data)] = []
    private(set) var posts = 0
    /// Store the record but lose the answer.
    var loseAnswerFor: String?
    /// Fail before storing, as if Signal were down.
    var down = false

    func post(_ bytes: Data) async throws -> RecordAnswer {
        try lock.withLock {
            posts += 1
            if down { throw URLError(.cannotConnectToHost) }
            let key = try JSON.parse(bytes)["key"]!.stringValue!
            if let existing = stored.firstIndex(where: { $0.key == key }) {
                return stored[existing].bytes == bytes ? .stored(eventID: "evt_\(existing)", replayed: true)
                    : .conflict(reason: "idempotency_conflict")
            }
            let parts = key.split(separator: ":")
            let journal = String(parts[1]), seq = Int(parts[2])!
            if let last = stored.last {
                let lastParts = last.key.split(separator: ":")
                let lastJournal = String(lastParts[1]), lastSeq = Int(lastParts[2])!
                let ok = journal == lastJournal ? seq > lastSeq : (seq == 1 && journal > lastJournal)
                if !ok { return .conflict(reason: "journal_order") }
            }
            stored.append((key, bytes))
            if loseAnswerFor == key {
                loseAnswerFor = nil
                throw URLError(.networkConnectionLost)
            }
            return .stored(eventID: "evt_\(stored.count - 1)", replayed: false)
        }
    }
}

func filledJournal(_ path: String, at t0: Date, count: Int = 3) throws -> (Journal, [String]) {
    let journal = try Journal(path: path, at: t0)
    var ids: [String] = []
    for n in 0..<count {
        var params = mergeParams.objectValue!
        params["number"] = .int(Int64(100 + n))
        let proposal = Proposal(request: request("github.merge", .object(params)), client: "c", at: t0 + Double(n))
        try journal.add(proposal)
        ids.append(proposal.id)
    }
    return (journal, ids)
}

struct DeliveryTests {
    let t0 = Date(timeIntervalSince1970: 1_800_000_000)

    @Test func deliversInJournalOrderOnce() async throws {
        let (journal, ids) = try filledJournal(scratchPath(), at: t0)
        try journal.decline(ids[1], note: "Not now.", at: t0 + 10)
        let signal = FakeSignal()
        #expect(try await OutboxSender(journal: journal, transport: signal, person: person).drain() == .done(sent: 4))
        #expect(signal.stored.map(\.key) == (1...4).map { "beacon:\(journal.id):\($0)" })
        #expect(try await OutboxSender(journal: journal, transport: signal, person: person).drain() == .done(sent: 0))
        #expect(signal.posts == 4)
    }

    /// Signal stores a record but the answer is lost, and Beacon crashes. After relaunch the same bytes go again,
    /// Signal answers with a replay, and every record is stored once, in order.
    @Test func lostAnswerAndCrashReplayIdentically() async throws {
        let path = scratchPath()
        var (journal, _) = try filledJournal(path, at: t0)
        let signal = FakeSignal()
        signal.loseAnswerFor = "beacon:\(journal.id):2"
        #expect(try await OutboxSender(journal: journal, transport: signal, person: person).drain() == .retry(sent: 1))
        let first = signal.stored[1].bytes

        journal = try Journal(path: path, at: t0 + 60, nonce: { "ffffffffffffffffffffffffffffffff" })
        #expect(try await OutboxSender(journal: journal, transport: signal, person: person).drain() == .done(sent: 2))
        #expect(signal.stored.count == 3 && signal.stored[1].bytes == first)
        #expect(signal.posts == 4)
    }

    @Test func anUnreachableSignalKeepsTheSameBytes() async throws {
        let path = scratchPath()
        let (journal, _) = try filledJournal(path, at: t0, count: 1)
        let signal = FakeSignal()
        signal.down = true
        #expect(try await OutboxSender(journal: journal, transport: signal, person: person).drain() == .retry(sent: 0))
        let pending = try #require(try journal.nextDelivery(person: person)).bytes
        let reopened = try Journal(path: path, at: t0 + 1, nonce: { "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" })
        #expect(try reopened.nextDelivery(person: person)?.bytes == pending)
        signal.down = false
        #expect(try await OutboxSender(journal: reopened, transport: signal, person: person).drain() == .done(sent: 1))
        #expect(signal.stored.first?.bytes == pending)
    }

    /// A conflict stops delivery at that record; nothing after it is sent.
    @Test func aConflictStopsDelivery() async throws {
        let path = scratchPath()
        let (journal, _) = try filledJournal(path, at: t0)
        let signal = FakeSignal()
        // Signal already holds different bytes under the second key.
        _ = try await signal.post(try #require(try journal.nextDelivery(person: person)).bytes)
        try journal.delivered(1, eventID: "evt_0")
        var foreign = try JSON.parse(try #require(try journal.nextDelivery(person: person)).bytes).objectValue!
        foreign["occurred_at"] = "2030-01-01T00:00:00.000Z"
        _ = try await signal.post(JSON.object(foreign).canonicalData)

        let outcome = try await OutboxSender(journal: journal, transport: signal, person: person).drain()
        #expect(outcome == .stopped(sent: 0, reason: "conflict:idempotency_conflict"))
        #expect(signal.stored.count == 2)
        #expect(try journal.deliveryStatus() == .init(delivered: 1, waiting: 2, stopped: "conflict:idempotency_conflict"))
        #expect(try await OutboxSender(journal: journal, transport: signal, person: person).drain()
                == .stopped(sent: 0, reason: "conflict:idempotency_conflict"))
    }

    /// A reset store starts a new journal at sequence 1 with a later id; Signal accepts it.
    @Test func aNewJournalStartsAtOne() async throws {
        let signal = FakeSignal()
        let (old, _) = try filledJournal(scratchPath(), at: t0, count: 2)
        _ = try await OutboxSender(journal: old, transport: signal, person: person).drain()
        try await Task.sleep(nanoseconds: 2_000_000) // A later millisecond, so the new UUIDv7 id sorts after.
        let (fresh, _) = try filledJournal(scratchPath(), at: t0, count: 1)
        #expect(fresh.id > old.id)
        #expect(try await OutboxSender(journal: fresh, transport: signal, person: person).drain() == .done(sent: 1))
        #expect(signal.stored.last?.key == "beacon:\(fresh.id):1")
    }
}
