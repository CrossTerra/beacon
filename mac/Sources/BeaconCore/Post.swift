import Foundation

/// `slack.post` (13.00, _Slack at the write_). Only a started approval can write, and only once. A missing or
/// ambiguous reply is reconciled read-only, including on relaunch.
public final class PostHandler: ActionResolver, Sendable {
    private let journal: Journal
    private let settings: @Sendable () -> SlackSettings?
    private let connect: @Sendable (SlackSettings, TimeInterval) throws -> ServiceClient
    /// The window's preflight and the final reads before a write; shared with GitHub.
    private let budget: ReadBudget
    /// Read-back of lost replies, metered on its own so it never competes with the window.
    private let readBackBudget: ReadBudget
    /// Intake lookups, per admitted client identity, so a proposer cannot spend the window's reads.
    private let intakePerHour: Int
    private let intakeBudgets = Locked<[String: ReadBudget]>([:])
    /// A verified `auth.test` for intake, reused for `identityCacheTime`. The window and the write always read afresh.
    private let identityCache = Locked<(key: String, identity: SlackIdentity, at: Date)?>(nil)
    static let identityCacheTime: TimeInterval = 600
    private let timeouts: SlackAPI.Timeouts
    private let now: @Sendable () -> Date
    private let rates = SlackRateLimits()
    private let reconciling = Locked<Set<String>>([])
    /// Automatic read-backs made in this process, per proposal: how many and when the last began.
    private let readBacks = Locked<[String: (count: Int, last: Date)]>([:])

    public convenience init(journal: Journal, settings: @escaping @Sendable () -> SlackSettings?, vault: Vault,
                            budget: ReadBudget = ReadBudget()) {
        self.init(journal: journal, settings: settings, vault: vault, budget: budget, now: { Date() },
                  connect: { try SlackAPI.connect($0, timeout: $1) })
    }

    init(journal: Journal, settings: @escaping @Sendable () -> SlackSettings?, vault: Vault, budget: ReadBudget = ReadBudget(),
         intakePerHour: Int = 30, readBackBudget: ReadBudget = ReadBudget(perHour: 60, concurrent: 2),
         timeouts: SlackAPI.Timeouts = SlackAPI.productionTimeouts,
         now: @escaping @Sendable () -> Date, connect: @escaping SlackAPI.Connect) {
        self.journal = journal
        self.settings = settings
        self.timeouts = timeouts
        self.budget = budget
        self.intakePerHour = intakePerHour
        self.readBackBudget = readBackBudget
        self.now = now
        self.connect = { settings, timeout in
            let token = try vault.credential(for: settings.binding)
            guard token.isSlackUserToken else { throw SlackRefusal.notAUser }
            return try connect(token, timeout)
        }
    }

    /// Signal requires the same subject on every record, and may receive `proposed` before the window opens.
    /// Do the minimum lookup before journaling it. The request's optional context is never a record's authority.
    /// `client` is the admitted client identity; each has its own intake budget.
    public func place(_ request: ProposalRequest, client: String) async throws -> SlackPlacement {
        guard request.type == .slackPost else { throw NoResolver.Unsupported() }
        let draft = try SlackDraft(request.params)
        let settings = try configured(for: draft)
        let budget = intakeBudgets.withLock { budgets in
            if let existing = budgets[client] { return existing }
            let created = ReadBudget(perHour: intakePerHour, concurrent: 2)
            budgets[client] = created
            return created
        }
        return try await preflight(settings, .intake, budget) { api in
            let identity = try await self.identify(api, settings, cached: true)
            let info = try await api.conversation(draft.channel)
            guard let visibility = info.visibility else { throw SlackRefusal.visibilityUnknown }
            let words = try await self.words(api, draft: draft, settings: settings, identity: identity,
                                              conversation: info, visibility: visibility)
            try self.requireCurrent(settings)
            return SlackPlacement(account: settings.account, visibility: visibility, words: words.line)
        }
    }

