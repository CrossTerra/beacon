import Darwin
import Testing

/// Reap an owned child without blocking a test or its cleanup. False means the child has not exited yet.
func reapFixtureChild(_ pid: pid_t, seconds: Int, status: UnsafeMutablePointer<Int32>? = nil) -> Bool {
    let deadline = ContinuousClock.now + .seconds(seconds)
    repeat {
        let reaped = waitpid(pid, status, WNOHANG)
        if reaped == pid { return true }
        if reaped == -1 && errno != EINTR {
            Issue.record("could not reap fixture child (errno \(errno))")
            return true // Do not signal a pid we cannot wait on.
        }
        if ContinuousClock.now >= deadline { return false }
        usleep(20_000)
    } while true
}
