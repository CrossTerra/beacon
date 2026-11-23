import AppKit
import BeaconCore
import SwiftUI

@main
struct BeaconApp: App {
    @NSApplicationDelegateAdaptor(BeaconDelegate.self) private var delegate

    var body: some Scene {
        MenuBarExtra {
            BeaconMenu(delegate: delegate)
        } label: {
            MenuTitle(delegate: delegate)
        }
    }
}

private struct MenuTitle: View {
    @ObservedObject var delegate: BeaconDelegate

    var body: some View {
        // An icon, not a title: a crowded menu bar (or the notch) hides wide items first.
        let pending = delegate.pendingCount
        HStack(spacing: 2) {
            Image(systemName: MenuBarItem.symbol(pending: pending))
            if let badge = MenuBarItem.badge(pending: pending) {
                Text(badge).monospacedDigit()
            }
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(MenuBarItem.accessibilityLabel(pending: pending))
    }
}

private struct RunsLine: View {
    @ObservedObject var runs: RunHandler

    var body: some View {
        if !runs.running.isEmpty {
            Text(runs.running.count == 1 ? "1 agent run working" : "\(runs.running.count) agent runs working")
        }
    }
}

private struct BeaconMenu: View {
    @ObservedObject var delegate: BeaconDelegate

    var body: some View {
        Button(delegate.shortcutAvailable ? "Approvals… (⌥⌘B)" : "Approvals…") {
            delegate.showApprovals()
        }
        if !delegate.shortcutAvailable {
            Text("⌥⌘B unavailable")
        }
        Button("Settings…") {
            delegate.showSettings()
        }
        Divider()
        Text(delegate.servicesStatus)
        if let runs = delegate.approvals.runs { RunsLine(runs: runs) }
        Text(delegate.approvals.status)
        Text(delegate.approvals.delivery)
        Button("Resume delivery") { delegate.approvals.resumeDelivery() }
        Button("Quit Beacon") {
            NSApp.terminate(nil)
        }
    }
}

@MainActor
final class BeaconDelegate: NSObject, NSApplicationDelegate, ObservableObject {
    @Published private(set) var shortcutAvailable = false
    @Published private(set) var servicesStatus = "Services not installed"
    private var shortcut: ApprovalShortcut?
    private var services: TetherSession?
    @Published private(set) var pendingCount = 0
    let approvals = ApprovalService()
    private var stopping = false
    private var awaitingQuit = false
    /// The session started without the collector: the retired LaunchAgent is installed.
    private var collectorHeldBack = false
    private var countTimer: Timer?
    private var terminate: (any DispatchSourceSignal)?

    private lazy var approvalsWindow: NSWindow = {
        let window = NSWindow(contentViewController: NSHostingController(rootView: ApprovalsView(service: approvals) { [weak self] in
            self?.showSettings()
        }))
        window.title = "Beacon Approvals"
        window.isReleasedWhenClosed = false
        window.setContentSize(NSSize(width: 820, height: 520))
        window.center()
        return window
    }()

    func applicationDidFinishLaunching(_ notification: Notification) {
        guard !stopping else { return }
        // SIGTERM (scripts/install.sh replacing the app, logout) quits as the menu's Quit does: services stop and
        // approvals without a start expire, instead of the process just ending.
        signal(SIGTERM, SIG_IGN)
        let terminate = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .main)
        terminate.setEventHandler { NSApp.terminate(nil) }
        terminate.activate()
        self.terminate = terminate
        // The journal's recovery expires every earlier approval without a start before anything is shown.
        approvals.start()
        countTimer = Timer.scheduledTimer(withTimeInterval: 5, repeats: true) { [weak self] _ in
            MainActor.assumeIsolated { self?.updateCount() }
        }
        updateCount()
        shortcut = ApprovalShortcut { [weak self] in self?.showApprovals() }
        shortcutAvailable = shortcut != nil
        openServices()
    }

    private func openServices() {
        services = TetherSession.open { [weak self] event in
            self?.servicesChanged(event)
        }
        servicesStatus = services == nil ? "Services unavailable" : "Starting services…"
    }

    /// Restarts of tether after it ended on its own, at most three in ten minutes. A new session first stops every
    /// process the old one left (its marker sweep), runs included, then starts the collector again.
    private var restarts: [Date] = []

