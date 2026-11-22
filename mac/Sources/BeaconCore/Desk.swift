import Combine
import Foundation

/// What Beacon resolved for a proposal before it can be approved: the acting account, the service origin (nil
/// for a local run) and the exact destination, from its settings and the provider (13.00-D8). Handlers supply
/// resolvers in part 3; until then nothing resolves and Approve stays disabled.
public struct ResolvedAction: Equatable, Sendable {
    public let origin: String?
    public let account: String
    public let destination: JSON
    /// Facts read from the provider, shown beside the proposer's words.
    public let facts: [String: String]
    public let readAt: Date
    /// The account and destination in the person's words (`winton`; `main · #147 · CrossTerra/signal`). The
    /// exact `account` and `destination` stay under _Show exact parameters_.
    public let accountLabel: String?
    public let destinationLabel: String?
    /// The record subject as the provider spells it (`github:pr:CrossTerra/signal#147`), for the records from the
    /// approval on. Local only: not part of any hash.
    public let subject: String?
    /// Where the target can be seen, such as the pull request's page.
    public let link: URL?

    public init(origin: String?, account: String, destination: JSON, facts: [String: String] = [:], readAt: Date,
                accountLabel: String? = nil, destinationLabel: String? = nil, subject: String? = nil, link: URL? = nil) {
        self.origin = origin
        self.account = account
        self.destination = destination
        self.facts = facts
        self.readAt = readAt
        self.accountLabel = accountLabel
        self.destinationLabel = destinationLabel
        self.subject = subject
        self.link = link
    }
}

public protocol ActionResolver: Sendable {
    func resolve(_ proposal: Proposal, model: String?) async throws -> ResolvedAction
}

/// No handler yet: nothing resolves.
public struct NoResolver: ActionResolver {
    public struct Unsupported: Error {}

    public init() {}

    public func resolve(_ proposal: Proposal, model: String?) async throws -> ResolvedAction {
        throw Unsupported()
    }
}

/// The approval window's one state owner (13.00-D3, D29). Approve is enabled only while `frozen` holds a
/// snapshot of the selected, pending proposal at its current version, with the resolved account, origin and
/// destination. The control carries that snapshot's token. A selection change, model change, _Read again_,
/// withdrawal, expiry or Quit clears the snapshot first, so input bound to the old view finds nothing to approve.
/// Approving consumes the snapshot before the journal commits, so a second click or a repeated key does nothing,
/// and the selection never moves to another row by itself.
@MainActor
public final class ApprovalDesk: ObservableObject {
    public struct Frozen: Equatable, Sendable {
        public let token: UUID
        public let approval: FrozenApproval
        public let resolved: ResolvedAction
    }

    @Published public private(set) var pending: [Proposal] = []
    @Published public private(set) var selection: String?
    @Published public private(set) var frozen: Frozen?
    /// An agent run's model, when the person picks one other than the proposal's.
    @Published public private(set) var model: String?
    @Published public private(set) var problem: String?
    @Published public private(set) var stopping = false

    public let journal: Journal
    /// Called with the proposal id after an approval commits; the app hands it to the action's handler.
    public var onApproved: ((String) -> Void)?
    /// Called before the journal consumes an approval: a refusal (a run's checkout is busy) leaves it pending, with
    /// that reason shown. `release` undoes a claim the journal then refused.
    public var claim: ((FrozenApproval) -> String?)?
    public var release: ((String) -> Void)?
    private let resolver: any ActionResolver
    private let now: @Sendable () -> Date
    private var generation = 0
    /// The resolve in flight, for tests.
    public private(set) var freezing: Task<Void, Never>?

    public init(journal: Journal, resolver: any ActionResolver, now: @escaping @Sendable () -> Date = { Date() }) {
        self.journal = journal
        self.resolver = resolver
        self.now = now
        journal.observe { [weak self] _ in
            Task { @MainActor [weak self] in self?.refresh() }
        }
        refresh()
    }

    public var selected: Proposal? {
        selection.flatMap { journal.proposal($0, at: now()) }
    }

    /// Reloads the pending list. A snapshot whose proposal changed, left pending or expired is dropped.
    public func refresh() {
        pending = journal.pending(at: now())
        if let frozen, let current = journal.proposal(frozen.approval.id, at: now()),
           current.status != .pending || current.version != frozen.approval.version {
            invalidate()
        }
    }

    public func select(_ id: String?) {
        invalidate()
        selection = id
        model = nil
        problem = nil
        freeze()
    }

    /// Only an agent run's model can change (13.00-D7).
    public func choose(model: String) {
        guard let proposal = selected, proposal.type == .agentRun, Field.matches(model, Field.model) else { return }
        invalidate()
        self.model = model == proposal.params["model"]?.stringValue ? nil : model
        freeze()
    }

    /// _Read again_: drop the snapshot and resolve afresh.
    public func readAgain() {
        invalidate()
        freeze()
    }

