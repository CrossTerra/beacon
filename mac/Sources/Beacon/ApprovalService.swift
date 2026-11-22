import BeaconCore
import Foundation

/// Opens the vault, the journal and the proposal socket at launch, and closes them first at Quit. Each needs the
/// app signed as `dev.beacon.app` with a certificate; an unsigned or ad-hoc build opens none of them.
@MainActor
final class ApprovalService: ObservableObject {
    @Published private(set) var status = "Approvals are not available"
    private(set) var desk: ApprovalDesk?
    private var server: ProposalServer?

    func start(paths: AppPaths = .standard, bundle: Bundle = .main) {
        guard let requirement = ClientTrust.ownRequirement() else {
            status = "Approvals need Beacon.app signed with your Apple Development certificate"
            return
        }
        do {
            try FileManager.default.createDirectory(at: paths.data, withIntermediateDirectories: true,
                                                    attributes: [.posixPermissions: 0o700])
            // Opening the vault creates or reads its Keychain key, which only this signed app reads.
            try Vault(url: paths.vault, key: KeychainVaultKey()).unlock()
            let journal = try Journal(path: paths.journal.path, at: Date())
            desk = ApprovalDesk(journal: journal, resolver: NoResolver())
            guard try ClientTrust.ensure(paths.act, requirement: requirement) != .mismatch else {
                status = "Not accepting proposals: \(paths.act.path) names another app signature. Remove it to trust this build."
                return
            }
            let cli = bundle.bundleURL.appendingPathComponent("Contents/Helpers/beacon").path
            guard let client = ClientTrust.requirement(of: cli) else {
                status = "Not accepting proposals: the bundled beacon CLI is not signed"
                return
            }
            // No action type is enabled until its handler exists (plan 13.00 part 3).
            let server = ProposalServer(path: paths.socket.path, clients: [AdmittedClient(name: "beacon-act", requirement: client)],
                                        book: journal, enabled: [])
            try server.start()
            self.server = server
            status = "Accepting proposals (no action type enabled yet)"
        } catch VaultError.keychain(let code) {
            status = "The vault's Keychain key is not available (\(code))"
        } catch {
            status = "Approvals are not available: \(error)"
        }
    }

    /// Quit: no approval from now on, approvals without a start expire, and the socket closes.
    func stop() {
        desk?.beginShutdown()
        server?.stop()
        server = nil
    }
}
