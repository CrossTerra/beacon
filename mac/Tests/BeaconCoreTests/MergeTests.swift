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
        #expect(resolved.facts["Checks"] == "1 passed")
        #expect(resolved.facts["Required checks"] == nil)
        #expect(resolved.accountLabel == "winton")
        #expect(resolved.destinationLabel == "main · #140 · CrossTerra/signal")
        #expect(resolved.link == URL(string: h.github.origin + "/CrossTerra/signal/pull/140"))
        try h.journal.approve(try FrozenApproval(proposal, origin: resolved.origin, account: resolved.account,
                                                 destination: resolved.destination), subject: resolved.subject, at: h.clock.now)

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
        #expect(records.allSatisfy { $0["subject"] == "github:pr:CrossTerra/signal#140" })
        // The window says what happened, and links to the pull request.
        let done = h.journal.proposal(id, at: h.clock.now)!
        let outcome = ActionOutcome.describe(done, approval: h.journal.approval(id), result: h.journal.outcome(id))
        #expect(outcome?.text == "Merged as ccccccc")
        #expect(outcome?.link == URL(string: h.github.origin + "/CrossTerra/signal/pull/140"))
        // Nothing runs twice: a second execute cannot start.
        await #expect(throws: JournalError.transition) { try await h.handler.execute(id) }
        #expect(h.merges.count == 1)
    }

    @Test func preflightRefusesWhatCannotBeMerged() async throws {
        let cases: [(MergeRefusal, (FakeGitHub) -> Void)] = [
            (.headMoved, { $0.pull.head = String(repeating: "b", count: 40) }),
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
        let checks: [(String, (FakeGitHub) -> Void)] = [
            ("1 failed: ci", { $0.checkRuns = ["ci": "failure"] }),
            ("1 pending: ci", { $0.checkRuns = ["ci": nil] }),
            ("1 pending: deploy", { $0.checkRuns = [:]; $0.statuses = ["deploy": "pending"]; $0.ruleChecks = ["deploy"] }),
            ("1 failed: deploy", { $0.checkRuns = [:]; $0.statuses = ["deploy": "error"]; $0.ruleChecks = [] }),
            ("1 pending: lint · 1 passed", { $0.protectionChecks = ["lint"] }),
        ]
        for (summary, change) in checks {
            let h = try MergeHarness()
            let id = try h.propose()
            h.github.update(change)
            await #expect(throws: ChecksNotGreen.self, "\(summary)") {
                try await h.handler.resolve(h.journal.proposal(id, at: h.clock.now)!, model: nil)
            }
            do {
                _ = try await h.handler.resolve(h.journal.proposal(id, at: h.clock.now)!, model: nil)
            } catch let refusal as ChecksNotGreen {
                #expect(refusal.report.summary == summary)
            }
        }
        // A changed request later approved overrides an older one by the same reviewer.
        let h = try MergeHarness()
        let id = try h.propose()
        h.github.reviews = [(10, "CHANGES_REQUESTED"), (10, "APPROVED")]
        _ = try await h.handler.resolve(h.journal.proposal(id, at: h.clock.now)!, model: nil)
    }

    /// Only the latest run of each check name and app counts (Signal's executor, #131).
    @Test func onlyTheLatestRunOfACheckCounts() async throws {
        func run(_ id: Int?, _ conclusion: String?, app: Int = 1, started: String? = "2026-11-20T10:00:00Z") -> [String: Any] {
            var run: [String: Any] = ["name": "ci", "app": ["id": app],
                                      "status": conclusion == nil ? "in_progress" : "completed",
                                      "conclusion": conclusion ?? NSNull(), "started_at": started ?? NSNull()]
            if let id { run["id"] = id }
            return run
        }
        let cases: [(String, [[String: Any]], Bool)] = [
            ("a re-run that passes clears an older failure", [run(1, "failure"), run(2, "success")], true),
            ("a newer failure is not cleared by an older pass", [run(2, "failure"), run(1, "success")], false),
            ("the higher id wins over a later start", [run(5, "success", started: "2026-11-20T09:00:00Z"),
                                                       run(4, "failure", started: "2026-11-20T11:00:00Z")], true),
            ("without ids, the later start wins", [run(nil, "cancelled", started: "2026-11-20T09:00:00Z"),
                                                   run(nil, "success", started: "2026-11-20T10:00:00Z")], true),
            ("a completed run with no start is the oldest", [run(nil, "success"), run(nil, "cancelled", started: nil)], true),
            ("a run with no start that has not finished is the newest", [run(nil, "success"), run(nil, nil, started: nil)], false),
            ("the latest run still in progress", [run(1, "success"), run(2, nil)], false),
            ("another app's failing run of the same name", [run(1, "success"), run(2, "failure", app: 2)], false),
            ("skipped and neutral are not failures", [run(1, "skipped"), run(1, "neutral", app: 2)], true),
        ]
        for (name, runs, green) in cases {
            let h = try MergeHarness()
            let id = try h.propose()
            h.github.update { $0.checkRuns = [:]; $0.runs = runs }
            let proposal = h.journal.proposal(id, at: h.clock.now)!
            if green {
                _ = try await h.handler.resolve(proposal, model: nil)
            } else {
                await #expect(throws: ChecksNotGreen.self, "\(name)") { try await h.handler.resolve(proposal, model: nil) }
            }
        }
    }

    /// Without any required check, every check run and commit status on the head must still have passed: a red
    /// check that branch protection does not require stops the merge, before approval and at the final read.
    @Test func everyCheckCountsWithoutRequiredChecks() async throws {
        func harness(_ change: @escaping (FakeGitHub) -> Void) throws -> (MergeHarness, String) {
            let h = try MergeHarness()
            h.github.update { $0.ruleChecks = []; $0.protectionChecks = [] }
            h.github.update(change)
            return (h, try h.propose())
        }
        var (h, id) = try harness { $0.checkRuns = ["ci": "success", "e2e": "failure"] }
        do {
            _ = try await h.handler.resolve(h.journal.proposal(id, at: h.clock.now)!, model: nil)
            Issue.record("resolved with a failing check")
        } catch let refusal as ChecksNotGreen {
            #expect(refusal.report.summary == "1 failed: e2e · 1 passed")
        }
        (h, id) = try harness { $0.checkRuns = ["ci": "success"]; $0.statuses = ["vercel": "failure"] }
        await #expect(throws: ChecksNotGreen.self) { try await h.handler.resolve(h.journal.proposal(id, at: h.clock.now)!, model: nil) }
        (h, id) = try harness { $0.checkRuns = ["ci": "success", "e2e": nil] }
        await #expect(throws: ChecksNotGreen.self) { try await h.handler.resolve(h.journal.proposal(id, at: h.clock.now)!, model: nil) }
        (h, id) = try harness { $0.checkRuns = ["ci": "success", "docs": "skipped", "lint": "neutral"] }
        #expect(try await h.handler.resolve(h.journal.proposal(id, at: h.clock.now)!, model: nil).facts["Checks"] == "3 passed")
        (h, id) = try harness { $0.checkRuns = [:] }
        #expect(try await h.handler.resolve(h.journal.proposal(id, at: h.clock.now)!, model: nil).facts["Checks"] == "none reported")

        // A non-required check that fails after approval stops the merge at the final read.
        (h, id) = try harness { _ in }
        try await h.approve(id)
        h.github.update { $0.checkRuns["e2e"] = "failure" }
        #expect(try await h.handler.execute(id) == .failed(error: "checks_not_green", confirmedBy: "pre_dispatch"))
        #expect(h.merges.isEmpty)
    }

    /// The record subject keeps GitHub's spelling of the repository from the approval on; a full_name for another
    /// repository is not taken.
    @Test func subjectsKeepGitHubsSpelling() async throws {
        let h = try MergeHarness()
        var params = mergeParams.objectValue!
        params["repo"] = "crossterra/signal"
        let proposal = Proposal(request: request("github.merge", .object(params)), client: "beacon-act", at: h.clock.now)
        try h.journal.add(proposal)
        let resolved = try await h.handler.resolve(proposal, model: nil)
        #expect(resolved.subject == "github:pr:CrossTerra/signal#140")
        try h.journal.approve(try FrozenApproval(proposal, origin: resolved.origin, account: resolved.account,
                                                 destination: resolved.destination), subject: resolved.subject, at: h.clock.now)
        let records = try h.records()
        #expect(records.map { $0["subject"]!.stringValue! } == ["github:pr:crossterra/signal#140", "github:pr:CrossTerra/signal#140"])

        #expect(MergeHandler.canonicalName("Someone/else", for: "crossterra/signal") == "crossterra/signal")
        #expect(MergeHandler.canonicalName(nil, for: "crossterra/signal") == "crossterra/signal")
        // A subject for another target is not kept.
        let other = try MergeHarness()
        let id = try other.propose()
        let p = other.journal.proposal(id, at: other.clock.now)!
        try other.journal.approve(try FrozenApproval(p, origin: other.github.origin, account: "github:user:123",
                                                     destination: mergeDestination), subject: "github:pr:CrossTerra/other#140",
                                  at: other.clock.now)
        #expect(try other.records().allSatisfy { $0["subject"] == "github:pr:CrossTerra/signal#140" })
    }

    @Test func theWindowDescribesEachOutcome() throws {
        let h = try MergeHarness()
        let id = try h.propose()
        let p = h.journal.proposal(id, at: h.clock.now)!
        #expect(ActionOutcome.describe(p, approval: nil, result: nil) == nil)
        var failed = p
        failed.status = .failed
        let approval = try FrozenApproval(p, origin: "https://api.github.com", account: "github:user:123", destination: mergeDestination)
        let described = ActionOutcome.describe(failed, approval: approval, result: .object(["error": "head_moved"]))
        #expect(described?.text == "Not merged: head moved")
        #expect(described?.link == URL(string: "https://github.com/CrossTerra/signal/pull/140"))
        var unknown = p
        unknown.status = .outcomeUnknown
        #expect(ActionOutcome.describe(unknown, approval: approval, result: nil)?.text.hasPrefix("Outcome unknown") == true)
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
        // Open reads within the minute after the unknown outcome do not count.
        h.clock.advance(30)
        #expect(try await h.handler.reconcile(id) == .unknown)
        #expect(try await h.handler.reconcile(id) == .unknown)
        h.github.down = true
        h.clock.advance(MergeHandler.settleTime)
        #expect(try await h.handler.reconcile(id) == .unknown)
        h.github.down = false
        // Two reads past the minute, both open, a minute apart.
        #expect(try await h.handler.reconcile(id) == .unknown)
        h.clock.advance(MergeHandler.openReadSpacing)
        #expect(try await h.handler.reconcile(id) == .failed(error: "not_merged", confirmedBy: "read_back"))
        #expect(h.merges.count == 1)
    }

    /// Bugbot on #40: Read again on an unknown merge reads back too, but clicks cannot hurry "not merged". Two open
    /// reads past the settle time must be a minute apart, however they were asked for; a click shows the state.
    @Test @MainActor func readAgainCannotFinalizeAnOpenMerge() async throws {
        let h = try MergeHarness()
        let id = try h.propose()
        try await h.approve(id)
        h.github.merge = .loseReplyWithoutMerging
        #expect(try await h.handler.execute(id) == .unknown)
        h.clock.advance(MergeHandler.settleTime)
        let clock = h.clock, handler = h.handler
        let desk = ApprovalDesk(journal: h.journal, resolver: NoResolver(), now: { clock.now })
        nonisolated(unsafe) var reads: [Task<MergeHandler.Outcome, Error>] = []
        desk.onReadBack = { id in reads.append(Task { try await handler.reconcile(id) }) }
        desk.select(id)
        for _ in 0..<4 { // A timer read, then the person clicks Read again three times.
            desk.readAgain()
            #expect(try await reads.removeLast().value == .unknown)
        }
        h.clock.advance(MergeHandler.openReadSpacing - 1)
        desk.readAgain()
        #expect(try await reads.removeLast().value == .unknown)
        #expect(h.journal.proposal(id, at: clock.now)?.status == .outcomeUnknown)
        h.clock.advance(1)
        desk.readAgain()
        #expect(try await reads.removeLast().value == .failed(error: "not_merged", confirmedBy: "read_back"))
        #expect(h.merges.count == 1)
    }

    /// Read-back uses only the approved account and origin; with Settings changed since, it stays unknown.
    @Test func readBackNeedsTheApprovedAccount() async throws {
        let h = try MergeHarness()
        let id = try h.propose()
        try await h.approve(id)
        // GitHub goes away right after the last read: the PUT and the immediate read-back get no answer.
        h.github.update { github in
            github.pullReads = 0
            github.afterPullRead = { n, g in if n == 2 { g.down = true } }
        }
        #expect(try await h.handler.execute(id) == .unknown)
        h.github.update {
            $0.down = false
            $0.pull.merged = true
            $0.pull.mergedBy = 123
            $0.pull.mergeCommit = String(repeating: "c", count: 40)
        }
        let approved = try AppSettings.load(h.settingsURL)
        for other in [GitHubSettings(origin: h.github.origin, account: "github:user:124", login: "x"),
                      GitHubSettings(origin: FakeGitHub().origin, account: "github:user:123", login: "winton")] {
            try AppSettings(github: other).save(h.settingsURL)
            let before = h.github.requests.count
            #expect(try await h.handler.reconcile(id) == .unknown)
            #expect(h.github.requests.count == before)
        }
        try approved.save(h.settingsURL)
        #expect(try await h.handler.reconcile(id) == .executed(confirmedBy: "read_back"))
    }

    /// The PR is read once more right before the PUT: a change after the full read still stops the merge.
    @Test func theLastReadBeforeThePutStopsAChange() async throws {
        let cases: [(String, (FakeGitHub) -> Void)] = [
            ("head_moved", { $0.pull.head = String(repeating: "b", count: 40) }),
            ("base_changed", { $0.pull.base = "release" }),
            ("repository_changed", { $0.repositoryID = 789 }),
            ("already_merged", { $0.pull.merged = true }),
        ]
        for (error, change) in cases {
            let h = try MergeHarness()
            let id = try h.propose()
            try await h.approve(id)
            h.github.update { github in
                github.pullReads = 0
                github.afterPullRead = { n, g in if n == 1 { change(g) } }
            }
            #expect(try await h.handler.execute(id) == .failed(error: error, confirmedBy: "pre_dispatch"))
            #expect(h.merges.isEmpty && h.github.pullReads == 2)
        }
    }

    /// Every read counts against the hourly budget as it is made; past it the preflight stops mid-way.
    @Test func theBudgetStopsAPreflightMidWay() async throws {
        let h = try MergeHarness(budget: ReadBudget(perHour: 5))
        let id = try h.propose()
        await #expect(throws: MergeRefusal.busy) { try await h.handler.resolve(h.journal.proposal(id, at: h.clock.now)!, model: nil) }
        #expect(h.github.requests.count == 5)
        await #expect(throws: MergeRefusal.busy) { try await h.handler.resolve(h.journal.proposal(id, at: h.clock.now)!, model: nil) }
        #expect(h.github.requests.count == 5)
        h.clock.advance(3601)
        await #expect(throws: MergeRefusal.busy) { try await h.handler.resolve(h.journal.proposal(id, at: h.clock.now)!, model: nil) }
        #expect(h.github.requests.count == 10)
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

struct GitHubTokenPageTests {
    @Test func theNewTokenPageIsOnTheOriginsWebHost() {
        let query = "/settings/tokens/new?scopes=repo&description=Beacon%20(merges)"
        #expect(GitHubSetup.newTokenURL(apiOrigin: "https://api.github.com").absoluteString == "https://github.com" + query)
        #expect(GitHubSetup.newTokenURL(apiOrigin: "https://API.github.com/").absoluteString == "https://github.com" + query)
        #expect(GitHubSetup.newTokenURL(apiOrigin: "https://api.acme.ghe.com").absoluteString == "https://acme.ghe.com" + query)
        #expect(GitHubSetup.newTokenURL(apiOrigin: "https://git.example.com:8443").absoluteString == "https://git.example.com:8443" + query)
        #expect(GitHubSetup.newTokenURL(apiOrigin: "not an origin").absoluteString == "https://github.com" + query)
        #expect(GitHubSetup.newTokenURL(apiOrigin: "http://api.evil.test").absoluteString == "https://github.com" + query)
    }
}
