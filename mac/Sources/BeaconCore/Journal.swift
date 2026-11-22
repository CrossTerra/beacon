import Foundation

/// What the person approved, frozen before the Approve control is enabled (13.00-D29): the proposal and the
/// version it was frozen at, the final params, and the account, origin and destination Beacon resolved.
public struct FrozenApproval: Equatable, Sendable {
    public let id: String
    public let type: ActionType
    public let version: Int64
    public let params: JSON
    public let salt: String
    /// The service origin, or nil for a local run.
    public let origin: String?
    public let account: String
    public let destination: JSON
    /// The proposal's model, when the person picked another (`edited: ["model"]`).
    public let originalModel: String?
    public let approvedParamsHash: String
    public let approvedHash: String

    public var edited: [String] { originalModel == nil ? [] : ["model"] }

    /// Freezes `proposal` with the resolved binding. `model` replaces an agent run's model; nothing else changes.
    public init(_ proposal: Proposal, origin: String?, account: String, destination: JSON, model: String? = nil) throws {
        var params = proposal.params
        var original: String?
        if let model, proposal.type == .agentRun, var fields = params.objectValue,
           let current = fields["model"]?.stringValue, current != model {
            fields["model"] = .string(model)
            params = try proposal.type.validate(params: .object(fields))
            original = current
        } else if let model, proposal.params["model"]?.stringValue != model {
            throw Invalid(field: "/params/model")
        }
        id = proposal.id
        type = proposal.type
        version = proposal.version
        self.params = params
        salt = proposal.salt
        self.origin = origin
        self.account = account
        self.destination = destination
        originalModel = original
        approvedParamsHash = ApprovalHash.params(type: type.rawValue, params: params, salt: salt)
        approvedHash = ApprovalHash.approved(id: id, type: type.rawValue, params: params, origin: origin,
                                             account: account, destination: destination, salt: salt)
    }
}

/// What a run launched by Beacon did after its launch (`app.runner.*`, docs/approval-app.md). The words are the
/// runner's own, 1 to 32,000 Unicode scalar values, delivered sealed and marked `from: "runner"`.
public enum RunnerEvent: Equatable, Sendable {
    case working
    case waiting(question: String)
    case stalled(lastOutput: String)
    case finished(report: String)
    /// `error` is a lowercase code (`exit_1`, `stopped`, `interrupted`); `detail` the runner's words, if any.
    case failed(error: String, detail: String?)
    /// The run's process group is gone: nothing of it is left running.
    case closed

    var kind: String {
        switch self {
        case .working: "runner_working"
        case .waiting: "runner_waiting"
        case .stalled: "runner_stalled"
        case .finished: "runner_finished"
        case .failed: "runner_failed"
        case .closed: "runner_closed"
        }
    }
}

public enum JournalError: Error, Equatable, Sendable {
    case notFound
    /// The proposal is not in a state that allows this step.
    case transition
    /// The proposal changed since the approval was frozen, or the frozen values do not match it.
    case stale
    case expired
    case shuttingDown
}

/// Beacon's local, append-only record of what it was asked and what it did (13.00-D10, D33). Each entry and its
/// outbox row commit in one SQLite transaction. Entries are never updated or deleted (triggers refuse it). State
/// is rebuilt from the entries when the journal opens, and recovery then expires every approval without a start
/// and marks every start without a result as unknown. Nothing here dispatches an action.
public final class Journal: ProposalBook, @unchecked Sendable {
    public var changed: ((String) -> Void)?
    public let id: String
    private var observers: [(String) -> Void] = []

    /// Also called after every change, on the thread that made it.
    public func observe(_ observer: @escaping (String) -> Void) {
        locked { observers.append(observer) }
    }

    /// An approval must start within ten minutes (13.00-D9).
    public static let startWindow: TimeInterval = 10 * 60

    struct State {
        var proposal: Proposal
        var runnerID: String?
        var approval: FrozenApproval?
        var approvedAt: Date?
        var startedAt: Date?
        var unknownAt: Date?
        /// The executed or failed entry's body.
        var outcome: JSON?
        /// A launched run's latest runner event (`runner_working` … `runner_closed`).
        var runner: String?
        /// The body of its last event with words: the report, the failure and its detail.
        var runnerWords: JSON?
    }

