import Foundation

/// Where a proposal is in its life. The raw values are the words Signal stores as `status`.
public enum ProposalStatus: String, Sendable {
    case pending, approved, declined, withdrawn, expired, started
    case outcomeUnknown = "outcome_unknown"
    case executed, failed
}

/// One proposal as the socket sees it.
public struct Proposal: Equatable, Sendable {
    public let id: String
    public let type: ActionType
    public let params: JSON
    public let salt: String
    public let paramsHash: String
    public let reason: String
    public let agent: String
    public let session: String?
    public let subject: String?
    /// The admitted client identity that proposed it. Only that identity can read, wait on or withdraw it.
    public let client: String
    public let proposedAt: Date
    public let expiresAt: Date
    public var status: ProposalStatus
    /// Rises on every change, for `wait`.
    public var version: Int64

    /// A pending proposal expires after 24 hours (13.00-D9).
    public static let lifetime: TimeInterval = 24 * 60 * 60

    public init(request: ProposalRequest, client: String, at date: Date, id: String = PublicID.make("apr"),
                salt: String = randomHex(16), expiresAt: Date? = nil) {
        self.id = id
        type = request.type
        params = request.params
        self.salt = salt
        paramsHash = ApprovalHash.params(type: request.type.rawValue, params: request.params, salt: salt)
        reason = request.reason
        agent = request.agent
        session = request.session
        subject = request.subject
        self.client = client
        proposedAt = date
        self.expiresAt = expiresAt ?? date.addingTimeInterval(Proposal.lifetime)
        status = .pending
        version = 1
    }
}

public enum BookError: Error, Equatable, Sendable {
    case notFound
    case notPending
}

/// The proposals the socket reads and writes. The journal implements it durably; `MemoryBook` is for tests.
/// Every method is called on the server's queue. `changed` is called after any change, from any source, on
/// that queue, so waits can answer.
public protocol ProposalBook: AnyObject {
    var changed: ((String) -> Void)? { get set }
    func add(_ proposal: Proposal) throws
    func proposal(_ id: String, at date: Date) -> Proposal?
    func withdraw(_ id: String, at date: Date) throws -> Proposal
    func pendingCount(client: String?, at date: Date) -> Int
}

public final class MemoryBook: ProposalBook {
    public var changed: ((String) -> Void)?
    private var proposals: [String: Proposal] = [:]

    public init() {}

    public func add(_ proposal: Proposal) throws {
        proposals[proposal.id] = proposal
        changed?(proposal.id)
    }

    public func proposal(_ id: String, at date: Date) -> Proposal? {
        guard var proposal = proposals[id] else { return nil }
        if proposal.status == .pending, date >= proposal.expiresAt {
            proposal.status = .expired
            proposal.version += 1
            proposals[id] = proposal
        }
        return proposal
    }

    public func withdraw(_ id: String, at date: Date) throws -> Proposal {
        guard var proposal = self.proposal(id, at: date) else { throw BookError.notFound }
        guard proposal.status == .pending else { throw BookError.notPending }
        proposal.status = .withdrawn
        proposal.version += 1
        proposals[id] = proposal
        changed?(id)
        return proposal
    }

    public func pendingCount(client: String?, at date: Date) -> Int {
        proposals.keys.filter { id in
            guard let p = proposal(id, at: date) else { return false }
            return p.status == .pending && (client == nil || p.client == client)
        }.count
    }

    /// Sets a status as the approval window would. Tests only.
    public func set(_ id: String, _ status: ProposalStatus) {
        guard var proposal = proposals[id] else { return }
        proposal.status = status
        proposal.version += 1
        proposals[id] = proposal
        changed?(id)
    }
}
