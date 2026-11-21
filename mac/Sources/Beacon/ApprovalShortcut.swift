import Carbon.HIToolbox

/// Registers just ⌥⌘B, without monitoring keystrokes or requesting Accessibility access.
final class ApprovalShortcut {
    static let identifier = EventHotKeyID(signature: 0x4265_636E, id: 1) // 'Becn'

    private let open: @MainActor () -> Void
    private var handler: EventHandlerRef?
    private var hotKey: EventHotKeyRef?

    // The app delegate owns this registration on the main thread for its entire lifetime.
    @MainActor
    init?(_ open: @escaping @MainActor () -> Void) {
        self.open = open
        let target = GetApplicationEventTarget()
        var spec = EventTypeSpec(eventClass: OSType(kEventClassKeyboard), eventKind: UInt32(kEventHotKeyPressed))
        let context = Unmanaged.passUnretained(self).toOpaque()
        guard InstallEventHandler(target, { _, event, context in
            guard let event, let context, ApprovalShortcut.matches(event) else {
                return OSStatus(eventNotHandledErr)
            }
            let action = Unmanaged<ApprovalShortcut>.fromOpaque(context).takeUnretainedValue().open
            // The application event target delivers hot-key events on the main thread.
            MainActor.assumeIsolated { action() }
            return noErr
        }, 1, &spec, context, &handler) == noErr else { return nil }

        guard RegisterEventHotKey(
            UInt32(kVK_ANSI_B), UInt32(optionKey | cmdKey), Self.identifier, target,
            UInt32(kEventHotKeyExclusive), &hotKey
        ) == noErr else { return nil }
    }

    deinit {
        if let hotKey { UnregisterEventHotKey(hotKey) }
        if let handler { RemoveEventHandler(handler) }
    }

    static func matches(_ event: EventRef) -> Bool {
        var id = EventHotKeyID()
        return GetEventParameter(
            event, EventParamName(kEventParamDirectObject), EventParamType(typeEventHotKeyID), nil,
            MemoryLayout<EventHotKeyID>.size, nil, &id
        ) == noErr && id.signature == identifier.signature && id.id == identifier.id
    }
}