    private func restartServices() {
        restarts = restarts.filter { Date().timeIntervalSince($0) < 600 }
        guard !stopping, services == nil, restarts.count < 3 else { return }
        restarts.append(Date())
        openServices()
    }

    private func servicesChanged(_ event: TetherSession.Event) {
        let running: String
        let runs = approvals.runs
        switch event {
        case .active, .legacyCollectorInstalled:
            // Both indicate the startup sweep completed. Only the collector is held back by the old LaunchAgent.
            if let services { runs?.servicesReady(services) }
            collectorHeldBack = event == .legacyCollectorInstalled
            running = event == .active ? "Starting collector…"
                : "Collector not started: run beacon service uninstall, then reopen Beacon"
        case .collectorRunning where collectorHeldBack, .collectorRestarting where collectorHeldBack,
             .collectorExited where collectorHeldBack:
            // No collector runs in this session; keep saying why.
            return
        case .collectorRunning: running = "Collector running"
        case .collectorRestarting: running = "Collector stopped with an error; restarting soon"
        case .collectorExited: running = "Collector stopped"
        case .busy:
            servicesStatus = "Another Beacon session is running"
            finishServices()
            return
        case .stopped(let verified):
            servicesStatus = verified ? "Services stopped" : "Services stopped without confirmation"
            runs?.servicesStopped(verified: verified)
            finishServices()
            if !stopping {
                // tether ended on its own. A new session sweeps what it left (an unconfirmed run's processes too) and
                // starts the collector again.
                servicesStatus = "Services stopped; restarting them to stop anything left behind…"
                DispatchQueue.main.asyncAfter(deadline: .now() + 2) { [weak self] in self?.restartServices() }
            }
            return
        case .runPrepared(let id, let pid):
            runs?.prepared(id, pid: pid)
            return
        case .runStarted(let id, let pid):
            runs?.started(id, pid: pid)
            return
        case .runNotStarted(let id, let code):
            runs?.notStarted(id, errno: code)
            return
        case .runExited(let id, let status):
            runs?.exited(id, status: status)
            return
        case .runGone(let id):
            runs?.gone(id)
            return
        }
        if !stopping { servicesStatus = running }
    }

    private func finishServices() {
        services = nil
        if awaitingQuit {
            awaitingQuit = false
            NSApp.reply(toApplicationShouldTerminate: true)
        }
    }

    /// Opening Beacon.app again (Finder, Spotlight, `open`) while it runs shows the approval window.
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        showApprovals()
        return false
    }

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        stopping = true
        approvals.stop()
        countTimer?.invalidate()
        shortcut = nil
        if services == nil, approvals.runs?.hasUnclosedRuns == true {
            // tether is gone, but a run it could not confirm stopped may still be running: a session started only to
            // be stopped sweeps every process the last one left, then confirms.
            openServices()
        }
        guard let services else { return .terminateNow }
        awaitingQuit = true
        servicesStatus = "Stopping services…"
        services.stop()
        // Quit never hangs: tether gives up on its own, and this covers a helper that stops responding.
        DispatchQueue.main.asyncAfter(deadline: .now() + TetherSession.stopTimeout) { [weak self] in
            guard let self, self.awaitingQuit else { return }
            self.servicesStatus = "Services stopped without confirmation"
            self.finishServices()
        }
        return .terminateLater
    }

    func applicationWillTerminate(_ notification: Notification) {
        shortcut = nil
    }

    private func updateCount() {
        approvals.desk?.refresh()
        pendingCount = approvals.desk?.pending.count ?? 0
    }

    private lazy var settingsWindow: NSWindow = {
        let window = NSWindow(contentViewController: NSHostingController(rootView: SettingsView(service: approvals)))
        window.title = "Beacon Settings"
        window.isReleasedWhenClosed = false
        window.center()
        return window
    }()

    func showSettings() {
        guard !stopping else { return }
        NSApp.activate()
        settingsWindow.makeKeyAndOrderFront(nil)
    }

    func showApprovals() {
        guard !stopping else { return }
        NSApp.activate()
        approvalsWindow.makeKeyAndOrderFront(nil)
    }
}
