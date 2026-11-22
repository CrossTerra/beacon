import BeaconCore
import SwiftUI

/// Settings: the GitHub account Beacon merges as. The token field is secure, cleared once saved, and the token is
/// never shown again.
struct SettingsView: View {
    @ObservedObject var service: ApprovalService
    @State private var origin = "https://api.github.com"
    @State private var token = ""
    @State private var message = ""
    @State private var saving = false
    @State private var signalOrigin = ""
    @State private var person = ""
    @State private var appKey = ""
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
                    LabeledContent("App actor", value: "\(signal.handle) (\(signal.actor))")
                    LabeledContent("Person", value: signal.person)
                    LabeledContent("Origin", value: signal.origin)
                    Button("Remove", role: .destructive) { service.removeSignal() }
                }
                TextField("Signal origin, https://…", text: $signalOrigin)
                TextField("Person, act_…", text: $person)
                SecureField("App actor's key", text: $appKey)
                Button(service.signal == nil ? "Save" : "Replace key") {
                    let value = appKey
                    appKey = ""
                    saving = true
                    Task {
                        signalMessage = await service.registerSignal(key: value, origin: signalOrigin, person: person)
                        saving = false
                    }
                }
                .disabled(appKey.isEmpty || signalOrigin.isEmpty || person.isEmpty || saving)
                if !signalMessage.isEmpty { Text(signalMessage).font(.caption) }
                Text(service.delivery).font(.caption)
            }
            Text(service.status).font(.caption).foregroundStyle(.secondary)
        }
        .formStyle(.grouped)
        .frame(minWidth: 480, minHeight: 460)
    }
}