    let db: SQLite
    private let lock = NSLock()
    private let nonce: () -> String
    private let makeID: (String) -> String
    private var states: [String: State] = [:]
    private var order: [String] = []
    private var shuttingDown = false

    /// Opens or creates the journal at `path` and recovers it at `date`.
    public init(path: String, at date: Date, nonce: @escaping () -> String = { randomHex(16) },
                makeID: @escaping (String) -> String = { PublicID.make($0) }) throws {
        // SQLite's no-follow open refuses a symlink anywhere in the path; resolve the directory (on macOS /var is
        // one) and keep refusing a symlinked database file.
        let url = URL(fileURLWithPath: path)
        guard let directory = realpath(url.deletingLastPathComponent().path, nil) else {
            throw SQLiteError(code: 14, message: "the journal's directory does not exist")
        }
        defer { free(directory) }
        let db = try SQLite(path: String(cString: directory) + "/" + url.lastPathComponent)
        self.db = db
        self.nonce = nonce
        self.makeID = makeID
        try db.execute("""
            PRAGMA journal_mode = WAL;
            PRAGMA synchronous = FULL;
            PRAGMA fullfsync = ON;
            PRAGMA checkpoint_fullfsync = ON;
            CREATE TABLE IF NOT EXISTS meta (name TEXT PRIMARY KEY, value TEXT NOT NULL);
            CREATE TABLE IF NOT EXISTS entries (
                seq INTEGER PRIMARY KEY, proposal TEXT NOT NULL, kind TEXT NOT NULL, at INTEGER NOT NULL,
                body TEXT NOT NULL);
            CREATE TABLE IF NOT EXISTS outbox (
                seq INTEGER PRIMARY KEY REFERENCES entries(seq), key TEXT NOT NULL UNIQUE, nonce TEXT NOT NULL,
                bytes BLOB, state TEXT NOT NULL DEFAULT 'pending', event_id TEXT, stopped TEXT);
            CREATE TRIGGER IF NOT EXISTS entries_no_update BEFORE UPDATE ON entries
                BEGIN SELECT RAISE(ABORT, 'the journal is append-only'); END;
            CREATE TRIGGER IF NOT EXISTS entries_no_delete BEFORE DELETE ON entries
                BEGIN SELECT RAISE(ABORT, 'the journal is append-only'); END;
            """)
        // `ambiguous`: a send of this record once had no definite answer (a lost reply, a 5xx), so Signal may hold
        // its bytes and they must never change. Added after the first release, so an older journal gains it here.
        if try !db.query("PRAGMA table_info(outbox)").contains(where: { $0[1].text == "ambiguous" }) {
            try db.execute("ALTER TABLE outbox ADD COLUMN ambiguous INTEGER NOT NULL DEFAULT 0")
        }
        // Bytes are written once. They may be cleared to be rebuilt only for a stopped record that Signal answered
        // definitely every time, so it never stored them.
        try db.execute("""
            DROP TRIGGER IF EXISTS outbox_bytes_once;
            CREATE TRIGGER outbox_bytes_once BEFORE UPDATE OF bytes, key, nonce ON outbox
                WHEN OLD.key IS NOT NEW.key OR OLD.nonce IS NOT NEW.nonce
                    OR (OLD.bytes IS NOT NULL AND NOT (NEW.bytes IS NULL AND OLD.ambiguous = 0 AND OLD.state = 'stopped'))
                BEGIN SELECT RAISE(ABORT, 'outbox bytes are written once'); END;
            CREATE TRIGGER IF NOT EXISTS outbox_ambiguous_stays BEFORE UPDATE OF ambiguous ON outbox
                WHEN OLD.ambiguous = 1 AND NEW.ambiguous = 0
                BEGIN SELECT RAISE(ABORT, 'an ambiguous send stays ambiguous'); END;
            """)
        id = try db.transaction {
            if let existing = try db.query("SELECT value FROM meta WHERE name = 'journal_id'").first?.first?.text {
                return existing
            }
            let fresh = makeID("jrn")
            try db.query("INSERT INTO meta (name, value) VALUES ('journal_id', ?)", [.text(fresh)])
            return fresh
        }
        for row in try db.query("SELECT proposal, kind, at, body FROM entries ORDER BY seq") {
            // A committed body was written by this journal as canonical JSON; one that no longer parses is skipped.
            guard let body = try? JSON.parse(Data(row[3].text!.utf8)) else { continue }
            fold(id: row[0].text!, kind: row[1].text!, at: Date(milliseconds: row[2].int!), body: body)
        }
        try recover(at: date)
    }

