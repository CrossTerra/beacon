import Foundation
@testable import BeaconCore

/// A fake GitHub API behind a URL protocol: each instance answers for its own `https://` host, records every
/// request Beacon makes, and can lose a reply after acting on it.
final class FakeGitHub: @unchecked Sendable {
    struct Request {
        let method: String
        let path: String
        let authorization: String?
        let body: [String: Any]?
    }

    struct Pull {
        var state = "open"
        var merged = false
        var mergeable: Bool? = true
        var mergeableState = "clean"
        var base = "main"
        var head = String(repeating: "a", count: 40)
        var mergeCommit: String?
        var mergedBy: Int64?
    }

    enum MergeBehaviour {
        case normal
        /// Answer with this status; merge first when `merging`.
        case status(Int, merging: Bool)
        /// Merge, then lose the reply.
        case loseReplyAfterMerging
        /// Lose the connection without merging.
        case loseReplyWithoutMerging
    }

    private static let lock = NSLock()
    nonisolated(unsafe) private static var byHost: [String: FakeGitHub] = [:]

    let host = "gh-\(randomHex(4)).test"
    var origin: String { "https://\(host)" }
    private let lock = NSLock()
    private(set) var requests: [Request] = []

    var user: (id: Int64, login: String, type: String) = (123, "winton", "User")
    var repositoryID: Int64 = 456
    var fullName = "CrossTerra/signal"
    var pull = Pull()
    /// `(user id, state)` in order.
    var reviews: [(Int64, String)] = [(9, "APPROVED")]
    var ruleChecks = ["ci"]
    var protectionChecks: [String] = []
    /// Check runs on the head: name to conclusion (`nil` while in progress).
    var checkRuns: [String: String?] = ["ci": "success"]
    var statuses: [String: String] = [:]
    /// More check runs, as GitHub reports them: `id`, `app`, `name`, `status`, `conclusion`, `started_at`.
    var runs: [[String: Any]] = []
    var merge = MergeBehaviour.normal
    /// Counts reads of the PR, and may change it after the n-th (to move it between Beacon's reads).
    var pullReads = 0
    var afterPullRead: ((Int, FakeGitHub) -> Void)?
    var down = false
    /// Answer `GET /user` with a redirect to this URL.
    var redirectUser: String?

    init() {
        FakeGitHub.lock.withLock { FakeGitHub.byHost[host] = self }
    }

    deinit {
        _ = FakeGitHub.lock.withLock { FakeGitHub.byHost.removeValue(forKey: host) }
    }

    func update(_ body: (FakeGitHub) -> Void) {
        lock.withLock { body(self) }
    }

    static var configuration: URLSessionConfiguration {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [FakeGitHubProtocol.self]
        return configuration
    }

    static func handler(for host: String) -> FakeGitHub? {
        lock.withLock { byHost[host] }
    }

    enum Answer {
        case json(Int, Any)
        case redirect(String)
        case lost
    }

