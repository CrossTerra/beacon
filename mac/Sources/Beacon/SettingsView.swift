import BeaconCore
import SwiftUI

/// Settings for the accounts and Signal connection. Credential fields are secure and cleared once used.
struct SettingsView: View {
    @ObservedObject var service: ApprovalService
    @Environment(\.openURL) private var openURL
    @State private var origin = "https://api.github.com"
    @State private var token = ""
    @State private var message = ""
    @State private var saving = false
    @State private var setupCode = ""
    @State private var signalMessage = ""
    @State private var slackToken = ""
    @State private var slackMessage = ""

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
            Section("Slack") {
                if let slack = service.slack {
                    LabeledContent("Workspace", value: visibleText("\(slack.team) (\(slack.workspace))"))
                    LabeledContent("User", value: visibleText("\(slack.user) (\(slack.userID))"))
                    Button("Remove", role: .destructive) { slackMessage = service.removeSlack() }.disabled(saving)
                }
                Text("In your Slack app’s OAuth & Permissions page, add User Token Scopes, install or reinstall the app in your workspace, and copy the User OAuth Token (xoxp-…). Beacon posts as you.")
                    .font(.caption).foregroundStyle(.secondary)
                Text("User scopes: chat:write, channels:read, groups:read, im:read, mpim:read, users:read. For thread previews and lost replies, also add channels:history, groups:history, im:history and mpim:history for the conversations you use.")
                    .font(.caption).foregroundStyle(.secondary)
                Button("Open Slack apps") { openURL(URL(string: "https://api.slack.com/apps")!) }
                SecureField("User OAuth Token", text: $slackToken)
                Button(service.slack == nil ? "Save" : "Replace token") {
                    let value = slackToken
                    slackToken = ""
                    saving = true
                    Task {
                        slackMessage = await service.registerSlack(token: value)
                        saving = false
                    }
                }
                .disabled(slackToken.isEmpty || saving)
                if !slackMessage.isEmpty { Text(visibleText(slackMessage)).font(.caption) }
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