    // MARK: Reading

    public func proposal(_ id: String, at date: Date) -> Proposal? {
        sweep(at: date)
        return locked { states[id]?.proposal }
    }

    /// Pending proposals, oldest first.
    public func pending(at date: Date) -> [Proposal] {
        sweep(at: date)
        return locked { order.compactMap { states[$0]?.proposal }.filter { $0.status == .pending } }
    }

    public func pendingCount(client: String?, at date: Date) -> Int {
        pending(at: date).filter { client == nil || $0.client == client }.count
    }

    /// Every proposal, newest first, for the window's history.
    public func all() -> [Proposal] {
        locked { order.reversed().compactMap { states[$0]?.proposal } }
    }

    public func approval(_ id: String) -> FrozenApproval? {
        locked { states[id]?.approval }
    }

    /// When the durable start was recorded.
    public func startedAt(_ id: String) -> Date? {
        locked { states[id]?.startedAt }
    }

    /// When the outcome was recorded unknown: after the write was sent, or at the relaunch that found the start.
    public func unknownAt(_ id: String) -> Date? {
        locked { states[id]?.unknownAt }
    }

    /// The result entry of an executed or failed proposal: `native`, `result`, `error`, `evidence`, `confirmed_by`.
    public func outcome(_ id: String) -> JSON? {
        locked { states[id]?.outcome }
    }

    /// A launched run's latest runner event: `runner_working`, `runner_finished`, `runner_closed`, …; nil before one.
    public func runnerState(_ id: String) -> String? {
        locked { states[id]?.runner }
    }

    /// The words of a run's last event that had any: `report`, `error` and `detail`, `question`, `last_output`.
    public func runnerWords(_ id: String) -> JSON? {
        locked { states[id]?.runnerWords }
    }

    public func runnerID(_ id: String) -> String? {
        locked { states[id]?.runnerID }
    }

    // MARK: Writing

    public func add(_ proposal: Proposal) throws {
        try append(proposal.id, kind: "proposed", at: proposal.proposedAt, new: proposal) { _ in
            var body: [String: JSON] = [
                "type": .string(proposal.type.rawValue), "params": proposal.params, "salt": .string(proposal.salt),
                "reason": .string(proposal.reason), "agent": .string(proposal.agent), "client": .string(proposal.client),
                "expires_at": .int(proposal.expiresAt.milliseconds),
            ]
            body["session"] = proposal.session.map(JSON.string)
            body["subject"] = proposal.subject.map(JSON.string)
            body["client_path"] = proposal.clientPath.map(JSON.string)
            body["client_requirement"] = proposal.clientRequirement.map(JSON.string)
            body["proposer_runner_id"] = proposal.runner.map(JSON.string)
            if proposal.type == .agentRun { body["runner_id"] = .string(makeID("run")) }
            return body
        }
    }

    public func withdraw(_ id: String, at date: Date) throws -> Proposal {
        sweep(at: date)
        do {
            try append(id, kind: "withdrawn", at: date) { state in
                guard state.proposal.status == .pending else { throw JournalError.transition }
                return ["reason": "proposer_withdrew"]
            }
        } catch JournalError.notFound {
            throw BookError.notFound
        } catch JournalError.transition {
            throw BookError.notPending
        }
        return proposal(id, at: date)!
    }

