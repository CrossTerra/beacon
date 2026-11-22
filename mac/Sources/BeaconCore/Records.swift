import Foundation

/// Builds the request Signal's record API takes for one journal entry (docs/approval-app.md in Signal). The bytes
/// are canonical JSON, built once and kept, so a resend is byte for byte the same.
enum Record {
    static func bytes(key: String, nonce: String, proposal: String, kind: String, at date: Date, body: JSON,
                      proposed: JSON, person: String, handle: String? = nil, subject spelled: String? = nil) throws -> Data {
        guard PublicID.isValid(person, prefix: "act") else { throw Invalid(field: "/metadata/person") }
        let action = proposed["type"]!.stringValue!
        var metadata: [String: JSON] = ["proposal": .string(proposal), "person": .string(person), "action": .string(action)]
        var sealed: [String: JSON] = [:]
        func copy(_ names: [String], into target: inout [String: JSON]) {
            for name in names { if let value = body[name] { target[name] = value } }
        }
        switch kind {
        case "proposed":
            metadata["params_hash"] = .string(ApprovalHash.params(type: action, params: body["params"]!,
                                                                   salt: body["salt"]!.stringValue!))
            metadata["expires_at"] = .string(timestamp(Date(milliseconds: body["expires_at"]!.intValue!)))
            // Verified: the proposal came from a run Beacon launched, by the process group of the connecting peer.
            var proposer: [String: JSON] = ["agent": body["agent"]!, "verified": .bool(body["proposer_runner_id"] != nil)]
            proposer["session"] = body["session"]
            proposer["runner_id"] = body["proposer_runner_id"]
            metadata["proposer"] = .object(proposer)
            metadata["runner_id"] = body["runner_id"]
            copy(["params", "salt", "reason"], into: &sealed)
        case "approved":
            copy(["approved_params_hash", "approved_hash", "edited"], into: &metadata)
            sealed = ["params": body["params"]!, "salt": proposed["salt"]!, "origin": body["origin"]!,
                      "account": body["account"]!, "destination": body["destination"]!]
            copy(["original_model"], into: &sealed)
        case "declined", "withdrawn", "expired":
            copy(["reason"], into: &metadata)
            copy(["note"], into: &sealed)
        case "started":
            copy(["approved_hash"], into: &metadata)
        case "outcome_unknown":
            copy(["reason"], into: &metadata)
        case "executed":
            copy(["confirmed_by", "native"], into: &metadata)
            if action == ActionType.agentRun.rawValue { metadata["runner_id"] = proposed["runner_id"] }
            copy(["evidence", "result"], into: &sealed)
        case "failed":
            copy(["confirmed_by", "error"], into: &metadata)
            copy(["evidence", "result"], into: &sealed)
        case "runner_working", "runner_closed":
            metadata["runner_id"] = proposed["runner_id"]
        case "runner_waiting", "runner_stalled", "runner_finished", "runner_failed":
            metadata["runner_id"] = proposed["runner_id"]
            metadata["from"] = "runner"
            copy(["error"], into: &metadata)
            copy(["question", "last_output", "report", "detail"], into: &sealed)
        default:
            throw Invalid(field: "/type")
        }
        let type = kind.hasPrefix("runner_") ? "app.runner." + kind.dropFirst("runner_".count) : "app.action." + kind
        var record: [String: JSON] = [
            "version": .int(1), "key": .string(key), "type": .string(type),
            "subject": .string(try subject(action: action, params: proposed["params"]!, person: person, handle: handle,
                                           runnerID: proposed["runner_id"]?.stringValue, spelled: spelled)),
            "occurred_at": .string(timestamp(date)), "metadata": .object(metadata),
        ]
        if !sealed.isEmpty {
            record["sealed"] = .object(["level": "owner+agents", "replay_nonce": .string(nonce), "metadata": .object(sealed)])
        }
        return JSON.object(record).canonicalData
    }

    /// A merge is about its pull request, with the repository as the proposal spelled it, or, from the approval
    /// on, as GitHub spelled it (`spelled`, kept at approval; the same subject case aside, which is how Signal
    /// compares them). A run is about itself, `agent-run:{handle}:{runner id body}`, restricted to the person in
    /// Signal; without the person's handle it cannot be built. A post is about the person's own subject until its
    /// handler can tell whether the conversation's subject is public (a private channel is under `slack:dm:`).
    static func subject(action: String, params: JSON, person: String, handle: String? = nil, runnerID: String? = nil,
                        spelled: String? = nil) throws -> String {
        if action == ActionType.githubMerge.rawValue, let repo = params["repo"]?.stringValue,
           let number = params["number"]?.intValue {
            let proposed = "github:pr:\(repo)#\(number)"
            if let spelled, spelled.lowercased() == proposed.lowercased() { return spelled }
            return proposed
        }
        if action == ActionType.agentRun.rawValue {
            guard let handle, Field.matches(handle, Field.handle), let runnerID, PublicID.isValid(runnerID, prefix: "run")
            else { throw Invalid(field: "/subject") }
            return "agent-run:\(handle):\(runnerID.dropFirst("run_".count))"
        }
        return "signal:actor:\(person)"
    }

    static func timestamp(_ date: Date) -> String {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        formatter.timeZone = TimeZone(identifier: "UTC")
        return formatter.string(from: date)
    }
}
