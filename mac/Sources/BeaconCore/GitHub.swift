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

    /// The contexts that passed on `sha`: completed check runs that succeeded (or were neutral or skipped), and
    /// commit statuses in `success`. Any other result is not green.
    public func greenChecks(_ repo: String, sha: String) async throws -> Set<String> {
        let segments = try GitHubAPI.segments(repo)
        var green = Set<String>(), seen = Set<String>(), failed = Set<String>()
        for page in 1...10 {
            let runs = try await object("/repos/\(segments)/commits/\(sha)/check-runs",
                                        query: [.init(name: "per_page", value: "100"), .init(name: "page", value: "\(page)")])
            let list = runs["check_runs"] as? [[String: Any]] ?? []
            for run in list {
                guard let name = run["name"] as? String else { continue }
                seen.insert(name)
                if run["status"] as? String == "completed",
                   ["success", "neutral", "skipped"].contains(run["conclusion"] as? String ?? "") {
                    green.insert(name)
                } else {
                    failed.insert(name)
                }
            }
            if list.count < 100 { break }
            if page == 10 { throw ServiceError.tooLarge }
        }
        let status = try await object("/repos/\(segments)/commits/\(sha)/status")
        for entry in status["statuses"] as? [[String: Any]] ?? [] {
            guard let context = entry["context"] as? String, !seen.contains(context) else { continue }
            if entry["state"] as? String == "success" { green.insert(context) } else { failed.insert(context) }
        }
        // A name that both passed and failed (a re-run alongside an old run) is not green.
        return green.subtracting(failed)
    }

    /// `PUT …/merge` with the frozen method and `sha: head`. GitHub refuses if the head moved.
    public func merge(_ repo: String, _ number: Int64, head: String, method: String) async throws -> (status: Int, sha: String?) {
        let (status, body, _) = try await client.send("PUT", "/repos/\(try GitHubAPI.segments(repo))/pulls/\(number)/merge",
                                                      json: ["sha": head, "merge_method": method])
        let answer = (try? JSONSerialization.jsonObject(with: body)) as? [String: Any]
        return (status, answer?["merged"] as? Bool == true ? answer?["sha"] as? String : nil)
    }
}
