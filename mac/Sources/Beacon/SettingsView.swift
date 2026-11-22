import BeaconCore
import SwiftUI

/// Settings: the GitHub account Beacon merges as, and the Signal it records to, from Signal's setup code. The token and
/// code fields are secure and cleared once used; neither is shown again.
struct SettingsView: View {
    @ObservedObject var service: ApprovalService
    @Environment(\.openURL) private var openURL
    @State private var origin = "https://api.github.com"
    @State private var token = ""
    @State private var message = ""
    @State private var saving = false
    @State private var setupCode = ""
    @State private var signalMessage = ""

    var body: some View {
        Form {
            Section("GitHub") {
                if let github = service.github {
                    LabeledContent("Account", value: "\(github.login) (\(github.account))")
                    LabeledContent("Origin", value: github.origin)
                    Button("Remove", role: .destructive) { service.removeGitHub() }
                }
                TextField("API origin", text: $origin)
                Text("Create a classic token for your own account with the repo scope, then paste it here. Beacon merges as you.")
                    .font(.caption).foregroundStyle(.secondary)
                Button("Create a token on GitHub") { openURL(GitHubSetup.newTokenURL(apiOrigin: origin)) }
                Text("If your organization uses SAML single sign-on, click Configure SSO → Authorize next to the new token.")
                    .font(.caption).foregroundStyle(.secondary)
                DisclosureGroup("Or a fine-grained token") {
                    Text("""
                        Resource owner: the organization that owns the repositories, with access to them. Repository \
                        permissions: Metadata read; Contents read and write; Pull requests read and write; Checks read; \
                        Commit statuses read; Administration read (branch protection). The organization may need to \
                        approve the token first.
                        """)
                        .font(.caption).foregroundStyle(.secondary)
                }
                SecureField("Personal access token", text: $token)
                Button(service.github == nil ? "Save" : "Replace token") {
                    let value = token
                    token = ""
                    saving = true
                    Task {
                        message = await service.registerGitHub(token: value, origin: origin)
                        saving = false
                    }
                }
                .disabled(token.isEmpty || saving)
                if !message.isEmpty { Text(message).font(.caption) }
            }
            Section("Signal") {
                if let signal = service.signal {
                    Text(signal.connected)
                    LabeledContent("App actor", value: "\(signal.handle) (\(signal.actor))")
                    if !signal.usable {
                        Text("These settings name the app as the person. Paste the setup code again.")
                            .font(.caption).foregroundStyle(.red)
                    }
                    Button("Remove", role: .destructive) { service.removeSignal() }
                }
                SecureField("Beacon setup code", text: $setupCode)
                Text("From Signal's /beacon page. It holds the app's key, which goes to the vault.")
                    .font(.caption).foregroundStyle(.secondary)
                Button(service.signal == nil ? "Connect" : "Replace") {
                    let value = setupCode
                    setupCode = ""
                    saving = true
                    Task {
                        signalMessage = await service.registerSignal(code: value)
                        saving = false
                    }
                }
                .disabled(setupCode.isEmpty || saving)
                if !signalMessage.isEmpty { Text(signalMessage).font(.caption) }
                Text(service.delivery).font(.caption)
                Button("Resume delivery") { service.resumeDelivery() }
                    .disabled(service.signal == nil)
            }
            Text(service.status).font(.caption).foregroundStyle(.secondary)
        }
        .formStyle(.grouped)
        .frame(minWidth: 480, minHeight: 460)
    }
}
