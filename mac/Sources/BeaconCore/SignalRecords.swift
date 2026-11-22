import Foundation

/// Signal's record API (`POST /api/app-records`, docs/approval-app.md in Signal), with the app actor's key from the
/// vault, bound to the Signal origin and that actor. It sends the outbox's exact bytes and maps Signal's answer;
/// it never changes or rebuilds a record.
public struct SignalRecords: RecordTransport {
    let client: ServiceClient

    public init(client: ServiceClient) {
        self.client = client
    }

    /// A transport for the configured Signal, with the vault's key for that origin and actor only.
    public static func connect(_ settings: SignalSettings, vault: Vault,
                               configuration: URLSessionConfiguration = .ephemeral) throws -> SignalRecords {
        let key = try vault.credential(for: try settings.binding)
        return SignalRecords(client: try ServiceClient(origin: settings.origin, authorization: key,
                                                       headers: ["User-Agent": "beacon-app"], configuration: configuration,
                                                       maxBytes: 1 << 20))
    }

    public func post(_ bytes: Data) async throws -> RecordAnswer {
        let (status, body, response) = try await client.send("POST", "/api/app-records", raw: bytes)
        let answer = (try? JSONSerialization.jsonObject(with: body)) as? [String: Any]
        let error = answer?["error"] as? [String: Any]
        let details = error?["details"] as? [String: Any]
        let field = details?["field"] as? String
        return SignalRecords.classify(status: status, code: error?["code"] as? String, reason: details?["reason"] as? String,
                                      field: field, retryAfter: response.value(forHTTPHeaderField: "Retry-After"),
                                      eventID: answer?["event_id"] as? String, replayed: answer?["replayed"] as? Bool)
    }

    /// What to do with Signal's answer. Only refusals resending cannot fix stop delivery: a conflict, the subject,
    /// an invalid record, a body too large. A refused key, a rate limit, a permission that may come back, a clock
    /// ahead of Signal's and Signal's own failures are sent again later.
    static func classify(status: Int, code: String?, reason: String?, field: String?, retryAfter: String?,
                         eventID: String?, replayed: Bool?) -> RecordAnswer {
        switch status {
        case 200, 201:
            guard let eventID else { return .retry(after: nil, ambiguous: true) }
            return .stored(eventID: eventID, replayed: replayed ?? (status == 200))
        case 409:
            return .stop(reason: "conflict:" + (reason ?? code ?? "conflict"))
        case 401:
            return .retry(after: nil, ambiguous: false)
        case 429:
            return .retry(after: retryAfter.flatMap(TimeInterval.init).map { max(0, min($0, 3600)) }, ambiguous: false)
        case 403 where field == "/subject":
            return .stop(reason: "refused:403:/subject")
        case 403:
            return .retry(after: nil, ambiguous: false)
        case 400 where field == "/occurred_at":
            return .retry(after: nil, ambiguous: false)
        case 400..<500:
            return .stop(reason: "refused:\(status)" + (field.map { ":" + $0 } ?? ""))
        default:
            // Sealing unavailable, or a server error: Signal may have stored it.
            return .retry(after: retryAfter.flatMap(TimeInterval.init).map { max(0, min($0, 3600)) }, ambiguous: true)
        }
    }
}

/// Signal's `/beacon` page gives the person one setup code to paste into Settings:
/// `beacon-setup:v1:` + base64url(JSON `{ "origin", "key" }`). It holds the app actor's key, so it is read once into a
/// `Secret`, never kept, shown or logged; a refusal says what was wrong without echoing any of it.
public enum SignalSetupCode {
    public static let prefix = "beacon-setup:v1:"

    public enum Problem: Error, Equatable, Sendable {
        /// Not a `beacon-setup:v1:` code.
        case prefix
        /// The rest is not base64url, or not a JSON object.
        case encoding
        /// `origin` or `key` is missing, empty, not a string, or not usable (an origin other than `https://host`).
        case fields
    }

    public static func decode(_ text: String) throws -> (origin: String, key: Secret) {
        let code = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard code.hasPrefix(prefix) else { throw Problem.prefix }
        let body = code.dropFirst(prefix.count)
        guard !body.isEmpty, body.allSatisfy({ $0.isASCII && ($0.isLetter || $0.isNumber || $0 == "-" || $0 == "_" || $0 == "=") })
        else { throw Problem.encoding }
        var base64 = body.replacingOccurrences(of: "-", with: "+").replacingOccurrences(of: "_", with: "/")
        base64 = base64.trimmingCharacters(in: CharacterSet(charactersIn: "="))
        base64 += String(repeating: "=", count: (4 - base64.count % 4) % 4)
        guard let data = Data(base64Encoded: base64),
              let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else { throw Problem.encoding }
        guard let origin = object["origin"] as? String, let normalized = try? CredentialBinding.normalize(origin),
              let key = object["key"] as? String, !key.isEmpty,
              key.unicodeScalars.allSatisfy({ $0.isASCII && $0.value > 0x20 && $0.value < 0x7f }) else { throw Problem.fields }
        return (normalized, Secret(key))
    }
}

