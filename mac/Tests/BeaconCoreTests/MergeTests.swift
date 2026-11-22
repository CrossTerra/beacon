import CryptoKit
import Foundation
import Testing
@testable import BeaconCore

/// A merge handler against a fake GitHub, with a fixture vault key and a test clock.
final class MergeHarness: @unchecked Sendable {
    let github = FakeGitHub()
    let clock = Clock()
    let journal: Journal
    let vault: Vault
    let settingsURL: URL
    let handler: MergeHandler
    let token = "ghp_fixture_token_never_printed"

    init(budget: ReadBudget = ReadBudget()) throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent("merge-" + randomHex(6))
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        journal = try Journal(path: directory.appendingPathComponent("journal.sqlite").path, at: clock.now)
        vault = Vault(url: directory.appendingPathComponent("app/vault"), key: FixtureVaultKey())
        settingsURL = directory.appendingPathComponent("app/settings.json")
        let settings = GitHubSettings(origin: github.origin, account: "github:user:123", login: "winton")
        try vault.store(Secret(token), for: try settings.binding)
        try AppSettings(github: settings).save(settingsURL)
        let url = settingsURL, clock = self.clock
        handler = MergeHandler(journal: journal, settings: { (try? AppSettings.load(url))?.github },
                               connect: MergeHandler.connect(vault: vault, configuration: FakeGitHub.configuration),
                               budget: budget, now: { clock.now })
    }

    func propose(head: String = String(repeating: "a", count: 40)) throws -> String {
        var params = mergeParams.objectValue!
        params["head"] = .string(head)
        let proposal = Proposal(request: request("github.merge", .object(params)), client: "beacon-act", at: clock.now)
        try journal.add(proposal)
        return proposal.id
    }

    /// Resolves, freezes and approves, as the window does.
    func approve(_ id: String) async throws {
        let proposal = journal.proposal(id, at: clock.now)!
        let resolved = try await handler.resolve(proposal, model: nil)
        try journal.approve(try FrozenApproval(proposal, origin: resolved.origin, account: resolved.account,
                                               destination: resolved.destination), at: clock.now)
    }

    var merges: [FakeGitHub.Request] { github.requests.filter { $0.method == "PUT" } }
    func records() throws -> [JSON] { try deliverAll(journal) }
}