    public func resolve(_ proposal: Proposal, model: String?) async throws -> ResolvedAction {
        guard proposal.type == .slackPost else { throw NoResolver.Unsupported() }
        let draft = try SlackDraft(proposal.params)
        try draft.checkLength()
        let settings = try configured(for: draft)
        guard let placement = proposal.slackPlacement, placement.account == settings.account else {
            throw SlackRefusal.destinationChanged
        }
        let at = now()
        return try await preflight(settings, .preflight, budget) { api in
            try api.canPost()
            let (identity, conversation, parent) = try await self.read(api, draft, settings, placement)
            let words = try await self.words(api, draft: draft, settings: settings, identity: identity,
                                              conversation: conversation, visibility: placement.visibility)
            try self.requireCurrent(settings)
            var facts = ["Workspace": words.workspace, "Visibility": words.visibility]
            if draft.thread != nil, let parent {
                let firstLine = parent.text?.prefix(while: { !$0.isNewline }) ?? ""
                facts["Thread"] = SlackDisplay.shown(String(firstLine)) ?? "(no text on first line)"
            }
            return ResolvedAction(origin: SlackSettings.origin, account: settings.account, destination: draft.destination,
                                  facts: facts, readAt: at, accountLabel: SlackDisplay.shown(identity.user) ?? identity.user,
                                  destinationLabel: words.conversation)
        }
    }

    /// The destination in the person's words. A failed name lookup does not fail the proposal: the window says
    /// which channel it could not name. User names are cached for ten minutes, on the caller's read budget.
    private struct Words {
        let conversation: String
        let visibility: String
        let workspace: String
        var line: String { "\(conversation) · \(visibility) · \(workspace)" }
    }

    private struct CachedName { let value: String; let at: Date }
    private let userNames = Locked<[String: CachedName]>([:])
    private let groupPhrases = Locked<[String: CachedName]>([:])

    private func words(_ api: SlackAPI, draft: SlackDraft, settings: SlackSettings, identity: SlackIdentity,
                       conversation: SlackConversation, visibility: SlackVisibility) async throws -> Words {
        let phrase = try await conversationWords(api, draft: draft, user: settings.userID, conversation: conversation,
                                                   visibility: visibility)
        return Words(conversation: phrase, visibility: visibility.word,
                     workspace: SlackDisplay.shown(identity.team) ?? "unknown workspace")
    }

    private func conversationWords(_ api: SlackAPI, draft: SlackDraft, user: String, conversation: SlackConversation,
                                   visibility: SlackVisibility) async throws -> String {
        switch visibility {
        case .public, .private:
            if let raw = conversation.name, let name = SlackDisplay.shown(raw) { return "#\(name)" }
            return SlackDisplay.unknown(draft.channel)
        case .dm:
            guard let other = conversation.user, Field.matches(other, "[UW][A-Z0-9]{0,31}") else {
                return SlackDisplay.unknown(draft.channel)
            }
            if other == user { return "DM with yourself" }
            if let name = try await displayUser(api, other) { return "DM with \(name)" }
            return SlackDisplay.unknown(draft.channel)
        case .groupDM:
            return try await groupWords(api, draft.channel, except: user)
        }
    }

    /// Other members' real names, in user-id order, at most eight. A lookup that fails is omitted; if none can be
    /// read, the channel id is shown rather than a blank.
    private func groupWords(_ api: SlackAPI, _ channel: String, except user: String) async throws -> String {
        let key = "\(api.workspace)/\(channel)", at = now()
        if let hit = groupPhrases.withLock({ $0[key] }), at >= hit.at, at.timeIntervalSince(hit.at) < Self.identityCacheTime {
            return hit.value
        }
        let ids: [String]
        do {
            ids = try await api.memberIDs(channel)
        } catch {
            if error is CancellationError { throw error }
            return SlackDisplay.unknown(channel)
        }
        let others = ids.filter { $0 != user && Field.matches($0, "[UW][A-Z0-9]{0,31}") }.sorted()
        var names: [String] = []
        for id in others.prefix(8) {
            if let name = try await displayUser(api, id) { names.append(name) }
        }
        guard !names.isEmpty else { return SlackDisplay.unknown(channel) }
        var phrase = "Group DM with \(names.joined(separator: ", "))"
        if others.count > names.count { phrase += " +\(others.count - names.count)" }
        groupPhrases.withLock { $0[key] = CachedName(value: phrase, at: at) }
        return phrase
    }

    /// `users.info`'s real name, cached for ten minutes. Nil when Slack will not say; cancellation still propagates.
    private func displayUser(_ api: SlackAPI, _ id: String) async throws -> String? {
        let key = "\(api.workspace)/\(id)", at = now()
        if let hit = userNames.withLock({ $0[key] }), at >= hit.at, at.timeIntervalSince(hit.at) < Self.identityCacheTime {
            return hit.value
        }
        do {
            guard let name = SlackDisplay.shown(try await api.userName(id)) else { return nil }
            userNames.withLock { $0[key] = CachedName(value: name, at: at) }
            return name
        } catch {
            if error is CancellationError { throw error }
            return nil
        }
    }

