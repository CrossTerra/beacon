import Testing
@testable import Beacon

struct AppLifetimeTests {
    @Test func quitAndVerifiedRecoveryExpireOnlyApprovalsWithoutAStart() {
        let fixture: [String: AppLifetime.Approval] = [
            "waiting": .pending, "approved": .approved, "started": .started, "expired": .expired,
        ]
        var quitting = AppLifetime(approvals: fixture)
        quitting.beginShutdown()
        quitting.beginShutdown()
        #expect(quitting.stopping)
        let recovered = AppLifetime.recovering(fixture)
        #expect(!recovered.stopping)
        #expect(recovered.approvals == quitting.approvals)
        #expect(recovered.approvals == ["waiting": .pending, "approved": .expired, "started": .started, "expired": .expired])
    }
}
