/// What Beacon's menu-bar item shows: an icon, with the pending count beside it only when there is one, and `!` when
/// the collector needs attention.
public enum MenuBarItem {
    /// The SF Symbol: filled while proposals wait for the person.
    public static func symbol(pending: Int) -> String {
        pending > 0 ? "light.beacon.max.fill" : "light.beacon.max"
    }

    /// The text beside the icon, or nil when nothing is pending and the collector is healthy. Capped so the item stays
    /// narrow.
    public static func badge(pending: Int, unhealthy: Bool = false) -> String? {
        let count: String? = pending > 0 ? (pending > 99 ? "99+" : String(pending)) : nil
        guard unhealthy else { return count }
        return count.map { "! \($0)" } ?? "!"
    }

    /// What VoiceOver says for the item.
    public static func accessibilityLabel(pending: Int, unhealthy: Bool = false) -> String {
        let approvals = switch pending {
        case ..<1: "Beacon, no pending approvals"
        case 1: "Beacon, 1 pending approval"
        default: "Beacon, \(pending) pending approvals"
        }
        return unhealthy ? approvals + ", collector needs attention" : approvals
    }
}
