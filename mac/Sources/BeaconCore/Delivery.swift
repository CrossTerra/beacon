import Foundation

/// Signal's answer to one record (docs/approval-app.md, _Replay and order_), as delivery acts on it.
public enum RecordAnswer: Equatable, Sendable {
    /// `201`, or `200` with `replayed: true` for the identical bytes again.
    case stored(eventID: String, replayed: Bool)
    /// Send the same record again later: `401`, `429` (after `Retry-After`), a `403` not about the subject, a `400`
    /// about `occurred_at`, a `5xx`. `ambiguous` when Signal may have stored it (a `5xx`).
    case retry(after: TimeInterval?, ambiguous: Bool)
    /// Stop at this record until someone resumes delivery: a `409`, a `403` about the subject, any other `400`, a
    /// `413`, or another `4xx`. Signal did not store it.
    case stop(reason: String)
}

/// Posts one record's exact bytes. Throws when no answer arrived (no connection, a timeout): Signal may have stored
/// it, and the same bytes are sent again later.
public protocol RecordTransport: Sendable {
    func post(_ bytes: Data) async throws -> RecordAnswer
}

/// Drains the outbox to Signal in journal order (13.00-D10, D33). Only the first undelivered record is ever sent;
/// the cursor moves only on Signal's acknowledgement of it or of its identical replay. A definite refusal stops
/// delivery at that record until it is resumed; anything else is sent again later. Sending records never executes
/// an action.
public final class OutboxSender: @unchecked Sendable {
    public enum Outcome: Equatable, Sendable {
        /// Everything is delivered.
        case done(sent: Int)
        /// Try again later with the same bytes, not before `after` seconds when Signal said so.
        case retry(sent: Int, after: TimeInterval?)
        /// Delivery stopped at a record until it is resumed.
        case stopped(sent: Int, reason: String)
    }

    private let journal: Journal
    private let transport: any RecordTransport
    private let person: String

    public init(journal: Journal, transport: any RecordTransport, person: String) {
        self.journal = journal
        self.transport = transport
        self.person = person
    }

    public func drain() async throws -> Outcome {
        var sent = 0
        while let (seq, bytes) = try journal.nextDelivery(person: person) {
            let answer: RecordAnswer
            do {
                answer = try await transport.post(bytes)
            } catch {
                try journal.markAmbiguous(seq)
                return .retry(sent: sent, after: nil)
            }
            switch answer {
            case .stored(let eventID, _):
                try journal.delivered(seq, eventID: eventID)
                sent += 1
            case .retry(let after, let ambiguous):
                if ambiguous { try journal.markAmbiguous(seq) }
                return .retry(sent: sent, after: after)
            case .stop(let reason):
                try journal.stopDelivery(seq, reason: reason)
                return .stopped(sent: sent, reason: reason)
            }
        }
        if let stopped = try journal.deliveryStatus().stopped { return .stopped(sent: sent, reason: stopped) }
        return .done(sent: sent)
    }
}
