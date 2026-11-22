import Foundation

/// Why a merge is not offered, or was stopped before the write. Lowercase codes, as Signal records them.
public enum MergeRefusal: String, Error, Sendable {
    case notConfigured = "not_configured"
    case accountMismatch = "account_mismatch"
    case repositoryChanged = "repository_changed"
    case notOpen = "not_open"
    case alreadyMerged = "already_merged"
    case baseChanged = "base_changed"
    case headMoved = "head_moved"
    case mergeabilityUnknown = "mergeability_unknown"
    case notMergeable = "not_mergeable"
    case changesRequested = "changes_requested"
    case checksNotGreen = "checks_not_green"
    case unavailable
    case busy
}

/// Bounds preflight reads (13.00, _The local API_): at most two preflights at once, and 120 provider reads an hour
/// across all of them, pages and _Read again_ included. Past a limit the preflight fails as busy, without a read.
public actor ReadBudget {
    private let perHour: Int
    private let concurrent: Int
    private var reads: [Date] = []
    private var running = 0

    public init(perHour: Int = 120, concurrent: Int = 2) {
        self.perHour = perHour
        self.concurrent = concurrent
    }

    /// A preflight begins: at most `concurrent` at once.
    func enter() throws {
        guard running < concurrent else { throw MergeRefusal.busy }
        running += 1
    }

    func leave() {
        running -= 1
    }

    /// One provider read, counted as it is made. Past the hourly limit the preflight stops as busy, mid-way if
    /// need be, and that read is not made.
    func take(at date: Date) throws {
        reads = reads.filter { date.timeIntervalSince($0) < 3600 }
        guard reads.count < perHour else { throw MergeRefusal.busy }
        reads.append(date)
    }
}

/// The `github.merge` handler (13.00, _Merge at the write_). Before approval it reads the account, the pull
/// request, its reviews and its required checks, and freezes the repository id, PR and base with the account. After
/// approval it records the durable start, reads everything again and stops on any change, then sends one merge with
/// `sha: head` and the frozen method. A reply settles it; a lost reply leaves it unknown until a read-back shows that
/// this account merged this head into this base, or that it did not. Nothing is sent twice.
public final class MergeHandler: ActionResolver, @unchecked Sendable {
    public typealias Connect = @Sendable (GitHubSettings) throws -> GitHubAPI

    private let journal: Journal
    private let settings: @Sendable () -> GitHubSettings?
    private let connect: Connect
    private let budget: ReadBudget
    private let now: @Sendable () -> Date
    /// A start with no reply is decided as not merged only after this long, and two reads, so a merge GitHub is
    /// still finishing is never recorded as failed.
    public static let settleTime: TimeInterval = 60
    private let openReads = Locked<[String: Int]>([:])

    public init(journal: Journal, settings: @escaping @Sendable () -> GitHubSettings?, connect: @escaping Connect,
                budget: ReadBudget = ReadBudget(), now: @escaping @Sendable () -> Date = { Date() }) {
        self.journal = journal
        self.settings = settings
        self.connect = connect
        self.budget = budget
        self.now = now
    }

    /// A client for the configured account, with the vault's token for that origin and account only.
    public static func connect(vault: Vault, configuration: URLSessionConfiguration = .ephemeral) -> Connect {
        { settings in
            let token = try vault.credential(for: try settings.binding)
            return GitHubAPI(client: try ServiceClient(origin: settings.origin, authorization: token,
                                                       headers: GitHubAPI.headers, configuration: configuration))
        }
    }

    struct Pinned {
        let repo: String
        let number: Int64
        let base: String
        let head: String
        let method: String

        init(_ params: JSON) throws {
            guard let repo = params["repo"]?.stringValue, let number = params["number"]?.intValue,
                  let base = params["base"]?.stringValue, let head = params["head"]?.stringValue,
                  let method = params["method"]?.stringValue else { throw Invalid(field: "/params") }
            (self.repo, self.number, self.base, self.head, self.method) = (repo, number, base, head, method)
        }
    }

