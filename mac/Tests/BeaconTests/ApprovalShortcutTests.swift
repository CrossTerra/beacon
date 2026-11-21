import Carbon.HIToolbox
import Testing
@testable import Beacon

struct ApprovalShortcutTests {
    @Test func matchesOnlyTheRegisteredHotKey() throws {
        let registered = ApprovalShortcut.identifier
        #expect(try matches(registered))
        #expect(try !matches(EventHotKeyID(signature: registered.signature, id: registered.id + 1)))
        #expect(try !matches(EventHotKeyID(signature: registered.signature + 1, id: registered.id)))
        #expect(try !matches(nil))
    }

    // Construct a local event; the test never installs a system-wide hot key or opens a window.
    private func matches(_ id: EventHotKeyID?) throws -> Bool {
        var created: EventRef?
        try #require(CreateEvent(
            nil, OSType(kEventClassKeyboard), UInt32(kEventHotKeyPressed), 0, EventAttributes(), &created
        ) == noErr)
        let event = try #require(created)
        defer { ReleaseEvent(event) }
        if var id {
            try #require(SetEventParameter(
                event, EventParamName(kEventParamDirectObject), EventParamType(typeEventHotKeyID),
                MemoryLayout<EventHotKeyID>.size, &id
            ) == noErr)
        }
        return ApprovalShortcut.matches(event)
    }
}
