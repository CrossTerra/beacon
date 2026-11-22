import Foundation
import Testing
@testable import BeaconCore

struct SlackTests {
    @Test func settingsVerifyAndBindOnlyTheConfiguredSlackUser() async throws {
        let h = try SlackHarness()
        await #expect(throws: SlackRefusal.notAUser) { try await h.register("xoxb-fixture-bot") }
        #expect(try await h.fixture.calls().isEmpty)
        try await h.fixture.configure(["bot": .bool(true)])
        await #expect(throws: SlackRefusal.notAUser) { try await h.register() }
        #expect(try AppSettings.load(h.settingsURL).slack == nil)
        try await h.fixture.configure(["bot": .bool(false)])

        let saved = try await h.register()
        #expect(saved.connected == "Posts to Fixture Workspace (T0WORKSPACE) as fixture-user (U0PERSON)")
        #expect(try AppSettings.load(h.settingsURL).slack == saved)
        #expect(try CredentialBinding.normalize(SlackSettings.origin) == "https://slack.com")
        let binding = try saved.binding
        let secret = try h.vault.credential(for: binding)
        #expect(String(describing: secret) == "[redacted]")
        let settings = try Data(contentsOf: h.settingsURL)
        let encrypted = try Data(contentsOf: h.directory.appendingPathComponent("app/vault"))
        #expect(settings.range(of: Data(SlackHarness.token.utf8)) == nil)
        #expect(encrypted.range(of: Data(SlackHarness.token.utf8)) == nil)
        let foreign = try CredentialBinding(service: "slack", origin: "https://example.test", account: saved.account, workspace: saved.workspace)
        #expect(throws: VaultError.notFound) { try h.vault.credential(for: foreign) }

        await #expect(throws: SlackRefusal.accountMismatch) { try await h.register(SlackHarness.other) }
        try await h.fixture.configure(["workspace": "T0OTHER"])
        await #expect(throws: SlackRefusal.workspaceMismatch) { try await h.register() }
        #expect(try AppSettings.load(h.settingsURL).slack == saved)
        #expect(try h.vault.credential(for: binding).value == secret.value)
        #expect(try await h.fixture.calls().allSatisfy { $0["path"] == "/api/auth.test" && $0["authed"] == .bool(true) })
        try SlackSetup.remove(vault: h.vault, settings: h.settingsURL)
        #expect(try AppSettings.load(h.settingsURL).slack == nil)
        #expect(throws: VaultError.notFound) { try h.vault.credential(for: binding) }
    }

    @Test @MainActor func socketIntakeAndApprovalPostTheExactFrozenReply() async throws {
        let h = try SlackHarness()
        try await h.register()
        let clock = h.clock
        let server = ProposalServer(path: h.directory.appendingPathComponent("run/s").path,
                                    clients: [.init(name: "beacon-act", requirement: selfRequirement())], book: h.journal,
                                    enabled: [.slackPost], now: { clock.now }, slackLookup: h.handler.place(_:client:))
        try server.start()
        defer { server.stop() }
        let client = try Client(path: server.path)
        #expect(array(client.call(.object(["op": "types"]))?["types"]).map { $0["type"] } == ["slack.post"])
        var invalid = SlackHarness.frame().objectValue!
        var params = invalid["params"]!.objectValue!
        params["origin"] = "https://example.test"
        invalid["params"] = .object(params)
        #expect(client.call(.object(invalid))?["field"] == "/params/origin")
        #expect(client.call(SlackHarness.frame(workspace: "T0OTHER"))?["error"] == "unavailable")
        #expect(try await h.fixture.calls().count == 1) // Only the setup read.

        let id = try #require(client.call(SlackHarness.frame())?["id"]?.stringValue)
        // `proposed` can be sent before the window even exists.
        let first = try #require(try h.journal.nextDelivery(person: person))
        let subject = JSON.string("slack:C0GENERAL:\(SlackHarness.root)")
        #expect(try JSON.parse(first.bytes)["subject"] == subject)
        #expect(try await h.fixture.calls().compactMap { $0["path"]?.stringValue }.suffix(2) ==
                ["/api/auth.test", "/api/conversations.info"])