    func handle(_ request: URLRequest, body: Data?) -> Answer {
        lock.withLock {
            // GitHub reads owner and repository names case-insensitively.
            var path = request.url!.path
            if path.lowercased().hasPrefix("/repos/crossterra/signal/") {
                path = "/repos/CrossTerra/signal/" + path.dropFirst("/repos/crossterra/signal/".count)
            }
            let parsed = body.flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] }
            requests.append(Request(method: request.httpMethod ?? "GET", path: path,
                                    authorization: request.value(forHTTPHeaderField: "Authorization"), body: parsed))
            if down { return .lost }
            let repo = "/repos/CrossTerra/signal"
            switch (request.httpMethod ?? "GET", path) {
            case ("GET", "/user"):
                if let redirectUser { return .redirect(redirectUser) }
                return .json(200, ["id": user.id, "login": user.login, "type": user.type])
            case ("GET", repo + "/pulls/140"):
                pullReads += 1
                defer { afterPullRead?(pullReads, self) }
                var answer: [String: Any] = [
                    "state": pull.state, "merged": pull.merged, "mergeable_state": pull.mergeableState,
                    "title": "Add the record API",
                    "base": ["ref": pull.base, "repo": ["id": repositoryID, "full_name": fullName]], "head": ["sha": pull.head],
                ]
                answer["mergeable"] = pull.mergeable ?? NSNull()
                answer["merge_commit_sha"] = pull.mergeCommit ?? NSNull()
                answer["merged_by"] = pull.mergedBy.map { ["id": $0] } ?? NSNull()
                return .json(200, answer)
            case ("GET", repo + "/pulls/140/reviews"):
                return .json(200, reviews.map { ["user": ["id": $0.0], "state": $0.1] })
            case ("GET", repo + "/rules/branches/main"):
                return .json(200, [["type": "required_status_checks",
                                    "parameters": ["required_status_checks": ruleChecks.map { ["context": $0] }]]])
            case ("GET", repo + "/branches/main"):
                if protectionChecks.isEmpty { return .json(200, ["name": "main"]) }
                return .json(200, ["name": "main", "protection": ["required_status_checks": ["contexts": protectionChecks]]])
            case ("GET", let p) where p.hasPrefix(repo + "/commits/") && p.hasSuffix("/check-runs"):
                return .json(200, ["check_runs": checkRuns.map { name, conclusion in
                    ["name": name, "status": conclusion == nil ? "in_progress" : "completed",
                     "conclusion": conclusion ?? NSNull(), "app": ["id": 1]] as [String: Any]
                } + runs])
            case ("GET", let p) where p.hasPrefix(repo + "/commits/") && p.hasSuffix("/status"):
                return .json(200, ["statuses": statuses.map { ["context": $0, "state": $1] }])
            case ("PUT", repo + "/pulls/140/merge"):
                return mergeRequest(parsed ?? [:])
            default:
                return .json(404, ["message": "Not Found"])
            }
        }
    }

    private func mergeRequest(_ body: [String: Any]) -> Answer {
        func doMerge() {
            pull.merged = true
            pull.state = "closed"
            pull.mergeCommit = String(repeating: "c", count: 40)
            pull.mergedBy = user.id
        }
        switch merge {
        case .status(let status, let merging):
            if merging { doMerge() }
            return .json(status, ["message": "fake"])
        case .loseReplyAfterMerging:
            doMerge()
            return .lost
        case .loseReplyWithoutMerging:
            return .lost
        case .normal:
            guard body["sha"] as? String == pull.head else { return .json(409, ["message": "Head branch was modified."]) }
            guard pull.mergeable == true, !pull.merged else { return .json(405, ["message": "Pull Request is not mergeable"]) }
            doMerge()
            return .json(200, ["merged": true, "sha": pull.mergeCommit!, "message": "Pull Request successfully merged"])
        }
    }
}

final class FakeGitHubProtocol: URLProtocol {
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        guard let host = request.url?.host, let fake = FakeGitHub.handler(for: host) else {
            client?.urlProtocol(self, didFailWithError: URLError(.cannotFindHost))
            return
        }
        var body = request.httpBody
        if body == nil, let stream = request.httpBodyStream {
            var data = Data()
            stream.open()
            var buffer = [UInt8](repeating: 0, count: 4096)
            while stream.hasBytesAvailable {
                let n = stream.read(&buffer, maxLength: buffer.count)
                if n <= 0 { break }
                data.append(buffer, count: n)
            }
            stream.close()
            body = data
        }
        switch fake.handle(request, body: body) {
        case .json(let status, let value):
            let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1",
                                           headerFields: ["Content-Type": "application/json"])!
            client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: try! JSONSerialization.data(withJSONObject: value))
            client?.urlProtocolDidFinishLoading(self)
        case .redirect(let location):
            let response = HTTPURLResponse(url: request.url!, statusCode: 301, httpVersion: "HTTP/1.1",
                                           headerFields: ["Location": location])!
            var next = URLRequest(url: URL(string: location)!)
            next.allHTTPHeaderFields = request.allHTTPHeaderFields
            client?.urlProtocol(self, wasRedirectedTo: next, redirectResponse: response)
            client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: Data())
            client?.urlProtocolDidFinishLoading(self)
        case .lost:
            client?.urlProtocol(self, didFailWithError: URLError(.networkConnectionLost))
        }
    }

    override func stopLoading() {}
}
