import Darwin
import Testing
@testable import Beacon

@MainActor
struct TetherSessionTests {
    /// Bugbot on #38: a command written after tether closed its end of the lease fails with EPIPE; it does not
    /// raise SIGPIPE, which would kill Beacon. SIGPIPE is at its default for the test, so a regression ends the run.
    @Test func aClosedLeaseFailsTheCommandInsteadOfKillingTheApp() throws {
        var pair: [Int32] = [-1, -1]
        try #require(socketpair(AF_UNIX, SOCK_STREAM, 0, &pair) == 0)
        let previous = signal(SIGPIPE, SIG_DFL)
        defer { signal(SIGPIPE, previous) }
        var events: [TetherSession.Event] = []
        let session = TetherSession(fd: pair[0]) { events.append($0) }
        // tether is alive: the command is written whole.
        #expect(session.launch("run_a1"))
        var buffer = [UInt8](repeating: 0, count: 64)
        let n = read(pair[1], &buffer, buffer.count)
        #expect(n > 0 && String(decoding: buffer[..<n], as: UTF8.self) == "L run_a1\n")
        #expect(session.acknowledgeLaunch("run_a1"))
        let ack = read(pair[1], &buffer, buffer.count)
        #expect(ack > 0 && String(decoding: buffer[..<ack], as: UTF8.self) == "A run_a1\n")
        // tether is gone.
        close(pair[1])
        #expect(!session.launch("run_a2"))
        #expect(!session.acknowledgeLaunch("run_a1"))
        #expect(!session.stopRun("run_a1"))
        #expect(!session.stop("run_a1"))
        _ = events
    }
}
