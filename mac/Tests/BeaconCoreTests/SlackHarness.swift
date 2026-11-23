import Darwin
import Foundation
import Security
import Testing
@testable import BeaconCore

/// A real loopback TLS server, rather than a URLProtocol that cannot expose CFNetwork replaying a write.
final class SlackFixture {
    private let child: Process
    private let lifetime: FileHandle
    private let control: ServiceClient
    let connect: SlackAPI.Connect
    /// The request deadline of every client the handler made, in order.
    let timeouts: Locked<[TimeInterval]>

    init() throws {
        let process = Process()
        let input = Pipe(), output = Pipe()
        process.standardInput = input
        process.standardOutput = output
        process.executableURL = URL(fileURLWithPath: "/usr/bin/env")
        // mac/Tests/BeaconCoreTests/SlackHarness.swift -> repository root. No module downloads or inherited secrets.
        let file = URL(fileURLWithPath: #filePath)
        process.currentDirectoryURL = (0..<4).reduce(file) { url, _ in url.deletingLastPathComponent() }
        process.environment = [
            "PATH": ProcessInfo.processInfo.environment["PATH"] ?? "/usr/bin:/bin:/opt/homebrew/bin",
            "HOME": NSHomeDirectory(), "GOENV": "off", "GOWORK": "off", "GOTOOLCHAIN": "local",
            "GOPROXY": "off", "GOSUMDB": "off", "GOAUTH": "off", "CGO_ENABLED": "0",
        ]
        process.arguments = ["go", "run", "-mod=readonly", "./mac/Tests/SlackFixture"]
        try process.run()
        let lifetime = input.fileHandleForWriting
        do {
            let output = output.fileHandleForReading
            let deadline = ContinuousClock.now + .seconds(45)
            var line = Data()
            while !line.contains(10) {
                var fd = pollfd(fd: output.fileDescriptor, events: Int16(POLLIN), revents: 0)
                guard ContinuousClock.now < deadline, line.count < 4096, poll(&fd, 1, 1000) >= 0 else { throw ServiceError.malformed }
                if fd.revents == 0 { continue }
                let data = output.availableData
                guard !data.isEmpty else { throw ServiceError.malformed }
                line.append(data)
            }
            let fields = String(decoding: line, as: UTF8.self).split(whereSeparator: \.isWhitespace)
            guard fields.count == 2, let origin = URL(string: String(fields[0])), origin.scheme == "https", origin.host == "127.0.0.1",
                  origin.port != nil, let certificate = Data(base64Encoded: String(fields[1])) else { throw ServiceError.malformed }
            let timeouts = Locked<[TimeInterval]>([])
            self.timeouts = timeouts
            let connect: SlackAPI.Connect = { token, timeout in
                timeouts.withLock { $0.append(timeout) }
                return try ServiceClient(origin: origin.absoluteString, authorization: token, timeout: timeout,
                                         resourceTimeout: timeout, delegate: FixtureTLS(certificate: certificate))
            }
            self.connect = connect
            control = try ServiceClient(origin: origin.absoluteString, authorization: nil, timeout: 30, resourceTimeout: 30,
                                        delegate: FixtureTLS(certificate: certificate))
        } catch {
            try? lifetime.close()
            process.terminate()
            process.waitUntilExit()
            throw error
        }
        child = process
        self.lifetime = lifetime
    }

    deinit {
        try? lifetime.close()
        child.waitUntilExit()
    }

    func configure(_ fields: [String: JSON]) async throws {
        let response = try await control.send("PUT", "/fixture", raw: JSON.object(fields).canonicalData)
        try #require(response.status == 200)
    }

    func calls() async throws -> [JSON] {
        let response = try await control.send("GET", "/fixture")
        try #require(response.status == 200)
        return array(try JSON.parse(response.body)["calls"])
    }

    func posts() async throws -> [JSON] {
        try await calls().filter { $0["method"] == "POST" }
    }
}

private final class FixtureTLS: ServiceClient.Delegate, @unchecked Sendable {
    let certificate: Data

    init(certificate: Data) { self.certificate = certificate }

