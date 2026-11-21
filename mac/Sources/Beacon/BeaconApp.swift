import AppKit
import SwiftUI

@main
struct BeaconApp: App {
    @NSApplicationDelegateAdaptor(BeaconDelegate.self) private var delegate

    var body: some Scene {
        MenuBarExtra("Beacon · 0") {
            BeaconMenu(delegate: delegate)
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
        Divider()
        Text(delegate.servicesStatus)
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
    private var lifetime = AppLifetime.recovering([:])
    private var awaitingQuit = false

    private lazy var approvalsWindow: NSWindow = {
        let window = NSWindow(contentViewController: NSHostingController(rootView: ApprovalsView()))
        window.title = "Beacon Approvals"
        window.isReleasedWhenClosed = false
        window.setContentSize(NSSize(width: 480, height: 260))
        window.center()
        return window
    }()

    func applicationDidFinishLaunching(_ notification: Notification) {
        guard !lifetime.stopping else { return }
        shortcut = ApprovalShortcut { [weak self] in self?.showApprovals() }
        shortcutAvailable = shortcut != nil
        services = TetherSession.open { [weak self] event in
            self?.servicesChanged(event)
        }
        servicesStatus = services == nil ? "Services unavailable" : "Starting services…"
    }

    private func servicesChanged(_ event: TetherSession.Event) {
        switch event {
        case .active:
            if !lifetime.stopping { servicesStatus = "Collector running" }
            return
        case .legacyCollectorInstalled:
            if !lifetime.stopping { servicesStatus = "Collector not started: run beacon service uninstall" }
            return
        case .busy:
            servicesStatus = "Another Beacon session is running"
        case .stopped(let verified):
            servicesStatus = verified ? "Services stopped" : "Services stopped without confirmation"
        }
        services = nil
        if awaitingQuit {
            awaitingQuit = false
            NSApp.reply(toApplicationShouldTerminate: true)
        }
    }

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        lifetime.beginShutdown()
        shortcut = nil
        guard let services else { return .terminateNow }
        awaitingQuit = true
        servicesStatus = "Stopping services…"
        services.stop()
        return .terminateLater
    }

    func applicationWillTerminate(_ notification: Notification) {
        shortcut = nil
    }

    func showApprovals() {
        guard !lifetime.stopping else { return }
        NSApp.activate()
        approvalsWindow.makeKeyAndOrderFront(nil)
    }
}

private struct ApprovalsView: View {
    var body: some View {
        ContentUnavailableView(
            "No pending approvals",
            systemImage: "checkmark.shield",
            description: Text("This build does not accept or execute proposals.")
        )
        .frame(minWidth: 420, minHeight: 220)
    }
}
