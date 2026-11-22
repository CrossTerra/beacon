import Foundation

/// Signal's answer to one record (docs/approval-app.md, _Replay and order_).
public enum RecordAnswer: Equatable, Sendable {
    /// `201`, or `200` with `replayed: true` for the identical bytes again.
    case stored(eventID: String, replayed: Bool)
    /// `409`: different bytes under a stored key, or out of journal order. Resending cannot fix it.
    case conflict(reason: String)
    /// Any other refusal Signal gives for these bytes (`400`, `403`, …). Resending cannot fix it either.
    case refused(status: Int)
}

/// Posts one record's exact bytes. Throws when the answer is unknown (no connection, a timeout, a `5xx`): the
/// same bytes are sent again later.
public protocol RecordTransport: Sendable {
    func post(_ bytes: Data) async throws -> RecordAnswer
}

/// Drains the outbox to Signal in journal order (13.00-D10, D33). Only the first undelivered record is ever sent;
/// the cursor moves only on Signal's acknowledgement of it or of its identical replay. A conflict or refusal stops
/// delivery at that record. Sending records never executes an action.
public final class OutboxSender: @unchecked Sendable {
    public enum Outcome: Equatable, Sendable {
        /// Everything is delivered.
        case done(sent: Int)
        /// Signal could not be reached or did not answer; try again later with the same bytes.
        case retry(sent: Int)
        /// Delivery stopped at a record until someone looks.
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
                return .retry(sent: sent)
            }
            switch answer {
            case .stored(let eventID, _):
                try journal.delivered(seq, eventID: eventID)
                sent += 1
            case .conflict(let reason):
                try journal.stopDelivery(seq, reason: "conflict:" + reason)
                return .stopped(sent: sent, reason: "conflict:" + reason)
            case .refused(let status):
                try journal.stopDelivery(seq, reason: "refused:\(status)")
                return .stopped(sent: sent, reason: "refused:\(status)")
            }
        }
        if let stopped = try journal.deliveryStatus().stopped { return .stopped(sent: sent, reason: stopped) }
        return .done(sent: sent)
    }
}
