/// What Beacon's menu-bar item shows: an icon, with the pending count beside it only when there is one.
public enum MenuBarItem {
    /// The SF Symbol: filled while proposals wait for the person.
    public static func symbol(pending: Int) -> String {
        pending > 0 ? "light.beacon.max.fill" : "light.beacon.max"
    }

    /// The number beside the icon, or nil when nothing is pending. Capped so the item stays narrow.
    public static func badge(pending: Int) -> String? {
        guard pending > 0 else { return nil }
        return pending > 99 ? "99+" : String(pending)
    }

    /// What VoiceOver says for the item.
    public static func accessibilityLabel(pending: Int) -> String {
        switch pending {
        case ..<1: "Beacon, no pending approvals"
        case 1: "Beacon, 1 pending approval"
        default: "Beacon, \(pending) pending approvals"
        }
    }
}
