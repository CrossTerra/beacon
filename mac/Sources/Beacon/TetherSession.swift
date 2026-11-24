import Darwin
import Dispatch
import Foundation
import BeaconCore
import TetherSupport

/// Owns the app's only end of the supervisor lease, on the same actor as Quit.
@MainActor
final class TetherSession: RunLauncher {
    enum Event: Equatable {
        case active, legacyCollectorInstalled, busy
        case collectorRunning, collectorRestarting, collectorExited
        /// Cleanup ended. `verified` is false when the helper closed the lease without confirming it.
        case stopped(verified: Bool)
        /// A run's group leader is suspended until Beacon acknowledges its group.
        case runPrepared(id: String, pid: Int32)
        /// A run's process started, as the leader of its own group.
        case runStarted(id: String, pid: Int32)
        /// A run could not start; nothing of it runs.
        case runNotStarted(id: String, errno: Int32)
        /// A run's process exited: its code, or 128 plus the signal.
        case runExited(id: String, status: Int32)
        /// Every process of a run is gone.
        case runGone(id: String)
    }

    private let fd: Int32
    private let reader: TetherLeaseReader
    private let event: (Event) -> Void
    private var stopping = false

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

    /// Takes ownership of the app's end of the lease. Writing to it never raises SIGPIPE: if tether has closed its
    /// end, a command fails with EPIPE instead of killing Beacon.
    init(fd: Int32, event: @escaping (Event) -> Void) {
        var one: Int32 = 1
        _ = setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))
        self.fd = fd
        self.event = event
        let box = TetherSessionBox()
        // The read source is not on the main queue. Quit's modal loop does not drain that queue when terminate
        // was entered from a main-queue callback; the run loop still runs, including in the modal mode.
        reader = TetherLeaseReader(fd: fd) { event in
            RunLoop.main.perform(inModes: [.default, .common, .modalPanel]) {
                MainActor.assumeIsolated { box.session?.event(event) }
            }
        }
        box.session = self
        reader.start()
    }

    /// The longest Quit waits for confirmation: tether's grace period plus its give-up margin, and a little more.
    static let stopTimeout: TimeInterval = 15 + 5 + 3

    func stop() {
        // Close only the write half. The read half waits for the verified-clean acknowledgement.
        stopping = true
        _ = shutdown(fd, SHUT_WR)
    }

    /// Asks tether to start the run described in its `runs/<id>/` directory. False if the request could not be sent.
    func launch(_ id: String) -> Bool { command("L", id) }

    func acknowledgeLaunch(_ id: String) -> Bool { command("A", id) }

    /// Asks tether to stop the run: TERM to its group, KILL after the grace period.
    @discardableResult
    func stopRun(_ id: String) -> Bool { command("K", id) }

    @discardableResult
    func stop(_ runnerID: String) -> Bool { stopRun(runnerID) }

    /// One whole command line, or false: an invalid id, Quit begun, or a lease tether has closed (EPIPE).
    private func command(_ verb: String, _ id: String) -> Bool {
        guard !stopping, (1...40).contains(id.utf8.count),
              id.utf8.allSatisfy({ (0x61...0x7a).contains($0) || (0x30...0x39).contains($0) || $0 == 0x5f })
        else { return false }
        let bytes = Array("\(verb) \(id)\n".utf8)
        var sent = 0
        while sent < bytes.count {
            let n = bytes.withUnsafeBytes { send(fd, $0.baseAddress! + sent, $0.count - sent, 0) }
            if n < 0 && errno == EINTR { continue }
            guard n > 0 else { return false }
            sent += n
        }
        return true
    }

}

/// Holds the session until `init` has finished, so the reader's first event can find it.
private final class TetherSessionBox: @unchecked Sendable {
    weak var session: TetherSession?
}

/// Reads the supervisor lease off the main queue and delivers each event on the main run loop.
private final class TetherLeaseReader: @unchecked Sendable {
    private let source: DispatchSourceRead
    private let deliver: @Sendable (TetherSession.Event) -> Void
    private var line: [UInt8]?

    init(fd: Int32, deliver: @escaping @Sendable (TetherSession.Event) -> Void) {
        self.deliver = deliver
        source = DispatchSource.makeReadSource(fileDescriptor: fd, queue: DispatchQueue(label: "dev.beacon.tether.read"))
        source.setCancelHandler { close(fd) }
        source.setEventHandler { [weak self] in self?.receive(fd) }
    }

    func start() { source.activate() }

    private func receive(_ fd: Int32) {
        var byte: UInt8 = 0
        while true {
            let size = recv(fd, &byte, 1, MSG_DONTWAIT)
            if size < 0 && (errno == EAGAIN || errno == EINTR) { return }
            guard size == 1 else { return end(nil) }
            if line != nil {
                if byte == UInt8(ascii: "\n") {
                    runLine(String(decoding: line!, as: UTF8.self))
                    line = nil
                } else if line!.count < 256 {
                    line!.append(byte)
                }
                continue
            }
            switch byte {
            case UInt8(ascii: "#"): line = []
            case UInt8(ascii: "A"): deliver(.active)
            case UInt8(ascii: "L"): deliver(.legacyCollectorInstalled)
            case UInt8(ascii: "R"): deliver(.collectorRunning)
            case UInt8(ascii: "W"): deliver(.collectorRestarting)
            case UInt8(ascii: "X"): deliver(.collectorExited)
            default: return end(byte)
            }
        }
    }

    private func end(_ byte: UInt8?) {
        source.cancel()
        if byte == UInt8(ascii: "B") {
            deliver(.busy)
        } else {
            deliver(.stopped(verified: byte == UInt8(ascii: "S")))
        }
    }

    private func runLine(_ text: String) {
        let words = text.split(separator: " ").map(String.init)
        guard words.count >= 2 else { return }
        let id = words[1], value = words.count > 2 ? Int32(words[2]) : nil
        switch (words[0], value) {
        case ("prepared", let pid?): deliver(.runPrepared(id: id, pid: pid))
        case ("started", let pid?): deliver(.runStarted(id: id, pid: pid))
        case ("failed", let code?): deliver(.runNotStarted(id: id, errno: code))
        case ("exited", let status?): deliver(.runExited(id: id, status: status))
        case ("gone", _): deliver(.runGone(id: id))
        default: break
        }
    }
}
