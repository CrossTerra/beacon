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
    public convenience init(origin: String, authorization: Secret?, headers: [String: String] = [:],
                            configuration: URLSessionConfiguration = .ephemeral, maxBytes: Int = 8 << 20,
                            timeout: TimeInterval = 20, resourceTimeout: TimeInterval? = nil) throws {
        try self.init(origin: origin, authorization: authorization, headers: headers, configuration: configuration,
                      maxBytes: maxBytes, timeout: timeout, resourceTimeout: resourceTimeout, delegate: Delegate())
    }

    // The TLS fixture supplies its own trust delegate. Production uses the system trust evaluation.
    init(origin: String, authorization: Secret?, headers: [String: String] = [:],
         configuration: URLSessionConfiguration = .ephemeral, maxBytes: Int = 8 << 20,
         timeout: TimeInterval, resourceTimeout: TimeInterval? = nil, delegate: Delegate) throws {
        guard let url = URL(string: try CredentialBinding.normalize(origin)) else { throw VaultError.origin }
        self.origin = url
        var all = headers
        if let authorization { all["Authorization"] = "Bearer " + authorization.value }
        self.headers = all
        self.maxBytes = maxBytes
        let configuration = configuration.copy() as! URLSessionConfiguration
        configuration.connectionProxyDictionary = [:]
        configuration.httpCookieStorage = nil
        configuration.urlCredentialStorage = nil
        configuration.urlCache = nil
        configuration.requestCachePolicy = .reloadIgnoringLocalCacheData
        configuration.timeoutIntervalForRequest = timeout
        configuration.timeoutIntervalForResource = resourceTimeout ?? timeout * 3
        configuration.httpMaximumConnectionsPerHost = 2
        configuration.waitsForConnectivity = false
        let queue = OperationQueue()
        queue.maxConcurrentOperationCount = 1
        session = URLSession(configuration: configuration, delegate: delegate, delegateQueue: queue)
        super.init()
    }

    deinit { session.invalidateAndCancel() }

    /// Sends one request. `path` is already percent-encoded and starts with `/`. `json` is serialised; `raw` is
    /// sent as these exact bytes, as JSON. For an at-most-once write, `oneShot` makes the body non-replayable.
    public func send(_ method: String, _ path: String, query: [URLQueryItem] = [], json body: Any? = nil,
                     raw: Data? = nil, oneShot: Bool = false) async throws -> (status: Int, body: Data, response: HTTPURLResponse)
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
        let payload = try body.map { try JSONSerialization.data(withJSONObject: $0, options: [.sortedKeys]) } ?? raw
        if let payload {
            if oneShot {
                request.httpBodyStream = InputStream(data: payload)
                request.setValue(String(payload.count), forHTTPHeaderField: "Content-Length")
            } else {
                request.httpBody = payload
            }
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        do {
            let (bytes, response) = try await session.bytes(for: request)
            guard let http = response as? HTTPURLResponse else {
                bytes.task.cancel()
                throw ServiceError.malformed
            }
            if (300..<400).contains(http.statusCode) {
                bytes.task.cancel()
                throw ServiceError.redirect(http.statusCode)
            }
            if response.expectedContentLength > maxBytes {
                bytes.task.cancel()
                throw ServiceError.tooLarge
            }
            var data = Data()
            for try await byte in bytes {
                guard data.count < maxBytes else {
                    bytes.task.cancel()
                    throw ServiceError.tooLarge
                }
                data.append(byte)
            }
            return (http.statusCode, data, http)
        } catch let error as ServiceError {
            throw error
        } catch let error as URLError {
            throw ServiceError.transport(error.code.rawValue.description)
        } catch {
            throw ServiceError.transport("unknown")
        }
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
    class Delegate: NSObject, URLSessionTaskDelegate, @unchecked Sendable {
        func urlSession(_ session: URLSession, didReceive challenge: URLAuthenticationChallenge,
                        completionHandler: @escaping @Sendable (URLSession.AuthChallengeDisposition, URLCredential?) -> Void) {
            completionHandler(.performDefaultHandling, nil)
        }

        func urlSession(_ session: URLSession, task: URLSessionTask, didReceive challenge: URLAuthenticationChallenge,
                        completionHandler: @escaping @Sendable (URLSession.AuthChallengeDisposition, URLCredential?) -> Void) {
            urlSession(session, didReceive: challenge, completionHandler: completionHandler)
        }

        func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse,
                        newRequest request: URLRequest, completionHandler: @escaping (URLRequest?) -> Void) {
            completionHandler(nil)
        }

        // CFNetwork may ask for a new stream after a connection failure. Never provide the body again.
        func urlSession(_ session: URLSession, task: URLSessionTask, needNewBodyStream completionHandler: @escaping (InputStream?) -> Void) {
            completionHandler(nil)
        }
    }
}
