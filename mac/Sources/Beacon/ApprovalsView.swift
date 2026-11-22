import BeaconCore
import SwiftUI

/// The approval window: pending proposals on the left, oldest first; the selected action on the right, in
/// Beacon's words, with the proposer's words kept apart. Approve (or ⌘↩ in this window) is enabled only for the
/// frozen snapshot, and carries that snapshot's token.
struct ApprovalsView: View {
    @ObservedObject var service: ApprovalService
    let openSettings: () -> Void

    var body: some View {
        Group {
            if let desk = service.desk, let runs = service.runs {
                DeskView(desk: desk, runs: runs, status: service.status, openSettings: openSettings)
            } else {
                ContentUnavailableView {
                    Label("No approvals", systemImage: "checkmark.shield")
                } description: {
                    Text(service.status)
                } actions: {
                    SettingsButton(action: openSettings)
                }
            }
        }
        .frame(minWidth: 720, minHeight: 420)
    }
}

private struct DeskView: View {
    @ObservedObject var desk: ApprovalDesk
    @ObservedObject var runs: RunHandler
    let status: String
    let openSettings: () -> Void
    @State private var note = ""

    var body: some View {
        NavigationSplitView {
            List(desk.pending, id: \.id, selection: Binding(get: { desk.selection }, set: { desk.select($0) })) { proposal in
                VStack(alignment: .leading) {
                    Text(summary(proposal)).lineLimit(1)
                    Text(proposal.proposedAt.formatted(date: .abbreviated, time: .shortened)).font(.caption).foregroundStyle(.secondary)
                }
            }
            .overlay {
                if desk.pending.isEmpty { ContentUnavailableView("No pending approvals", systemImage: "checkmark.shield") }
            }
            .safeAreaInset(edge: .bottom) {
                VStack(alignment: .leading, spacing: 4) {
                    Text(status).font(.caption).foregroundStyle(.secondary)
                    SettingsButton(action: openSettings)
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(6)
            }
        } detail: {
            if let proposal = desk.selected {
                detail(proposal)
            } else {
                Text("Select a proposal").foregroundStyle(.secondary)
            }
        }
    }

    private func detail(_ proposal: Proposal) -> some View {
        let token = desk.frozen?.approval.id == proposal.id ? desk.frozen?.token : nil
        return ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                Text(summary(proposal)).font(.title3)
                Text("Status: \(proposal.status.rawValue) · expires \(proposal.expiresAt.formatted())")
                    .font(.caption).foregroundStyle(.secondary)
                if let outcome = desk.outcome(of: proposal) {
                    GroupBox("Result") {
                        VStack(alignment: .leading, spacing: 4) {
                            Text(visibleText(outcome.text)).font(.headline).textSelection(.enabled)
                            if let link = outcome.link { Link("Open the pull request on GitHub", destination: link) }
                            if runs.isRunning(proposal.id) {
                                Button("Stop the run", role: .destructive) { runs.stop(proposal.id) }
                            }
                        }.frame(maxWidth: .infinity, alignment: .leading)
                    }
                }
                if proposal.type == .agentRun, let words = desk.journal.runnerWords(proposal.id) {
                    GroupBox(words["report"] != nil ? "The run's report (its own words)" : "What the run said") {
                        ScrollView {
                            Text(visibleText(words["report"]?.stringValue ?? words["detail"]?.stringValue
                                    ?? words["error"]?.stringValue ?? ""))
                                .textSelection(.enabled).frame(maxWidth: .infinity, alignment: .leading)
                        }.frame(maxHeight: 320)
                    }
                }
                if proposal.type == .agentRun, proposal.status == .pending {
                    modelPicker(proposal)
                }
                if let frozen = desk.frozen, frozen.approval.id == proposal.id {
                    GroupBox("What Beacon will do") {
                        VStack(alignment: .leading, spacing: 4) {
                            row("Account", frozen.resolved.accountLabel ?? frozen.approval.account)
                            row("Origin", frozen.approval.origin ?? "this Mac")
                            row("Destination", frozen.resolved.destinationLabel ?? frozen.approval.destination.canonical)
                            ForEach(frozen.resolved.facts.sorted(by: { $0.key < $1.key }), id: \.key) { row($0.key, $0.value) }
                            row("Read at", frozen.resolved.readAt.formatted(date: .omitted, time: .standard))
                            if let link = frozen.resolved.link { Link("Open the pull request on GitHub", destination: link) }
                        }.frame(maxWidth: .infinity, alignment: .leading)
                    }
                } else if proposal.status == .pending {
                    Text(desk.problem ?? "Resolving…").foregroundStyle(.secondary)
                }
                GroupBox("The agent says (unverified)") {
                    VStack(alignment: .leading, spacing: 4) {
                        row("Agent", proposal.agent + (proposal.session.map { " · session \($0)" } ?? ""))
                        if let runner = proposal.runner {
                            row("From", "run \(runner), launched by Beacon (verified by its process group)")
                        }
                        row("Client", proposal.client + (proposal.clientPath.map { " · \($0)" } ?? ""))
                        Text(visibleText(proposal.reason)).textSelection(.enabled)
                    }.frame(maxWidth: .infinity, alignment: .leading)
                }
                if let text = proposal.params["text"]?.stringValue ?? proposal.params["prompt"]?.stringValue {
                    GroupBox(proposal.type == .agentRun ? "Prompt, in full (\(text.unicodeScalars.count) characters)" : "Message") {
                        ScrollView {
                            Text(visibleText(text)).font(.body.monospaced()).textSelection(.enabled)
                                .frame(maxWidth: .infinity, alignment: .leading)
                        }.frame(minHeight: 60, maxHeight: 360)
                    }
                }
                DisclosureGroup("Show exact parameters") {
                    VStack(alignment: .leading, spacing: 4) {
                        if let approval = desk.frozen?.approval.id == proposal.id ? desk.frozen!.approval : desk.journal.approval(proposal.id) {
                            row("Account", approval.account)
                            row("Destination", approval.destination.canonical)
                            row("Approval hash", approval.approvedHash)
                        }
                        Text(visibleText((desk.frozen?.approval.id == proposal.id ? desk.frozen!.approval.params : proposal.params).canonical))
                            .font(.caption.monospaced()).textSelection(.enabled)
                    }.frame(maxWidth: .infinity, alignment: .leading)
                }
                if proposal.status == .pending {
                    HStack {
                        Button("Read again") { desk.readAgain() }
                        Spacer()
                        TextField("Note for the agent (optional)", text: $note).frame(maxWidth: 260)
                        Button("Decline") {
                            desk.decline(note: note)
                            note = ""
                        }
                        Button("Approve") {
                            if let token { desk.approve(token) }
                        }
                        .keyboardShortcut(.return, modifiers: .command)
                        .disabled(token == nil || desk.stopping)
                    }
                }
                if let problem = desk.problem, desk.frozen == nil, proposal.status != .pending {
                    Text(problem).foregroundStyle(.red)
                }
            }
            .padding()
        }
    }

    /// The one field the person may change (D7): opencode's configured models, starting at the proposed one.
    private func modelPicker(_ proposal: Proposal) -> some View {
        let proposed = proposal.params["model"]?.stringValue ?? ""
        let choices = runs.models.contains(proposed) || runs.models.isEmpty ? runs.models : [proposed] + runs.models
        return Picker("Model", selection: Binding(get: { desk.model ?? proposed }, set: { desk.choose(model: $0) })) {
            ForEach(choices.isEmpty ? [proposed] : choices, id: \.self) { model in
                Text(model == proposed ? "\(model) (proposed)" : model).tag(model)
            }
        }
        .disabled(desk.stopping)
    }

    private func row(_ label: String, _ value: String) -> some View {
        HStack(alignment: .top) {
            Text(label).foregroundStyle(.secondary).frame(width: 110, alignment: .leading)
            Text(visibleText(value)).textSelection(.enabled)
        }
    }
}

/// Opens Beacon's Settings window; ⌘, as in other Mac apps.
private struct SettingsButton: View {
    let action: () -> Void

    var body: some View {
        Button("Settings…", action: action).keyboardShortcut(",", modifiers: .command)
    }
}

/// The action named in Beacon's own words.
func summary(_ proposal: Proposal) -> String {
    let p = proposal.params
    switch proposal.type {
    case .githubMerge:
        return "Merge \(p["repo"]?.stringValue ?? "") #\(p["number"]?.intValue ?? 0) into \(p["base"]?.stringValue ?? "") (\(p["method"]?.stringValue ?? ""))"
    case .slackPost:
        return "Post in Slack \(p["channel"]?.stringValue ?? "")" + (p["thread_ts"]?.stringValue.map { " thread \($0)" } ?? "")
    case .agentRun:
        return "Run opencode in \(p["checkout"]?.stringValue ?? "") with \(p["model"]?.stringValue ?? "")"
    }
}
