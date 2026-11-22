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
        Text("Beacon · \(delegate.pendingCount)")
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
        Divider()
        Text(delegate.servicesStatus)
        Text(delegate.approvals.status)
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
    private var countTimer: Timer?

    private lazy var approvalsWindow: NSWindow = {
        let window = NSWindow(contentViewController: NSHostingController(rootView: ApprovalsView(service: approvals)))
        window.title = "Beacon Approvals"
        window.isReleasedWhenClosed = false
        window.setContentSize(NSSize(width: 820, height: 520))
        window.center()
        return window
    }()

    func applicationDidFinishLaunching(_ notification: Notification) {
        guard !stopping else { return }
        // The journal's recovery expires every earlier approval without a start before anything is shown.
        approvals.start()
        countTimer = Timer.scheduledTimer(withTimeInterval: 5, repeats: true) { [weak self] _ in
            MainActor.assumeIsolated { self?.updateCount() }
        }
        updateCount()
        shortcut = ApprovalShortcut { [weak self] in self?.showApprovals() }
        shortcutAvailable = shortcut != nil
        services = TetherSession.open { [weak self] event in
            self?.servicesChanged(event)
        }
        servicesStatus = services == nil ? "Services unavailable" : "Starting services…"
    }

    private func servicesChanged(_ event: TetherSession.Event) {
        let running: String
        switch event {
        case .active: running = "Starting collector…"
        case .collectorRunning: running = "Collector running"
        case .collectorRestarting: running = "Collector stopped with an error; restarting soon"
        case .collectorExited: running = "Collector stopped"
        case .legacyCollectorInstalled:
            running = "Collector not started: run beacon service uninstall, then reopen Beacon"
        case .busy:
            servicesStatus = "Another Beacon session is running"
            finishServices()
            return
        case .stopped(let verified):
            servicesStatus = verified ? "Services stopped" : "Services stopped without confirmation"
            finishServices()
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

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        stopping = true
        approvals.stop()
        countTimer?.invalidate()
        shortcut = nil
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

    func showApprovals() {
        guard !stopping else { return }
        NSApp.activate()
        approvalsWindow.makeKeyAndOrderFront(nil)
    }
}
