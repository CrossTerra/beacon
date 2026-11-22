import Foundation
import Testing
@testable import BeaconCore

let person = "act_01m6a6ft00e40r2081040g2081"

func iso(_ text: String) -> Date {
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    return formatter.date(from: text)!
}

func scratchPath(_ name: String = "journal.sqlite") -> String {
    let directory = FileManager.default.temporaryDirectory.appendingPathComponent("journal-" + randomHex(6))
    try! FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
    return directory.appendingPathComponent(name).path
}

func request(_ type: String, _ params: JSON, reason: String = "Because.", session: String? = "ses_bosun_7") -> ProposalRequest {
    var agent: [String: JSON] = ["name": "bosun"]
    agent["session"] = session.map(JSON.string)
    return try! ProposalRequest(["op": "propose", "type": .string(type), "params": params, "reason": .string(reason),
                                 "agent": .object(agent)])
}

let mergeParams: JSON = .object(["repo": "CrossTerra/signal", "number": .int(140), "base": "main",
                                 "head": .string(String(repeating: "a", count: 40)), "method": "squash"])
let runParams: JSON = .object(["checkout": "signal", "prompt": "Review it.", "model": "anthropic/claude-opus-5"])
let mergeDestination: JSON = .object(["repository_id": .int(456), "pr": .int(140), "base": "main"])

func freeze(_ journal: Journal, _ id: String, at date: Date, model: String? = nil) throws -> FrozenApproval {
    let proposal = journal.proposal(id, at: date)!
    if proposal.type == .agentRun {
        return try FrozenApproval(proposal, origin: nil, account: "macos:user:winton",
                                  destination: .object(["checkout_id": "x", "branch": "main"]), model: model)
    }
    return try FrozenApproval(proposal, origin: "https://api.github.com", account: "github:user:123",
                              destination: mergeDestination, model: model)
}

/// Replays Signal's fixture journal through Beacon's journal with the fixture's ids, salts, nonces and times,
/// skipping its runner events (no runner handler yet). Returns the journal and the fixture entries it replayed.
func replaySignalFixture() throws -> (Journal, [JSON]) {
    let entries = array(try fixture("journal.json")).filter { !$0["body"]!["type"]!.stringValue!.hasPrefix("app.runner.") }
    var nonces = entries.map { $0["body"]?["sealed"]?["replay_nonce"]?.stringValue ?? randomHex(16) }
    let nonce = { nonces.removeFirst() }
    let ids = { (prefix: String) in prefix == "jrn" ? "jrn_01m6a6ft00e81840g2081040g2" : "run_01m7x1v080ec1r60r30c1g60r3" }
    let path = scratchPath()
    var journal = try Journal(path: path, at: iso("2026-11-20T16:59:00.000Z"), nonce: nonce, makeID: ids)
    #expect(journal.id == "jrn_01m6a6ft00e81840g2081040g2")

    for entry in entries {
        let body = entry["body"]!
        let metadata = body["metadata"]!
        let sealed = body["sealed"]?["metadata"]
        let id = metadata["proposal"]!.stringValue!
        let at = iso(body["occurred_at"]!.stringValue!)
        switch body["type"]!.stringValue! {
        case "app.action.proposed":
            let proposer = metadata["proposer"]!
            let proposal = Proposal(request: request(metadata["action"]!.stringValue!, sealed!["params"]!,
                                                     reason: sealed!["reason"]!.stringValue!,
                                                     session: proposer["session"]?.stringValue),
                                    client: "beacon-act", at: at, id: id, salt: sealed!["salt"]!.stringValue!)
            try journal.add(proposal)
        case "app.action.approved":
            let proposal = journal.proposal(id, at: at)!
            let model = sealed!["original_model"] == nil ? nil : sealed!["params"]!["model"]!.stringValue
            let approval = try FrozenApproval(proposal, origin: sealed!["origin"]!.stringValue,
                                              account: sealed!["account"]!.stringValue!,
                                              destination: sealed!["destination"]!, model: model)
            try journal.approve(approval, at: at)
        case "app.action.declined":
            try journal.decline(id, note: sealed?["note"]?.stringValue, at: at)
        case "app.action.withdrawn":
            _ = try journal.withdraw(id, at: at)
        case "app.action.expired":
            // The fixture's approval expired on Quit; the app then relaunched.
            try journal.shutdown(at: at)
            journal = try Journal(path: path, at: at, nonce: nonce, makeID: ids)
        case "app.action.started":
            _ = try journal.start(id, at: at)
        case "app.action.outcome_unknown":
            try journal.outcomeUnknown(id, reason: metadata["reason"]!.stringValue!, at: at)
        case "app.action.executed", "app.action.failed":
            var fields: [String: JSON] = [:]
            for name in ["native", "error"] { fields[name] = metadata[name] }
            for name in ["evidence", "result"] { fields[name] = sealed?[name] }
            try journal.result(id, executed: body["type"] == "app.action.executed",
                               confirmedBy: metadata["confirmed_by"]!.stringValue!, fields: fields, at: at)
        default:
            Issue.record("unexpected type")
        }
    }

    return (journal, entries)
}

