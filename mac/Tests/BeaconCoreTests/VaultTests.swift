import CryptoKit
import Foundation
import Testing
@testable import BeaconCore

/// Fixture keys and fixture secrets only: these tests never open a keychain.
struct VaultTests {
    func scratch() throws -> URL {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent("vault-" + randomHex(6))
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        return directory.appendingPathComponent("app/vault")
    }

    @Test func storesOnlyBoundCredentialsEncrypted() throws {
        let url = try scratch()
        let key = FixtureVaultKey()
        let vault = Vault(url: url, key: key)
        let github = try CredentialBinding(service: "github", origin: "https://API.GitHub.com:443/", account: "github:user:123")
        let slack = try CredentialBinding(service: "slack", origin: "https://slack.com", account: "slack:user:U1", workspace: "T1")
        try vault.store(Secret("fixture-github-secret"), for: github)
        try vault.store(Secret("fixture-slack-secret"), for: slack)
        #expect(github.origin == "https://api.github.com")

        let reopened = Vault(url: url, key: key)
        #expect(try reopened.credential(for: github).value == "fixture-github-secret")
        #expect(Set(try reopened.bindings()) == [github, slack])
        for other in [
            try CredentialBinding(service: "github", origin: "https://api.github.com:8443", account: "github:user:123"),
            try CredentialBinding(service: "github", origin: "https://github.example.com", account: "github:user:123"),
            try CredentialBinding(service: "github", origin: "https://api.github.com", account: "github:user:124"),
            try CredentialBinding(service: "slack", origin: "https://slack.com", account: "slack:user:U1", workspace: "T2"),
        ] {
            #expect(throws: VaultError.notFound) { try reopened.credential(for: other) }
        }

        let bytes = try Data(contentsOf: url)
        #expect(bytes.range(of: Data("fixture-github-secret".utf8)) == nil)
        #expect(bytes.range(of: Data("api.github.com".utf8)) == nil)
        let mode = try FileManager.default.attributesOfItem(atPath: url.path)[.posixPermissions] as? Int
        #expect(mode == 0o600)
        let directoryMode = try FileManager.default.attributesOfItem(atPath: url.deletingLastPathComponent().path)[.posixPermissions] as? Int
        #expect(directoryMode == 0o700)

        #expect(throws: VaultError.unreadable) { try Vault(url: url, key: FixtureVaultKey()).credential(for: github) }
        try reopened.remove(github)
        #expect(throws: VaultError.notFound) { try reopened.credential(for: github) }
        #expect("\(try reopened.credential(for: slack))" == "[redacted]")
    }

    @Test func refusesOriginsThatAreNotPlainHTTPS() {
        for origin in ["http://api.github.com", "https://user@api.github.com", "https://api.github.com/repos",
                       "https://api.github.com?x=1", "https://", "api.github.com", "https://api.github.com:0",
                       "https://bad_host.example"] {
            #expect(throws: VaultError.origin, "\(origin)") { try CredentialBinding.normalize(origin) }
        }
    }
}

/// Records whether each key request may create, and refuses creation when an item "already exists".
final class RecordingKey: VaultKeySource, @unchecked Sendable {
    private let lock = NSLock()
    private let inner = FixtureVaultKey()
    var existing = false
    private(set) var requests: [Bool] = []

    func key(create: Bool) throws -> SymmetricKey {
        try lock.withLock {
            requests.append(create)
            if create && existing { throw VaultError.existingKey }
            existing = true
            return try inner.key(create: create)
        }
    }
}

struct VaultKeyCreationTests {
    @Test func aKeyIsCreatedOnlyWithANewVault() throws {
        let url = try VaultTests().scratch()
        let key = RecordingKey()
        try Vault(url: url, key: key).unlock()
        try Vault(url: url, key: key).unlock()
        try Vault(url: url, key: key).store(Secret("fixture"), for: try CredentialBinding(
            service: "github", origin: "https://api.github.com", account: "github:user:1"))
        #expect(key.requests == [true, false, false, false])
    }

    @Test func anExistingItemWithoutAVaultIsRefused() throws {
        let url = try VaultTests().scratch()
        let key = RecordingKey()
        key.existing = true
        #expect(throws: VaultError.existingKey) { try Vault(url: url, key: key).unlock() }
        #expect(!FileManager.default.fileExists(atPath: url.path))
        #expect(throws: VaultError.existingKey) {
            try Vault(url: url, key: key).store(Secret("fixture"), for: try CredentialBinding(
                service: "github", origin: "https://api.github.com", account: "github:user:1"))
        }
    }
}
