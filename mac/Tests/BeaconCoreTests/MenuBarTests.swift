import Testing
@testable import BeaconCore

struct MenuBarTests {
    @Test func noBadgeWhenNothingIsPending() {
        #expect(MenuBarItem.badge(pending: 0) == nil)
        #expect(MenuBarItem.badge(pending: -1) == nil)
        #expect(MenuBarItem.symbol(pending: 0) == "light.beacon.max")
        #expect(MenuBarItem.accessibilityLabel(pending: 0) == "Beacon, no pending approvals")
    }

    @Test func badgeShowsTheCountCapped() {
        #expect(MenuBarItem.badge(pending: 1) == "1")
        #expect(MenuBarItem.badge(pending: 99) == "99")
        #expect(MenuBarItem.badge(pending: 100) == "99+")
        #expect(MenuBarItem.symbol(pending: 3) == "light.beacon.max.fill")
        #expect(MenuBarItem.accessibilityLabel(pending: 1) == "Beacon, 1 pending approval")
        #expect(MenuBarItem.accessibilityLabel(pending: 3) == "Beacon, 3 pending approvals")
    }

    @Test func anUnhealthyCollectorIsBadged() {
        #expect(MenuBarItem.badge(pending: 0, unhealthy: true) == "!")
        #expect(MenuBarItem.badge(pending: 4, unhealthy: true) == "! 4")
        #expect(MenuBarItem.accessibilityLabel(pending: 0, unhealthy: true) == "Beacon, no pending approvals, collector needs attention")
    }
}