struct JournalTests {
    /// Signal's fixture journal, replayed through Beacon's journal with the fixture's ids, salts, nonces and
    /// times. Every record Beacon would send is the fixture's body, except the subject Beacon chooses for posts and
    /// runs (the person's own) and the keys (Beacon's sequence skips the fixture's runner events).
    @Test func reproducesSignalsFixtureJournal() throws {
        let (journal, entries) = try replaySignalFixture()
        var keys: [Int64] = []
        for entry in entries {
            let (seq, bytes) = try #require(try journal.nextDelivery(person: person))
            var expected = entry["body"]!.objectValue!
            let action = expected["metadata"]!["action"]!.stringValue!
            if action != "github.merge" { expected["subject"] = .string("signal:actor:\(person)") }
            expected["key"] = .string("beacon:\(journal.id):\(seq)")
            #expect(try JSON.parse(bytes) == .object(expected), "\(entry["note"]!.stringValue!)")
            #expect(bytes == JSON.object(expected).canonicalData)
            keys.append(seq)
            try journal.delivered(seq, eventID: "evt_\(seq)")
        }
        #expect(keys == Array(1...Int64(entries.count)))
        #expect(try journal.nextDelivery(person: person) == nil)
    }

    @Test func entriesAreAppendOnly() throws {
        let path = scratchPath()
        let journal = try Journal(path: path, at: Date())
        try journal.add(Proposal(request: request("github.merge", mergeParams), client: "c", at: Date()))
        let directory = realpath((path as NSString).deletingLastPathComponent, nil)!
        defer { free(directory) }
        let raw = try SQLite(path: String(cString: directory) + "/journal.sqlite")
        #expect(throws: SQLiteError.self) { try raw.execute("UPDATE entries SET kind = 'approved'") }
        #expect(throws: SQLiteError.self) { try raw.execute("DELETE FROM entries") }
        _ = try journal.nextDelivery(person: person)
        #expect(throws: SQLiteError.self) { try raw.execute("UPDATE outbox SET bytes = x'00'") }
    }

    /// A crash after the approval commits and before a start: on relaunch the approval expires, and nothing can
    /// start it.
    @Test func crashBetweenApproveAndStart() throws {
        let path = scratchPath()
        let t0 = Date(timeIntervalSince1970: 1_800_000_000)
        var journal: Journal? = try Journal(path: path, at: t0)
        let proposal = Proposal(request: request("github.merge", mergeParams), client: "c", at: t0)
        try journal!.add(proposal)
        try journal!.approve(try freeze(journal!, proposal.id, at: t0), at: t0 + 1)
        journal = nil // No Quit: the process died.

        let relaunched = try Journal(path: path, at: t0 + 60)
        #expect(relaunched.proposal(proposal.id, at: t0 + 60)?.status == .expired)
        #expect(throws: JournalError.transition) { try relaunched.start(proposal.id, at: t0 + 61) }
        let records = try deliverAll(relaunched)
        #expect(records.map { $0["type"]!.stringValue! } == ["app.action.proposed", "app.action.approved", "app.action.expired"])
        #expect(records.last?["metadata"]?["reason"] == "restart_before_start")
    }

