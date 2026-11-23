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
        do {
            defer { close(pair[1]) }
            // Each synchronous send must leave a whole line readable immediately; never wait for missing bytes.
            try #require(session.launch("run_a1"))
            expectCommand("L run_a1\n", on: pair[1])
            try #require(session.acknowledgeLaunch("run_a1"))
            expectCommand("A run_a1\n", on: pair[1])
        }
        // tether is gone.
        #expect(!session.launch("run_a2"))
        #expect(!session.acknowledgeLaunch("run_a1"))
        #expect(!session.stopRun("run_a1"))
        #expect(!session.stop("run_a1"))
        _ = events
    }

    private func expectCommand(_ command: String, on fd: Int32) {
        var buffer = [UInt8](repeating: 0, count: 64)
        let n = recv(fd, &buffer, buffer.count, MSG_DONTWAIT)
        #expect(String(decoding: buffer.prefix(max(n, 0)), as: UTF8.self) == command)
    }
}
