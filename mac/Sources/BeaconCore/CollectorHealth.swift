import Foundation

/// The collector's health, as its own records say: `pass.json` (the last send pass, the lease) and `coverage.json`
/// (each source's failures), in the state directory its config names. Both hold only names, times and fixed codes.
/// Reading them contacts nothing and reads no credential.
public struct CollectorHealth: Equatable, Sendable {
    public enum Status: Equatable, Sendable {
        case collecting, sourcesFailing, notWriting
        /// No record yet, or one that cannot be read: nothing to badge.
        case unknown(String)
    }

    public struct FailingSource: Equatable, Sendable, Identifiable {
        public let name: String
        public let reason: String
        public var id: String { name }
        public var reasonClass: String { CollectorHealth.describe(reason) }
    }

    public var status: Status
    public var failing: [FailingSource] = []
    /// The lease and, when the last pass failed, why.
    public var details: [String] = []
    /// A refused keychain read caused a failure; `beacon keychain repair` fixes it.
    public var repairKeychain = false

    public init(status: Status) { self.status = status }

    public var unhealthy: Bool { status == .sourcesFailing || status == .notWriting }

    public var statusLine: String {
        switch status {
        case .collecting: "Collecting"
        case .sourcesFailing: "Some sources failing"
        case .notWriting: "Not writing to Signal"
        case .unknown(let why): why
        }
    }

    /// A held lease with no recorded deadline (a record from before `lease_until`) counts as lapsed after three
    /// default lease periods.
    static let legacyLeaseLife: TimeInterval = 45 * 60

    public static func read(config: URL = defaultConfig, now: Date = Date(), timeZone: TimeZone = .current) -> CollectorHealth {
        guard let state = stateDirectory(config: config) else { return CollectorHealth(status: .unknown("Collector health unknown: no config")) }
        let pass = load(PassRecord.self, state.appendingPathComponent("pass.json"))
        let coverage = load(CoverageRecord.self, state.appendingPathComponent("coverage.json"))
        return evaluate(pass: pass, coverage: coverage, now: now, timeZone: timeZone)
    }

    public static var defaultConfig: URL {
        FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Library/Application Support/beacon/config.yaml")
    }

    enum Loaded<T> { case missing, unreadable, record(T) }

    static func evaluate(pass: Loaded<PassRecord>, coverage: Loaded<CoverageRecord>, now: Date,
                         timeZone: TimeZone) -> CollectorHealth {
        var health = CollectorHealth(status: .collecting)
        if case .record(let cov) = coverage {
            health.failing = cov.sources.filter { $0.value.failures >= coverageFailingAfter }
                .map { FailingSource(name: $0.key, reason: $0.value.reason ?? "source_failed") }
                .sorted { $0.name < $1.name }
        }
        var notWriting = false
        switch pass {
        case .missing:
            if case .unreadable = coverage { return CollectorHealth(status: .unknown("Collector status unreadable")) }
            guard !health.failing.isEmpty else { return CollectorHealth(status: .unknown("No collector pass yet")) }
            health.details.append("Signal lease: no send pass recorded")
        case .unreadable:
            return CollectorHealth(status: .unknown("Collector status unreadable"))
        case .record(let p):
            if failed(p.outcome) {
                notWriting = true
                health.details.append("Last pass failed: \(describe(p.outcome))")
                health.repairKeychain = p.outcome == keychainRefused
            }
            switch p.lease {
            case "held":
                let until = p.leaseUntil ?? p.at.addingTimeInterval(legacyLeaseLife)
                if until > now {
                    health.details.append("Signal lease: held until \(clock(until, timeZone))")
                } else {
                    notWriting = true
                    health.details.append("Signal lease: expired at \(clock(until, timeZone))")
                }
            case "other":
                notWriting = true
                health.details.append("Signal lease: held by another Beacon")
            case "not_taken":
                notWriting = true
                let why = p.leaseReason.map { " (\(describe($0)))" } ?? ""
                health.details.append("Signal lease: not taken\(why)")
            default:
                health.details.append("Signal lease: none")
            }
            if let reason = p.sendReason {
                notWriting = true
                health.details.append("Sending stopped: \(describe(reason))")
            }
        }
        if health.failing.contains(where: { $0.reason == keychainRefused }) { health.repairKeychain = true }
        health.status = notWriting ? .notWriting : health.failing.isEmpty ? .collecting : .sourcesFailing
        return health
    }

    static let keychainRefused = "keychain_read_refused"
    /// coverage.FailingAfter: one failed pass is not yet failing.
    static let coverageFailingAfter = 2

    static func failed(_ outcome: String) -> Bool { outcome != "ok" && outcome != "stopped" }

    /// A fixed reason code in words. Codes come from a validated record; an unknown one is shown as itself.
    public static func describe(_ code: String) -> String {
        switch code {
        case keychainRefused: "Keychain read refused"
        case "keychain": "Credential not in the Keychain"
        case "source_unauthorized": "Credential refused"
        case "unauthorized": "Signal refused the key"
        case "unavailable": "Unreachable"
        case "rate_limited": "Rate limited"
        case "scope_denied": "Missing scope"
        case "identity_mismatch": "Wrong account"
        case "lease_refused": "Lease refused"
        case "lease_expired": "Lease expired"
        case "config": "Config or rules error"
        case "signing_mismatch": "Signing mismatch"
        case "state_files": "State files unreadable"
        case "egress_denied": "Host not allowed"
        case "outbox_locked": "Outbox locked"
        case "no_read_scope": "Signal key lacks read scope"
        case "source_failed", "internal": "Failed"
        default: "Failed (\(code))"
        }
    }