    /// Approves the snapshot `token` names, once. False when it is no longer the current snapshot.
    @discardableResult
    public func approve(_ token: UUID) -> Bool {
        guard !stopping, let frozen, frozen.token == token, frozen.approval.id == selection else { return false }
        invalidate()
        if let refusal = claim?(frozen.approval) {
            problem = refusal
            freeze()
            return false
        }
        do {
            try journal.approve(frozen.approval, subject: frozen.resolved.subject, at: now())
            refresh()
            onApproved?(frozen.approval.id)
            return true
        } catch {
            release?(frozen.approval.id)
            problem = "Not approved: the proposal changed, expired or was withdrawn."
            refresh()
            return false
        }
    }

    /// What happened to an approved proposal, in Beacon's words, and where to see it; nil before its start.
    public func outcome(of proposal: Proposal) -> (text: String, link: URL?)? {
        ActionOutcome.describe(proposal, approval: journal.approval(proposal.id), result: journal.outcome(proposal.id),
                               runner: journal.runnerState(proposal.id), words: journal.runnerWords(proposal.id))
    }

    public func decline(note: String?) {
        guard !stopping, let id = selection else { return }
        invalidate()
        do {
            try journal.decline(id, note: note?.isEmpty == false ? note : nil, at: now())
        } catch {
            problem = "Not declined: the proposal is no longer pending."
        }
        refresh()
    }

    /// Quit: nothing can be approved from here on, and approvals without a start expire.
    public func beginShutdown() {
        stopping = true
        invalidate()
        try? journal.shutdown(at: now())
    }

    private func invalidate() {
        generation += 1
        frozen = nil
        freezing?.cancel()
        freezing = nil
    }

    private func freeze() {
        guard !stopping, let proposal = selected, proposal.status == .pending else { return }
        let generation = self.generation
        let model = self.model
        let resolver = self.resolver
        freezing = Task { @MainActor [weak self] in
            do {
                let resolved = try await resolver.resolve(proposal, model: model)
                guard let self, !Task.isCancelled, generation == self.generation, !self.stopping,
                      let current = self.selected, current.status == .pending, current.version == proposal.version
                else { return }
                let approval = try FrozenApproval(current, origin: resolved.origin, account: resolved.account,
                                                  destination: resolved.destination, model: model)
                self.frozen = Frozen(token: UUID(), approval: approval, resolved: resolved)
            } catch let refusal as MergeRefusal {
                guard let self, generation == self.generation else { return }
                self.problem = "Not approvable: \(refusal.rawValue.replacingOccurrences(of: "_", with: " "))."
            } catch let refusal as ChecksNotGreen {
                guard let self, generation == self.generation else { return }
                self.problem = "Not approvable: checks \(refusal.report.summary)."
            } catch let refusal as RunRefusal {
                guard let self, generation == self.generation else { return }
                self.problem = "Not approvable: \(refusal.rawValue.replacingOccurrences(of: "_", with: " "))."
            } catch {
                guard let self, generation == self.generation else { return }
                self.problem = "Beacon cannot resolve this action yet."
            }
        }
    }
}

/// An approved action's state after approval, as the window shows it.
public enum ActionOutcome {
    public static func describe(_ proposal: Proposal, approval: FrozenApproval?, result: JSON?, runner: String? = nil,
                                words: JSON? = nil) -> (text: String, link: URL?)? {
        if proposal.type == .agentRun, proposal.status == .executed {
            let error = words?["error"]?.stringValue?.replacingOccurrences(of: "_", with: " ") ?? "failed"
            switch runner {
            case "runner_finished": return ("Finished. Its report is below and goes to Signal for the session that asked.", nil)
            case "runner_failed": return ("The run ended: \(error).", nil)
            case "runner_closed":
                return (words?["report"] != nil ? "Finished; nothing of the run is left running." :
                        "The run ended: \(error). Nothing of it is left running.", nil)
            default: return ("Running in its checkout.", nil)
            }
        }
        var link: URL?
        if proposal.type == .githubMerge, let approval, let origin = approval.origin,
           let repo = approval.params["repo"]?.stringValue, let number = approval.params["number"]?.intValue {
            link = MergeHandler.pullURL(origin: origin, repo: repo, number: number)
        }
        let commit = result?["native"]?.stringValue.flatMap { native -> String? in
            native.hasPrefix("github:merge:") ? String(native.dropFirst("github:merge:".count).prefix(7)) : nil
        }
        switch proposal.status {
        case .approved: return ("Approved; starting…", link)
        case .started: return ("Started…", link)
        case .outcomeUnknown: return ("Outcome unknown: Beacon is reading it back. It will not be sent again.", link)
        case .executed:
            if let commit, proposal.type == .githubMerge { return ("Merged as \(commit)", link) }
            return ("Done", link)
        case .failed:
            let error = result?["error"]?.stringValue?.replacingOccurrences(of: "_", with: " ") ?? "failed"
            switch proposal.type {
            case .githubMerge: return ("Not merged: \(error)", link)
            case .agentRun: return ("Not started: \(error)", link)
            default: return ("Failed: \(error)", link)
            }
        default: return nil
        }
    }
}
