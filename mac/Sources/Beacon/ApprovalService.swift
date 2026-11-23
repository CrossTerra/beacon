import AppKit
import BeaconCore
import Foundation

/// Opens the vault, the journal and the proposal socket at launch, and closes them first at Quit. Each needs the
/// app signed as `dev.beacon.app` with a certificate; an unsigned or ad-hoc build opens none of them.
@MainActor
final class ApprovalService: ObservableObject {
    @Published private(set) var status = "Approvals are not available"
    @Published private(set) var github: GitHubSettings?
    @Published private(set) var slack: SlackSettings?
    @Published private(set) var signal: SignalSettings?
    @Published private(set) var delivery = "Records are not sent: add Signal in Settings"
    @Published private(set) var checkouts: [CheckoutSettings] = []
    /// The agent.run handler, once the journal is open.
    @Published private(set) var runs: RunHandler?
    private var journal: Journal?
    private var deliverer: Timer?
    private var scheduler: DeliveryScheduler?
    private(set) var desk: ApprovalDesk?
    private var server: ProposalServer?
    private var vault: Vault?
    private var merges: MergeHandler?
    private var posts: PostHandler?
    private var reconciler: Timer?
    private var readBack: Task<Void, Never>?
    private var postReadBack: Timer?
    private var postReading: Task<Void, Never>?
    private var paths = AppPaths.standard

    func start(paths: AppPaths = .standard, bundle: Bundle = .main) {
        self.paths = paths
        guard let requirement = ClientTrust.ownRequirement() else {
            status = "Approvals need Beacon.app signed with your Apple Development certificate"
            return
        }
        do {
            try FileManager.default.createDirectory(at: paths.data, withIntermediateDirectories: true,
                                                    attributes: [.posixPermissions: 0o700])
            // Opening the vault creates or reads its Keychain key, which only this signed app reads.
            let vault = Vault(url: paths.vault, key: KeychainVaultKey())
            try vault.unlock()
            self.vault = vault
            let settings = try AppSettings.load(paths.settings)
            github = settings.github
            slack = settings.slack
            signal = settings.signal
            checkouts = settings.checkouts ?? []
            let journal = try Journal(path: paths.journal.path, at: Date())
            self.journal = journal
            let settingsURL0 = paths.settings
            let scheduler = DeliveryScheduler(journal: journal, connect: {
                guard let signal = try AppSettings.load(settingsURL0).signal else { throw VaultError.notFound }
                guard signal.usable else { throw SignalSetup.Refusal.noPerson }
                return (try SignalRecords.connect(signal, vault: vault), signal.person, signal.personHandle)
            }, message: delivery)
            scheduler.onMessage = { [weak self] message in self?.delivery = message }
            self.scheduler = scheduler
            // Every new entry is sent in journal order; a timer resends after an outage.
            journal.observe { [weak self] _ in
                Task { @MainActor in self?.deliver() }
            }
            deliverer = Timer.scheduledTimer(withTimeInterval: 30, repeats: true) { [weak self] _ in
                MainActor.assumeIsolated { self?.deliver() }
            }
            let settingsURL = paths.settings
            let budget = ReadBudget()
            let merges = MergeHandler(journal: journal, settings: { (try? AppSettings.load(settingsURL))?.github },
                                      connect: MergeHandler.connect(vault: vault), budget: budget)
            let posts = PostHandler(journal: journal, settings: { (try? AppSettings.load(settingsURL))?.slack },
                                    vault: vault, budget: budget)
            self.merges = merges
            self.posts = posts
            let runs = RunHandler(journal: journal, checkouts: { (try? AppSettings.load(settingsURL))?.checkouts ?? [] },
                                  runs: paths.runs)
            self.runs = runs
            let desk = ApprovalDesk(journal: journal, resolver: ActionRouter(merges: merges, runs: runs, posts: posts),
                                    runServicesReady: { runs.isReady })
            runs.onAvailabilityChanged = { [weak desk] in desk?.runAvailabilityChanged() }
            desk.claim = { runs.claim($0) }
            desk.release = { runs.release($0) }
            desk.onApproved = { [weak journal] id in
                switch journal?.proposal(id, at: Date())?.type {
                case .agentRun: Task { try? await runs.execute(id) }
                case .githubMerge: Task { _ = try? await merges.execute(id) }
                case .slackPost: Task { _ = try? await posts.execute(id) }
                case nil: break
                }
            }
            // A run's unknown launch is settled by tether's recovery, not by Read again.
            desk.onReadBack = { [weak self] id in
                Task { @MainActor in
                    switch journal.proposal(id, at: Date())?.type {
                    case .githubMerge: _ = try? await merges.reconcile(id)
                    case .slackPost: _ = try? await posts.reconcile(id)
                    default: break
                    }
                    self?.desk?.refresh()
                }
            }
            self.desk = desk
            // Starts found by recovery, and lost replies, are settled by reading back, never by dispatch again.
            // GitHub reads back every minute; Slack on its own backoff, which ends after its search window.
            reconcile()
            readBackPosts()
            reconciler = Timer.scheduledTimer(withTimeInterval: 60, repeats: true) { [weak self] _ in
                MainActor.assumeIsolated { self?.reconcile() }
            }
            postReadBack = Timer.scheduledTimer(withTimeInterval: 5, repeats: true) { [weak self] _ in
                MainActor.assumeIsolated { self?.readBackPosts() }
            }
            guard try ClientTrust.ensure(paths.act, requirement: requirement) != .mismatch else {
                status = "Not accepting proposals: \(paths.act.path) names another app signature. Remove it to trust this build."
                return
            }
            let cli = bundle.bundleURL.appendingPathComponent("Contents/Helpers/beacon").path
            guard let clientRequirement = ClientTrust.requirement(of: cli) else {
                status = "Not accepting proposals: the bundled beacon CLI is not signed"
                return
            }
            let client = AdmittedClient(name: "beacon-act", requirement: clientRequirement)
            let server = ProposalServer(path: paths.socket.path, clients: [client],
                                        book: journal, enabled: [], attribution: runs.attribution,
                                        slackLookup: posts.place)
            // scripts/install.sh quits a running Beacon through the bundled CLI, as the menu's Quit does, without
            // asking for permission to control the app.
            server.quit = (client: client, action: {
                DispatchQueue.main.async { NSApplication.shared.terminate(nil) }
            })
            try server.start()
            self.server = server
            updateEnabled()
            deliver()
        } catch VaultError.existingKey {
            status = "Not opening the vault: a Keychain vault key from before the vault could not be removed (see docs/approvals.md)"
        } catch VaultError.keychain(let code) {
            status = "The vault's Keychain key is not available (\(code))"
        } catch {
            status = "Approvals are not available: \(error)"
        }
    }