    /// Consumes the pending proposal `approval` was frozen from, once. It must be unchanged since then, unexpired,
    /// and the frozen values must be its own (only an agent run's model may differ).
    /// `subject` is the record subject as the provider spells it, kept in the local entry for the records from here
    /// on; it must name the same target as the proposal's subject, case aside, or it is ignored.
    public func approve(_ approval: FrozenApproval, subject: String? = nil, at date: Date) throws {
        sweep(at: date)
        try append(approval.id, kind: "approved", at: date) { state in
            let proposal = state.proposal
            guard !shuttingDown else { throw JournalError.shuttingDown }
            guard proposal.status == .pending else { throw JournalError.transition }
            guard date < proposal.expiresAt else { throw JournalError.expired }
            let rebuilt = try FrozenApproval(proposal, origin: approval.origin, account: approval.account,
                                             destination: approval.destination,
                                             model: approval.params["model"]?.stringValue)
            guard proposal.version == approval.version, rebuilt == approval else { throw JournalError.stale }
            var body: [String: JSON] = [
                "params": approval.params, "origin": approval.origin.map(JSON.string) ?? .null,
                "account": .string(approval.account), "destination": approval.destination,
                "approved_params_hash": .string(approval.approvedParamsHash),
                "approved_hash": .string(approval.approvedHash),
                "edited": .array(approval.edited.map(JSON.string)),
            ]
            body["original_model"] = approval.originalModel.map(JSON.string)
            let proposed = (try? Record.subject(action: proposal.type.rawValue, params: proposal.params, person: "")) ?? ""
            if let subject, subject != proposed, subject.lowercased() == proposed.lowercased(), subject.hasPrefix("github:pr:") {
                body["subject"] = .string(subject)
            }
            return body
        }
    }

    public func decline(_ id: String, note: String?, at date: Date) throws {
        sweep(at: date)
        if let note, !(1...Limits.note).contains(note.unicodeScalars.count) { throw Invalid(field: "/note") }
        try append(id, kind: "declined", at: date) { state in
            guard state.proposal.status == .pending else { throw JournalError.transition }
            var body: [String: JSON] = ["reason": "person_declined"]
            body["note"] = note.map(JSON.string)
            return body
        }
    }

    /// Records the durable start, before any final provider check or write (13.00-D33). Returns what was
    /// approved, for the handler. A second call, a start after Quit, after ten minutes or after recovery fails.
    public func start(_ id: String, at date: Date) throws -> FrozenApproval {
        sweep(at: date)
        var approved: FrozenApproval?
        try append(id, kind: "started", at: date) { state in
            guard !shuttingDown else { throw JournalError.shuttingDown }
            guard state.proposal.status == .approved, let approval = state.approval, let at = state.approvedAt else {
                throw JournalError.transition
            }
            guard date.timeIntervalSince(at) <= Journal.startWindow else { throw JournalError.expired }
            approved = approval
            return ["approved_hash": .string(approval.approvedHash)]
        }
        return approved!
    }

    /// A start whose result is not known: a lost reply, or a start found by recovery.
    public func outcomeUnknown(_ id: String, reason: String, at date: Date) throws {
        try append(id, kind: "outcome_unknown", at: date) { state in
            guard state.proposal.status == .started else { throw JournalError.transition }
            return ["reason": .string(reason)]
        }
    }

    /// A definite result. After an unknown outcome only read-back evidence may settle it.
    public func result(_ id: String, executed: Bool, confirmedBy: String, fields: [String: JSON], at date: Date) throws {
        try append(id, kind: executed ? "executed" : "failed", at: date) { state in
            switch state.proposal.status {
            case .started: break
            case .outcomeUnknown where confirmedBy == "read_back": break
            default: throw JournalError.transition
            }
            var body = fields
            body["confirmed_by"] = .string(confirmedBy)
            return body
        }
    }

