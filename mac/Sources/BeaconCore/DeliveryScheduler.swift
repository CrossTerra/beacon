import Foundation

/// Runs outbox drains one at a time, waits after failed attempts, and resumes stopped records. A request made while
/// a drain runs is kept, with its force and resume, and acted on as soon as that drain ends: a resume during a drain
/// clears the wait and resumes whatever that drain stopped, instead of being lost to its backoff.
@MainActor
public final class DeliveryScheduler {
    public typealias Connect = () throws -> (transport: any RecordTransport, person: String)

    public private(set) var message: String
    /// Called with every new message, for the app's status line.
    public var onMessage: ((String) -> Void)?

    private let journal: Journal
    private let connect: Connect
    private let now: () -> Date
    private var running: Task<Void, Never>?
    private var again = false
    private var againForced = false
    private var againResume = false
    private var failures = 0
    private var notBefore: Date?

    public init(journal: Journal, connect: @escaping Connect, now: @escaping () -> Date = { Date() },
                message: String = "Records not sent yet") {
        self.journal = journal
        self.connect = connect
        self.now = now
        self.message = message
    }

    /// Drains now, or after the current drain. Unforced, it waits out a backoff or `Retry-After`.
    public func deliver(force: Bool = false) {
        if running != nil {
            again = true
            againForced = againForced || force
            return
        }
        if force { notBefore = nil }
        if !force, let notBefore, now() < notBefore { return }
        let journal = self.journal, connect = self.connect
        running = Task { [weak self] in
            let message: String
            var outcome: OutboxSender.Outcome?
            do {
                let (transport, person) = try connect()
                do {
                    outcome = try await OutboxSender(journal: journal, transport: transport, person: person).drain()
                    message = ""
                } catch {
                    message = "Delivery error in the journal: \(error)"
                }
            } catch SignalSetup.Refusal.noPerson {
                message = "Records are not sent: Signal's settings do not name the person. Paste the setup code again in Settings"
            } catch {
                // The vault or its key: nothing about any record. Tried again on the next request.
                message = "Records are not sent: the Signal key is not available in the vault"
            }
            self?.finish(outcome, message)
        }
    }

    /// Resume: records stopped at a refusal go back to pending and are sent now. During a drain, this happens as
    /// soon as it ends, so a record that drain stops is resumed too.
    public func resume() {
        if running != nil {
            again = true
            againForced = true
            againResume = true
            return
        }
        do {
            try journal.resumeDelivery()
        } catch {
            update("Delivery error in the journal: \(error)")
            return
        }
        failures = 0
        deliver(force: true)
    }

    /// Returns once no drain is running, including drains started by a request made during one.
    public func idle() async {
        while let running { await running.value }
    }

    private func finish(_ outcome: OutboxSender.Outcome?, _ error: String) {
        running = nil
        if let outcome, let status = try? journal.deliveryStatus() {
            switch outcome {
            case .done:
                failures = 0
                notBefore = nil
                update("Records delivered (\(status.delivered))")
            case .retry(_, let after):
                failures += 1
                let backoff = min(30 * pow(2, Double(min(failures - 1, 5))), 900)
                notBefore = now().addingTimeInterval(max(after ?? 0, backoff))
                update("Signal did not take records just now; \(status.waiting) waiting, sending again soon")
            case .stopped(_, let reason):
                update("Delivery stopped at a record (\(reason)); \(status.waiting) waiting. Resume after fixing it.")
            }
        } else {
            update(error)
        }
        guard again else { return }
        let forced = againForced, resuming = againResume
        again = false
        againForced = false
        againResume = false
        if resuming {
            resume()
        } else {
            deliver(force: forced)
        }
    }

    private func update(_ text: String) {
        message = text
        onMessage?(text)
    }
}
