import Foundation

/// What Beacon reads from GitHub about a pull request, before approval and again just before the merge.
public struct PullRequestState: Equatable, Sendable {
    public var repositoryID: Int64
    public var state: String
    public var merged: Bool
    /// Nil while GitHub is still computing it.
    public var mergeable: Bool?
    public var mergeableState: String
    public var base: String
    public var head: String
    public var title: String
    public var mergeCommit: String?
    public var mergedBy: Int64?
}

/// GitHub's REST API for the reads and the one write a merge needs. Every path is built from the validated
/// repository, number, branch and SHA; nothing from an agent becomes a URL.
public struct GitHubAPI: Sendable {
    public let client: ServiceClient

    public init(client: ServiceClient) {
        self.client = client
    }

    public static let headers = ["Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28",
                                 "User-Agent": "beacon-app"]

    public struct Failure: Error, Equatable, Sendable {
        public let status: Int
    }

    /// `owner/repo` as two path segments. `.` and `..` match Signal's pattern but would change the path.
    static func segments(_ repo: String) throws -> String {
        let parts = repo.split(separator: "/", omittingEmptySubsequences: false).map(String.init)
        guard parts.count == 2, parts.allSatisfy({ !$0.isEmpty && $0 != "." && $0 != ".." }) else {
            throw Invalid(field: "/params/repo")
        }
        return parts.map(encode).joined(separator: "/")
    }

    static func encode(_ segment: String) -> String {
        var allowed = CharacterSet.alphanumerics
        allowed.insert(charactersIn: "-._~")
        return segment.addingPercentEncoding(withAllowedCharacters: allowed)!
    }

    private func object(_ path: String, query: [URLQueryItem] = []) async throws -> [String: Any] {
        let (status, value) = try await client.json("GET", path, query: query)
        guard let object = value as? [String: Any] else { throw Failure(status: status) }
        return object
    }

    /// The token's user: `(id, login, type)`.
    public func user() async throws -> (id: Int64, login: String, type: String) {
        let user = try await object("/user")
        guard let id = (user["id"] as? NSNumber)?.int64Value, let login = user["login"] as? String,
              let type = user["type"] as? String else { throw ServiceError.malformed }
        return (id, login, type)
    }

    public func pull(_ repo: String, _ number: Int64) async throws -> PullRequestState {
        let pull = try await object("/repos/\(try GitHubAPI.segments(repo))/pulls/\(number)")
        let base = pull["base"] as? [String: Any], head = pull["head"] as? [String: Any]
        guard let repository = ((base?["repo"] as? [String: Any])?["id"] as? NSNumber)?.int64Value,
              let state = pull["state"] as? String, let baseRef = base?["ref"] as? String,
              let headSHA = head?["sha"] as? String else { throw ServiceError.malformed }
        return PullRequestState(
            repositoryID: repository, state: state, merged: pull["merged"] as? Bool ?? false,
            mergeable: pull["mergeable"] as? Bool, mergeableState: pull["mergeable_state"] as? String ?? "unknown",
            base: baseRef, head: headSHA, title: pull["title"] as? String ?? "",
            mergeCommit: pull["merge_commit_sha"] as? String,
            mergedBy: ((pull["merged_by"] as? [String: Any])?["id"] as? NSNumber)?.int64Value)
    }

    /// Whether any reviewer's latest review requests changes.
    public func changesRequested(_ repo: String, _ number: Int64) async throws -> Bool {
        var latest: [Int64: String] = [:]
        for page in 1...10 {
            let (status, value) = try await client.json("GET", "/repos/\(try GitHubAPI.segments(repo))/pulls/\(number)/reviews",
                                                        query: [.init(name: "per_page", value: "100"), .init(name: "page", value: "\(page)")])
            guard let reviews = value as? [[String: Any]] else { throw Failure(status: status) }
            for review in reviews {
                guard let user = ((review["user"] as? [String: Any])?["id"] as? NSNumber)?.int64Value,
                      let state = review["state"] as? String, state != "COMMENTED", state != "PENDING" else { continue }
                latest[user] = state
            }
            if reviews.count < 100 { return latest.values.contains("CHANGES_REQUESTED") }
        }
        throw ServiceError.tooLarge
    }

    /// The status check contexts required on `base`, from repository rules and classic branch protection. A
    /// rule or protection this account cannot read (403, 404) adds nothing; GitHub still enforces it.
    public func requiredChecks(_ repo: String, base: String) async throws -> Set<String> {
        let segments = try GitHubAPI.segments(repo), branch = GitHubAPI.encode(base)
        var required = Set<String>()
        let (rulesStatus, rules) = try await client.json("GET", "/repos/\(segments)/rules/branches/\(branch)")
        if let rules = rules as? [[String: Any]] {
            for rule in rules where rule["type"] as? String == "required_status_checks" {
                let checks = (rule["parameters"] as? [String: Any])?["required_status_checks"] as? [[String: Any]] ?? []
                required.formUnion(checks.compactMap { $0["context"] as? String })
            }
        } else if ![403, 404].contains(rulesStatus) {
            throw Failure(status: rulesStatus)
        }
        let (branchStatus, branchValue) = try await client.json("GET", "/repos/\(segments)/branches/\(branch)")
        if let protection = (branchValue as? [String: Any])?["protection"] as? [String: Any],
           let checks = protection["required_status_checks"] as? [String: Any] {
            required.formUnion(checks["contexts"] as? [String] ?? [])
            required.formUnion((checks["checks"] as? [[String: Any]] ?? []).compactMap { $0["context"] as? String })
        } else if branchValue == nil, ![403, 404].contains(branchStatus) {
            throw Failure(status: branchStatus)
        }
        return required
    }