    /// A runner event for a launched run (docs/approval-app.md, _Transitions_): only after the run's
    /// `app.action.executed`, nothing after `closed`, and only `closed` after `finished` or `failed`.
    public func runner(_ id: String, _ event: RunnerEvent, at date: Date) throws {
        func words(_ text: String, _ field: String) throws -> JSON {
            guard (1...Limits.text).contains(text.unicodeScalars.count) else { throw Invalid(field: field) }
            return .string(text)
        }
        var body: [String: JSON] = [:]
        switch event {
        case .working, .closed: break
        case .waiting(let question): body["question"] = try words(question, "/question")
        case .stalled(let output): body["last_output"] = try words(output, "/last_output")
        case .finished(let report): body["report"] = try words(report, "/report")
        case .failed(let error, let detail):
            guard Field.matches(error, "[a-z][a-z0-9_]{0,63}") else { throw Invalid(field: "/error") }
            body["error"] = .string(error)
            if let detail { body["detail"] = try words(detail, "/detail") }
        }
        try append(id, kind: event.kind, at: date) { state in
            guard state.proposal.type == .agentRun, state.proposal.status == .executed, state.runnerID != nil else {
                throw JournalError.transition
            }
            switch state.runner {
            case "runner_closed": throw JournalError.transition
            case "runner_finished", "runner_failed": guard event == .closed else { throw JournalError.transition }
            default: break
            }
            return body
        }
    }

    /// Quit: no approval or start from now on, and every approval without a start expires (13.00-D9).
    public func shutdown(at date: Date) throws {
        locked { shuttingDown = true }
        for id in locked({ order.filter { states[$0]?.proposal.status == .approved } }) {
            try expire(id, from: .approved, reason: "quit_before_start", at: date)
        }
    }

    /// Writes `expired` only if the proposal is still `status` when the entry is committed, under the lock.
    /// Another thread may have expired, started or decided it since the list was made; that is not an error.
    private func expire(_ id: String, from status: ProposalStatus, reason: String, at date: Date) throws {
        do {
            try append(id, kind: "expired", at: date) { state in
                guard state.proposal.status == status else { throw JournalError.transition }
                return ["reason": .string(reason)]
            }
        } catch JournalError.transition {
        }
    }

    // MARK: Delivery

    /// The next record to send, in journal order, with its exact bytes. The bytes are built once, at the first
    /// call, and kept. Nil when everything is delivered or delivery stopped at a conflict.
    public func nextDelivery(person: String, handle: String? = nil) throws -> (seq: Int64, bytes: Data)? {
        try locked {
            guard let row = try db.query("""
                SELECT o.seq, o.key, o.nonce, o.bytes, o.state, e.proposal, e.kind, e.at, e.body
                FROM outbox o JOIN entries e ON e.seq = o.seq WHERE o.state != 'delivered' ORDER BY o.seq LIMIT 1
                """).first else { return nil }
            guard row[4].text == "pending" else { return nil }
            let seq = row[0].int!
            if let bytes = row[3].blob { return (seq, bytes) }
            let bytes: Data
            do {
                let proposed = try db.query(
                    "SELECT body FROM entries WHERE proposal = ? AND kind = 'proposed'", [.text(row[5].text!)]).first![0].text!
                // From the approval on, the subject as the provider spelled it at approval, if Beacon kept one.
                let approved = try db.query(
                    "SELECT body FROM entries WHERE proposal = ? AND kind = 'approved' AND seq <= ?",
                    [.text(row[5].text!), .int(seq)]).first?[0].text
                let subject = try approved.flatMap { try JSON.parse(Data($0.utf8))["subject"]?.stringValue }
                bytes = try Record.bytes(
                    key: row[1].text!, nonce: row[2].text!, proposal: row[5].text!, kind: row[6].text!,
                    at: Date(milliseconds: row[7].int!), body: try JSON.parse(Data(row[8].text!.utf8)),
                    proposed: try JSON.parse(Data(proposed.utf8)), person: person, handle: handle, subject: subject)
            } catch {
                // The record cannot be built (an invalid person id, an unreadable entry): stop here, visibly, until a
                // resume. Nothing was sent, so it can be rebuilt then.
                let reason = (error as? Invalid).map { "record:" + $0.field } ?? "record:unreadable"
                try db.query("UPDATE outbox SET state = 'stopped', stopped = ? WHERE seq = ?", [.text(reason), .int(seq)])
                return nil
            }
            try db.query("UPDATE outbox SET bytes = ? WHERE seq = ?", [.blob(bytes), .int(seq)])
            return (seq, bytes)
        }
    }