    func registerSlack(token: String) async -> String {
        guard let vault else { return status }
        do {
            let saved = try await SlackSetup.register(token: Secret(token), vault: vault, settings: paths.settings)
            slack = saved
            if desk?.selected?.type == .slackPost { desk?.readAgain() }
            updateEnabled()
            return saved.connected
        } catch let refusal as SlackRefusal {
            return "Not saved: \(refusal.explanation)"
        } catch {
            return "Not saved: the settings or the vault could not be written."
        }
    }

    func removeSlack() -> String {
        guard let vault else { return status }
        do {
            try SlackSetup.remove(vault: vault, settings: paths.settings)
            slack = nil
            if desk?.selected?.type == .slackPost { desk?.readAgain() }
            updateEnabled()
            return "Slack removed."
        } catch {
            return "Could not remove Slack."
        }
    }

    /// Registers the GitHub token. It goes to the vault; this never shows or keeps it.
    func registerGitHub(token: String, origin: String) async -> String {
        guard let vault else { return status }
        do {
            github = try await GitHubSetup.register(token: Secret(token), origin: origin, vault: vault, settings: paths.settings)
            updateEnabled()
            return "Merges as \(github!.login)"
        } catch GitHubSetup.Refusal.notAUser {
            return "Not saved: that token is not a user's."
        } catch GitHubSetup.Refusal.otherUser(let login) {
            return "Not saved: Beacon is set up for \(login). Remove that account first."
        } catch GitHubSetup.Refusal.rejected {
            return "Not saved: GitHub refused the token."
        } catch {
            return "Not saved: GitHub could not be reached, or the origin is not https://host."
        }
    }

