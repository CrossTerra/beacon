/// Part 2 has no proposal intake. The journal will supply these states when it is added;
/// `started` means a durable start, not a queued task or a provider reply.
struct AppLifetime {
    enum Approval: Equatable {
        case pending, approved, started, expired
    }

    private(set) var approvals: [String: Approval]
    private(set) var stopping = false

    init(approvals: [String: Approval] = [:]) {
        self.approvals = approvals
    }

    static func recovering(_ approvals: [String: Approval]) -> Self {
        var lifetime = Self(approvals: approvals)
        lifetime.expireUnstarted()
        return lifetime
    }

    mutating func beginShutdown() {
        stopping = true
        expireUnstarted()
    }

    private mutating func expireUnstarted() {
        approvals = approvals.mapValues { $0 == .approved ? .expired : $0 }
    }
}
