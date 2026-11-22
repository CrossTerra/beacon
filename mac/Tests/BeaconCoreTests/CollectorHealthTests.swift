import Foundation
import Testing
@testable import BeaconCore

struct CollectorHealthTests {
    let now = Date(timeIntervalSince1970: 1_794_423_600) // 2026-11-11T19:00:00Z
    let utc = TimeZone(identifier: "UTC")!

    /// A config and state directory in a temporary directory, as `beacon init` writes them.
    func setup(pass: String?, coverage: String?, state: String = "state") throws -> URL {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let stateDir = state.hasPrefix("/") ? URL(fileURLWithPath: state) : dir.appendingPathComponent(state)
        try FileManager.default.createDirectory(at: stateDir, withIntermediateDirectories: true)
        let config = dir.appendingPathComponent("config.yaml")
        try "signal:\n  url: https://signal.example.com\nrules: rules.yaml\nstate: \(state) # local\nmode: send\n"
            .write(to: config, atomically: true, encoding: .utf8)
        if let pass { try pass.write(to: stateDir.appendingPathComponent("pass.json"), atomically: true, encoding: .utf8) }
        if let coverage {
            try coverage.write(to: stateDir.appendingPathComponent("coverage.json"), atomically: true, encoding: .utf8)
        }
        return config
    }

    func health(pass: String?, coverage: String?) throws -> CollectorHealth {
        CollectorHealth.read(config: try setup(pass: pass, coverage: coverage), now: now, timeZone: utc)
    }

    let held = #"{"at":"2026-11-11T18:59:00.123456789Z","outcome":"ok","lease":"held","holder":"0123456789abcdef","lease_until":"2026-11-11T19:30:00Z"}"#

