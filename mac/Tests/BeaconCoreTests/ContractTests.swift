import Foundation
import Testing
@testable import BeaconCore

/// Signal's fixture contract (docs/approval-app/*.json), copied into Fixtures/approval-app.
func fixture(_ name: String) throws -> JSON {
    let url = Bundle.module.url(forResource: "Fixtures/approval-app/" + name, withExtension: nil)!
    return try JSON.parse(try Data(contentsOf: url))
}

func array(_ value: JSON?) -> [JSON] {
    if case .array(let values) = value { return values }
    return []
}

struct ContractTests {
    @Test func canonicalJSONAndHashesMatchSignal() throws {
        let cases = array(try fixture("hashes.json"))
        #expect(cases.count >= 7)
        for item in cases {
            let value = item["value"]!
            #expect(value.canonical == item["canonical"]?.stringValue, "\(item["note"]!.stringValue!)")
            #expect(value.hash == item["hash"]?.stringValue)
            // Parsing the canonical text gives the same value back.
            #expect(try JSON.parse(Data(item["canonical"]!.stringValue!.utf8)) == value)
        }
    }

    /// Every proposal and approval in Signal's journal: the plain hashes recompute from the sealed parts with
    /// Beacon's code, including an approval whose model was changed.
    @Test func journalHashesRecompute() throws {
        var proposals: [String: (type: String, hash: String)] = [:]
        var checked = 0
        for entry in array(try fixture("journal.json")) {
            let body = entry["body"]!
            let metadata = body["metadata"]!
            let sealed = body["sealed"]?["metadata"]
            let id = metadata["proposal"]?.stringValue ?? ""
            let action = metadata["action"]?.stringValue ?? ""
            #expect(id.isEmpty || PublicID.isValid(id, prefix: "apr"))
            switch body["type"]?.stringValue {
            case "app.action.proposed":
                let type = ActionType(rawValue: action)!
                let params = try type.validate(params: sealed!["params"])
                let hash = ApprovalHash.params(type: action, params: params, salt: sealed!["salt"]!.stringValue!)
                #expect(hash == metadata["params_hash"]?.stringValue)
                proposals[id] = (action, hash)
                checked += 1
            case "app.action.approved":
                let params = sealed!["params"]!
                let salt = sealed!["salt"]!.stringValue!
                #expect(ApprovalHash.params(type: action, params: params, salt: salt) == metadata["approved_params_hash"]?.stringValue)
                let origin = sealed!["origin"]?.stringValue
                #expect(ApprovalHash.approved(id: id, type: action, params: params, origin: origin,
                                              account: sealed!["account"]!.stringValue!,
                                              destination: sealed!["destination"]!, salt: salt)
                        == metadata["approved_hash"]?.stringValue)
                // The proposal's params are the approval's with the original model.
                var original = params.objectValue!
                if let model = sealed!["original_model"] { original["model"] = model }
                #expect(ApprovalHash.params(type: action, params: .object(original), salt: salt) == proposals[id]?.hash)
                checked += 1
            default:
                break
            }
        }
        #expect(checked >= 10)
    }

    @Test func refusesWhatSignalRefuses() throws {
        for text in ["1.0", "1e0", "-1E2", "9007199254740992", "-9007199254740992", "01", "{\"a\":1,\"a\":2}",
                     "\"\\ud800\"", "\"\\udc00x\"", "\"a\u{01}b\"", "[1,]", "{}x", "", "nul", "\"\\x41\""] {
            #expect(throws: JSONError.self, "\(text)") { try JSON.parse(Data(text.utf8)) }
        }
        #expect(throws: JSONError.self) { try JSON.parse(Data([0x22, 0xff, 0x22])) }
        #expect(try JSON.parse(Data("\"\\ud83d\\udc4d\"".utf8)) == .string("👍"))
        #expect(try JSON.parse(Data(" [9007199254740991, -0, {\"b\": null}] ".utf8)).canonical == "[9007199254740991,0,{\"b\":null}]")
    }

    @Test func publicIDs() {
        let random: [UInt8] = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10]
        let early = PublicID.make("apr", at: Date(timeIntervalSince1970: 1_700_000_000), random: random)
        let late = PublicID.make("apr", at: Date(timeIntervalSince1970: 1_700_000_001), random: random)
        #expect(PublicID.isValid(early, prefix: "apr") && !PublicID.isValid(early, prefix: "jrn"))
        #expect(early.count == 30 && early < late)
        #expect(PublicID.isValid("apr_01m7wyd4m0f858m2ga1850m2ga", prefix: "apr"))
        #expect(PublicID.isValid(PublicID.make("jrn"), prefix: "jrn"))
        for body in ["81m7wyd4m0f858m2ga1850m2ga", "01m7wyd4m0f858m2ga1850m2g", "01m7wyd4m0f858m2ga1850m2gu"] {
            #expect(!PublicID.isValid("apr_" + body, prefix: "apr"))
        }
        let bytes = PublicID.decode("01m7wyd4m0f858m2ga1850m2ga")!
        #expect(PublicID.encode(bytes) == "01m7wyd4m0f858m2ga1850m2ga")
        #expect(randomHex(16).count == 32)
    }

    @Test func actionSchemas() throws {
        let head = String(repeating: "a", count: 40)
        func merge(_ fields: [String: JSON]) -> JSON {
            var base: [String: JSON] = ["repo": "CrossTerra/signal", "number": .int(140), "base": "main",
                                        "head": .string(head), "method": "squash"]
            fields.forEach { base[$0] = $1 }
            return .object(base)
        }
        _ = try ActionType.githubMerge.validate(params: merge([:]))
        let refused: [(ActionType, JSON, String)] = [
            (.githubMerge, merge(["head": "abc"]), "/params/head"),
            (.githubMerge, merge(["method": "fast-forward"]), "/params/method"),
            (.githubMerge, merge(["number": .int(0)]), "/params/number"),
            (.githubMerge, merge(["extra": .bool(true)]), "/params/extra"),
            (.slackPost, .object(["workspace": "T1", "channel": "C1", "thread_ts": "1.2", "text": "hi"]), "/params/thread_ts"),
            (.slackPost, .object(["workspace": "T1", "channel": "C1", "thread_ts": .null, "text": ""]), "/params/text"),
            (.slackPost, .object(["workspace": "T1", "channel": "C1", "text": "hi"]), "/params/thread_ts"),
            (.agentRun, .object(["checkout": "Signal", "prompt": "x", "model": "a/b"]), "/params/checkout"),
            (.agentRun, .object(["checkout": "signal", "prompt": "x", "model": "opus"]), "/params/model"),
            (.agentRun, .object(["checkout": "signal", "prompt": .string(String(repeating: "👍", count: 32_001)), "model": "a/b"]), "/params/prompt"),
        ]
        for (type, params, field) in refused {
            #expect(throws: Invalid(field: field)) { try type.validate(params: params) }
        }
        _ = try ActionType.slackPost.validate(params: .object(["workspace": "T1", "channel": "C1", "thread_ts": .null, "text": "hi"]))
        _ = try ActionType.agentRun.validate(params: .object(["checkout": "signal", "model": "anthropic/claude-opus-5",
                                                               "prompt": .string(String(repeating: "👍", count: 32_000))]))
    }
}