    private func configured(for draft: SlackDraft) throws -> SlackSettings {
        guard let settings = settings() else { throw SlackRefusal.notConfigured }
        guard settings.workspace == draft.workspace else { throw SlackRefusal.workspaceMismatch }
        return settings
    }

    private func requireCurrent(_ approved: SlackSettings) throws {
        guard let current = settings(), current.workspace == approved.workspace, current.userID == approved.userID else {
            throw SlackRefusal.destinationChanged
        }
    }

    /// A client for one purpose: intake keeps its short deadline, everything else has its own client with the
    /// action deadline, and each purpose keeps its own 429 deadlines.
    private func api(_ settings: SlackSettings, _ purpose: SlackRateLimits.Purpose) throws -> SlackAPI {
        let timeout = purpose == .intake ? timeouts.intake : timeouts.action
        return SlackAPI(client: try connect(settings, timeout), workspace: settings.workspace, rates: rates, purpose: purpose, now: now)
    }

    private func preflight<T>(_ settings: SlackSettings, _ purpose: SlackRateLimits.Purpose, _ budget: ReadBudget,
                              _ work: (SlackAPI) async throws -> T) async throws -> T {
        do { try await budget.enter() } catch { throw SlackRefusal.busy }
        do {
            try Task.checkCancellation()
            var api = try api(settings, purpose)
            let now = self.now
            api.meter = { try await budget.take(at: now()) }
            let result = try await work(api)
            await budget.leave()
            return result
        } catch {
            await budget.leave()
            if let refusal = error as? SlackRefusal { throw refusal }
            if error is ReadBudget.Exhausted { throw SlackRefusal.busy }
            throw SlackRefusal.unavailable
        }
    }

    private func identify(_ api: SlackAPI, _ settings: SlackSettings, cached: Bool = false) async throws -> SlackIdentity {
        let key = "\(settings.workspace)/\(settings.userID)", at = now()
        if cached, let hit = identityCache.withLock({ $0 }), hit.key == key,
           at >= hit.at, at.timeIntervalSince(hit.at) < Self.identityCacheTime {
            return hit.identity
        }
        let identity = try await api.identity()
        guard identity.user_id == settings.userID else {
            identityCache.withLock { $0 = nil }
            throw SlackRefusal.accountMismatch
        }
        guard identity.team_id == settings.workspace else {
            identityCache.withLock { $0 = nil }
            throw SlackRefusal.workspaceMismatch
        }
        identityCache.withLock { $0 = (key, identity, at) }
        return identity
    }

    private func read(_ api: SlackAPI, _ draft: SlackDraft, _ settings: SlackSettings, _ placement: SlackPlacement)
        async throws -> (SlackIdentity, SlackConversation, SlackMessage?) {
        let identity = try await identify(api, settings)
        let info = try await api.conversation(draft.channel)
        guard info.visibility == placement.visibility else { throw SlackRefusal.destinationChanged }
        guard info.is_archived != true, info.is_frozen != true else { throw SlackRefusal.archived }
        try await api.requireMember(info, user: settings.userID)
        if let thread = draft.thread { return (identity, info, try await api.parent(channel: draft.channel, thread: thread)) }
        return (identity, info, nil)
    }

    public enum Outcome: Equatable, Sendable {
        case executed(confirmedBy: String)
        case failed(error: String, confirmedBy: String)
        case unknown
    }

