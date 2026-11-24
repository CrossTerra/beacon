import Foundation

/// Fixed reasons only: neither Slack's error text nor a message is part of a refusal.
public enum SlackRefusal: Error, Equatable, Sendable {
    case notConfigured, notAUser, accountMismatch, workspaceMismatch, destinationChanged
    case archived, notMember, channelNotFound, threadNotFound, visibilityUnknown
    /// `needed` is the scope Slack named, or the scopes Settings found missing. Nil when Slack did not name one.
    case missingScope(String?)
    case rejected, unavailable, busy, messageTooLong, shuttingDown, metadataRefused
    case rateLimited(Int)

    var code: String {
        switch self {
        case .notConfigured: "not_configured"
        case .notAUser: "not_a_user"
        case .accountMismatch: "account_mismatch"
        case .workspaceMismatch: "workspace_mismatch"
        case .destinationChanged: "destination_changed"
        case .archived: "channel_archived"
        case .notMember: "not_in_channel"
        case .channelNotFound: "channel_not_found"
        case .threadNotFound: "thread_not_found"
        case .visibilityUnknown: "visibility_unknown"
        case .missingScope: "missing_scope"
        case .rejected: "slack_refused"
        case .unavailable: "slack_unavailable"
        case .busy: "busy"
        case .messageTooLong: "message_too_long"
        case .shuttingDown: "shutting_down"
        case .metadataRefused: "metadata_refused"
        case .rateLimited: "rate_limited"
        }
    }

    public var explanation: String {
        switch self {
        case .notConfigured: "Add your Slack account in Settings."
        case .notAUser: "Paste a Slack User OAuth Token (xoxp-…), not a bot or app token."
        case .accountMismatch: "Slack returned a different user. Check the token, or remove the old account before switching."
        case .workspaceMismatch: "The workspace does not match the one in Settings."
        case .destinationChanged: "The account or conversation changed. Ask for a new proposal."
        case .archived: "The conversation is archived."
        case .notMember: "You are not a member of this conversation."
        case .channelNotFound: "Slack could not find the conversation."
        case .threadNotFound: "Slack could not find the parent message."
        case .visibilityUnknown: "Slack did not confirm the conversation’s visibility."
        case .missingScope(let needed):
            if let needed, !needed.isEmpty {
                needed.contains(", ") ? "The token lacks Slack scopes: \(needed)."
                    : "The token lacks the Slack scope \(needed)."
            } else {
                "The token lacks a Slack scope. Check the scopes listed in Settings."
            }
        case .rejected: "Slack refused the request."
        case .unavailable: "Slack could not be read."
        case .busy: "Beacon has reached its preflight read limit. Try again later."
        case .messageTooLong: "The draft exceeds 4,000 UTF-16 code units. Ask for a shorter proposal."
        case .shuttingDown: "Beacon is quitting."
        case .metadataRefused: "Slack refused the operation id Beacon attaches as message metadata. Nothing was posted."
        case .rateLimited(let seconds): "Slack is rate limiting requests. Try again in \(seconds) seconds."
        }
    }
}

/// The user scopes a Slack approval token must hold. History is included: a reply's preview and a lost post's
/// read-back use it. Names are the scope tokens Slack uses, not a secret.
enum SlackScopes {
    static let required = [
        "chat:write",
        "channels:read", "groups:read", "im:read", "mpim:read", "users:read",
        "channels:history", "groups:history", "im:history", "mpim:history",
    ]

    /// A comma-separated scope list, or nil when it is not one. Slack's free text never reaches the journal.
    static func shown(_ raw: String?) -> String? {
        guard let raw else { return nil }
        let parts = raw.split(separator: ",").map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty }
        guard !parts.isEmpty, parts.allSatisfy({ Field.matches($0, "[a-z][a-z0-9_:-]{0,63}") }) else { return nil }
        return parts.joined(separator: ", ")
    }

    static func missing(from header: String) -> [String] {
        let have = Set(header.split(separator: ",").map { $0.trimmingCharacters(in: .whitespaces) })
        return required.filter { !have.contains($0) }
    }
}