    @Test func healthyCollectorIsCollecting() throws {
        let h = try health(pass: held, coverage: #"{"sources":{"slack":{"every_seconds":15,"last_pass":"2026-11-11T18:59:00Z","failures":1,"reason":"unavailable"}}}"#)
        #expect(h.status == .collecting && !h.unhealthy)
        #expect(h.failing.isEmpty) // One failed pass is not yet failing.
        #expect(h.details == ["Signal lease: held until 19:30"])
        #expect(!h.repairKeychain)
    }

    /// This morning's failure: every source refused its keychain read and the lease ran out.
    @Test func keychainRefusalsAndALapsedLeaseAreNotWriting() throws {
        let pass = #"{"at":"2026-11-11T18:00:00Z","outcome":"ok","lease":"held","holder":"0123456789abcdef","lease_until":"2026-11-11T18:15:00Z"}"#
        let coverage = #"{"sources":{"github":{"last_pass":"2026-11-11T18:59:00Z","failures":60,"reason":"keychain_read_refused"},"slack":{"last_pass":"2026-11-11T18:59:00Z","failures":200,"reason":"source_unauthorized"}}}"#
        let h = try health(pass: pass, coverage: coverage)
        #expect(h.status == .notWriting && h.unhealthy)
        #expect(h.statusLine == "Not writing to Signal")
        #expect(h.failing.map { "\($0.name): \($0.reasonClass)" } == ["github: Keychain read refused", "slack: Credential refused"])
        #expect(h.details == ["Signal lease: expired at 18:15"])
        #expect(h.repairKeychain)
    }

    @Test func aFailedPassNamesItsReason() throws {
        let h = try health(pass: #"{"at":"2026-11-11T18:59:00Z","outcome":"keychain_read_refused","failures":3}"#, coverage: nil)
        #expect(h.status == .notWriting)
        #expect(h.details == ["Last pass failed: Keychain read refused", "Signal lease: none"])
        #expect(h.repairKeychain)
    }

    @Test func failingSourcesWhileStillSending() throws {
        let h = try health(pass: held, coverage: #"{"sources":{"jira":{"last_pass":"2026-11-11T18:59:00Z","failures":2,"reason":"rate_limited"}}}"#)
        #expect(h.status == .sourcesFailing && h.statusLine == "Some sources failing" && h.unhealthy)
        #expect(h.failing == [.init(name: "jira", reason: "rate_limited")] && !h.repairKeychain)
    }

    @Test func leaseStates() throws {
        let other = try health(pass: #"{"at":"2026-11-11T18:59:00Z","outcome":"ok","lease":"other"}"#, coverage: nil)
        #expect(other.status == .notWriting && other.details == ["Signal lease: held by another Beacon"])
        let notTaken = try health(pass: #"{"at":"2026-11-11T18:59:00Z","outcome":"ok","lease":"not_taken","lease_reason":"unavailable"}"#, coverage: nil)
        #expect(notTaken.details == ["Signal lease: not taken (Unreachable)"] && notTaken.status == .notWriting)
        let stopped = try health(pass: held.replacingOccurrences(of: #""outcome":"ok""#, with: #""outcome":"ok","send_reason":"lease_expired""#), coverage: nil)
        #expect(stopped.status == .notWriting && stopped.details.last == "Sending stopped: Lease expired")
        // A record from before lease_until: the lease lapses three periods after the pass.
        let legacy = #"{"at":"2026-11-11T18:00:00Z","outcome":"ok","lease":"held","holder":"0123456789abcdef"}"#
        #expect(try health(pass: legacy, coverage: nil).status == .notWriting)
    }

    @Test func missingOrTamperedRecordsAreNotGuessed() throws {
        #expect(try health(pass: nil, coverage: nil).status == .unknown("No collector pass yet"))
        for bad in [#"{"at":"2026-11-11T18:59:00Z","outcome":"ok\nslack: fine"}"#, #"{"at":"yesterday","outcome":"ok"}"#,
                    #"{"at":"2026-11-11T18:59:00Z","outcome":"ok","lease":"mine"}"#, "not json"] {
            #expect(try health(pass: bad, coverage: nil).status == .unknown("Collector status unreadable"))
        }
        let badName = #"{"sources":{"Slack Channel":{"last_pass":"2026-11-11T18:59:00Z","failures":3}}}"#
        #expect(try health(pass: held, coverage: badName).status == .collecting) // The coverage is ignored, not shown.
        let huge = #"{"at":"2026-11-11T18:59:00Z","outcome":"ok","pad":""# + String(repeating: "x", count: 70_000) + #""}"#
        #expect(try health(pass: huge, coverage: nil).status == .unknown("Collector status unreadable"))
        let missing = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        #expect(CollectorHealth.read(config: missing, now: now).status == .unknown("Collector health unknown: no config"))
    }

    @Test func theStateDirectoryResolvesAsTheCollectorDoes() throws {
        let config = try setup(pass: held, coverage: nil)
        #expect(CollectorHealth.stateDirectory(config: config) == config.deletingLastPathComponent().appendingPathComponent("state", isDirectory: true).standardizedFileURL)
        let absolute = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString).path
        let abs = try setup(pass: held, coverage: nil, state: "\"\(absolute)\"")
        #expect(CollectorHealth.stateDirectory(config: abs)?.path == absolute)
        for value in ["~/state", "{a: b}", "", "\"a\\nb\""] {
            let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
            let file = dir.appendingPathComponent("config.yaml")
            try "rules: r\nstate: \(value)\n".write(to: file, atomically: true, encoding: .utf8)
            #expect(CollectorHealth.stateDirectory(config: file) == nil)
        }
    }

    @Test func repairScriptRunsTheBundledCLIQuoted() {
        #expect(KeychainRepair.script(cli: "/Users/o'neil/Applications/Beacon.app/Contents/Helpers/beacon") ==
                "#!/bin/sh\n'/Users/o'\\''neil/Applications/Beacon.app/Contents/Helpers/beacon' keychain repair\n")
    }
}