    /// A crash after the durable start: the outcome is unknown, never dispatched again, and only read-back
    /// evidence can settle it.
    @Test func crashAfterStartGoesToReadBack() throws {
        let path = scratchPath()
        let t0 = Date(timeIntervalSince1970: 1_800_000_000)
        var journal: Journal? = try Journal(path: path, at: t0)
        let proposal = Proposal(request: request("github.merge", mergeParams), client: "c", at: t0)
        try journal!.add(proposal)
        try journal!.approve(try freeze(journal!, proposal.id, at: t0), at: t0 + 1)
        let started = try journal!.start(proposal.id, at: t0 + 2)
        #expect(started.approvedHash == journal!.approval(proposal.id)?.approvedHash)
        #expect(throws: JournalError.transition) { try journal!.start(proposal.id, at: t0 + 3) }
        journal = nil

        let relaunched = try Journal(path: path, at: t0 + 60)
        #expect(relaunched.proposal(proposal.id, at: t0 + 60)?.status == .outcomeUnknown)
        #expect(throws: JournalError.transition) { try relaunched.start(proposal.id, at: t0 + 61) }
        #expect(throws: JournalError.transition) {
            try relaunched.result(proposal.id, executed: true, confirmedBy: "reply", fields: [:], at: t0 + 62)
        }
        try relaunched.result(proposal.id, executed: true, confirmedBy: "read_back",
                              fields: ["native": .string("github:merge:" + String(repeating: "b", count: 40))], at: t0 + 63)
        #expect(throws: JournalError.transition) {
            try relaunched.result(proposal.id, executed: false, confirmedBy: "read_back", fields: ["error": "x"], at: t0 + 64)
        }
        let types = try deliverAll(relaunched).map { $0["type"]!.stringValue! }
        #expect(types.suffix(2) == ["app.action.outcome_unknown", "app.action.executed"])
        // A second relaunch changes nothing that was settled.
        let again = try Journal(path: path, at: t0 + 120)
        #expect(again.proposal(proposal.id, at: t0 + 120)?.status == .executed)
        #expect(try again.nextDelivery(person: person) == nil)
    }

    @Test func quitExpiresApprovalsAndRefusesStarts() throws {
        let t0 = Date(timeIntervalSince1970: 1_800_000_000)
        let journal = try Journal(path: scratchPath(), at: t0)
        let a = Proposal(request: request("github.merge", mergeParams), client: "c", at: t0)
        let b = Proposal(request: request("agent.run", runParams), client: "c", at: t0)
        try journal.add(a)
        try journal.add(b)
        try journal.approve(try freeze(journal, a.id, at: t0), at: t0 + 1)
        let frozen = try freeze(journal, b.id, at: t0)
        try journal.shutdown(at: t0 + 2)
        #expect(journal.proposal(a.id, at: t0 + 2)?.status == .expired)
        #expect(throws: JournalError.shuttingDown) { try journal.approve(frozen, at: t0 + 3) }
        #expect(journal.proposal(b.id, at: t0 + 3)?.status == .pending)
        #expect(journal.runnerID(b.id).map { PublicID.isValid($0, prefix: "run") } == true)
    }

    @Test func approvalBindsExactlyTheFrozenProposal() throws {
        let t0 = Date(timeIntervalSince1970: 1_800_000_000)
        let journal = try Journal(path: scratchPath(), at: t0)
        let merge = Proposal(request: request("github.merge", mergeParams), client: "c", at: t0)
        let run = Proposal(request: request("agent.run", runParams), client: "c", at: t0)
        try journal.add(merge)
        try journal.add(run)
        #expect(throws: Invalid(field: "/params/model")) { try freeze(journal, merge.id, at: t0, model: "openai/gpt-6") }

        // The model is the only edit, and both hashes are kept.
        let edited = try freeze(journal, run.id, at: t0, model: "openai/gpt-6")
        #expect(edited.edited == ["model"] && edited.originalModel == "anthropic/claude-opus-5")
        #expect(edited.approvedParamsHash != run.paramsHash)

        // A snapshot taken before a change no longer applies.
        let stale = try freeze(journal, merge.id, at: t0)
        try journal.decline(merge.id, note: "No.", at: t0 + 1)
        #expect(throws: JournalError.transition) { try journal.approve(stale, at: t0 + 2) }

        try journal.approve(edited, at: t0 + 3)
        #expect(throws: JournalError.transition) { try journal.approve(edited, at: t0 + 4) }

        // An approval not started within ten minutes expires.
        #expect(throws: JournalError.transition) { try journal.start(run.id, at: t0 + 3 + Journal.startWindow + 1) }
        #expect(journal.proposal(run.id, at: t0 + 700)?.status == .expired)
    }

    @Test func pendingProposalsExpireAfterADay() throws {
        let t0 = Date(timeIntervalSince1970: 1_800_000_000)
        let journal = try Journal(path: scratchPath(), at: t0)
        let merge = Proposal(request: request("github.merge", mergeParams), client: "c", at: t0)
        try journal.add(merge)
        let frozen = try freeze(journal, merge.id, at: t0)
        #expect(journal.pending(at: t0 + Proposal.lifetime - 1).count == 1)
        #expect(journal.pending(at: t0 + Proposal.lifetime).isEmpty)
        #expect(throws: JournalError.transition) { try journal.approve(frozen, at: t0 + Proposal.lifetime + 1) }
    }
}