enum SlackVisibility: String, Sendable {
    case `public` = "Public channel"
    case `private` = "Private channel"
    case dm = "DM"
    case groupDM = "Group DM"

    /// The word the window shows. The raw value stays the journal's, so an old placement still decodes.
    var word: String {
        switch self {
        case .public: "public"
        case .private: "private"
        case .dm: "DM"
        case .groupDM: "group DM"
        }
    }
}

/// A name safe to put in the window: one line, controls made visible, and not so long it crowds out the ids below.
enum SlackDisplay {
    static func unknown(_ id: String) -> String { "unknown channel \(id)" }

    static func shown(_ raw: String, limit: Int = 80) -> String? {
        let flat = raw.replacingOccurrences(of: "\n", with: " ").replacingOccurrences(of: "\r", with: " ")
        let text = visibleText(flat).trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty else { return nil }
        let scalars = text.unicodeScalars
        if scalars.count <= limit { return text }
        return String(String.UnicodeScalarView(scalars.prefix(limit)))
    }
}

/// The approval window's name for a Slack post. The raw ids stay under Show exact parameters.
public func slackApprovalTitle(words: String?, channel: String) -> String {
    "Post in Slack \(words ?? SlackDisplay.unknown(channel))"
}

/// Obtained by the authenticated Slack lookup before journaling; never from the proposal frame.
public struct SlackPlacement: Equatable, Sendable {
    let account: String
    let visibility: SlackVisibility
    /// People's words for the window (`#general · public · Fixture Workspace`). Local only: not a record field,
    /// not part of a hash, and not checked as the destination.
    let words: String?

    var json: JSON {
        var fields: [String: JSON] = ["account": .string(account), "visibility": .string(visibility.rawValue)]
        if let words { fields["words"] = .string(words) }
        return .object(fields)
    }

    init(account: String, visibility: SlackVisibility, words: String? = nil) {
        self.account = account
        self.visibility = visibility
        self.words = words
    }

    init?(_ json: JSON?) {
        guard let account = json?["account"]?.stringValue, let value = json?["visibility"]?.stringValue,
              let visibility = SlackVisibility(rawValue: value) else { return nil }
        let words = json?["words"]?.stringValue
        self.init(account: account, visibility: visibility, words: words.flatMap { $0.isEmpty ? nil : $0 })
    }
}

/// Typed parameters and destination from Signal's schema. `thread_ts` is null for a top-level post.
struct SlackDraft {
    let workspace: String
    let channel: String
    let thread: String?
    let text: String

    init(_ params: JSON) throws {
        let p = try ActionType.slackPost.validate(params: params)
        workspace = p["workspace"]!.stringValue!
        channel = p["channel"]!.stringValue!
        thread = p["thread_ts"]?.stringValue
        text = p["text"]!.stringValue!
    }

    var destination: JSON {
        .object(["workspace": .string(workspace), "channel": .string(channel), "thread_ts": thread.map(JSON.string) ?? .null])
    }

    func publicSubject(at date: Date) -> String {
        if let thread { return "slack:\(channel):\(thread)" }
        // Match the collector's local day for a top-level message. Signal pins the subject at the proposal, so
        // a post approved after midnight still records on the proposal's day.
        let formatter = DateFormatter()
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.calendar = Calendar(identifier: .gregorian)
        formatter.timeZone = .current
        formatter.dateFormat = "yyyy-MM-dd"
        return "slack:\(channel):\(formatter.string(from: date))"
    }

    func checkLength() throws {
        // Slack may split longer text into multiple messages. Don't offer an approval for that.
        guard text.utf16.count <= 4_000 else { throw SlackRefusal.messageTooLong }
    }
}

struct SlackConversation: Decodable {
    let id: String
    let name: String?
    let user: String?
    let context_team_id: String?
    let is_channel: Bool?
    let is_group: Bool?
    let is_private: Bool?
    let is_im: Bool?
    let is_mpim: Bool?
    let is_archived: Bool?
    let is_frozen: Bool?
    let is_member: Bool?

