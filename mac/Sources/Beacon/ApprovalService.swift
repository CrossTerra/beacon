import BeaconCore
import Foundation

/// Opens the vault, the journal and the proposal socket at launch, and closes them first at Quit. Each needs the
/// app signed as `dev.beacon.app` with a certificate; an unsigned or ad-hoc build opens none of them.
@MainActor
final class ApprovalService: ObservableObject {
    @Published private(set) var status = "Approvals are not available"
    @Published private(set) var github: GitHubSettings?
    private(set) var desk: ApprovalDesk?
    private var server: ProposalServer?
    private var vault: Vault?
    private var merges: MergeHandler?
    private var reconciler: Timer?
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
            github = try AppSettings.load(paths.settings).github
            let journal = try Journal(path: paths.journal.path, at: Date())
            let settingsURL = paths.settings
            let merges = MergeHandler(journal: journal, settings: { (try? AppSettings.load(settingsURL))?.github },
                                      connect: MergeHandler.connect(vault: vault))
            self.merges = merges
            let desk = ApprovalDesk(journal: journal, resolver: merges)
            desk.onApproved = { id in
                Task { _ = try? await merges.execute(id) }
            }
            self.desk = desk
            // Starts found by recovery, and lost replies, are settled by reading back, never by merging again.
            reconcile()
            reconciler = Timer.scheduledTimer(withTimeInterval: 60, repeats: true) { [weak self] _ in
                MainActor.assumeIsolated { self?.reconcile() }
            }
            guard try ClientTrust.ensure(paths.act, requirement: requirement) != .mismatch else {
                status = "Not accepting proposals: \(paths.act.path) names another app signature. Remove it to trust this build."
                return
            }
            let cli = bundle.bundleURL.appendingPathComponent("Contents/Helpers/beacon").path
            guard let client = ClientTrust.requirement(of: cli) else {
                status = "Not accepting proposals: the bundled beacon CLI is not signed"
                return
            }
            let server = ProposalServer(path: paths.socket.path, clients: [AdmittedClient(name: "beacon-act", requirement: client)],
                                        book: journal, enabled: [])
            try server.start()
            self.server = server
            updateEnabled()
        } catch VaultError.existingKey {
            status = "Not opening the vault: a Keychain vault key from before the vault could not be removed (see docs/approvals.md)"
        } catch VaultError.keychain(let code) {
            status = "The vault's Keychain key is not available (\(code))"
        } catch {
            status = "Approvals are not available: \(error)"
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

    func removeGitHub() {
        guard let vault else { return }
        try? GitHubSetup.remove(vault: vault, settings: paths.settings)
        github = nil
        updateEnabled()
    }

    private func updateEnabled() {
        guard let server else { return }
        server.enabled = github == nil ? [] : [.githubMerge]
        status = github == nil ? "Accepting proposals (no action type enabled: add a GitHub account in Settings)"
            : "Accepting github.merge proposals"
    }

    private func reconcile() {
        guard let merges else { return }
        let desk = self.desk
        Task {
            for id in merges.unknown() { _ = try? await merges.reconcile(id) }
            await MainActor.run { desk?.refresh() }
        }
    }

    /// Quit: no approval from now on, approvals without a start expire, and the socket closes. A merge already
    /// started settles from its reply, or by read-back after the next launch.
    func stop() {
        reconciler?.invalidate()
        desk?.beginShutdown()
        server?.stop()
        server = nil
    }
}