    @discardableResult
    public func execute(_ id: String) async throws -> Outcome {
        guard let proposal = journal.proposal(id, at: now()), proposal.type == .slackPost else { throw JournalError.notFound }
        let approval = try journal.start(id, at: now())
        let draft: SlackDraft, settings: SlackSettings, api: SlackAPI, marker: String
        do {
            draft = try SlackDraft(approval.params)
            try draft.checkLength()
            settings = try configured(for: draft)
            guard let placement = proposal.slackPlacement, placement.account == approval.account,
                  approval.account == settings.account, approval.origin == SlackSettings.origin,
                  approval.destination == draft.destination, let recorded = journal.slackMarker(id) else {
                throw SlackRefusal.destinationChanged
            }
            marker = recorded
            api = try self.api(settings, .preflight)
            try api.canPost()
            _ = try await read(api, draft, settings, placement)
            try requireCurrent(settings)
            guard journal.mayDispatch(id) else { throw SlackRefusal.shuttingDown }
        } catch {
            return try fail(id, error as? SlackRefusal ?? .unavailable, by: "pre_dispatch")
        }

        let reply: SlackAPI.PostReply
        do {
            reply = try await api.post(draft, marker: marker, user: settings.userID)
        } catch {
            // Only the cooldown check throws; a transport failure is ambiguous instead.
            return try fail(id, error as? SlackRefusal ?? .unavailable, by: "pre_dispatch")
        }
        switch reply {
        case .posted(let ts): return try executed(id, draft.channel, ts, by: "reply")
        case .refused(let refusal): return try fail(id, refusal, by: "reply")
        case .ambiguous(let reason):
            try journal.outcomeUnknown(id, reason: reason, at: now())
            return try await reconcile(id)
        }
    }

    /// Match the operation id, as message metadata or as `client_msg_id`, AND exact text, from the approved user and
    /// destination, around the original start. Never infer anything from a missing message or matching text alone.
    /// This is the person's _Read again_ and the read after a lost reply; `readBackDue` decides automatic ones. The
    /// reads count against the read-back budget and their 429s against read-back only.
    @discardableResult
    public func reconcile(_ id: String) async throws -> Outcome {
        guard reconciling.withLock({ $0.insert(id).inserted }) else { return .unknown }
        defer { reconciling.withLock { _ = $0.remove(id) } }
        guard let proposal = journal.proposal(id, at: now()), proposal.type == .slackPost, proposal.status == .outcomeUnknown,
              let approval = journal.approval(id), let placement = proposal.slackPlacement, let started = journal.startedAt(id),
              let marker = journal.slackMarker(id), let draft = try? SlackDraft(approval.params),
              let settings = try? configured(for: draft), approval.account == settings.account,
              placement.account == approval.account, approval.origin == SlackSettings.origin,
              approval.destination == draft.destination else { return .unknown }
        let at = now()
        readBacks.withLock { $0[id] = (($0[id]?.count ?? 0) + 1, at) }
        let ts: String
        do {
            let match = try await preflight(settings, .readBack, readBackBudget) { api -> String? in
                _ = try await self.identify(api, settings)
                let info = try await api.conversation(draft.channel)
                guard info.visibility == placement.visibility else { return nil }
                try await api.requireMember(info, user: settings.userID)
                guard let match = try await self.find(api, draft, user: settings.userID, marker: marker, started: started) else { return nil }
                try self.requireCurrent(settings)
                return match
            }
            guard let match else { return .unknown }
            ts = match
        } catch {
            return .unknown
        }
        return try executed(id, draft.channel, ts, by: "read_back")
    }

    public func unknown() -> [String] {
        journal.all().filter { $0.type == .slackPost && $0.status == .outcomeUnknown }.map(\.id)
    }

    /// Read-back searches `started - 60 … started + 300`. Automatic read-back backs off (15, 30, 60, 120 seconds)
    /// and stops a minute after that window has closed; after that only a relaunch (once) or _Read again_ reads.
    static let readBackWindow: TimeInterval = 300
    static let automaticReadBackEnd: TimeInterval = readBackWindow + 60

    static func readBackDelay(after count: Int) -> TimeInterval {
        min(15 * pow(2, Double(max(count, 1) - 1)), 120)
    }

    /// The unknown posts an automatic read-back may read now: each once after launch, then on the backoff until
    /// the search window has closed.
    public func readBackDue() -> [String] {
        let at = now()
        let attempts = readBacks.withLock { $0 }
        return unknown().filter { id in
            guard let started = journal.startedAt(id) else { return false }
            guard let last = attempts[id] else { return true }
            return at.timeIntervalSince(started) <= Self.automaticReadBackEnd
                && at.timeIntervalSince(last.last) >= Self.readBackDelay(after: last.count)
        }
    }

    /// The app's timer: reads back what `readBackDue` names.
    public func automaticReadBack() async {
        for id in readBackDue() where !Task.isCancelled { _ = try? await reconcile(id) }
    }

