import Foundation

/// `slack.post` (13.00, _Slack at the write_). Only a started approval can write, and only once. A missing or
/// ambiguous reply is reconciled read-only, including on relaunch.
public final class PostHandler: ActionResolver, Sendable {
    private let journal: Journal
    private let settings: @Sendable () -> SlackSettings?
    private let connect: @Sendable (SlackSettings) throws -> ServiceClient
    private let budget: ReadBudget
    private let now: @Sendable () -> Date
    private let rates = SlackRateLimits()
    private let reconciling = Locked<Set<String>>([])

    public convenience init(journal: Journal, settings: @escaping @Sendable () -> SlackSettings?, vault: Vault,
                            budget: ReadBudget = ReadBudget()) {
        self.init(journal: journal, settings: settings, vault: vault, budget: budget, now: { Date() }, connect: SlackAPI.connect)
    }

    init(journal: Journal, settings: @escaping @Sendable () -> SlackSettings?, vault: Vault, budget: ReadBudget = ReadBudget(),
         now: @escaping @Sendable () -> Date, connect: @escaping SlackAPI.Connect) {
        self.journal = journal
        self.settings = settings
        self.budget = budget
        self.now = now
        self.connect = { settings in
            let token = try vault.credential(for: settings.binding)
            guard token.isSlackUserToken else { throw SlackRefusal.notAUser }
            return try connect(token)
        }
    }

    /// Signal requires the same subject on every record, and may receive `proposed` before the window opens.
    /// Do the minimum lookup before journaling it. The request's optional context is never a record's authority.
    public func place(_ request: ProposalRequest) async throws -> SlackPlacement {
        guard request.type == .slackPost else { throw NoResolver.Unsupported() }
        let draft = try SlackDraft(request.params)
        let settings = try configured(for: draft)
        return try await preflight(settings) { api in
            _ = try await self.identify(api, settings)
            let info = try await api.conversation(draft.channel)
            guard let visibility = info.visibility else { throw SlackRefusal.visibilityUnknown }
            try self.requireCurrent(settings)
            return SlackPlacement(account: settings.account, visibility: visibility)
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
        return try await preflight(settings) { api in
            try api.canPost()
            let (identity, conversation, parent) = try await self.read(api, draft, settings, placement)
            let label: String
            if placement.visibility == .dm {
                guard let other = conversation.user else { throw SlackRefusal.unavailable }
                label = "DM with \(try await api.userName(other)) (\(other))"
            } else if let name = conversation.name, !name.isEmpty {
                label = placement.visibility == .groupDM ? name : "#\(name)"
            } else if placement.visibility == .groupDM {
                label = "Group DM"
            } else {
                throw SlackRefusal.unavailable
            }
            try self.requireCurrent(settings)
            var facts = ["Workspace": "\(identity.team) (\(identity.team_id))", "Visibility": placement.visibility.rawValue]
            if let thread = draft.thread, let parent {
                let firstLine = parent.text?.prefix(while: { !$0.isNewline }) ?? ""
                facts["Thread"] = "\(thread) · \(firstLine.isEmpty ? "(no text on first line)" : String(firstLine))"
            }
            return ResolvedAction(origin: SlackSettings.origin, account: settings.account, destination: draft.destination,
                                  facts: facts, readAt: at, accountLabel: "\(identity.user) (\(identity.user_id))",
                                  destinationLabel: "\(label) (\(draft.channel))")
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

    private func api(_ settings: SlackSettings) throws -> SlackAPI {
        SlackAPI(client: try connect(settings), workspace: settings.workspace, rates: rates, now: now)
    }

    private func preflight<T>(_ settings: SlackSettings, _ work: (SlackAPI) async throws -> T) async throws -> T {
        do { try await budget.enter() } catch { throw SlackRefusal.busy }
        do {
            try Task.checkCancellation()
            var api = try api(settings)
            let budget = self.budget, now = self.now
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

    private func identify(_ api: SlackAPI, _ settings: SlackSettings) async throws -> SlackIdentity {
        let identity = try await api.identity()
        guard identity.user_id == settings.userID else { throw SlackRefusal.accountMismatch }
        guard identity.team_id == settings.workspace else { throw SlackRefusal.workspaceMismatch }
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
            api = try self.api(settings)
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

    /// Match an echoed UUID AND exact text, from the approved user and destination, around the original start.
    /// Do not assume Slack retains `client_msg_id`, or infer anything from a missing message or matching text alone.
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
        let ts: String
        do {
            let api = try api(settings)
            _ = try await identify(api, settings)
            let info = try await api.conversation(draft.channel)
            guard info.visibility == placement.visibility else { return .unknown }
            try await api.requireMember(info, user: settings.userID)
            guard let match = try await find(api, draft, user: settings.userID, marker: marker, started: started) else { return .unknown }
            try requireCurrent(settings)
            ts = match
        } catch {
            return .unknown
        }
        return try executed(id, draft.channel, ts, by: "read_back")
    }

    public func unknown() -> [String] {
        journal.all().filter { $0.type == .slackPost && $0.status == .outcomeUnknown }.map(\.id)
    }

    private func find(_ api: SlackAPI, _ draft: SlackDraft, user: String, marker: String, started: Date) async throws -> String? {
        // Include Slack/Mac clock skew and the final reads, but never search all history or drift with relaunch time.
        let oldest = floor(started.timeIntervalSince1970 - 60)
        let latest = ceil(min(now().timeIntervalSince1970 + 60, started.timeIntervalSince1970 + 300))
        guard oldest <= latest else { return nil }
        var cursor: String?
        var found: String?
        for _ in 0..<4 {
            let page = try await api.messages(channel: draft.channel, thread: draft.thread, limit: 15,
                                              oldest: "\(Int64(oldest)).000000", latest: "\(Int64(latest)).000000", cursor: cursor)
            for message in page.messages where message.client_msg_id == marker {
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
        try await register(token: token, vault: vault, settings: url, connect: SlackAPI.connect)
    }

    static func register(token: Secret, vault: Vault, settings url: URL, connect: SlackAPI.Connect) async throws -> SlackSettings {
        guard token.isSlackUserToken else { throw SlackRefusal.notAUser }
        let identity: SlackIdentity
        do {
            let api = SlackAPI(client: try connect(token), workspace: "", rates: SlackRateLimits(), now: { Date() })
            identity = try await api.identity()
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