/// Registers the app actor's key in Settings (13.00-D16, D25). Signal must answer `GET /api/me` with an integration
/// actor whose key has exactly `read` and `write`, and `GET /api/actors/{id}` must show that actor active, marked
/// `approval_app`, and working for an active person (`works_for`). That person is who the records are about: Beacon
/// takes it from Signal and never asks for it. The key goes straight to the vault, bound to the Signal origin and the
/// app actor; it is never shown, logged or passed on.
public enum SignalSetup {
    public enum Refusal: Error, Equatable, Sendable {
        /// The key's actor is not an active integration.
        case notAnApp
        /// The actor is not marked `approval_app`.
        case notApprovalApp
        /// The actor works for nobody, or not for an active person.
        case noPerson
        /// The setup code could not be read.
        case code(SignalSetupCode.Problem)
        case scopes
        /// `401` or `403`: Signal refused the key.
        case rejected
        /// No answer, a `429` or a `5xx`: try again, after `retryAfter` seconds when Signal said.
        case unreachable(retryAfter: Int?)
        /// Any other answer, such as a `404` from `/api/me` (not a Signal at that origin) or a `400`.
        case unexpected(status: Int)
    }

    /// Registers from the setup code the person pasted.
    public static func register(code: String, vault: Vault, settings url: URL,
                                configuration: URLSessionConfiguration = .ephemeral) async throws -> SignalSettings {
        let decoded: (origin: String, key: Secret)
        do {
            decoded = try SignalSetupCode.decode(code)
        } catch let problem as SignalSetupCode.Problem {
            throw Refusal.code(problem)
        }
        return try await register(key: decoded.key, origin: decoded.origin, vault: vault, settings: url,
                                  configuration: configuration)
    }

    static func register(key: Secret, origin: String, vault: Vault, settings url: URL,
                                configuration: URLSessionConfiguration = .ephemeral) async throws -> SignalSettings {
        let normalized = try CredentialBinding.normalize(origin)
        let client = try ServiceClient(origin: normalized, authorization: key, headers: ["User-Agent": "beacon-app"],
                                       configuration: configuration, maxBytes: 1 << 20)
        let me = try await read(client, "/api/me", missing: .unexpected(status: 404))
        let actor = me["actor"] as? [String: Any]
        guard actor?["type"] as? String == "integration", let id = actor?["id"] as? String,
              PublicID.isValid(id, prefix: "act") else { throw Refusal.notAnApp }
        guard Set(me["scopes"] as? [String] ?? []) == ["read", "write"], (me["scopes"] as? [String])?.count == 2 else {
            throw Refusal.scopes
        }
        let app = try await read(client, "/api/actors/\(id)", missing: .notAnApp)
        guard app["id"] as? String == id, app["type"] as? String == "integration", app["deactivated_at"] is NSNull
        else { throw Refusal.notAnApp }
        guard app["approval_app"] as? Bool == true else { throw Refusal.notApprovalApp }
        guard let person = app["works_for"] as? String, PublicID.isValid(person, prefix: "act"), person != id else {
            throw Refusal.noPerson
        }
        let human = try await read(client, "/api/actors/\(person)", missing: .noPerson)
        guard human["id"] as? String == person, human["type"] as? String == "human", human["deactivated_at"] is NSNull
        else { throw Refusal.noPerson }
        var settings = try AppSettings.load(url)
        if let existing = settings.signal { try? vault.remove(try existing.binding) }
        let signal = SignalSettings(origin: normalized, person: person, actor: id,
                                    handle: actor?["handle"] as? String ?? id,
                                    personHandle: human["handle"] as? String, personName: human["name"] as? String)
        try vault.store(key, for: try signal.binding)
        settings.signal = signal
        try settings.save(url)
        return signal
    }

    /// One read. `200` with an object is the answer; `404` is `missing` (an actor Signal does not show, only asked
    /// once `/api/me` accepted the key); anything else is classified, never taken as a fact about the key.
    private static func read(_ client: ServiceClient, _ path: String, missing: Refusal) async throws -> [String: Any] {
        let answer: (status: Int, body: Data, response: HTTPURLResponse)
        do {
            answer = try await client.send("GET", path)
        } catch {
            throw Refusal.unreachable(retryAfter: nil)
        }
        let (status, body, response) = answer
        switch status {
        case 200:
            guard let object = (try? JSONSerialization.jsonObject(with: body)) as? [String: Any] else {
                throw Refusal.unexpected(status: status)
            }
            return object
        case 401, 403: throw Refusal.rejected
        case 404: throw missing
        case 429, 500...599:
            let seconds = response.value(forHTTPHeaderField: "Retry-After")
                .flatMap { Int($0.trimmingCharacters(in: .whitespaces)) }
            throw Refusal.unreachable(retryAfter: seconds.map { max(0, min($0, 3600)) })
        default: throw Refusal.unexpected(status: status)
        }
    }

    public static func remove(vault: Vault, settings url: URL) throws {
        var settings = try AppSettings.load(url)
        if let signal = settings.signal { try vault.remove(try signal.binding) }
        settings.signal = nil
        try settings.save(url)
    }
}