    // MARK: Before approval

    public func resolve(_ proposal: Proposal, model: String?) async throws -> ResolvedAction {
        guard proposal.type == .githubMerge else { throw NoResolver.Unsupported() }
        guard let settings = settings() else { throw MergeRefusal.notConfigured }
        let pinned = try Pinned(proposal.params)
        let at = now()
        try await budget.enter()
        do {
            var api = try connect(settings)
            let budget = self.budget, now = self.now
            api.meter = { try await budget.take(at: now()) }
            let (pull, checks) = try await read(api, settings, pinned, repositoryID: nil)
            await budget.leave()
            return ResolvedAction(
                origin: try CredentialBinding.normalize(settings.origin), account: settings.account,
                destination: .object(["repository_id": .int(pull.repositoryID), "pr": .int(pinned.number),
                                      "base": .string(pinned.base)]),
                facts: ["Title": pull.title, "Mergeable": pull.mergeableState, "Reviews": "no changes requested",
                        "Required checks": checks.isEmpty ? "none" : "\(checks.count) green: " + checks.sorted().joined(separator: ", "),
                        "Head": pinned.head],
                readAt: at)
        } catch {
            await budget.leave()
            throw error
        }
    }

    /// Reads the account, PR, reviews and checks, and refuses anything that differs from the pinned values or
    /// does not allow the merge. Returns the PR and its green required checks.
    func read(_ api: GitHubAPI, _ settings: GitHubSettings, _ pinned: Pinned, repositoryID: Int64?) async throws
        -> (PullRequestState, Set<String>)
    {
        do {
            guard try await api.user().id == settings.userID else { throw MergeRefusal.accountMismatch }
            let pull = try await api.pull(pinned.repo, pinned.number)
            if let repositoryID, pull.repositoryID != repositoryID { throw MergeRefusal.repositoryChanged }
            if pull.merged { throw MergeRefusal.alreadyMerged }
            guard pull.state == "open" else { throw MergeRefusal.notOpen }
            guard pull.base == pinned.base else { throw MergeRefusal.baseChanged }
            guard pull.head == pinned.head else { throw MergeRefusal.headMoved }
            guard let mergeable = pull.mergeable else { throw MergeRefusal.mergeabilityUnknown }
            guard mergeable, ["clean", "unstable", "has_hooks"].contains(pull.mergeableState) else {
                throw MergeRefusal.notMergeable
            }
            if try await api.changesRequested(pinned.repo, pinned.number) { throw MergeRefusal.changesRequested }
            let required = try await api.requiredChecks(pinned.repo, base: pinned.base)
            guard required.isSubset(of: try await api.greenChecks(pinned.repo, sha: pinned.head)) else {
                throw MergeRefusal.checksNotGreen
            }
            return (pull, required)
        } catch let refusal as MergeRefusal {
            throw refusal
        } catch {
            throw MergeRefusal.unavailable
        }
    }

    /// The pull request once more, right before the write: the same repository, open, unmerged, base and head.
    func lastRead(_ api: GitHubAPI, _ pinned: Pinned, repositoryID: Int64) async throws {
        let pull: PullRequestState
        do {
            pull = try await api.pull(pinned.repo, pinned.number)
        } catch {
            throw MergeRefusal.unavailable
        }
        if pull.repositoryID != repositoryID { throw MergeRefusal.repositoryChanged }
        if pull.merged { throw MergeRefusal.alreadyMerged }
        guard pull.state == "open" else { throw MergeRefusal.notOpen }
        guard pull.base == pinned.base else { throw MergeRefusal.baseChanged }
        guard pull.head == pinned.head else { throw MergeRefusal.headMoved }
    }

    // MARK: After approval

    public enum Outcome: Equatable, Sendable {
        case executed(confirmedBy: String)
        case failed(error: String, confirmedBy: String)
        case unknown
    }