    private static func clock(_ date: Date, _ timeZone: TimeZone) -> String {
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX")
        f.timeZone = timeZone
        f.dateFormat = "HH:mm"
        return f.string(from: date)
    }

    // MARK: Records

    static var namePattern: Regex<Substring> { /^[a-z][a-z0-9_]{0,63}$/ }
    static var codePattern: Regex<Substring> { /^[a-z][a-z0-9_]{0,31}$/ }

    struct PassRecord: Decodable {
        let at: Date
        let outcome: String
        let lease: String?
        let leaseUntil: Date?
        let leaseReason: String?
        let sendReason: String?

        enum CodingKeys: String, CodingKey {
            case at, outcome, lease, leaseUntil = "lease_until", leaseReason = "lease_reason", sendReason = "send_reason"
        }

        init(from decoder: Decoder) throws {
            let c = try decoder.container(keyedBy: CodingKeys.self)
            at = try goTime(c.decode(String.self, forKey: .at))
            outcome = try code(c.decode(String.self, forKey: .outcome))
            lease = try c.decodeIfPresent(String.self, forKey: .lease)
            leaseUntil = try c.decodeIfPresent(String.self, forKey: .leaseUntil).map(goTime)
            leaseReason = try c.decodeIfPresent(String.self, forKey: .leaseReason).map(code)
            sendReason = try c.decodeIfPresent(String.self, forKey: .sendReason).map(code)
            guard [nil, "held", "other", "not_taken"].contains(lease) else { throw RecordError() }
        }
    }

    struct CoverageRecord: Decodable {
        struct Source: Decodable {
            let failures: Int
            let reason: String?

            enum CodingKeys: String, CodingKey { case failures, reason }

            init(from decoder: Decoder) throws {
                let c = try decoder.container(keyedBy: CodingKeys.self)
                failures = try c.decodeIfPresent(Int.self, forKey: .failures) ?? 0
                reason = try c.decodeIfPresent(String.self, forKey: .reason).map(code)
                guard failures >= 0 else { throw RecordError() }
            }
        }

        let sources: [String: Source]

        enum CodingKeys: String, CodingKey { case sources }

        init(from decoder: Decoder) throws {
            let c = try decoder.container(keyedBy: CodingKeys.self)
            sources = try c.decodeIfPresent([String: Source].self, forKey: .sources) ?? [:]
            guard sources.count <= 64, sources.keys.allSatisfy({ $0.wholeMatch(of: namePattern) != nil }) else {
                throw RecordError()
            }
        }
    }

    struct RecordError: Error {}

    /// The records are at most 64 KiB, as the collector reads them.
    static let maxRecord = 1 << 16

    static func load<T: Decodable>(_ type: T.Type, _ url: URL) -> Loaded<T> {
        guard let handle = try? FileHandle(forReadingFrom: url) else {
            return FileManager.default.fileExists(atPath: url.path) ? .unreadable : .missing
        }
        defer { try? handle.close() }
        guard let data = try? handle.read(upToCount: maxRecord + 1), data.count <= maxRecord,
              let record = try? JSONDecoder().decode(T.self, from: data) else { return .unreadable }
        return .record(record)
    }

    /// The `state:` the collector's config names, resolved as the collector resolves it: relative to the config's
    /// directory. Only a plain top-level scalar is read; anything else is not guessed at.
    public static func stateDirectory(config: URL) -> URL? {
        guard let data = try? Data(contentsOf: config), data.count <= 1 << 20,
              let raw = String(data: data, encoding: .utf8) else { return nil }
        // YAML allows CRLF and CR line breaks; read them as the collector's parser does.
        let text = raw.replacingOccurrences(of: "\r\n", with: "\n").replacingOccurrences(of: "\r", with: "\n")
        for line in text.split(separator: "\n", omittingEmptySubsequences: false) {
            guard line.hasPrefix("state:") else { continue }
            var value = line.dropFirst("state:".count).trimmingCharacters(in: .whitespaces)
            if let hash = value.range(of: " #") { value = String(value[..<hash.lowerBound]).trimmingCharacters(in: .whitespaces) }
            if value.count >= 2, let first = value.first, first == "\"" || first == "'", value.last == first {
                value = String(value.dropFirst().dropLast())
                if first == "\"" && value.contains("\\") { return nil }
            }
            guard !value.isEmpty, !value.hasPrefix("~"), !value.contains("\r"),
                  !["\"", "'", "{", "[", "|", ">", "&", "*", "!"].contains(where: value.hasPrefix) else { return nil }
            if value.hasPrefix("/") { return URL(fileURLWithPath: value, isDirectory: true).standardizedFileURL }
            return config.deletingLastPathComponent().appendingPathComponent(value, isDirectory: true).standardizedFileURL
        }
        return nil
    }
}

private func code(_ s: String) throws -> String {
    guard s.wholeMatch(of: CollectorHealth.codePattern) != nil else { throw CollectorHealth.RecordError() }
    return s
}

/// Go's RFC 3339 times, with up to nine fractional digits.
private func goTime(_ s: String) throws -> Date {
    let whole = s.replacing(/\.[0-9]{1,9}(?=Z|[+-][0-9]{2}:[0-9]{2}$)/, with: "")
    guard let date = ISO8601DateFormatter().date(from: whole) else { throw CollectorHealth.RecordError() }
    return date
}

/// The script the menu's _Run beacon keychain repair_ opens in Terminal: the bundled CLI, which re-creates each item
/// with its own access. The person answers each prompt; the app reads no credential.
public enum KeychainRepair {
    public static func script(cli: String) -> String {
        "#!/bin/sh\n'\(cli.replacingOccurrences(of: "'", with: "'\\''"))' keychain repair\n"
    }
}
