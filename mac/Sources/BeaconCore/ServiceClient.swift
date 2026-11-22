import Foundation

public enum ServiceError: Error, Equatable, Sendable {
    /// The request may or may not have reached the service: no connection, a timeout, a lost reply.
    case transport(String)
    /// The service answered with a redirect, which Beacon never follows.
    case redirect(Int)
    /// The answer was larger than the client accepts.
    case tooLarge
    /// The answer could not be read.
    case malformed
    /// A request for a URL outside the client's origin. Nothing was sent.
    case foreignURL
}

/// One HTTPS origin and the credential bound to it (13.00-D31). Paths are built by typed clients from validated
/// ids; the client refuses any URL whose scheme, host or port is not its origin, follows no redirect, ignores
/// proxy settings, caps each answer, and never retries. A request either returns the service's answer or throws.
public final class ServiceClient: NSObject, @unchecked Sendable {
    public let origin: URL
    private let headers: [String: String]
    private let session: URLSession
    private let maxBytes: Int

    /// `origin` is `https://host[:port]`; `configuration` is for tests (a URL protocol standing in for the
    /// service). The credential goes only in the `Authorization` header of requests to this origin.
    public init(origin: String, authorization: Secret?, headers: [String: String] = [:],
                configuration: URLSessionConfiguration = .ephemeral, maxBytes: Int = 8 << 20,
                timeout: TimeInterval = 20) throws {
        guard let url = URL(string: try CredentialBinding.normalize(origin)) else { throw VaultError.origin }
        self.origin = url
        var all = headers
        if let authorization { all["Authorization"] = "Bearer " + authorization.value }
        self.headers = all
        self.maxBytes = maxBytes
        let configuration = configuration.copy() as! URLSessionConfiguration
        configuration.connectionProxyDictionary = [:]
        configuration.httpCookieStorage = nil
        configuration.urlCache = nil
        configuration.requestCachePolicy = .reloadIgnoringLocalCacheData
        configuration.timeoutIntervalForRequest = timeout
        configuration.timeoutIntervalForResource = timeout * 3
        configuration.httpMaximumConnectionsPerHost = 2
        configuration.waitsForConnectivity = false
        let queue = OperationQueue()
        queue.maxConcurrentOperationCount = 1
        session = URLSession(configuration: configuration, delegate: ServiceClient.Delegate(), delegateQueue: queue)
        super.init()
    }

    deinit { session.invalidateAndCancel() }

    /// Sends one request. `path` is already percent-encoded and starts with `/`. `json` is serialised; `raw` is
    /// sent as these exact bytes, as JSON.
    public func send(_ method: String, _ path: String, query: [URLQueryItem] = [], json body: Any? = nil,
                     raw: Data? = nil) async throws -> (status: Int, body: Data, response: HTTPURLResponse)
    {
        guard path.hasPrefix("/"), var components = URLComponents(url: origin, resolvingAgainstBaseURL: false) else {
            throw ServiceError.foreignURL
        }
        components.percentEncodedPath = path
        components.queryItems = query.isEmpty ? nil : query
        guard let url = components.url, sameOrigin(url) else { throw ServiceError.foreignURL }
        var request = URLRequest(url: url)
        request.httpMethod = method
        headers.forEach { request.setValue($1, forHTTPHeaderField: $0) }
        if let body {
            request.httpBody = try JSONSerialization.data(withJSONObject: body, options: [.sortedKeys])
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        } else if let raw {
            request.httpBody = raw
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        let data: Data
        let response: URLResponse
        do {
            (data, response) = try await session.data(for: request)
        } catch let error as URLError {
            throw ServiceError.transport(error.code.rawValue.description)
        } catch {
            throw ServiceError.transport("unknown")
        }
        guard let http = response as? HTTPURLResponse else { throw ServiceError.malformed }
        if (300..<400).contains(http.statusCode) { throw ServiceError.redirect(http.statusCode) }
        guard data.count <= maxBytes else { throw ServiceError.tooLarge }
        return (http.statusCode, data, http)
    }

    /// Sends a request and parses a JSON object or array from a 2xx answer.
    public func json(_ method: String, _ path: String, query: [URLQueryItem] = []) async throws -> (status: Int, value: Any?) {
        let (status, body, _) = try await send(method, path, query: query)
        guard (200..<300).contains(status) else { return (status, nil) }
        guard let value = try? JSONSerialization.jsonObject(with: body) else { throw ServiceError.malformed }
        return (status, value)
    }

    private func sameOrigin(_ url: URL) -> Bool {
        url.scheme == origin.scheme && url.host == origin.host && url.port == origin.port && url.user == nil
    }

    /// Refuses every redirect: the answer is returned as it is, and the credential goes nowhere else.
    final class Delegate: NSObject, URLSessionTaskDelegate {
        func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse,
                        newRequest request: URLRequest, completionHandler: @escaping (URLRequest?) -> Void) {
            completionHandler(nil)
        }
    }
}