    /// Carries out one approved merge. The start is durable before any read or write for it; nothing is retried.
    @discardableResult
    public func execute(_ id: String) async throws -> Outcome {
        let approval = try journal.start(id, at: now())
        guard let settings = settings(), settings.account == approval.account,
              (try? CredentialBinding.normalize(settings.origin)) == approval.origin,
              let repositoryID = approval.destination["repository_id"]?.intValue else {
            return try fail(id, MergeRefusal.notConfigured.rawValue, by: "pre_dispatch")
        }
        let pinned = try Pinned(approval.params)
        let api: GitHubAPI
        do {
            api = try connect(settings)
            _ = try await read(api, settings, pinned, repositoryID: repositoryID)
            try await lastRead(api, pinned, repositoryID: repositoryID)
        } catch let refusal as MergeRefusal {
            return try fail(id, refusal.rawValue, by: "pre_dispatch")
        } catch {
            return try fail(id, MergeRefusal.unavailable.rawValue, by: "pre_dispatch")
        }
        let status: Int, sha: String?
        do {
            (status, sha) = try await api.merge(pinned.repo, pinned.number, head: pinned.head, method: pinned.method)
        } catch {
            // The request may have reached GitHub. Only a read-back can tell.
            try journal.outcomeUnknown(id, reason: "reply_lost", at: now())
            return try await reconcile(id)
        }
        switch status {
        case 200 where sha.map({ Field.matches($0, "[0-9a-f]{40}") }) == true:
            try journal.result(id, executed: true, confirmedBy: "reply",
                               fields: ["native": .string("github:merge:\(sha!)"),
                                        "result": .string("Merged as \(sha!.prefix(7)).")], at: now())
            return .executed(confirmedBy: "reply")
        case 405: return try fail(id, "not_mergeable", by: "reply")
        case 409: return try fail(id, "head_moved", by: "reply")
        case 403: return try fail(id, "forbidden", by: "reply")
        case 404: return try fail(id, "not_found", by: "reply")
        case 422: return try fail(id, "rejected", by: "reply")
        case 400..<500: return try fail(id, "github_\(status)", by: "reply")
        default:
            // A 2xx without a merge commit, or a 5xx: GitHub may have merged.
            try journal.outcomeUnknown(id, reason: status >= 500 ? "server_error" : "reply_unclear", at: now())
            return try await reconcile(id)
        }
    }

    /// Settles an unknown outcome by reading the PR, never by merging again. Executed only if this account merged
    /// the approved head into the approved base; failed if the PR was merged otherwise, closed, or still open long
    /// enough after the start; otherwise it stays unknown, to be read again later.
    @discardableResult
    public func reconcile(_ id: String) async throws -> Outcome {
        // Read back only as the account and origin that were approved; settings changed since leave it unknown.
        guard let proposal = journal.proposal(id, at: now()), proposal.status == .outcomeUnknown,
              let approval = journal.approval(id), let settings = settings(), settings.account == approval.account,
              (try? CredentialBinding.normalize(settings.origin)) == approval.origin,
              let userID = Int64(approval.account.dropFirst("github:user:".count)) else { return .unknown }
        let pinned = try Pinned(approval.params)
        let pull: PullRequestState
        do {
            pull = try await connect(settings).pull(pinned.repo, pinned.number)
        } catch {
            return .unknown
        }
        if pull.merged {
            guard pull.mergedBy == userID, pull.head == pinned.head, pull.base == pinned.base,
                  let sha = pull.mergeCommit, Field.matches(sha, "[0-9a-f]{40}") else {
                return try fail(id, "merged_otherwise", by: "read_back")
            }
            try journal.result(id, executed: true, confirmedBy: "read_back",
                               fields: ["native": .string("github:merge:\(sha)"),
                                        "evidence": .string("Read back: merged as \(sha.prefix(7)) by this account.")],
                               at: now())
            return .executed(confirmedBy: "read_back")
        }
        if pull.state != "open" { return try fail(id, "not_merged", by: "read_back") }
        // Still open: not merged only once two reads, both at least a minute after the unknown outcome was
        // recorded (after the write was sent), show it open.
        let since = journal.unknownAt(id) ?? now()
        guard now().timeIntervalSince(since) >= MergeHandler.settleTime else { return .unknown }
        let reads = openReads.withLock { reads -> Int in
            reads[id, default: 0] += 1
            return reads[id]!
        }
        if reads >= 2 { return try fail(id, "not_merged", by: "read_back") }
        return .unknown
    }

