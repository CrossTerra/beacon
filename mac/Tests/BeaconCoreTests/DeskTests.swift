import Foundation
import Testing
@testable import BeaconCore

/// Resolves every proposal at once, or, while gated, waits until `release` opens the gate for every waiter.
final class FakeResolver: ActionResolver, @unchecked Sendable {
    private let lock = NSLock()
    private var waiters: [CheckedContinuation<Void, Never>] = []
    private var closed = false

    var gated: Bool {
        get { lock.withLock { closed } }
        set { lock.withLock { closed = newValue } }
    }

    func resolve(_ proposal: Proposal, model: String?) async throws -> ResolvedAction {
        await withCheckedContinuation { continuation in
            lock.withLock {
                if closed { waiters.append(continuation) } else { continuation.resume() }
            }
        }
        if proposal.type == .agentRun {
            return ResolvedAction(origin: nil, account: "macos:user:winton",
                                  destination: .object(["checkout_id": "x", "branch": "main"]), readAt: Date())
        }
        return ResolvedAction(origin: "https://api.github.com", account: "github:user:123",
                              destination: mergeDestination, facts: ["review": "approved"], readAt: Date())
    }

    func release() {
        let waiting = lock.withLock {
            closed = false
            defer { waiters = [] }
            return waiters
        }
        waiting.forEach { $0.resume() }
    }
}

@MainActor
struct DeskTests {
    let clock = Clock()
    let resolver = FakeResolver()

    func desk(_ count: Int = 2) throws -> (ApprovalDesk, Journal, [String]) {
        let (journal, ids) = try filledJournal(scratchPath(), at: clock.now, count: count)
        let clock = self.clock
        return (ApprovalDesk(journal: journal, resolver: resolver, now: { clock.now }), journal, ids)
    }

    func frozen(_ desk: ApprovalDesk, _ id: String) async throws -> UUID {
        desk.select(id)
        await desk.freezing?.value
        return try #require(desk.frozen?.token)
    }

    func approvals(_ journal: Journal) throws -> Int {
        try deliverAll(journal).filter { $0["type"] == "app.action.approved" }.count
    }

    @Test func approveIsDisabledUntilFrozenAndConsumesOnce() async throws {
        let (desk, journal, ids) = try desk()
        #expect(desk.pending.map(\.id) == ids)
        #expect(!desk.approve(UUID()))
        resolver.gated = true
        desk.select(ids[0])
        #expect(desk.frozen == nil && !desk.approve(UUID()))
        resolver.release()
        await desk.freezing?.value
        let token = try #require(desk.frozen?.token)
        #expect(desk.frozen?.approval.account == "github:user:123")

        // A click and the keystroke racing for one proposal, and a held key: one approval.
        #expect(desk.approve(token))
        #expect(!desk.approve(token))
        #expect(!desk.approve(token))
        #expect(journal.proposal(ids[0], at: clock.now)?.status == .approved)
        // The selection stays put: the next row is not frozen, so a repeated key cannot approve it.
        #expect(desk.selection == ids[0] && desk.frozen == nil)
        #expect(journal.proposal(ids[1], at: clock.now)?.status == .pending)
        #expect(try approvals(journal) == 1)
    }

    @Test func inputBoundToAnOldViewApprovesNothing() async throws {
        let (desk, journal, ids) = try desk(3)

        let old = try await frozen(desk, ids[0])
        desk.select(ids[1])
        #expect(!desk.approve(old))

        let reread = try await frozen(desk, ids[1])
        desk.readAgain()
        #expect(!desk.approve(reread))

        let withdrawn = try await frozen(desk, ids[2])
        _ = try journal.withdraw(ids[2], at: clock.now)
        await Task.yield()
        desk.refresh()
        #expect(desk.frozen == nil && !desk.approve(withdrawn))

        let expiring = try await frozen(desk, ids[0])
        clock.advance(Proposal.lifetime + 1)
        desk.refresh()
        #expect(!desk.approve(expiring))
        #expect(try approvals(journal) == 0)
    }

    @Test func aSlowResolveForAnotherRowNeverFreezes() async throws {
        let (desk, journal, ids) = try desk()
        resolver.gated = true
        desk.select(ids[0])
        let first = desk.freezing
        desk.select(ids[1])
        resolver.release()
        await first?.value
        await desk.freezing?.value
        #expect(desk.frozen?.approval.id == ids[1])
        #expect(journal.proposal(ids[0], at: clock.now)?.status == .pending)
    }