    override func urlSession(_ session: URLSession, didReceive challenge: URLAuthenticationChallenge,
                             completionHandler: @escaping @Sendable (URLSession.AuthChallengeDisposition, URLCredential?) -> Void) {
        guard challenge.protectionSpace.authenticationMethod == NSURLAuthenticationMethodServerTrust,
              challenge.protectionSpace.host == "127.0.0.1", let trust = challenge.protectionSpace.serverTrust,
              let anchor = SecCertificateCreateWithData(nil, certificate as CFData),
              SecTrustSetAnchorCertificates(trust, [anchor] as CFArray) == errSecSuccess,
              SecTrustSetAnchorCertificatesOnly(trust, true) == errSecSuccess,
              SecTrustSetNetworkFetchAllowed(trust, false) == errSecSuccess,
              SecTrustEvaluateWithError(trust, nil),
              let chain = SecTrustCopyCertificateChain(trust) as? [SecCertificate], let leaf = chain.first,
              SecCertificateCopyData(leaf) as Data == certificate else {
            completionHandler(.cancelAuthenticationChallenge, nil)
            return
        }
        completionHandler(.useCredential, URLCredential(trust: trust))
    }
}

private let fixtureChannel: JSON = .object([
    "id": "C0GENERAL", "name": "general", "context_team_id": "T0WORKSPACE",
    "is_channel": .bool(true), "is_private": .bool(false), "is_archived": .bool(false), "is_member": .bool(true),
])

final class SlackHarness {
    static let token = "xoxp-fixture-token"
    static let other = "xoxp-fixture-token-2"
    static let root = "1799999900.000100"
    static let posted = "1800000000.000200"
    static let draft = " *Ship* to <#C0GENERAL|another name> &amp; check `logs`\n<https://example.test|Runbook> — cafe\u{0301} 👋 "
    static let preview = " Ship to #C0GENERAL & check logs\nRunbook (https://example.test) — cafe\u{0301} 👋 "
    let fixture: SlackFixture
    let clock = Clock()
    let directory: URL
    let settingsURL: URL
    let journalURL: URL
    let journal: Journal
    let vault: Vault
    let handler: PostHandler

    init(budget: ReadBudget = ReadBudget(), intakePerHour: Int = 30,
         readBackBudget: ReadBudget = ReadBudget(perHour: 60, concurrent: 2)) throws {
        fixture = try SlackFixture()
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent("slk-" + randomHex(4))
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false,
                                                attributes: [.posixPermissions: 0o700])
        self.directory = directory
        journalURL = directory.appendingPathComponent("journal.sqlite")
        settingsURL = directory.appendingPathComponent("app/settings.json")
        vault = Vault(url: directory.appendingPathComponent("app/vault"), key: FixtureVaultKey())
        journal = try Journal(path: journalURL.path, at: clock.now)
        handler = Self.handler(journal: journal, vault: vault, url: settingsURL, clock: clock, budget: budget,
                               intakePerHour: intakePerHour, readBackBudget: readBackBudget, connect: fixture.connect)
    }

    deinit { try? FileManager.default.removeItem(at: directory) }

    /// Generous: the whole suite's TLS fixtures run at once, and a slow CI runner can take longer than the
    /// production deadlines to schedule a request. Production keeps its own.
    static let testTimeouts = SlackAPI.Timeouts(intake: 30, action: 60)

    private static func handler(journal: Journal, vault: Vault, url: URL, clock: Clock, budget: ReadBudget,
                                intakePerHour: Int = 30, readBackBudget: ReadBudget = ReadBudget(perHour: 60, concurrent: 2),
                                connect: @escaping SlackAPI.Connect) -> PostHandler {
        PostHandler(journal: journal, settings: { (try? AppSettings.load(url))?.slack }, vault: vault, budget: budget,
                    intakePerHour: intakePerHour, readBackBudget: readBackBudget, timeouts: testTimeouts,
                    now: { clock.now }, connect: connect)
    }

    func restart() throws -> (Journal, PostHandler) {
        let relaunched = try Journal(path: journalURL.path, at: clock.now)
        return (relaunched, Self.handler(journal: relaunched, vault: vault, url: settingsURL, clock: clock,
                                        budget: ReadBudget(), connect: fixture.connect))
    }

    @discardableResult
    func register(_ token: String = SlackHarness.token) async throws -> SlackSettings {
        try await SlackSetup.register(token: Secret(token), vault: vault, settings: settingsURL, connect: fixture.connect)
    }

    func channel(_ changes: [String: JSON]) async throws {
        var fields = fixtureChannel.objectValue!
        fields.merge(changes, uniquingKeysWith: { _, new in new })
        // Clear fields such as is_im from the previous channel, rather than merging into it.
        try await fixture.configure(["channel": .null])
        try await fixture.configure(["channel": .object(fields)])
    }

    static func frame(channel: String = "C0GENERAL", thread: String? = root, text: String = draft,
                      workspace: String = "T0WORKSPACE") -> JSON {
        .object([
            "op": "propose", "type": "slack.post", "agent": .object(["name": "bosun"]), "reason": "Send this draft.",
            "params": .object(["workspace": .string(workspace), "channel": .string(channel),
                               "thread_ts": thread.map(JSON.string) ?? .null, "text": .string(text)]),
            "context": .object(["subject": "slack:C0UNTRUSTED:2027-01-01"]),
        ])
    }

    func propose(channel: String = "C0GENERAL", thread: String? = root, text: String = draft,
                 client: String = "beacon-act") async throws -> Proposal {
        let request = try ProposalRequest(Self.frame(channel: channel, thread: thread, text: text).objectValue!)
        let placement = try await handler.place(request, client: client)
        let proposal = Proposal(request: request, client: "beacon-act", at: clock.now, slackPlacement: placement)
        try journal.add(proposal)
        return proposal
    }

    func approve(_ proposal: Proposal) async throws {
        let resolved = try await handler.resolve(proposal, model: nil)
        try journal.approve(try FrozenApproval(proposal, origin: resolved.origin, account: resolved.account,
                                               destination: resolved.destination), at: clock.now)
    }
}