    private func find(_ api: SlackAPI, _ draft: SlackDraft, user: String, marker: String, started: Date) async throws -> String? {
        // Include Slack/Mac clock skew and the final reads, but never search all history or drift with relaunch time.
        let oldest = floor(started.timeIntervalSince1970 - 60)
        let latest = ceil(min(now().timeIntervalSince1970 + 60, started.timeIntervalSince1970 + Self.readBackWindow))
        guard oldest <= latest else { return nil }
        var cursor: String?
        var found: String?
        for _ in 0..<4 {
            let page = try await api.messages(channel: draft.channel, thread: draft.thread, limit: 15,
                                              oldest: "\(Int64(oldest)).000000", latest: "\(Int64(latest)).000000", cursor: cursor)
            for message in page.messages {
                let op = message.metadata?.eventType == SlackMetadata.eventType ? message.metadata?.op : nil
                // Slack may keep either id, or assign its own client_msg_id; a different Beacon operation is not ours.
                guard op == marker || (message.client_msg_id == marker && op == nil) else {
                    if message.client_msg_id == marker { return nil }
                    continue
                }
                guard message.type == "message", message.subtype == nil, message.user == user, let ts = message.ts, ts != draft.thread,
                      Field.matches(ts, Field.slackTS), let time = Double(ts), (oldest...latest).contains(time),
                      message.thread_ts == draft.thread || (draft.thread == nil && message.thread_ts == ts),
                      message.channel == nil || message.channel == draft.channel,
                      let text = message.text, text.utf8.elementsEqual(draft.text.utf8) else { return nil }
                if let found, found != ts { return nil } // More than one distinct match is ambiguous.
                found = ts
            }
            guard let next = try page.response_metadata?.next() else { return page.has_more == true ? nil : found }
            guard next != cursor else { return nil }
            cursor = next
        }
        return nil
    }

    private func executed(_ id: String, _ channel: String, _ ts: String, by: String) throws -> Outcome {
        try journal.result(id, executed: true, confirmedBy: by, fields: ["native": .string("slack:message:\(channel):\(ts)")], at: now())
        return .executed(confirmedBy: by)
    }

    private func fail(_ id: String, _ refusal: SlackRefusal, by: String) throws -> Outcome {
        let fields: [String: JSON] = ["error": .string(refusal.code), "result": .string(refusal.explanation)]
        try journal.result(id, executed: false, confirmedBy: by, fields: fields, at: now())
        return .failed(error: refusal.code, confirmedBy: by)
    }
}

/// Settings accepts only a Slack user token. The workspace and user come from `auth.test`, not typed ids or an
/// agent. Saving a replacement cannot switch either identity without first removing the account.
public enum SlackSetup {
    public static func register(token: Secret, vault: Vault, settings url: URL) async throws -> SlackSettings {
        try await register(token: token, vault: vault, settings: url, connect: { try SlackAPI.connect($0, timeout: $1) })
    }

    static func register(token: Secret, vault: Vault, settings url: URL, connect: SlackAPI.Connect) async throws -> SlackSettings {
        guard token.isSlackUserToken else { throw SlackRefusal.notAUser }
        let identity: SlackIdentity
        do {
            let api = SlackAPI(client: try connect(token, SlackAPI.actionTimeout), workspace: "", rates: SlackRateLimits(),
                               now: { Date() })
            let auth = try await api.account()
            identity = auth.identity
            let header = auth.scopes?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
            guard !header.isEmpty else { throw SlackRefusal.missingScope(nil) }
            let missing = SlackScopes.missing(from: header)
            guard missing.isEmpty else { throw SlackRefusal.missingScope(missing.joined(separator: ", ")) }
        } catch let refusal as SlackRefusal {
            throw refusal
        } catch {
            throw SlackRefusal.unavailable
        }
        var settings = try AppSettings.load(url)
        if let existing = settings.slack {
            guard existing.workspace == identity.team_id else { throw SlackRefusal.workspaceMismatch }
            guard existing.userID == identity.user_id else { throw SlackRefusal.accountMismatch }
        }
        let slack = SlackSettings(workspace: identity.team_id, userID: identity.user_id, team: identity.team, user: identity.user)
        try vault.store(token, for: slack.binding)
        settings.slack = slack
        try settings.save(url)
        return slack
    }

    public static func remove(vault: Vault, settings url: URL) throws {
        var settings = try AppSettings.load(url)
        if let slack = settings.slack { try vault.remove(slack.binding) }
        settings.slack = nil
        try settings.save(url)
    }
}