    @Test func modelChangeRefreezesWithBothHashes() async throws {
        let journal = try Journal(path: scratchPath(), at: clock.now)
        let run = Proposal(request: request("agent.run", runParams), client: "c", at: clock.now)
        try journal.add(run)
        let clock = self.clock
        let desk = ApprovalDesk(journal: journal, resolver: resolver, now: { clock.now })
        let original = try await frozen(desk, run.id)
        desk.choose(model: "openai/gpt-6")
        #expect(!desk.approve(original))
        await desk.freezing?.value
        let token = try #require(desk.frozen?.token)
        #expect(desk.frozen?.approval.edited == ["model"])
        #expect(desk.approve(token))
        let approved = try #require(try deliverAll(journal).last)
        #expect(approved["metadata"]?["edited"] == .array(["model"]))
        #expect(approved["sealed"]?["metadata"]?["original_model"] == "anthropic/claude-opus-5")
    }

    @Test func quitDisablesApprovalAndExpiresUnstarted() async throws {
        let (desk, journal, ids) = try desk()
        let token = try await frozen(desk, ids[0])
        #expect(desk.approve(token))
        let other = try await frozen(desk, ids[1])
        desk.beginShutdown()
        #expect(!desk.approve(other))
        desk.select(ids[1])
        await desk.freezing?.value
        #expect(desk.frozen == nil)
        #expect(journal.proposal(ids[0], at: clock.now)?.status == .expired)
        #expect(journal.proposal(ids[1], at: clock.now)?.status == .pending)
    }

    @Test func declineRecordsTheNoteAndNothingElse() async throws {
        let (desk, journal, ids) = try desk(1)
        _ = try await frozen(desk, ids[0])
        desk.decline(note: "Not in that thread, please.")
        #expect(desk.frozen == nil)
        let records = try deliverAll(journal)
        #expect(records.map { $0["type"]!.stringValue! } == ["app.action.proposed", "app.action.declined"])
        #expect(records[1]["sealed"]?["metadata"]?["note"] == "Not in that thread, please.")
        #expect(throws: JournalError.transition) { try journal.start(ids[0], at: clock.now) }
    }
}

struct SetupTests {
    @Test func actFileIsWrittenOnceAndChecked() throws {
        let url = URL(fileURLWithPath: scratchPath("act.json"))
        #expect(try ClientTrust.ensure(url, requirement: "identifier \"dev.beacon.app\"") == .written)
        #expect(try FileManager.default.attributesOfItem(atPath: url.path)[.posixPermissions] as? Int == 0o600)
        #expect(try ClientTrust.ensure(url, requirement: "identifier \"dev.beacon.app\"") == .matches)
        #expect(try ClientTrust.ensure(url, requirement: "identifier \"dev.beacon.other\"") == .mismatch)
        try Data("not json".utf8).write(to: url)
        #expect(try ClientTrust.ensure(url, requirement: "identifier \"dev.beacon.app\"") == .mismatch)
        // The test host is not dev.beacon.app: it never trusts itself.
        #expect(ClientTrust.ownRequirement() == nil)
    }

    @Test func invisibleCharactersAreShown() {
        #expect(visibleText("ok\u{202E}evil\u{200B}\u{1}") == "ok⟨U+202E⟩evil⟨U+200B⟩⟨U+0001⟩")
        #expect(visibleText("line\nnext — ✅") == "line\nnext — ✅")
    }
}

/// The socket and the window share the journal: a proposal arrives over the socket, survives a relaunch, is
/// approved in the window, and the proposer's wait sees it.
@MainActor
struct EndToEndTests {
    @Test func proposeOverTheSocketApproveInTheWindow() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent("e2e-" + randomHex(4)).path
        try FileManager.default.createDirectory(atPath: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(atPath: directory) }
        let journalPath = directory + "/journal.sqlite"
        var journal = try Journal(path: journalPath, at: Date())
        let queue = DispatchQueue(label: "e2e")
        var server = ProposalServer(path: directory + "/run/act.sock",
                                    clients: [AdmittedClient(name: "beacon-act", requirement: selfRequirement())],
                                    book: journal, enabled: [.githubMerge], queue: queue)
        try server.start()
        let id = try #require(try Client(path: server.path).call(mergeProposal())?["id"]?.stringValue)
        server.stop()

        // Relaunch: the pending proposal is still there.
        journal = try Journal(path: journalPath, at: Date())
        server = ProposalServer(path: directory + "/run/act.sock",
                                clients: [AdmittedClient(name: "beacon-act", requirement: selfRequirement())],
                                book: journal, enabled: [.githubMerge], queue: queue)
        try server.start()
        defer { server.stop() }
        let desk = ApprovalDesk(journal: journal, resolver: FakeResolver())
        #expect(desk.pending.map(\.id) == [id])

        let client = try Client(path: server.path)
        client.send(.object(["op": "wait", "id": .string(id), "after": .int(1), "timeout": .int(30)]))
        try await Task.sleep(nanoseconds: 100_000_000)
        desk.select(id)
        await desk.freezing?.value
        #expect(desk.approve(try #require(desk.frozen?.token)))
        let answer = await Task.detached { client.receive() }.value
        #expect(answer?["status"] == "approved" && answer?["version"] == .int(2))
    }
}
