import Foundation
import Testing
@testable import BeaconCore

/// A transport that holds each post until released, then answers from a script (stored when the script is empty).
final class GatedTransport: RecordTransport, @unchecked Sendable {
    private let lock = NSLock()
    private var waiters: [CheckedContinuation<Void, Never>] = []
    private var open = false
    var script: [RecordAnswer] = []
    private(set) var posts: [Data] = []

    var waiting: Int { lock.withLock { waiters.count } }

    func post(_ bytes: Data) async throws -> RecordAnswer {
        await withCheckedContinuation { continuation in
            lock.withLock {
                if open { continuation.resume() } else { waiters.append(continuation) }
            }
        }
        return lock.withLock {
            posts.append(bytes)
            let key = (try? JSON.parse(bytes))?["key"]?.stringValue ?? ""
            return script.isEmpty ? .stored(eventID: "evt_" + key, replayed: false) : script.removeFirst()
        }
    }

    /// Lets every held post through, and every later one.
    func release() {
        let held = lock.withLock {
            open = true
            defer { waiters = [] }
            return waiters
        }
        held.forEach { $0.resume() }
    }
}

@MainActor
struct DeliverySchedulerTests {
    func scheduler(_ transport: GatedTransport, count: Int = 2) throws -> (DeliveryScheduler, Journal) {
        let (journal, _) = try filledJournal(scratchPath(), at: Date(timeIntervalSince1970: 1_800_000_000), count: count)
        let scheduler = DeliveryScheduler(journal: journal, connect: { (transport, person, "winton") })
        return (scheduler, journal)
    }

    func whileWaiting(_ transport: GatedTransport) async throws {
        let deadline = ContinuousClock.now + .seconds(20)
        while transport.waiting == 0, ContinuousClock.now < deadline {
            try await Task.sleep(for: .milliseconds(20))
        }
        try #require(transport.waiting > 0, "the delivery never reached the gated transport")
    }

    /// A resume during a drain that ends in a retry is not lost to the backoff: it runs as soon as the drain ends.
    @Test func resumeDuringADrainThatEndsInARetry() async throws {
        let transport = GatedTransport()
        transport.script = [.retry(after: 600, ambiguous: false)]
        let (scheduler, journal) = try scheduler(transport)
        scheduler.deliver()
        defer { transport.release() }
        try await whileWaiting(transport)
        scheduler.resume()
        transport.release()
        await scheduler.idle()
        #expect(try journal.deliveryStatus() == .init(delivered: 2, waiting: 0, stopped: nil))
        #expect(transport.posts.count == 3)
        #expect(scheduler.message == "Records delivered (2)")
    }

    /// A resume during a drain that stops at a record resumes that record as soon as the drain ends.
    @Test func resumeDuringADrainThatStops() async throws {
        let transport = GatedTransport()
        transport.script = [.stop(reason: "refused:400:/metadata/person")]
        let (scheduler, journal) = try scheduler(transport)
        scheduler.deliver()
        defer { transport.release() }
        try await whileWaiting(transport)
        scheduler.resume()
        transport.release()
        await scheduler.idle()
        #expect(try journal.deliveryStatus() == .init(delivered: 2, waiting: 0, stopped: nil))
        #expect(scheduler.message == "Records delivered (2)")
    }

    /// Without a resume, a retry waits for its backoff, and an unforced request does nothing meanwhile.
    @Test func anUnforcedRequestWaitsOutTheBackoff() async throws {
        let transport = GatedTransport()
        transport.script = [.retry(after: 600, ambiguous: false)]
        let (scheduler, journal) = try scheduler(transport)
        transport.release()
        scheduler.deliver()
        await scheduler.idle()
        scheduler.deliver()
        await scheduler.idle()
        #expect(transport.posts.count == 1)
        #expect(try journal.deliveryStatus().waiting == 2)
        scheduler.deliver(force: true)
        await scheduler.idle()
        #expect(try journal.deliveryStatus().delivered == 2)
    }

    /// A stop without a resume stays stopped, also after an unforced request.
    @Test func aStopWaitsForResume() async throws {
        let transport = GatedTransport()
        transport.script = [.stop(reason: "conflict:transition")]
        let (scheduler, journal) = try scheduler(transport)
        transport.release()
        scheduler.deliver()
        await scheduler.idle()
        scheduler.deliver(force: true)
        await scheduler.idle()
        #expect(try journal.deliveryStatus().stopped == "conflict:transition")
        #expect(scheduler.message.contains("conflict:transition"))
        scheduler.resume()
        await scheduler.idle()
        #expect(try journal.deliveryStatus() == .init(delivered: 2, waiting: 0, stopped: nil))
    }

    /// Settings that name no usable person send nothing and say to save the app key again.
    @Test func settingsWithoutAPersonSendNothing() async throws {
        let (journal, _) = try filledJournal(scratchPath(), at: Date(timeIntervalSince1970: 1_800_000_000), count: 1)
        let scheduler = DeliveryScheduler(journal: journal, connect: { throw SignalSetup.Refusal.noPerson })
        scheduler.deliver()
        await scheduler.idle()
        #expect(scheduler.message.contains("Paste the setup code again"))
        #expect(try journal.deliveryStatus().waiting == 1)
    }

    @Test func settingsNamingTheAppAreNotUsable() {
        let app = "act_01m6a6ft00e81840g2081040g3"
        #expect(!SignalSettings(origin: "https://s.test", person: app, actor: app, handle: "b").usable)
        #expect(!SignalSettings(origin: "https://s.test", person: "@winton", actor: app, handle: "b").usable)
        #expect(SignalSettings(origin: "https://s.test", person: person, actor: app, handle: "b").usable)
    }
}

struct CheckoutSettingsTests {
    @Test func checkoutsAreNamedDirectories() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent("checkout-" + randomHex(6))
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let file = directory.appendingPathComponent("file")
        FileManager.default.createFile(atPath: file.path, contents: Data())
        #expect(throws: CheckoutSettings.Refusal.name) { try CheckoutSettings.make(name: "Signal", path: directory.path) }
        #expect(throws: CheckoutSettings.Refusal.name) { try CheckoutSettings.make(name: "../x", path: directory.path) }
        #expect(throws: CheckoutSettings.Refusal.path) { try CheckoutSettings.make(name: "signal", path: "relative") }
        #expect(throws: CheckoutSettings.Refusal.path) { try CheckoutSettings.make(name: "signal", path: file.path) }
        #expect(throws: CheckoutSettings.Refusal.path) {
            try CheckoutSettings.make(name: "signal", path: directory.appendingPathComponent("missing").path)
        }
        // The path is kept resolved: /var is a symlink to /private/var.
        let checkout = try CheckoutSettings.make(name: "signal", path: directory.path)
        #expect(checkout.path == "/private" + directory.path || checkout.path == directory.path)
        #expect(!checkout.path.hasPrefix("/var/"))

        let url = directory.appendingPathComponent("settings.json")
        var settings = AppSettings()
        try settings.add(checkout)
        try settings.add(try CheckoutSettings.make(name: "signal-2", path: directory.path))
        #expect(throws: CheckoutSettings.Refusal.taken) { try settings.add(checkout) }
        try settings.save(url)
        #expect(try AppSettings.load(url).checkout("signal") == checkout)
        settings.removeCheckout("signal")
        settings.removeCheckout("signal-2")
        #expect(settings.checkouts == nil)

        // Settings from before agent runs have no checkouts.
        try Data(#"{"version":1}"#.utf8).write(to: url)
        #expect(try AppSettings.load(url).checkouts == nil)
    }
}
