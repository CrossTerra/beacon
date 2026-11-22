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

    public init(origin: String?, account: String, destination: JSON, facts: [String: String] = [:], readAt: Date) {
        self.origin = origin
        self.account = account
        self.destination = destination
        self.facts = facts
        self.readAt = readAt
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
        do {
            try journal.approve(frozen.approval, at: now())
            refresh()
            return true
        } catch {
            problem = "Not approved: the proposal changed, expired or was withdrawn."
            refresh()
            return false
        }
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
            } catch {
                guard let self, generation == self.generation else { return }
                self.problem = "Beacon cannot resolve this action yet."
            }
        }
    }
}