    /// One reported check: a check run (keyed by its app and name) or a commit status (by context).
    struct Check: Equatable {
        var kind: String
        var name: String
        var app: Int64?
        var id: Int64?
        var startedAt: Date?
        var completed: Bool
        var conclusion: String
    }

    static let failing: Set<String> = ["failure", "cancelled", "timed_out", "action_required", "stale", "startup_failure"]
    static let statusFailing: Set<String> = ["failure", "error"]

    /// 0 passing, 1 not finished, 2 failing: between checks of one name from different apps, failing wins, then a
    /// check still running, else passing (as Signal's executor reads them, #131).
    static func rank(_ check: Check) -> Int {
        guard check.completed else { return 1 }
        if check.kind == "status" { return statusFailing.contains(check.conclusion) ? 2 : (check.conclusion == "success" ? 0 : 1) }
        return failing.contains(check.conclusion) ? 2 : 0
    }

    /// Whether `a` is a later run than `b` of the same check. GitHub numbers runs as it creates them, so a re-run has
    /// the higher id. Without ids, the later start wins; a run with no start (skipped or cancelled before it began)
    /// is the oldest once completed, the newest while not.
    static func newer(_ a: Check, than b: Check) -> Bool {
        if let x = a.id, let y = b.id, x != y { return x > y }
        func start(_ c: Check) -> Double {
            c.startedAt?.timeIntervalSince1970 ?? (c.completed ? -.infinity : .infinity)
        }
        if start(a) != start(b) { return start(a) > start(b) }
        return rank(a) > rank(b)
    }

    /// The names that are green: only the latest run of each name and app counts, so a re-run that passes clears an
    /// older failure, and a newer failure is not cleared by an older pass. A name is green when its latest run from
    /// every app, and its commit status if any, passed (success, neutral or skipped).
    static func green(_ reported: [Check]) -> Set<String> {
        var latest: [String: Check] = [:]
        for check in reported {
            let key = check.kind == "status" ? "status\u{0}\(check.name)" : "check\u{0}\(check.app ?? -1)\u{0}\(check.name)"
            if let previous = latest[key], !newer(check, than: previous) { continue }
            latest[key] = check
        }
        var worst: [String: Int] = [:]
        for check in latest.values { worst[check.name] = max(worst[check.name] ?? 0, rank(check)) }
        return Set(worst.filter { $0.value == 0 }.keys)
    }

    /// The contexts that passed on `sha`, from its check runs and its combined commit status.
    public func greenChecks(_ repo: String, sha: String) async throws -> Set<String> {
        let segments = try GitHubAPI.segments(repo)
        let dates = ISO8601DateFormatter()
        var reported: [Check] = []
        for page in 1...10 {
            let runs = try await object("/repos/\(segments)/commits/\(sha)/check-runs",
                                        query: [.init(name: "per_page", value: "100"), .init(name: "page", value: "\(page)")])
            let list = runs["check_runs"] as? [[String: Any]] ?? []
            for run in list {
                guard let name = run["name"] as? String else { continue }
                reported.append(Check(kind: "check", name: name,
                                      app: ((run["app"] as? [String: Any])?["id"] as? NSNumber)?.int64Value,
                                      id: (run["id"] as? NSNumber)?.int64Value,
                                      startedAt: (run["started_at"] as? String).flatMap(dates.date(from:)),
                                      completed: run["status"] as? String == "completed",
                                      conclusion: run["conclusion"] as? String ?? ""))
            }
            if list.count < 100 { break }
            if page == 10 { throw ServiceError.tooLarge }
        }
        // The combined status already holds the latest state of each context.
        let status = try await object("/repos/\(segments)/commits/\(sha)/status")
        for entry in status["statuses"] as? [[String: Any]] ?? [] {
            guard let context = entry["context"] as? String, let state = entry["state"] as? String else { continue }
            reported.append(Check(kind: "status", name: context, app: nil, id: nil, startedAt: nil,
                                  completed: state != "pending", conclusion: state))
        }
        return GitHubAPI.green(reported)
    }

    /// `PUT …/merge` with the frozen method and `sha: head`. GitHub refuses if the head moved.
    public func merge(_ repo: String, _ number: Int64, head: String, method: String) async throws -> (status: Int, sha: String?) {
        let (status, body, _) = try await client.send("PUT", "/repos/\(try GitHubAPI.segments(repo))/pulls/\(number)/merge",
                                                      json: ["sha": head, "merge_method": method])
        let answer = (try? JSONSerialization.jsonObject(with: body)) as? [String: Any]
        return (status, answer?["merged"] as? Bool == true ? answer?["sha"] as? String : nil)
    }
}