    public func delivered(_ seq: Int64, eventID: String) throws {
        try locked {
            try db.query("UPDATE outbox SET state = 'delivered', event_id = ? WHERE seq = ? AND state = 'pending'",
                         [.text(eventID), .int(seq)])
        }
    }

    /// Delivery stops at this record until someone resumes it (a conflict, or a refusal resending cannot fix).
    public func stopDelivery(_ seq: Int64, reason: String) throws {
        try locked {
            try db.query("UPDATE outbox SET state = 'stopped', stopped = ? WHERE seq = ?", [.text(reason), .int(seq)])
        }
    }

    /// A send of this record had no definite answer: Signal may hold these bytes, so they never change.
    public func markAmbiguous(_ seq: Int64) throws {
        try locked { try db.query("UPDATE outbox SET ambiguous = 1 WHERE seq = ?", [.int(seq)]) }
    }

    /// Resume: stopped records go back to pending. One Signal definitely never stored (every answer a definite
    /// refusal) is rebuilt on its next send, so a corrected person id takes effect. One that ever went without a
    /// definite answer keeps its bytes.
    public func resumeDelivery() throws {
        try locked {
            try db.transaction {
                try db.query("UPDATE outbox SET bytes = NULL WHERE state = 'stopped' AND ambiguous = 0")
                try db.query("UPDATE outbox SET state = 'pending', stopped = NULL WHERE state = 'stopped'")
            }
        }
    }

    public struct DeliveryStatus: Equatable, Sendable {
        public let delivered: Int
        public let waiting: Int
        public let stopped: String?
    }

    public func deliveryStatus() throws -> DeliveryStatus {
        try locked {
            let rows = try db.query("SELECT state, stopped FROM outbox")
            return DeliveryStatus(delivered: rows.filter { $0[0].text == "delivered" }.count,
                                  waiting: rows.filter { $0[0].text != "delivered" }.count,
                                  stopped: rows.first { $0[0].text == "stopped" }?[1].text)
        }
    }

    // MARK: Internals

    @discardableResult
    private func locked<T>(_ body: () throws -> T) rethrows -> T {
        lock.lock()
        defer { lock.unlock() }
        return try body()
    }

    /// Checks the step against the current state, then commits the entry and its outbox row together, then
    /// applies it to the in-memory state.
    private func append(_ id: String, kind: String, at date: Date, new: Proposal? = nil,
                        _ build: (State) throws -> [String: JSON]) throws {
        try locked {
            let body: JSON
            if let new {
                guard states[id] == nil else { throw JournalError.transition }
                body = .object(try build(State(proposal: new)))
            } else {
                guard let state = states[id] else { throw JournalError.notFound }
                body = .object(try build(state))
            }
            try db.transaction {
                let seq = (try db.query("SELECT COALESCE(MAX(seq), 0) + 1 FROM entries").first![0].int)!
                try db.query("INSERT INTO entries (seq, proposal, kind, at, body) VALUES (?, ?, ?, ?, ?)",
                             [.int(seq), .text(id), .text(kind), .int(date.milliseconds), .text(body.canonical)])
                try db.query("INSERT INTO outbox (seq, key, nonce) VALUES (?, ?, ?)",
                             [.int(seq), .text("beacon:\(self.id):\(seq)"), .text(nonce())])
            }
            fold(id: id, kind: kind, at: date, body: body)
        }
        changed?(id)
        for observer in locked({ observers }) { observer(id) }
    }