    /// Registers Signal from the setup code on Signal's /beacon page. The key in it goes to the vault; this never
    /// shows or keeps it.
    func registerSignal(code: String) async -> String {
        guard let vault else { return status }
        do {
            signal = try await SignalSetup.register(code: code, vault: vault, settings: paths.settings)
            // A new key or person may be what a stopped record needed.
            resumeDelivery()
            return signal!.connected
        } catch SignalSetup.Refusal.code(.prefix) {
            return "Not saved: that is not a Beacon setup code. Copy it from Signal's /beacon page."
        } catch SignalSetup.Refusal.code {
            return "Not saved: the setup code is damaged or incomplete. Copy it again from Signal's /beacon page."
        } catch SignalSetup.Refusal.notAnApp {
            return "Not saved: that key is not an active integration actor's."
        } catch SignalSetup.Refusal.notApprovalApp {
            return "Not saved: that actor is not marked as an approval app in Signal."
        } catch SignalSetup.Refusal.noPerson {
            return "Not saved: that app does not work for an active person in Signal."
        } catch SignalSetup.Refusal.scopes {
            return "Not saved: the app's key must have exactly read and write."
        } catch SignalSetup.Refusal.rejected {
            return "Not saved: Signal refused the key. Rotate it on Signal's /beacon page and paste the new code."
        } catch SignalSetup.Refusal.unreachable(let retryAfter?) {
            return "Not saved: Signal is busy or unavailable. Try again in \(retryAfter) seconds."
        } catch SignalSetup.Refusal.unreachable(nil) {
            return "Not saved: Signal could not be reached. Try again."
        } catch SignalSetup.Refusal.unexpected(let status) {
            return "Not saved: Signal answered \(status). Check the setup code, or try again."
        } catch {
            return "Not saved: the settings or the vault could not be written."
        }
    }

    func removeSignal() {
        guard let vault else { return }
        try? SignalSetup.remove(vault: vault, settings: paths.settings)
        signal = nil
        delivery = "Records are not sent: add Signal in Settings"
    }

    /// Sends the outbox in journal order, one drain at a time (see `DeliveryScheduler`).
    func deliver(force: Bool = false) {
        guard signal != nil else { return }
        scheduler?.deliver(force: force)
    }

    /// Resume: records stopped at a refusal go back to pending and are sent now, also when a drain is running.
    func resumeDelivery() {
        guard signal != nil else { return }
        scheduler?.resume()
    }

    func removeGitHub() {
        guard let vault else { return }
        try? GitHubSetup.remove(vault: vault, settings: paths.settings)
        github = nil
        updateEnabled()
    }

    /// Adds a checkout agent runs may use. Returns why not, or nil.
    func addCheckout(name: String, path: String) -> String? {
        do {
            var settings = try AppSettings.load(paths.settings)
            try settings.add(try CheckoutSettings.make(name: name, path: path))
            try settings.save(paths.settings)
            checkouts = settings.checkouts ?? []
            updateEnabled()
            return nil
        } catch CheckoutSettings.Refusal.name {
            return "A checkout name is lowercase letters, digits, '.', '_' and '-', starting with a letter or digit."
        } catch CheckoutSettings.Refusal.path {
            return "That is not an existing folder."
        } catch CheckoutSettings.Refusal.taken {
            return "Another checkout has that name."
        } catch {
            return "The settings could not be saved."
        }
    }

    func removeCheckout(_ name: String) {
        guard var settings = try? AppSettings.load(paths.settings) else { return }
        settings.removeCheckout(name)
        try? settings.save(paths.settings)
        checkouts = settings.checkouts ?? []
        updateEnabled()
    }

    private func updateEnabled() {
        guard let server else { return }
        var types: [ActionType] = []
        if github != nil { types.append(.githubMerge) }
        if slack != nil { types.append(.slackPost) }
        if !checkouts.isEmpty { types.append(.agentRun) }
        server.enabled = types
        status = types.isEmpty
            ? "Accepting proposals (no action type enabled: add a GitHub or Slack account, or a checkout, in Settings)"
            : "Accepting \(types.map(\.rawValue).joined(separator: ", ")) proposals"
    }

    private func reconcile() {
        guard readBack == nil, let merges else { return }
        readBack = Task { [weak self] in
            defer { self?.readBack = nil }
            for id in merges.unknown() where !Task.isCancelled { _ = try? await merges.reconcile(id) }
            self?.desk?.refresh()
        }
    }

    /// Slack read-back on its own task, so a queue of unknown posts never delays GitHub recovery. Reads only what
    /// the handler says is due; most ticks read nothing.
    private func readBackPosts() {
        guard postReading == nil, let posts, !posts.readBackDue().isEmpty else { return }
        postReading = Task { [weak self] in
            defer { self?.postReading = nil }
            await posts.automaticReadBack()
            self?.desk?.refresh()
        }
    }

    /// Quit: no approval from now on, approvals without a start expire, and the socket closes. A write already
    /// started settles from its reply, or by read-back after the next launch.
    func stop() {
        reconciler?.invalidate()
        readBack?.cancel()
        postReadBack?.invalidate()
        postReading?.cancel()
        deliverer?.invalidate()
        runs?.beginShutdown()
        desk?.beginShutdown()
        server?.stop()
        server = nil
    }
}
