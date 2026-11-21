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
        Button("Quit Beacon") {
            NSApp.terminate(nil)
        }
    }
}

@MainActor
final class BeaconDelegate: NSObject, NSApplicationDelegate, ObservableObject {
    @Published private(set) var shortcutAvailable = false
    private var shortcut: ApprovalShortcut?

    private lazy var approvalsWindow: NSWindow = {
        let window = NSWindow(contentViewController: NSHostingController(rootView: ApprovalsView()))
        window.title = "Beacon Approvals"
        window.isReleasedWhenClosed = false
        window.setContentSize(NSSize(width: 480, height: 260))
        window.center()
        return window
    }()

    func applicationDidFinishLaunching(_ notification: Notification) {
        shortcut = ApprovalShortcut { [weak self] in self?.showApprovals() }
        shortcutAvailable = shortcut != nil
    }

    func applicationWillTerminate(_ notification: Notification) {
        shortcut = nil
    }

    func showApprovals() {
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