    var visibility: SlackVisibility? {
        if is_im == true { return .dm }
        if is_mpim == true { return .groupDM }
        if is_group == true || is_private == true { return .private }
        if is_channel == true && is_private == false { return .public }
        return nil
    }
}

struct SlackMessage: Decodable {
    let type: String?
    let subtype: String?
    let channel: String?
    let user: String?
    let ts: String?
    let thread_ts: String?
    let text: String?
    let client_msg_id: String?
    let metadata: SlackMetadata?
}

/// Message metadata, as `include_all_metadata=true` returns it. Anything not in this shape is ignored, not an error.
struct SlackMetadata: Decodable {
    static let eventType = "beacon_post"
    let eventType: String?
    let op: String?

    enum CodingKeys: String, CodingKey { case event_type, event_payload }
    enum PayloadKeys: String, CodingKey { case op }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        eventType = try? c.decodeIfPresent(String.self, forKey: .event_type)
        op = try? c.nestedContainer(keyedBy: PayloadKeys.self, forKey: .event_payload).decodeIfPresent(String.self, forKey: .op)
    }

    /// The metadata Beacon sends with a post: the operation id stored in `started`, and nothing else.
    static func json(op: String) -> JSON {
        .object(["event_type": .string(eventType), "event_payload": .object(["op": .string(op)])])
    }
}

struct SlackIdentity: Decodable, Sendable {
    let team_id: String
    let user_id: String
    let team: String
    let user: String
    let bot_id: String?
}

/// Slack's rate limit is per method and workspace. A 429 never sleeps a write and then sends it again. Deadlines are
/// kept per purpose, so a 429 met while reading back a lost reply does not stop a new proposal's preflight, nor an
/// intake lookup the window's.
final class SlackRateLimits: Sendable {
    enum Purpose: Sendable, Hashable {
        /// The bounded lookup before a proposal is recorded.
        case intake
        /// The window's preflight, the final reads before the write, and the write.
        case preflight
        /// Reading back a lost or unclear reply.
        case readBack
    }

    private struct Key: Hashable {
        let workspace: String
        let method: SlackAPI.Method
        let purpose: Purpose
    }
    private let deadlines = Locked<[Key: Date]>([:])

    func check(_ method: SlackAPI.Method, workspace: String, purpose: Purpose, at date: Date) throws {
        try deadlines.withLock {
            $0 = $0.filter { $0.value > date }
            if let until = $0[Key(workspace: workspace, method: method, purpose: purpose)] {
                throw SlackRefusal.rateLimited(Int(ceil(until.timeIntervalSince(date))))
            }
        }
    }

    func deferMethod(_ method: SlackAPI.Method, workspace: String, purpose: Purpose, at date: Date,
                     response: HTTPURLResponse) -> SlackRefusal {
        let header = response.value(forHTTPHeaderField: "Retry-After")?.trimmingCharacters(in: .whitespaces)
        let seconds = header.flatMap(Int32.init).flatMap { $0 >= 0 ? Int($0) : nil } ?? 60
        let key = Key(workspace: workspace, method: method, purpose: purpose)
        deadlines.withLock { $0[key] = max($0[key] ?? date, date.addingTimeInterval(TimeInterval(seconds))) }
        return .rateLimited(seconds)
    }
}

/// Only these paths and their typed query items are reachable with the Slack vault token.
struct SlackAPI: Sendable {
    enum Method: String, Sendable {
        case auth = "auth.test", info = "conversations.info", members = "conversations.members", user = "users.info"
        case history = "conversations.history", replies = "conversations.replies", post = "chat.postMessage"
    }

    /// A client for the token with this request deadline, in seconds.
    typealias Connect = @Sendable (Secret, TimeInterval) throws -> ServiceClient

    /// Intake makes at most two reads, and `beacon act propose` allows 10 seconds for the whole exchange.
    static let intakeTimeout: TimeInterval = 4
    /// Everything else, the write above all: a slow answer to chat.postMessage must not be cut short into an
    /// unknown outcome.
    static let actionTimeout: TimeInterval = 25

    /// The deadlines the app's clients use, unless a test injects its own.
    static let productionTimeouts = Timeouts(intake: intakeTimeout, action: actionTimeout)