/// Every undelivered record, parsed, marking each delivered.
func deliverAll(_ journal: Journal) throws -> [JSON] {
    var records: [JSON] = []
    while let (seq, bytes) = try journal.nextDelivery(person: person) {
        records.append(try JSON.parse(bytes))
        try journal.delivered(seq, eventID: "evt_\(seq)")
    }
    return records
}

struct JournalReviewTests {
    let t0 = Date(timeIntervalSince1970: 1_800_000_000)

    /// The window and the socket sweep at once: each due proposal expires exactly once, so Signal never sees a
    /// second `expired` and delivery never stops on one.
    @Test func concurrentSweepsExpireOnce() async throws {
        let journal = try Journal(path: scratchPath(), at: t0)
        var ids: [String] = []
        for n in 0..<20 {
            var params = mergeParams.objectValue!
            params["number"] = .int(Int64(200 + n))
            let proposal = Proposal(request: request("github.merge", .object(params)), client: "c", at: t0)
            try journal.add(proposal)
            ids.append(proposal.id)
        }
        let approved = try freeze(journal, ids[0], at: t0)
        try journal.approve(approved, at: t0)
        let all = ids
        let later = t0 + Proposal.lifetime + 1
        DispatchQueue.concurrentPerform(iterations: 16) { i in
            switch i % 4 {
            case 0: _ = journal.pending(at: later)
            case 1: _ = journal.proposal(all[i % all.count], at: later)
            case 2: _ = journal.pendingCount(client: nil, at: later)
            default: try? journal.shutdown(at: later)
            }
        }
        let expired = try deliverAll(journal).filter { $0["type"] == "app.action.expired" }
        #expect(expired.count == ids.count)
        #expect(Set(expired.map { $0["metadata"]!["proposal"]!.stringValue! }) == Set(ids))
    }

    @Test func commitsFlushTheDriveCache() throws {
        let journal = try Journal(path: scratchPath(), at: t0)
        #expect(try journal.db.query("PRAGMA fullfsync").first?.first == .int(1))
        #expect(try journal.db.query("PRAGMA checkpoint_fullfsync").first?.first == .int(1))
        #expect(try journal.db.query("PRAGMA synchronous").first?.first == .int(2))
    }