        let desk = ApprovalDesk(journal: h.journal, resolver: h.handler, now: { clock.now })
        desk.select(id)
        #expect(desk.frozen == nil)
        await desk.freezing?.value
        let frozen = try #require(desk.frozen)
        #expect(frozen.resolved.accountLabel == "fixture-user (U0PERSON)")
        #expect(frozen.resolved.destinationLabel == "#general (C0GENERAL)")
        #expect(frozen.resolved.facts == ["Workspace": "Fixture Workspace (T0WORKSPACE)", "Visibility": "Public channel",
                                            "Thread": "\(SlackHarness.root) · Parent’s first line"])
        #expect(frozen.approval.origin == "https://slack.com")
        let text = try #require(frozen.approval.params["text"]?.stringValue)
        #expect(text.utf8.elementsEqual(SlackHarness.draft.utf8))
        #expect(slackPlainPreview(text) == SlackHarness.preview)
        #expect(slackPlainPreview("<!here> &lt;code&gt; &amp;gt; <@U0OTHER|Untrusted>") ==
                "[@here — notifies everyone present] <code> &gt; @U0OTHER")
        #expect(slackPlainPreview("<!channel|@channel> and <!everyone> or <!subteam^S0OPS|@ops>") ==
                "[@channel — notifies everyone in the channel] and [@everyone — notifies everyone in the workspace] or [@S0OPS — notifies a user group]")
        #expect(slackHasSpecialMention("hi <!here>") && !slackHasSpecialMention(SlackHarness.draft))
        #expect(visibleText(slackPlainPreview("name\u{202E}choice")) == "name⟨U+202E⟩choice")
        #expect(try await h.fixture.posts().isEmpty)
        #expect(desk.approve(frozen.token))
        #expect(!desk.approve(frozen.token))

        #expect(try await h.handler.execute(id) == .executed(confirmedBy: "reply"))
        await #expect(throws: JournalError.transition) { try await h.handler.execute(id) }
        let calls = try await h.fixture.calls()
        #expect(calls.suffix(4).map { $0["path"] } == ["/api/auth.test", "/api/conversations.info", "/api/conversations.replies", "/api/chat.postMessage"])
        #expect(calls.allSatisfy { $0["authed"] == .bool(true) })
        let posts = calls.filter { $0["method"] == "POST" }
        #expect(posts.count == 1)
        let body = try #require(posts.first?["body"])
        #expect(body["channel"] == "C0GENERAL" && body["thread_ts"] == .string(SlackHarness.root))
        #expect(body["text"]?.stringValue?.utf8.elementsEqual(SlackHarness.draft.utf8) == true)
        #expect(body["mrkdwn"] == .bool(true) && body["unfurl_links"] == .bool(false) && body["unfurl_media"] == .bool(false))
        // The operation id goes both ways: as message metadata and as client_msg_id.
        let marker = try #require(h.journal.slackMarker(id))
        #expect(body["metadata"] == .object(["event_type": "beacon_post", "event_payload": .object(["op": .string(marker)])]))
        #expect(body["client_msg_id"] == .string(marker))

        let records = try deliverAll(h.journal)
        #expect(records.map { $0["type"] } == ["app.action.proposed", "app.action.approved", "app.action.started", "app.action.executed"])
        #expect(records.allSatisfy { $0["subject"] == subject && !$0.canonical.contains(SlackHarness.token) })
        #expect(records[1]["sealed"]?["metadata"]?["destination"] == frozen.approval.destination)
        #expect(records.last?["metadata"]?["native"] == .string("slack:message:C0GENERAL:\(SlackHarness.posted)"))
        #expect(client.call(.object(["op": "get", "id": .string(id)]))?["status"] == "executed")
        desk.refresh()
        #expect(desk.selected.flatMap { desk.outcome(of: $0)?.text } == "Posted to Slack")
    }

    @Test func publicTopLevelRecordsStayOnTheProposalsLocalDay() async throws {
        let h = try SlackHarness()
        try await h.register()
        let midnight = Calendar.current.startOfDay(for: h.clock.now)
        h.clock.advance(midnight.timeIntervalSince(h.clock.now) - 60)
        let proposal = try await h.propose(thread: nil)
        let day = Calendar(identifier: .gregorian).dateComponents(in: .current, from: h.clock.now)
        let subject = JSON.string(String(format: "slack:C0GENERAL:%04d-%02d-%02d", day.year!, day.month!, day.day!))
        let first = try #require(try h.journal.nextDelivery(person: person))
        #expect(try JSON.parse(first.bytes)["subject"] == subject)
        h.clock.advance(120)
        try await h.approve(proposal)
        #expect(try await h.handler.execute(proposal.id) == .executed(confirmedBy: "reply"))
        let post = try #require(try await h.fixture.posts().first)
        #expect(post["body"]?["thread_ts"] == nil)
        let (reloaded, _) = try h.restart()
        let records = try deliverAll(reloaded)
        #expect(records.count == 4 && records.allSatisfy { $0["subject"] == subject })
    }

    @Test func privateChannelsAndDMsStayOnThePersonalSubject() async throws {
        let h = try SlackHarness()
        try await h.register()
        let destinations: [(String, [String: JSON], SlackVisibility, String)] = [
            ("C0SECRET", ["is_private": .bool(true)], .private, "#secret (C0SECRET)"),
            ("D0DANA", ["is_im": .bool(true), "user": "U0OTHER"], .dm, "DM with Dana Scully (U0OTHER) (D0DANA)"),
            ("G0GROUP", ["is_mpim": .bool(true)], .groupDM, "secret (G0GROUP)"),
        ]
        for (id, flags, visibility, label) in destinations {
            try await h.channel(["id": .string(id), "name": "secret", "is_member": .null].merging(flags, uniquingKeysWith: { _, new in new }))
            let proposal = try await h.propose(channel: id)
            let resolved = try await h.handler.resolve(proposal, model: nil)
            #expect(resolved.facts["Visibility"] == visibility.rawValue)
            #expect(resolved.destinationLabel == label)
            // A person can decline a valid draft without a POST.
            try h.journal.decline(proposal.id, note: "Not today", at: h.clock.now)
            await #expect(throws: JournalError.transition) { try await h.handler.execute(proposal.id) }
        }
        let records = try deliverAll(h.journal)
        #expect(records.count == 6)
        #expect(records.allSatisfy { $0["subject"] == .string("signal:actor:\(person)") && !$0.canonical.contains("C0UNTRUSTED") })
        #expect(try await h.fixture.calls().filter { $0["path"] == "/api/conversations.members" }.count == 3)
        #expect(try await h.fixture.posts().isEmpty)
    }

    @Test func changedAccountWorkspaceVisibilityMembershipOrRootStopsTheWrite() async throws {
        let h = try SlackHarness()
        let saved = try await h.register()
        let refusals: [(SlackRefusal, [String: JSON])] = [
            (.archived, ["channel": .object(["is_archived": .bool(true)])]),
            (.notMember, ["channel": .object(["is_member": .bool(false)])]),
            (.workspaceMismatch, ["workspace": "T0OTHER"]),
            (.workspaceMismatch, ["channel": .object(["context_team_id": "T0OTHER"])]),
            (.destinationChanged, ["channel": .object(["is_private": .bool(true)])]),
            (.threadNotFound, ["missing_root": .bool(true)]),
            (.threadNotFound, ["parent": .object(["thread_ts": "1799999800.000100"])]), // Slack returned a reply, not a root.
            (.accountMismatch, [:]), // A different user's real (fixture) token under the original vault binding.
        ]
        for (refusal, change) in refusals {
            try await h.channel([:])
            try await h.fixture.configure(["workspace": "T0WORKSPACE", "missing_root": .bool(false),
                                           "parent": .object(["thread_ts": .string(SlackHarness.root)])])
            let proposal = try await h.propose()
            try await h.approve(proposal)
            if refusal == .accountMismatch {
                try h.vault.store(Secret(SlackHarness.other), for: saved.binding)
            } else {
                try await h.fixture.configure(change)
            }
            await #expect(throws: refusal) { try await h.handler.resolve(proposal, model: nil) }
            #expect(try await h.handler.execute(proposal.id) == .failed(error: refusal.code, confirmedBy: "pre_dispatch"))
            let outcome = try #require(h.journal.outcome(proposal.id))
            #expect(outcome["confirmed_by"] == "pre_dispatch" && outcome["native"] == nil)
        }
        #expect(try await h.fixture.posts().isEmpty)
    }

    @Test func deniedPreflightCannotFreezeTheApproveControl() async throws {
        let h = try SlackHarness()
        try await h.register()
        let proposal = try await h.propose()
        try await h.channel(["is_member": .bool(false)])
        let clock = h.clock
        let desk = await ApprovalDesk(journal: h.journal, resolver: h.handler, now: { clock.now })
        await desk.select(proposal.id)
        await desk.freezing?.value
        #expect(await desk.frozen == nil)
        #expect(await desk.problem == "Not approvable: You are not a member of this conversation.")
        #expect(await !desk.approve(UUID()))
        #expect(try await h.fixture.posts().isEmpty)
    }

    @Test func unknownVisibilityAndOversizeDraftsFailClosed() async throws {
        let h = try SlackHarness()
        try await h.register()
        try await h.channel(["is_private": .null, "is_member": .bool(true)])
        await #expect(throws: SlackRefusal.visibilityUnknown) { try await h.propose() }
        #expect(h.journal.all().isEmpty)
        try await h.channel([:])
        let proposal = try await h.propose(text: String(repeating: "🙂", count: 2_001))
        await #expect(throws: SlackRefusal.messageTooLong) { try await h.handler.resolve(proposal, model: nil) }
        #expect(try await h.fixture.posts().isEmpty)
    }

    @Test func knownSlackRefusalsAreDefiniteAndNotRetried() async throws {
        let h = try SlackHarness()
        try await h.register()
        let refusals = [("is_archived", "channel_archived"), ("not_in_channel", "not_in_channel"),
                        ("missing_scope", "missing_scope"), ("restricted_action", "slack_refused"),
                        ("msg_too_long", "message_too_long"), ("message_limit_exceeded", "slack_refused"),
                        ("invalid_blocks", "slack_refused"), ("too_many_attachments", "slack_refused"),
                        ("metadata_must_be_sent_from_app", "metadata_refused")]
        for (provider, error) in refusals {
            let proposal = try await h.propose()
            try await h.approve(proposal)
            try await h.fixture.configure(["post_mode": .string("refuse:" + provider)])
            #expect(try await h.handler.execute(proposal.id) == .failed(error: error, confirmedBy: "reply"))
            #expect(try await h.handler.reconcile(proposal.id) == .unknown)
            await #expect(throws: JournalError.transition) { try await h.handler.execute(proposal.id) }
        }
        let calls = try await h.fixture.calls()
        #expect(calls.filter { $0["method"] == "POST" }.count == refusals.count)
        #expect(!calls.contains { $0["query"]?["oldest"] != nil })
        #expect(h.handler.unknown().isEmpty)
        #expect(try deliverAll(h.journal).filter { $0["type"] == "app.action.failed" }.count == refusals.count)
        // Errors that can follow a write stay ambiguous.
        for error in ["internal_error", "fatal_error", "request_timeout", "something_new"] {
            #expect(SlackAPI.refusal(error) == nil)
        }
    }

    /// A token that may not post metadata is refused before the write: one POST, a definite failure, no read-back.
    @Test func refusedMetadataIsADefiniteFailure() async throws {
        let h = try SlackHarness()
        try await h.register()
        let proposal = try await h.propose()
        try await h.approve(proposal)
        try await h.fixture.configure(["metadata": "refuse"])
        #expect(try await h.handler.execute(proposal.id) == .failed(error: "metadata_refused", confirmedBy: "reply"))
        #expect(h.journal.outcome(proposal.id)?["result"] ==
                "Slack refused the operation id Beacon attaches as message metadata. Nothing was posted.")
        let calls = try await h.fixture.calls()
        #expect(calls.filter { $0["method"] == "POST" }.count == 1 && !calls.contains { $0["query"]?["oldest"] != nil })
    }

    @Test func retryAfterBlocksNewWritesButNeverRetriesTheOldOne() async throws {
        let h = try SlackHarness()
        try await h.register()
        let first = try await h.propose(thread: nil)
        let second = try await h.propose(thread: nil)
        try await h.approve(first)
        try await h.approve(second)
        try await h.fixture.configure(["rate_method": "chat.postMessage", "retry_after": .int(17)])
        #expect(try await h.handler.execute(first.id) == .failed(error: "rate_limited", confirmedBy: "reply"))
        #expect(h.journal.outcome(first.id)?["result"] == "Slack is rate limiting requests. Try again in 17 seconds.")
        try await h.fixture.configure(["rate_method": ""])
        #expect(try await h.handler.execute(second.id) == .failed(error: "rate_limited", confirmedBy: "pre_dispatch"))

        let third = try await h.propose(thread: nil)
        h.clock.advance(16)
        await #expect(throws: SlackRefusal.rateLimited(1)) { try await h.handler.resolve(third, model: nil) }
        #expect(try await h.fixture.posts().count == 1)
        h.clock.advance(1)
        try await h.approve(third)
        #expect(try await h.handler.execute(third.id) == .executed(confirmedBy: "reply"))
        await #expect(throws: JournalError.transition) { try await h.handler.execute(first.id) }
        #expect(try await h.fixture.posts().count == 2)
    }

    @Test func preflightReadsAreMeteredAndSlackRedirectsAreNotFollowed() async throws {
        let h = try SlackHarness(budget: ReadBudget(perHour: 4))
        try await h.register()
        let proposal = try await h.propose() // Two intake reads, from the intake budget.
        #expect(try await h.handler.resolve(proposal, model: nil).origin == "https://slack.com") // Three reads.
        await #expect(throws: SlackRefusal.busy) { try await h.handler.resolve(proposal, model: nil) }
        #expect(try await h.fixture.calls().count == 7) // Setup, intake two, three, then the fourth metered read only.
        h.clock.advance(3_601)
        try await h.fixture.configure(["redirect": "auth.test"])
        await #expect(throws: SlackRefusal.unavailable) { try await h.propose() } // The cached auth.test has expired.
        #expect(try await h.fixture.calls().last?["path"] == "/api/auth.test")
        #expect(try await h.fixture.calls().count == 8)
    }

    /// A proposer cannot spend the window's reads: intake has its own budget per client identity, and reuses a
    /// verified auth.test for ten minutes.
    @Test func intakeHasItsOwnBudgetPerClient() async throws {
        let h = try SlackHarness(budget: ReadBudget(perHour: 3), intakePerHour: 4)
        try await h.register()
        let first = try await h.propose() // auth.test and conversations.info.
        _ = try await h.propose() // conversations.info only.
        _ = try await h.propose()
        await #expect(throws: SlackRefusal.busy) { try await h.propose() }
        let intake = try await h.fixture.calls().dropFirst()
        #expect(intake.map { $0["path"] } == ["/api/auth.test", "/api/conversations.info", "/api/conversations.info", "/api/conversations.info"])
        // Another admitted client has its own budget.
        _ = try await h.propose(client: "other-client")
        // The window's Read again still has all of its reads.
        #expect(try await h.handler.resolve(first, model: nil).facts["Visibility"] == "Public channel")
        h.clock.advance(601)
        _ = try await h.propose(client: "other-client")
        #expect(try await h.fixture.calls().suffix(2).map { $0["path"] } == ["/api/auth.test", "/api/conversations.info"])
    }

    /// Intake keeps its short deadline; the window, the write and read-back have their own client and a long one.
    @Test func onlyIntakeUsesTheShortDeadline() async throws {
        let h = try SlackHarness()
        try await h.register()
        h.fixture.timeouts.withLock { $0 = [] }
        let proposal = try await h.propose()
        #expect(h.fixture.timeouts.withLock { $0 } == [SlackHarness.testTimeouts.intake])
        try await h.approve(proposal)
        try await h.fixture.configure(["post_mode": "unclear"])
        #expect(try await h.handler.execute(proposal.id) == .executed(confirmedBy: "read_back"))
        #expect(h.fixture.timeouts.withLock { $0 } == [30, 30, 30, 30])
    }

    /// Either operation id confirms a lost reply: Slack may keep the metadata, client_msg_id, or both.
    @Test func readBackMatchesEitherOperationID() async throws {
        let h = try SlackHarness()
        try await h.register()
        for settings: [String: JSON] in [["metadata": "", "drop_client_id": .bool(true)], ["metadata": "drop", "drop_client_id": .bool(false)]] {
            try await h.fixture.configure(settings.merging(["post_mode": "lost"], uniquingKeysWith: { $1 }))
            let proposal = try await h.propose()
            try await h.approve(proposal)
            #expect(try await h.handler.execute(proposal.id) == .executed(confirmedBy: "read_back"))
            let lookup = try #require(try await h.fixture.calls().last { $0["query"]?["oldest"] != nil })
            #expect(lookup["query"]?["include_all_metadata"] == .array(["true"]))
        }
        #expect(try await h.fixture.posts().count == 2)
    }

    /// Automatic read-back backs off and stops after the search window; a relaunch reads once more, and the
    /// person's Read again always reads. A read-back 429 does not stop a new thread reply's preflight.
    @Test func readBackBacksOffStopsAndKeepsItsOwnRateLimits() async throws {
        let h = try SlackHarness()
        try await h.register()
        let proposal = try await h.propose()
        try await h.approve(proposal)
        try await h.fixture.configure(["post_mode": "unclear", "hide_posts": .bool(true)])
        #expect(try await h.handler.execute(proposal.id) == .unknown)
        #expect(h.handler.readBackDue().isEmpty) // Just read.
        h.clock.advance(15)
        #expect(h.handler.readBackDue() == [proposal.id])
        await h.handler.automaticReadBack()
        h.clock.advance(15)
        #expect(h.handler.readBackDue().isEmpty) // Now 30 seconds apart.
        h.clock.advance(15)
        #expect(h.handler.readBackDue() == [proposal.id])

        // Slack rate limits read-back: that deadline is read-back's alone.
        try await h.fixture.configure(["rate_method": "conversations.replies", "retry_after": .int(600)])
        #expect(try await h.handler.reconcile(proposal.id) == .unknown)
        try await h.fixture.configure(["rate_method": ""])
        let before = try await h.fixture.calls().count
        #expect(try await h.handler.reconcile(proposal.id) == .unknown)
        #expect(try await h.fixture.calls().dropFirst(before).allSatisfy { $0["path"] != "/api/conversations.replies" })
        let reply = try await h.propose()
        #expect(try await h.handler.resolve(reply, model: nil).facts["Thread"] != nil)

        h.clock.advance(PostHandler.automaticReadBackEnd)
        #expect(h.handler.readBackDue().isEmpty) // The search window has closed.
        let (_, resumed) = try h.restart()
        #expect(resumed.readBackDue() == [proposal.id]) // Once after a relaunch.
        await resumed.automaticReadBack()
        h.clock.advance(3_600)
        #expect(resumed.readBackDue().isEmpty)
        try await h.fixture.configure(["hide_posts": .bool(false)])
        #expect(try await resumed.reconcile(proposal.id) == .executed(confirmedBy: "read_back")) // Read again.
        #expect(try await h.fixture.posts().count == 1)
    }

    @Test func readBackIsMetered() async throws {
        let h = try SlackHarness(readBackBudget: ReadBudget(perHour: 2, concurrent: 1))
        try await h.register()
        let proposal = try await h.propose()
        try await h.approve(proposal)
        try await h.fixture.configure(["post_mode": "unclear"])
        let before = try await h.fixture.calls().count
        #expect(try await h.handler.execute(proposal.id) == .unknown)
        // Final reads (3) and the write, then read-back stops after its two reads.
        #expect(try await h.fixture.calls().count == before + 3 + 1 + 2)
    }

    @Test func lostReplyIsReadBackWithOneWirePost() async throws {
        let h = try SlackHarness()
        try await h.register()
        let proposal = try await h.propose()
        try await h.approve(proposal)
        try await h.fixture.configure(["post_mode": "lost", "paginate": .bool(true)])

        #expect(try await h.handler.execute(proposal.id) == .executed(confirmedBy: "read_back"))
        let calls = try await h.fixture.calls()
        let posts = calls.filter { $0["method"] == "POST" }
        let post = try #require(posts.first)
        let marker = try #require(h.journal.slackMarker(proposal.id))
        #expect(UUID(uuidString: marker) != nil)
        #expect(posts.count == 1)
        #expect(post["body"]?["client_msg_id"] == .string(marker))
        let lookups = calls.filter { $0["query"]?["oldest"] != nil }
        #expect(lookups.count == 2 && lookups.allSatisfy { $0["query"]?["include_all_metadata"] == .array(["true"]) })
        #expect(lookups.allSatisfy { $0["path"] == "/api/conversations.replies" && $0["query"]?["ts"] == .array([.string(SlackHarness.root)]) })
        #expect(lookups.last?["query"]?["cursor"] == .array(["fake-page=="]))
        let records = try deliverAll(h.journal)
        #expect(records.map { $0["type"] }.suffix(3) == ["app.action.started", "app.action.outcome_unknown", "app.action.executed"])
        #expect(records.last?["metadata"]?["native"] == .string("slack:message:C0GENERAL:\(SlackHarness.posted)"))
        #expect(records.last?["metadata"]?["confirmed_by"] == "read_back")
        #expect(!records.contains { $0.canonical.contains(marker) })
    }

    @Test func readBackRequiresUniqueExactEvidenceAndSurvivesRestart() async throws {
        let h = try SlackHarness()
        try await h.register()
        let proposal = try await h.propose(thread: nil)
        try await h.approve(proposal)
        try await h.fixture.configure(["post_mode": "unclear", "hide_posts": .bool(true)])
        #expect(try await h.handler.execute(proposal.id) == .unknown)
        #expect(h.handler.unknown() == [proposal.id])
        let marker = try #require(h.journal.slackMarker(proposal.id))
        try await h.fixture.configure(["hide_posts": .bool(false)])
        let otherOp = JSON.object(["event_type": "beacon_post", "event_payload": .object(["op": .string(UUID().uuidString)])])
        let wrongEvidence: [[String: JSON]] = [
            ["client_msg_id": .null, "metadata": .null], // Matching text alone is never enough.
            ["client_msg_id": .null, "metadata": otherOp], // Another Beacon operation.
            ["metadata": otherOp], // Contradictory ids.
            ["client_msg_id": .null, "metadata": .object(["event_type": "other_app", "event_payload": .object(["op": .string(marker)])])],
            ["text": .string(SlackHarness.draft.precomposedStringWithCanonicalMapping)], // Equal as Swift strings, not bytes.
            ["user": "U0OTHER"], ["channel": "C0OTHER"], ["thread_ts": .string(SlackHarness.root)],
            ["ts": "1800001000.000200"],
        ]
        for echo in wrongEvidence {
            try await h.fixture.configure(["echo": .object(echo)])
            #expect(try await h.handler.reconcile(proposal.id) == .unknown)
            try await h.fixture.configure(["echo": .null])
        }
        try await h.fixture.configure(["duplicate": .bool(true)])
        #expect(try await h.handler.reconcile(proposal.id) == .unknown)
        try await h.fixture.configure(["duplicate": .bool(false)])

        h.clock.advance(3_600)
        let (journal, resumed) = try h.restart()
        #expect(resumed.unknown() == [proposal.id] && journal.slackMarker(proposal.id) == marker)
        await #expect(throws: JournalError.transition) { try await resumed.execute(proposal.id) }
        #expect(try await resumed.reconcile(proposal.id) == .executed(confirmedBy: "read_back"))
        let calls = try await h.fixture.calls()
        #expect(calls.filter { $0["method"] == "POST" }.count == 1)
        let history = try #require(calls.last)
        #expect(history["path"] == "/api/conversations.history")
        #expect(history["query"]?["oldest"] == .array(["1799999940.000000"]))
        #expect(history["query"]?["latest"] == .array(["1800000300.000000"]))
        let records = try deliverAll(journal)
        #expect(records.filter { $0["type"] == "app.action.outcome_unknown" }.count == 1)
        #expect(records.last?["metadata"]?["confirmed_by"] == "read_back")
    }

    @Test func recoveryOfAStartWithoutAResultDoesNotPostAgain() async throws {
        let h = try SlackHarness()
        let settings = try await h.register()
        let proposal = try await h.propose()
        try await h.approve(proposal)
        let approval = try h.journal.start(proposal.id, at: h.clock.now)
        let marker = try #require(h.journal.slackMarker(proposal.id))
        let clock = h.clock
        let api = SlackAPI(client: try h.fixture.connect(h.vault.credential(for: settings.binding), SlackHarness.testTimeouts.action),
                           workspace: settings.workspace, rates: SlackRateLimits(), now: { clock.now })
        // Simulate process death just after the service took the write, without appending a result or unknown.
        _ = try await api.post(SlackDraft(approval.params), marker: marker, user: settings.userID)
        let (journal, resumed) = try h.restart()
        #expect(resumed.unknown() == [proposal.id] && journal.slackMarker(proposal.id) == marker)
        await #expect(throws: JournalError.transition) { try await resumed.execute(proposal.id) }
        #expect(try await resumed.reconcile(proposal.id) == .executed(confirmedBy: "read_back"))
        #expect(try await h.fixture.posts().count == 1)
        let records = try deliverAll(journal)
        #expect(records.first { $0["type"] == "app.action.outcome_unknown" }?["metadata"]?["reason"] == "app_restarted")
    }
}