    /// Every merge whose outcome is unknown, for the app to reconcile at launch and on a timer.
    public func unknown() -> [String] {
        journal.all().filter { $0.type == .githubMerge && $0.status == .outcomeUnknown }.map(\.id)
    }

    private func fail(_ id: String, _ error: String, by confirmedBy: String) throws -> Outcome {
        try journal.result(id, executed: false, confirmedBy: confirmedBy, fields: ["error": .string(error)], at: now())
        return .failed(error: error, confirmedBy: confirmedBy)
    }
}

/// Registers the GitHub token in Settings (13.00, _Credentials and the keychain_). Only a user token is accepted, and
/// only for the configured user: a bot, an app installation or another user is refused. The token goes straight to
/// the vault, bound to the origin and the user's stable id; it is never shown, logged or passed on.
public enum GitHubSetup {
    public enum Refusal: Error, Equatable, Sendable {
        case notAUser
        case otherUser(login: String)
        case rejected
        case unreachable
    }

    /// GitHub's page for a new classic token with the `repo` scope, on the web host for this API origin:
    /// `api.github.com` → `github.com`, `api.{host}` → `{host}` (GitHub Enterprise Cloud), any other host as is.
    /// An origin that is not `https://host` gives github.com's page. Beacon opens it in the browser; Beacon itself
    /// does not contact it.
    public static func newTokenURL(apiOrigin: String) -> URL {
        var web = "https://github.com"
        if let normalized = try? CredentialBinding.normalize(apiOrigin), let parts = URLComponents(string: normalized),
           let host = parts.host {
            let site = host.hasPrefix("api.") ? String(host.dropFirst(4)) : host
            web = "https://" + site + (parts.port.map { ":\($0)" } ?? "")
        }
        return URL(string: web + "/settings/tokens/new?scopes=repo&description=Beacon%20(merges)")!
    }

    public static func register(token: Secret, origin: String, vault: Vault, settings url: URL,
                                configuration: URLSessionConfiguration = .ephemeral) async throws -> GitHubSettings {
        let normalized = try CredentialBinding.normalize(origin)
        let api = GitHubAPI(client: try ServiceClient(origin: normalized, authorization: token, headers: GitHubAPI.headers,
                                                      configuration: configuration))
        let user: (id: Int64, login: String, type: String)
        do {
            user = try await api.user()
        } catch is GitHubAPI.Failure {
            throw Refusal.rejected
        } catch {
            throw Refusal.unreachable
        }
        guard user.type == "User" else { throw Refusal.notAUser }
        var settings = try AppSettings.load(url)
        if let existing = settings.github, existing.account != "github:user:\(user.id)" {
            throw Refusal.otherUser(login: existing.login)
        }
        let github = GitHubSettings(origin: normalized, account: "github:user:\(user.id)", login: user.login)
        if let existing = settings.github, existing.origin != github.origin { try? vault.remove(try existing.binding) }
        try vault.store(token, for: try github.binding)
        settings.github = github
        try settings.save(url)
        return github
    }

    /// Forgets the account: the token leaves the vault and merges are no longer offered.
    public static func remove(vault: Vault, settings url: URL) throws {
        var settings = try AppSettings.load(url)
        if let github = settings.github { try vault.remove(try github.binding) }
        settings.github = nil
        try settings.save(url)
    }
}

/// A value behind a lock.
final class Locked<Value>: @unchecked Sendable {
    private let lock = NSLock()
    private var value: Value

    init(_ value: Value) {
        self.value = value
    }

    func withLock<T>(_ body: (inout Value) throws -> T) rethrows -> T {
        lock.lock()
        defer { lock.unlock() }
        return try body(&value)
    }
}
