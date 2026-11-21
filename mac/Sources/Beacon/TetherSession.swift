import Darwin
import Dispatch
import Foundation
import TetherSupport

/// Owns the app's only end of the supervisor lease, on the same actor as Quit.
@MainActor
final class TetherSession {
    enum Event: Equatable {
        case active, legacyCollectorInstalled, busy
        /// Cleanup ended. `verified` is false when the helper closed the lease without confirming it.
        case stopped(verified: Bool)
    }

    private let fd: Int32
    private let source: any DispatchSourceRead
    private let event: (Event) -> Void

    static func open(event: @escaping (Event) -> Void) -> TetherSession? {
        let helper = Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/tether").path
        guard FileManager.default.isExecutableFile(atPath: helper) else { return nil }
        var pid: pid_t = 0
        let fd = tether_open_session(helper, &pid)
        guard fd >= 0 else { return nil }
        let child = pid
        DispatchQueue.global().async {
            // Reap the helper. Cleanup does not depend on this wait: the helper watches the lease.
            while waitpid(child, nil, 0) < 0 && errno == EINTR {}
        }
        return TetherSession(fd: fd, event: event)
    }

    private init(fd: Int32, event: @escaping (Event) -> Void) {
        self.fd = fd
        self.event = event
        source = DispatchSource.makeReadSource(fileDescriptor: fd, queue: .main)
        source.setEventHandler { [weak self] in
            MainActor.assumeIsolated { self?.receive() }
        }
        source.setCancelHandler { close(fd) }
        source.activate()
    }

    func stop() {
        // Close only the write half. The read half waits for the verified-clean acknowledgement.
        _ = shutdown(fd, SHUT_WR)
    }

    private func receive() {
        var byte: UInt8 = 0
        let size = recv(fd, &byte, 1, MSG_DONTWAIT)
        if size < 0 && (errno == EAGAIN || errno == EINTR) { return }
        switch size == 1 ? byte : 0 {
        case UInt8(ascii: "A"): event(.active)
        case UInt8(ascii: "L"): event(.legacyCollectorInstalled)
        default:
            source.cancel()
            if size == 1 && byte == UInt8(ascii: "B") {
                event(.busy)
            } else {
                event(.stopped(verified: size == 1 && byte == UInt8(ascii: "S")))
            }
        }
    }
}
