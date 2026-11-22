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
        case scopes
        case rejected
        case unreachable
    }

    public static func register(key: Secret, origin: String, vault: Vault, settings url: URL,
                                configuration: URLSessionConfiguration = .ephemeral) async throws -> SignalSettings {
        let normalized = try CredentialBinding.normalize(origin)
        let client = try ServiceClient(origin: normalized, authorization: key, headers: ["User-Agent": "beacon-app"],
                                       configuration: configuration, maxBytes: 1 << 20)
        func read(_ path: String) async throws -> [String: Any]? {
            do {
                let (status, value) = try await client.json("GET", path)
                if status == 401 { throw Refusal.rejected }
                return status == 200 ? value as? [String: Any] : nil
            } catch let refusal as Refusal {
                throw refusal
            } catch {
                throw Refusal.unreachable
            }
        }
        guard let me = try await read("/api/me") else { throw Refusal.rejected }
        let actor = me["actor"] as? [String: Any]
        guard actor?["type"] as? String == "integration", let id = actor?["id"] as? String,
              PublicID.isValid(id, prefix: "act") else { throw Refusal.notAnApp }
        guard Set(me["scopes"] as? [String] ?? []) == ["read", "write"], (me["scopes"] as? [String])?.count == 2 else {
            throw Refusal.scopes
        }
        guard let app = try await read("/api/actors/\(id)") else { throw Refusal.notAnApp }
        guard app["id"] as? String == id, app["type"] as? String == "integration", app["deactivated_at"] is NSNull
        else { throw Refusal.notAnApp }
        guard app["approval_app"] as? Bool == true else { throw Refusal.notApprovalApp }
        guard let person = app["works_for"] as? String, PublicID.isValid(person, prefix: "act"), person != id,
              let human = try await read("/api/actors/\(person)"), human["id"] as? String == person,
              human["type"] as? String == "human", human["deactivated_at"] is NSNull else { throw Refusal.noPerson }
        var settings = try AppSettings.load(url)
        if let existing = settings.signal { try? vault.remove(try existing.binding) }
        let signal = SignalSettings(origin: normalized, person: person, actor: id,
                                    handle: actor?["handle"] as? String ?? id,
                                    personHandle: human["handle"] as? String)
        try vault.store(key, for: try signal.binding)
        settings.signal = signal
        try settings.save(url)
        return signal
    }

    public static func remove(vault: Vault, settings url: URL) throws {
        var settings = try AppSettings.load(url)
        if let signal = settings.signal { try vault.remove(try signal.binding) }
        settings.signal = nil
        try settings.save(url)
    }
}