    /// Applies one committed entry. It never throws and never checks today's schema, so every committed entry
    /// can always be read back. An entry it cannot read (a type this build does not know) is left out of the state;
    /// its outbox record is still delivered from the stored body.
    private func fold(id: String, kind: String, at date: Date, body: JSON) {
        if kind == "proposed" {
            guard let type = body["type"]?.stringValue.flatMap(ActionType.init(rawValue:)), let params = body["params"],
                  let salt = body["salt"]?.stringValue, let reason = body["reason"]?.stringValue,
                  let agent = body["agent"]?.stringValue, let client = body["client"]?.stringValue,
                  let expires = body["expires_at"]?.intValue else { return }
            let proposal = Proposal(stored: id, type: type, params: params, salt: salt, reason: reason, agent: agent,
                                    session: body["session"]?.stringValue, subject: body["subject"]?.stringValue,
                                    client: client, clientPath: body["client_path"]?.stringValue,
                                    clientRequirement: body["client_requirement"]?.stringValue,
                                    runner: body["proposer_runner_id"]?.stringValue, proposedAt: date, expiresAt: Date(milliseconds: expires))
            states[id] = State(proposal: proposal, runnerID: body["runner_id"]?.stringValue)
            order.append(id)
            return
        }
        guard var state = states[id] else { return }
        state.proposal.version += 1
        switch kind {
        case "approved":
            state.proposal.status = .approved
            state.approvedAt = date
            if let params = body["params"], let account = body["account"]?.stringValue, let destination = body["destination"],
               let paramsHash = body["approved_params_hash"]?.stringValue, let hash = body["approved_hash"]?.stringValue {
                state.approval = FrozenApproval(
                    stored: id, type: state.proposal.type, version: state.proposal.version - 1, params: params,
                    salt: state.proposal.salt, origin: body["origin"]?.stringValue, account: account,
                    destination: destination, originalModel: body["original_model"]?.stringValue,
                    approvedParamsHash: paramsHash, approvedHash: hash)
            }
        case "declined": state.proposal.status = .declined
        case "withdrawn": state.proposal.status = .withdrawn
        case "expired": state.proposal.status = .expired
        case "started":
            state.proposal.status = .started
            state.startedAt = date
        case "outcome_unknown":
            state.proposal.status = .outcomeUnknown
            state.unknownAt = date
        case "executed":
            state.proposal.status = .executed
            state.outcome = body
        case "failed":
            state.proposal.status = .failed
            state.outcome = body
        case let kind where kind.hasPrefix("runner_"):
            // A runner event changes the run's life, not the proposal: its version stays.
            state.proposal.version -= 1
            state.runner = kind
            if kind != "runner_working" && kind != "runner_closed" { state.runnerWords = body }
        default: return
        }
        states[id] = state
    }

    /// Expires pending proposals after 24 hours and approvals not started within ten minutes.
    private func sweep(at date: Date) {
        let due = locked {
            order.compactMap { id -> (String, ProposalStatus, String)? in
                guard let state = states[id] else { return nil }
                if state.proposal.status == .pending, date >= state.proposal.expiresAt {
                    return (id, .pending, "proposal_expired")
                }
                if state.proposal.status == .approved, let at = state.approvedAt,
                   date.timeIntervalSince(at) > Journal.startWindow { return (id, .approved, "start_window_passed") }
                return nil
            }
        }
        for (id, status, reason) in due {
            try? expire(id, from: status, reason: reason, at: date)
        }
    }

    /// After a crash or Quit: no prior approval may start, and no start may be dispatched again.
    private func recover(at date: Date) throws {
        for (id, status) in order.compactMap({ id in states[id].map { (id, $0.proposal.status) } }) {
            switch status {
            case .approved:
                try expire(id, from: .approved, reason: "restart_before_start", at: date)
            case .started:
                try append(id, kind: "outcome_unknown", at: date) { state in
                    guard state.proposal.status == .started else { throw JournalError.transition }
                    return ["reason": "app_restarted"]
                }
            default:
                break
            }
        }
        sweep(at: date)
    }
}

extension FrozenApproval {
    /// An approval rebuilt from its committed entry, with the hashes it was approved with.
    init(stored id: String, type: ActionType, version: Int64, params: JSON, salt: String, origin: String?,
         account: String, destination: JSON, originalModel: String?, approvedParamsHash: String, approvedHash: String) {
        self.id = id
        self.type = type
        self.version = version
        self.params = params
        self.salt = salt
        self.origin = origin
        self.account = account
        self.destination = destination
        self.originalModel = originalModel
        self.approvedParamsHash = approvedParamsHash
        self.approvedHash = approvedHash
    }
}

extension Date {
    init(milliseconds: Int64) {
        self.init(timeIntervalSince1970: TimeInterval(milliseconds) / 1000)
    }

    var milliseconds: Int64 { Int64((timeIntervalSince1970 * 1000).rounded()) }
}