    /// A committed entry that today's schema would refuse, and one of a type this build does not know: the
    /// journal still opens, and the outbox still delivers both.
    @Test func reopensEntriesTodaysSchemaRefuses() throws {
        let path = scratchPath()
        var journal: Journal? = try Journal(path: path, at: t0)
        let legacy = PublicID.make("apr", at: t0)
        let unknown = PublicID.make("apr", at: t0)
        let bodies: [(String, JSON)] = [
            (legacy, .object(["type": "agent.run", "params": .object(["checkout": "Old Name", "prompt": "", "model": "opus"]),
                              "salt": .string(String(repeating: "0", count: 32)), "reason": "r", "agent": "bosun",
                              "client": "c", "expires_at": .int((t0 + 3600).milliseconds),
                              "runner_id": .string(PublicID.make("run"))])),
            (unknown, .object(["type": "jira.comment", "params": .object(["issue": "X-1"]),
                               "salt": .string(String(repeating: "0", count: 32)), "reason": "r", "agent": "bosun",
                               "client": "c", "expires_at": .int((t0 + 3600).milliseconds)])),
        ]
        for (seq, (id, body)) in bodies.enumerated() {
            try journal!.db.query("INSERT INTO entries (seq, proposal, kind, at, body) VALUES (?, ?, 'proposed', ?, ?)",
                                  [.int(Int64(seq + 1)), .text(id), .int(t0.milliseconds), .text(body.canonical)])
            try journal!.db.query("INSERT INTO outbox (seq, key, nonce) VALUES (?, ?, ?)",
                                  [.int(Int64(seq + 1)), .text("beacon:\(journal!.id):\(seq + 1)"), .text(randomHex(16))])
        }
        journal = nil
        let reopened = try Journal(path: path, at: t0 + 1)
        #expect(reopened.proposal(legacy, at: t0 + 1)?.params["checkout"] == "Old Name")
        #expect(reopened.proposal(unknown, at: t0 + 1) == nil)
        #expect(try deliverAll(reopened).map { $0["metadata"]!["action"]!.stringValue! } == ["agent.run", "jira.comment"])
        // New requests are still checked.
        #expect(throws: Invalid(field: "/params/checkout")) {
            try ActionType.agentRun.validate(params: .object(["checkout": "Old Name", "prompt": "x", "model": "a/b"]))
        }
    }
}

struct JournalFileTests {
    private func mode(_ path: String) -> Int? {
        (try? FileManager.default.attributesOfItem(atPath: path)[.posixPermissions] as? NSNumber)?.intValue
    }

    /// The journal, its WAL and its shared memory are created 0600, and files an earlier build left 0644 are narrowed
    /// when the journal opens.
    @Test func theJournalsFilesAreTheOwnersAlone() throws {
        let path = scratchPath()
        let files = [path, path + "-wal", path + "-shm"]
        let (journal, _) = try filledJournal(path, at: Date(timeIntervalSince1970: 1_800_000_000), count: 1)
        for file in files { #expect(mode(file) == 0o600, "\(file)") }

        for file in files { try FileManager.default.setAttributes([.posixPermissions: 0o644], ofItemAtPath: file) }
        let again = try Journal(path: path, at: Date(timeIntervalSince1970: 1_800_000_100))
        for file in files { #expect(mode(file) == 0o600, "\(file)") }
        withExtendedLifetime((journal, again)) {}
    }

    @Test func aSymlinkedWALIsRefused() throws {
        let path = scratchPath()
        let elsewhere = scratchPath("elsewhere")
        FileManager.default.createFile(atPath: elsewhere, contents: Data())
        try FileManager.default.createSymbolicLink(atPath: path + "-wal", withDestinationPath: elsewhere)
        #expect(throws: SQLiteError.self) { try Journal(path: path, at: Date()) }
    }
}