struct MergeTests {
    @Test func mergesTheFrozenHeadWithTheFrozenMethod() async throws {
        let h = try MergeHarness()
        let id = try h.propose()
        let proposal = h.journal.proposal(id, at: h.clock.now)!
        let resolved = try await h.handler.resolve(proposal, model: nil)
        #expect(resolved.account == "github:user:123" && resolved.origin == h.github.origin)
        #expect(resolved.destination == .object(["repository_id": .int(456), "pr": .int(140), "base": "main"]))
        #expect(resolved.facts["Required checks"] == "1 green: ci")
        try h.journal.approve(try FrozenApproval(proposal, origin: resolved.origin, account: resolved.account,
                                                 destination: resolved.destination), at: h.clock.now)

        #expect(try await h.handler.execute(id) == .executed(confirmedBy: "reply"))
        #expect(h.merges.count == 1)
        #expect(h.merges[0].body?["sha"] as? String == String(repeating: "a", count: 40))
        #expect(h.merges[0].body?["merge_method"] as? String == "squash")
        #expect(h.github.requests.allSatisfy { $0.authorization == "Bearer " + h.token })
        let records = try h.records()
        #expect(records.map { $0["type"]!.stringValue! } ==
                ["app.action.proposed", "app.action.approved", "app.action.started", "app.action.executed"])
        #expect(records[3]["metadata"]?["native"] == .string("github:merge:" + String(repeating: "c", count: 40)))
        #expect(records[3]["metadata"]?["confirmed_by"] == "reply")
        let bytes = records.map { $0.canonical }.joined()
        #expect(!bytes.contains(h.token))
        // Nothing runs twice: a second execute cannot start.
        await #expect(throws: JournalError.transition) { try await h.handler.execute(id) }
        #expect(h.merges.count == 1)
    }

    @Test func preflightRefusesWhatCannotBeMerged() async throws {
        let cases: [(MergeRefusal, (FakeGitHub) -> Void)] = [
            (.headMoved, { $0.pull.head = String(repeating: "b", count: 40) }),
            (.checksNotGreen, { $0.checkRuns = ["ci": "failure"] }),
            (.checksNotGreen, { $0.checkRuns = ["ci": nil] }),
            (.checksNotGreen, { $0.checkRuns = [:]; $0.statuses = ["deploy": "pending"]; $0.ruleChecks = ["deploy"] }),
            (.checksNotGreen, { $0.protectionChecks = ["lint"] }),
            (.changesRequested, { $0.reviews = [(9, "APPROVED"), (10, "CHANGES_REQUESTED")] }),
            (.alreadyMerged, { $0.pull.merged = true; $0.pull.state = "closed" }),
            (.notOpen, { $0.pull.state = "closed" }),
            (.baseChanged, { $0.pull.base = "release" }),
            (.notMergeable, { $0.pull.mergeable = false; $0.pull.mergeableState = "dirty" }),
            (.notMergeable, { $0.pull.mergeableState = "blocked" }),
            (.mergeabilityUnknown, { $0.pull.mergeable = nil }),
            (.accountMismatch, { $0.user.id = 999 }),
        ]
        for (refusal, change) in cases {
            let h = try MergeHarness()
            let id = try h.propose()
            h.github.update(change)
            await #expect(throws: refusal) { try await h.handler.resolve(h.journal.proposal(id, at: h.clock.now)!, model: nil) }
            #expect(h.merges.isEmpty)
        }
        // A changed request later approved overrides an older one by the same reviewer.
        let h = try MergeHarness()
        let id = try h.propose()
        h.github.reviews = [(10, "CHANGES_REQUESTED"), (10, "APPROVED")]
        _ = try await h.handler.resolve(h.journal.proposal(id, at: h.clock.now)!, model: nil)
    }

    /// Anything that changed after approval stops the merge at the final read, with nothing sent.
    @Test func theFinalReadStopsAChangedMerge() async throws {
        let cases: [(String, (FakeGitHub) -> Void)] = [
            ("head_moved", { $0.pull.head = String(repeating: "b", count: 40) }),
            ("checks_not_green", { $0.checkRuns = ["ci": "failure"] }),
            ("changes_requested", { $0.reviews.append((11, "CHANGES_REQUESTED")) }),
            ("already_merged", { $0.pull.merged = true }),
            ("base_changed", { $0.pull.base = "release" }),
            ("repository_changed", { $0.repositoryID = 789 }),
            ("unavailable", { $0.down = true }),
        ]
        for (error, change) in cases {
            let h = try MergeHarness()
            let id = try h.propose()
            try await h.approve(id)
            h.github.update(change)
            #expect(try await h.handler.execute(id) == .failed(error: error, confirmedBy: "pre_dispatch"))
            #expect(h.merges.isEmpty)
            let last = try #require(try h.records().last)
            #expect(last["type"] == "app.action.failed" && last["metadata"]?["error"] == .string(error))
        }
    }

    @Test func githubsRefusalsAreRecordedFromTheReply() async throws {
        for (status, error) in [(409, "head_moved"), (405, "not_mergeable"), (422, "rejected"), (403, "forbidden")] {
            let h = try MergeHarness()
            let id = try h.propose()
            try await h.approve(id)
            h.github.merge = .status(status, merging: false)
            #expect(try await h.handler.execute(id) == .failed(error: error, confirmedBy: "reply"))
            #expect(h.merges.count == 1)
        }
    }

    /// GitHub merged, and the reply was lost: unknown, then the read-back establishes it. No second merge.
    @Test func aLostReplyIsSettledByReadingBack() async throws {
        let h = try MergeHarness()
        let id = try h.propose()
        try await h.approve(id)
        h.github.merge = .loseReplyAfterMerging
        #expect(try await h.handler.execute(id) == .executed(confirmedBy: "read_back"))
        #expect(h.merges.count == 1)
        let types = try h.records().map { $0["type"]!.stringValue! }
        #expect(types.suffix(3) == ["app.action.started", "app.action.outcome_unknown", "app.action.executed"])
    }

    @Test func aServerErrorAfterMergingIsSettledByReadingBack() async throws {
        let h = try MergeHarness()
        let id = try h.propose()
        try await h.approve(id)
        h.github.merge = .status(502, merging: true)
        #expect(try await h.handler.execute(id) == .executed(confirmedBy: "read_back"))
        #expect(try h.records().last?["metadata"]?["reason"] == nil)
    }

    /// The reply was lost and nothing merged: unknown until enough time has passed for the read to be conclusive.
    @Test func aLostReplyWithoutAMergeFailsOnlyOnceSettled() async throws {
        let h = try MergeHarness()
        let id = try h.propose()
        try await h.approve(id)
        h.github.merge = .loseReplyWithoutMerging
        #expect(try await h.handler.execute(id) == .unknown)
        h.github.down = true
        h.clock.advance(MergeHandler.settleTime + 1)
        #expect(try await h.handler.reconcile(id) == .unknown)
        h.github.down = false
        #expect(try await h.handler.reconcile(id) == .failed(error: "not_merged", confirmedBy: "read_back"))
        #expect(h.merges.count == 1)
    }

    @Test func someoneElsesMergeIsNotThisApprovals() async throws {
        let h = try MergeHarness()
        let id = try h.propose()
        try await h.approve(id)
        h.github.merge = .loseReplyWithoutMerging
        #expect(try await h.handler.execute(id) == .unknown)
        h.github.update { $0.pull.merged = true; $0.pull.mergedBy = 77; $0.pull.mergeCommit = String(repeating: "d", count: 40) }
        #expect(try await h.handler.reconcile(id) == .failed(error: "merged_otherwise", confirmedBy: "read_back"))
    }

    /// Beacon died after the durable start. On relaunch the start is unknown, and only a read-back settles it.
    @Test func aStartFoundAtRelaunchIsReadBackNotResent() async throws {
        let h = try MergeHarness()
        let id = try h.propose()
        try await h.approve(id)
        h.github.merge = .loseReplyAfterMerging
        h.github.down = false
        _ = try h.journal.start(id, at: h.clock.now) // The start committed, then the process died.
        h.github.update { $0.pull.merged = true; $0.pull.mergedBy = 123; $0.pull.mergeCommit = String(repeating: "e", count: 40) }
        let path = try #require(h.journal.db.query("PRAGMA database_list").first?[2].text)
        let relaunched = try Journal(path: path, at: h.clock.now)
        let url = h.settingsURL, clock = h.clock
        let handler = MergeHandler(journal: relaunched, settings: { (try? AppSettings.load(url))?.github },
                                   connect: MergeHandler.connect(vault: h.vault, configuration: FakeGitHub.configuration),
                                   now: { clock.now })
        #expect(handler.unknown() == [id])
        #expect(try await handler.reconcile(id) == .executed(confirmedBy: "read_back"))
        #expect(h.merges.isEmpty)
    }

    @Test func aRedirectGetsNoCredential() async throws {
        let h = try MergeHarness()
        let elsewhere = FakeGitHub()
        h.github.redirectUser = elsewhere.origin + "/user"
        let id = try h.propose()
        await #expect(throws: MergeRefusal.unavailable) { try await h.handler.resolve(h.journal.proposal(id, at: h.clock.now)!, model: nil) }
        #expect(elsewhere.requests.isEmpty)
    }

    @Test func preflightReadsAreBounded() async throws {
        let h = try MergeHarness(budget: ReadBudget(perHour: 7))
        let id = try h.propose()
        _ = try await h.handler.resolve(h.journal.proposal(id, at: h.clock.now)!, model: nil)
        let before = h.github.requests.count
        await #expect(throws: MergeRefusal.busy) { try await h.handler.resolve(h.journal.proposal(id, at: h.clock.now)!, model: nil) }
        #expect(h.github.requests.count == before)
        #expect(throws: Invalid(field: "/params/repo")) { try GitHubAPI.segments("CrossTerra/..") }
    }

    @Test func settingsAcceptOnlyTheConfiguredUser() async throws {
        let github = FakeGitHub()
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent("setup-" + randomHex(6))
        let vault = Vault(url: directory.appendingPathComponent("app/vault"), key: FixtureVaultKey())
        let url = directory.appendingPathComponent("app/settings.json")
        let token = Secret("ghp_fixture_setup")
        github.user.type = "Bot"
        await #expect(throws: GitHubSetup.Refusal.notAUser) {
            try await GitHubSetup.register(token: token, origin: github.origin, vault: vault, settings: url,
                                           configuration: FakeGitHub.configuration)
        }
        github.user.type = "User"
        let settings = try await GitHubSetup.register(token: token, origin: github.origin, vault: vault, settings: url,
                                                      configuration: FakeGitHub.configuration)
        #expect(settings == GitHubSettings(origin: github.origin, account: "github:user:123", login: "winton"))
        #expect(try vault.credential(for: try settings.binding).value == "ghp_fixture_setup")
        #expect(try String(contentsOf: url, encoding: .utf8).contains("ghp_") == false)

        github.user = (124, "someone", "User")
        await #expect(throws: GitHubSetup.Refusal.otherUser(login: "winton")) {
            try await GitHubSetup.register(token: Secret("ghp_other"), origin: github.origin, vault: vault, settings: url,
                                           configuration: FakeGitHub.configuration)
        }
        try GitHubSetup.remove(vault: vault, settings: url)
        #expect(throws: VaultError.notFound) { try vault.credential(for: try settings.binding) }
        #expect(try AppSettings.load(url).github == nil)
    }
}