    /// Tests inject generous deadlines: the whole suite's TLS fixtures run at once, and a slow CI runner can take
    /// longer than the production deadlines to schedule a request. Production keeps its own.
    struct Timeouts: Sendable {
        let intake: TimeInterval
        let action: TimeInterval
    }

    static func connect(_ token: Secret, timeout: TimeInterval) throws -> ServiceClient {
        try ServiceClient(origin: SlackSettings.origin, authorization: token, timeout: timeout, resourceTimeout: timeout)
    }

    let client: ServiceClient
    let workspace: String
    let rates: SlackRateLimits
    var purpose: SlackRateLimits.Purpose = .preflight
    let now: @Sendable () -> Date
    var meter: (@Sendable () async throws -> Void)?

    init(client: ServiceClient, workspace: String, rates: SlackRateLimits, purpose: SlackRateLimits.Purpose = .preflight,
         now: @escaping @Sendable () -> Date) {
        self.client = client
        self.workspace = workspace
        self.rates = rates
        self.purpose = purpose
        self.now = now
    }

    func identity() async throws -> SlackIdentity {
        try await account().identity
    }

    /// `auth.test`, plus the `X-OAuth-Scopes` header when Slack sent one.
    func account() async throws -> (identity: SlackIdentity, scopes: String?) {
        let (body, response) = try await fetch(.auth)
        let identity = try decode(SlackIdentity.self, from: body)
        guard identity.bot_id == nil, Field.matches(identity.user_id, "[UW][A-Z0-9]{0,31}") else { throw SlackRefusal.notAUser }
        guard Field.matches(identity.team_id, Field.slackID), !identity.team.isEmpty, !identity.user.isEmpty else {
            throw SlackRefusal.unavailable
        }
        return (identity, response.value(forHTTPHeaderField: "X-OAuth-Scopes"))
    }

    func conversation(_ channel: String) async throws -> SlackConversation {
        struct Info: Decodable { let channel: SlackConversation }
        let info = try await get(Info.self, .info, [.init(name: "channel", value: channel)]).channel
        guard info.id == channel else { throw SlackRefusal.channelNotFound }
        if let team = info.context_team_id, team != workspace { throw SlackRefusal.workspaceMismatch }
        return info
    }

    func requireMember(_ conversation: SlackConversation, user: String) async throws {
        if let member = conversation.is_member {
            guard member else { throw SlackRefusal.notMember }
            return
        }
        // Slack does not always include is_member (notably for DMs). A bounded members lookup proves membership.
        struct Members: Decodable { let members: [String]; let response_metadata: Cursor? }
        let query = [URLQueryItem(name: "channel", value: conversation.id), .init(name: "limit", value: "200")]
        var cursor: String?
        for _ in 0..<3 {
            let page = try await get(Members.self, .members, Self.withCursor(query, cursor))
            if page.members.contains(user) { return }
            guard let next = try page.response_metadata?.next(), next != cursor else { throw SlackRefusal.notMember }
            cursor = next
        }
        throw SlackRefusal.unavailable
    }

    /// One page of members, for naming a group direct message. Membership is still `requireMember`.
    func memberIDs(_ channel: String) async throws -> [String] {
        struct Members: Decodable { let members: [String] }
        guard Field.matches(channel, Field.slackID) else { throw SlackRefusal.unavailable }
        return try await get(Members.self, .members, [
            .init(name: "channel", value: channel), .init(name: "limit", value: "200"),
        ]).members
    }

    func userName(_ id: String) async throws -> String {
        struct User: Decodable { let id: String; let name: String; let real_name: String? }
        struct UserInfo: Decodable { let user: User }
        guard Field.matches(id, Field.slackID) else { throw SlackRefusal.unavailable }
        let user = try await get(UserInfo.self, .user, [.init(name: "user", value: id)]).user
        guard user.id == id, !user.name.isEmpty else { throw SlackRefusal.unavailable }
        return user.real_name.flatMap { $0.isEmpty ? nil : $0 } ?? user.name
    }

