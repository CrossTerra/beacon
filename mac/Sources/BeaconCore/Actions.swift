import Foundation

/// The v1 action types and their exact parameters (13.00, _Action types_; Signal's contract,
/// docs/approval-app.md). Extra fields are refused.
public enum ActionType: String, CaseIterable, Sendable {
    case githubMerge = "github.merge"
    case slackPost = "slack.post"
    case agentRun = "agent.run"
}

/// A refusal that names the field, without echoing its value.
public struct Invalid: Error, Equatable, Sendable {
    public let field: String
}

public enum Limits {
    /// A prompt or a Slack text, in Unicode scalar values.
    public static let text = 32_000
    /// A reason, note, preflight, evidence or result.
    public static let note = 4_000
}

/// Field checks that mirror Signal's (src/domain/appRecords.ts at the fixture commit).
enum Field {
    static func matches(_ value: String, _ pattern: String) -> Bool {
        value.range(of: "^(?:" + pattern + ")$", options: .regularExpression) != nil
    }

    static func string(_ value: JSON?, _ field: String, _ pattern: String) throws -> String {
        guard let text = value?.stringValue, matches(text, pattern) else { throw Invalid(field: field) }
        return text
    }

    static func text(_ value: JSON?, _ field: String, max: Int) throws -> String {
        guard let text = value?.stringValue, (1...max).contains(text.unicodeScalars.count) else {
            throw Invalid(field: field)
        }
        return text
    }

    static func positive(_ value: JSON?, _ field: String) throws -> Int64 {
        guard let number = value?.intValue, number > 0 else { throw Invalid(field: field) }
        return number
    }

    static func object(_ value: JSON?, _ field: String, required: Set<String>, optional: Set<String> = []) throws
        -> [String: JSON]
    {
        guard let fields = value?.objectValue else { throw Invalid(field: field) }
        for key in fields.keys where !required.contains(key) && !optional.contains(key) {
            throw Invalid(field: field + "/" + key)
        }
        for key in required where fields[key] == nil {
            throw Invalid(field: field + "/" + key)
        }
        return fields
    }

    static let opaque = "[A-Za-z0-9_.:@/-]{1,200}"
    static let repo = "[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}"
    static let branch = "[\\x21-\\x7e]{1,255}"
    static let slackID = "[A-Z0-9]{1,32}"
    static let slackTS = "[0-9]{1,16}\\.[0-9]{6}"
    static let model = "[A-Za-z0-9_.:@-]{1,80}/[A-Za-z0-9_.:@/-]{1,120}"
    static let checkout = "[a-z0-9][a-z0-9_.-]{0,63}"
    static let subject = "[\\x21-\\x7e]{1,200}"
}

extension ActionType {
    /// Validates `params` for this type and returns them unchanged.
    public func validate(params: JSON?) throws -> JSON {
        switch self {
        case .githubMerge:
            let p = try Field.object(params, "/params", required: ["repo", "number", "base", "head", "method"])
            _ = try Field.string(p["repo"], "/params/repo", Field.repo)
            _ = try Field.positive(p["number"], "/params/number")
            _ = try Field.string(p["base"], "/params/base", Field.branch)
            _ = try Field.string(p["head"], "/params/head", "[0-9a-f]{40}")
            _ = try Field.string(p["method"], "/params/method", "merge|squash|rebase")
        case .slackPost:
            let p = try Field.object(params, "/params", required: ["workspace", "channel", "thread_ts", "text"])
            _ = try Field.string(p["workspace"], "/params/workspace", Field.slackID)
            _ = try Field.string(p["channel"], "/params/channel", Field.slackID)
            if p["thread_ts"] != .null { _ = try Field.string(p["thread_ts"], "/params/thread_ts", Field.slackTS) }
            _ = try Field.text(p["text"], "/params/text", max: Limits.text)
        case .agentRun:
            let p = try Field.object(params, "/params", required: ["checkout", "prompt", "model"])
            _ = try Field.string(p["checkout"], "/params/checkout", Field.checkout)
            _ = try Field.text(p["prompt"], "/params/prompt", max: Limits.text)
            _ = try Field.string(p["model"], "/params/model", Field.model)
        }
        return params!
    }

    /// The parameter schema the socket's `types` operation returns, as JSON Schema.
    public var paramsSchema: JSON {
        func string(_ pattern: String) -> JSON { .object(["type": .string("string"), "pattern": .string("^(?:\(pattern))$")]) }
        let positive: JSON = .object(["type": .string("integer"), "minimum": .int(1), "maximum": .int(JSON.maxSafeInteger)])
        func text(_ max: Int) -> JSON {
            .object(["type": .string("string"), "minLength": .int(1), "maxLength": .int(Int64(max))])
        }
        let properties: [String: JSON]
        switch self {
        case .githubMerge:
            properties = [
                "repo": string(Field.repo), "number": positive, "base": string(Field.branch),
                "head": string("[0-9a-f]{40}"), "method": .object(["enum": .array(["merge", "squash", "rebase"].map(JSON.string))]),
            ]
        case .slackPost:
            properties = [
                "workspace": string(Field.slackID), "channel": string(Field.slackID),
                "thread_ts": .object(["type": .array([.string("string"), .string("null")]),
                                      "pattern": .string("^(?:\(Field.slackTS))$")]),
                "text": text(Limits.text),
            ]
        case .agentRun:
            properties = ["checkout": string(Field.checkout), "prompt": text(Limits.text), "model": string(Field.model)]
        }
        return .object([
            "type": .string("object"), "additionalProperties": .bool(false),
            "required": .array(properties.keys.sorted().map(JSON.string)), "properties": .object(properties),
        ])
    }
}

/// A `propose` request: `{ type, params, reason, agent: { name, session? }, context?: { subject } }`.
public struct ProposalRequest: Equatable, Sendable {
    public let type: ActionType
    public let params: JSON
    public let reason: String
    public let agent: String
    public let session: String?
    public let subject: String?

    public init(_ request: [String: JSON]) throws {
        let fields = try Field.object(.object(request), "", required: ["op", "type", "params", "reason", "agent"], optional: ["context"])
        guard let name = fields["type"]?.stringValue, let type = ActionType(rawValue: name) else { throw Invalid(field: "/type") }
        self.type = type
        params = try type.validate(params: fields["params"])
        reason = try Field.text(fields["reason"], "/reason", max: Limits.note)
        let agent = try Field.object(fields["agent"], "/agent", required: ["name"], optional: ["session"])
        self.agent = try Field.string(agent["name"], "/agent/name", Field.opaque)
        session = try agent["session"].map { try Field.string($0, "/agent/session", Field.opaque) }
        if let context = fields["context"] {
            let c = try Field.object(context, "/context", required: ["subject"])
            subject = try Field.string(c["subject"], "/context/subject", Field.subject)
        } else {
            subject = nil
        }
    }
}