    func parent(channel: String, thread: String) async throws -> SlackMessage {
        let page = try await messages(channel: channel, thread: thread, limit: 1)
        // Slack may return a reply as the first message if its ts was passed instead of the root's.
        guard let root = page.messages.first, root.type == "message", root.ts == thread,
              root.thread_ts == nil || root.thread_ts == thread, root.channel == nil || root.channel == channel,
              root.subtype != "tombstone", root.subtype != "message_deleted" else { throw SlackRefusal.threadNotFound }
        return root
    }

    struct Cursor: Decodable {
        let next_cursor: String?

        func next() throws -> String? {
            guard let next_cursor, !next_cursor.isEmpty else { return nil }
            guard next_cursor.utf8.count <= 2048 else { throw SlackRefusal.unavailable }
            return next_cursor
        }
    }

    struct Page: Decodable {
        let messages: [SlackMessage]
        let has_more: Bool?
        let response_metadata: Cursor?
    }

    func messages(channel: String, thread: String?, limit: Int, oldest: String? = nil, latest: String? = nil,
                  cursor: String? = nil) async throws -> Page {
        var query: [URLQueryItem] = [.init(name: "channel", value: channel), .init(name: "limit", value: String(limit))]
        if let thread { query.append(.init(name: "ts", value: thread)) }
        if let oldest, let latest {
            // A read-back: message metadata is returned only when asked for.
            query += [.init(name: "oldest", value: oldest), .init(name: "latest", value: latest), .init(name: "inclusive", value: "true"),
                      .init(name: "include_all_metadata", value: "true")]
        }
        return try await get(Page.self, thread == nil ? .history : .replies, Self.withCursor(query, cursor))
    }

    private static func withCursor(_ query: [URLQueryItem], _ cursor: String?) -> [URLQueryItem] {
        cursor.map { query + [.init(name: "cursor", value: $0)] } ?? query
    }

    enum PostReply {
        case posted(String), refused(SlackRefusal), ambiguous(String)
    }

    /// The operation id goes twice: as message metadata (plan 13.00), which Slack returns to a read with
    /// `include_all_metadata`, and as `client_msg_id`, which Slack's clients use and an API post may keep. Read-back
    /// accepts either.
    func post(_ draft: SlackDraft, marker: String, user: String) async throws -> PostReply {
        try canPost()
        var body: [String: JSON] = [
            "channel": .string(draft.channel), "text": .string(draft.text), "client_msg_id": .string(marker),
            "metadata": SlackMetadata.json(op: marker),
            "mrkdwn": .bool(true), "parse": "none", "unfurl_links": .bool(false), "unfurl_media": .bool(false),
        ]
        body["thread_ts"] = draft.thread.map(JSON.string)
        let response: (status: Int, body: Data, response: HTTPURLResponse)
        do {
            response = try await client.send("POST", "/api/\(Method.post.rawValue)", raw: JSON.object(body).canonicalData, oneShot: true)
        } catch {
            return .ambiguous("reply_lost")
        }
        if let limit = rateLimit(.post, response) { return .refused(limit) }
        if response.status >= 500 { return .ambiguous("server_error") }
        if response.status == 401 || response.status == 403 || response.status == 404 || response.status == 413 {
            return .refused(.rejected)
        }
        guard let envelope = try? JSONDecoder().decode(Envelope.self, from: response.body) else {
            return .ambiguous("reply_unclear")
        }
        guard envelope.ok else {
            // Slack documents that internal_error/fatal_error can follow a successful side effect. Only known
            // pre-write refusals are a definite failure; arbitrary provider strings never go into the journal.
            return Self.refusal(envelope).map(PostReply.refused) ?? .ambiguous("reply_unclear")
        }
        struct Posted: Decodable { let channel: String; let ts: String; let message: SlackMessage? }
        guard (200..<300).contains(response.status),
              let posted = try? JSONDecoder().decode(Posted.self, from: response.body),
               posted.channel == draft.channel, Field.matches(posted.ts, Field.slackTS), posted.ts != draft.thread,
               posted.message?.ts.map({ $0 == posted.ts }) != false,
               posted.message?.channel.map({ $0 == draft.channel }) != false,
               posted.message?.user.map({ $0 == user }) != false,
               posted.message?.thread_ts.map({ $0 == (draft.thread ?? posted.ts) }) != false else {
            return .ambiguous("reply_unclear")
        }
        return .posted(posted.ts)
    }

    func canPost() throws { try rates.check(.post, workspace: workspace, purpose: .preflight, at: now()) }

    private struct Envelope: Decodable { let ok: Bool; let error: String?; let needed: String? }

    private func fetch(_ method: Method, _ query: [URLQueryItem] = []) async throws -> (Data, HTTPURLResponse) {
        try rates.check(method, workspace: workspace, purpose: purpose, at: now())
        try await meter?()
        try Task.checkCancellation()
        let response = try await client.send("GET", "/api/\(method.rawValue)", query: query)
        if let limit = rateLimit(method, response) { throw limit }
        if response.status == 401 || response.status == 403 { throw SlackRefusal.rejected }
        guard (200..<300).contains(response.status) else { throw SlackRefusal.unavailable }
        return (response.body, response.response)
    }

    private func get<T: Decodable>(_ type: T.Type, _ method: Method, _ query: [URLQueryItem] = []) async throws -> T {
        try decode(type, from: try await fetch(method, query).0)
    }

    private func decode<T: Decodable>(_ type: T.Type, from body: Data) throws -> T {
        guard let envelope = try? JSONDecoder().decode(Envelope.self, from: body) else { throw SlackRefusal.unavailable }
        guard envelope.ok else { throw Self.refusal(envelope) ?? SlackRefusal.unavailable }
        guard let result = try? JSONDecoder().decode(type, from: body) else { throw SlackRefusal.unavailable }
        return result
    }

    private func rateLimit(_ method: Method, _ response: (status: Int, body: Data, response: HTTPURLResponse)) -> SlackRefusal? {
        guard response.status == 429 else { return nil }
        return rates.deferMethod(method, workspace: workspace, purpose: method == .post ? .preflight : purpose, at: now(),
                                 response: response.response)
    }

    /// Slack's documented refusals that are checked before anything is written. Any other error, `internal_error`,
    /// `fatal_error` and `request_timeout` among them, may follow a write and stays ambiguous.
    private static func refusal(_ envelope: Envelope) -> SlackRefusal? {
        if envelope.error == "missing_scope" { return .missingScope(SlackScopes.shown(envelope.needed)) }
        return refusal(envelope.error)
    }

    static func refusal(_ error: String?) -> SlackRefusal? {
        switch error {
        case "is_archived", "channel_is_archived": .archived
        case "channel_not_found": .channelNotFound
        case "thread_not_found", "cannot_reply_to_message": .threadNotFound
        case "not_in_channel": .notMember
        case "missing_scope": .missingScope(nil)
        case "msg_too_long", "msg_blocks_too_long": .messageTooLong
        case "metadata_must_be_sent_from_app", "invalid_metadata_format", "invalid_metadata_schema", "metadata_too_large":
            .metadataRefused
        case "invalid_auth", "not_authed", "account_inactive", "token_revoked", "token_expired", "not_allowed_token_type",
             "access_denied", "no_permission", "ekm_access_denied", "team_access_not_granted", "enterprise_is_restricted",
             "two_factor_setup_required", "org_login_required", "restricted_action", "restricted_action_read_only_channel",
             "restricted_action_non_threadable_channel", "restricted_action_thread_locked", "restricted_action_thread_only_channel",
             "posting_to_channel_denied", "message_limit_exceeded", "no_text", "invalid_blocks", "invalid_blocks_format",
             "too_many_attachments", "invalid_attachments", "too_many_contact_cards", "as_user_not_supported",
             "duplicate_channel_not_found", "duplicate_message_not_found", "invalid_arguments", "invalid_arg_name",
             "invalid_array_arg", "invalid_charset", "invalid_form_data", "invalid_post_type", "missing_post_type",
             "invalid_json", "json_not_object", "deprecated_endpoint", "method_deprecated", "upgrade_required": .rejected
        default: nil
        }
    }
}
