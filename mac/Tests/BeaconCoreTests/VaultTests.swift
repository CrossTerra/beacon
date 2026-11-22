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

/// A stand-in for the Keychain item: it keeps one key and its creation tag, and can fail just before or
/// just after making the item, as a crash would.
final class FakeKeychain: VaultKeySource, @unchecked Sendable {
    struct Item {
        let key: SymmetricKey
        let tag: String?
    }

    struct Crash: Error {}

    private let lock = NSLock()
    var item: Item?
    var crashBeforeCreate = false
    var crashAfterCreate = false
    private(set) var tags: [String?] = []

    func key(tag: String?) throws -> SymmetricKey {
        try lock.withLock {
            tags.append(tag)
            guard let tag else {
                guard let item else { throw VaultError.keychain(-25300) }
                return item.key
            }
            if let item {
                guard item.tag == tag else { throw VaultError.existingKey }
                return item.key
            }
            if crashBeforeCreate { throw Crash() }
            let made = Item(key: SymmetricKey(size: .bits256), tag: tag)
            item = made
            if crashAfterCreate { throw Crash() }
            return made.key
        }
    }
}

struct VaultKeyCreationTests {
    let binding = try! CredentialBinding(service: "github", origin: "https://api.github.com", account: "github:user:1")

    func pendingNonce(_ vault: Vault) -> String? {
        (try? Data(contentsOf: vault.pending)).flatMap { String(data: $0, encoding: .utf8) }
    }

    @Test func aKeyIsCreatedOnlyWithANewVault() throws {
        let url = try VaultTests().scratch()
        let keychain = FakeKeychain()
        let vault = Vault(url: url, key: keychain)
        try vault.unlock()
        try Vault(url: url, key: keychain).unlock()
        try Vault(url: url, key: keychain).store(Secret("fixture"), for: binding)
        #expect(keychain.tags.count == 4 && keychain.tags[0] != nil && keychain.tags.dropFirst().allSatisfy { $0 == nil })
        #expect(keychain.item?.tag == keychain.tags[0])
        #expect(pendingNonce(vault) == nil && FileManager.default.fileExists(atPath: url.path))
    }

    /// The pending file was written, then the app died before the item: the next launch makes the item with
    /// that same nonce and finishes.
    @Test func crashBeforeTheItemResumes() throws {
        let url = try VaultTests().scratch()
        let keychain = FakeKeychain()
        keychain.crashBeforeCreate = true
        let vault = Vault(url: url, key: keychain)
        #expect(throws: FakeKeychain.Crash.self) { try vault.unlock() }
        let nonce = try #require(pendingNonce(vault))
        #expect(keychain.item == nil && !FileManager.default.fileExists(atPath: url.path))

        keychain.crashBeforeCreate = false
        try Vault(url: url, key: keychain).unlock()
        #expect(keychain.item?.tag == nonce && pendingNonce(vault) == nil)
        try Vault(url: url, key: keychain).store(Secret("fixture"), for: binding)
        #expect(try Vault(url: url, key: keychain).credential(for: binding).value == "fixture")
    }

    /// The item was made, then the app died before the vault file: the next launch adopts that item, because it
    /// carries the pending nonce, instead of refusing it forever.
    @Test func crashAfterTheItemAdoptsIt() throws {
        let url = try VaultTests().scratch()
        let keychain = FakeKeychain()
        keychain.crashAfterCreate = true
        let vault = Vault(url: url, key: keychain)
        #expect(throws: FakeKeychain.Crash.self) { try vault.unlock() }
        let made = try #require(keychain.item)
        #expect(made.tag == pendingNonce(vault) && !FileManager.default.fileExists(atPath: url.path))

        keychain.crashAfterCreate = false
        try Vault(url: url, key: keychain).unlock()
        #expect(keychain.item?.tag == made.tag && pendingNonce(vault) == nil)
        try Vault(url: url, key: keychain).store(Secret("fixture"), for: binding)
        #expect(try Vault(url: url, key: keychain).credential(for: binding).value == "fixture")
    }

    /// An item Beacon did not make for this vault, untagged or tagged otherwise, is refused, also when a pending
    /// file is there.
    @Test func aPlantedItemIsRefused() throws {
        for planted in [nil, "0123456789abcdef0123456789abcdef"] as [String?] {
            let url = try VaultTests().scratch()
            let keychain = FakeKeychain()
            keychain.item = .init(key: SymmetricKey(size: .bits256), tag: planted)
            let vault = Vault(url: url, key: keychain)
            #expect(throws: VaultError.existingKey) { try vault.unlock() }
            #expect(throws: VaultError.existingKey) { try vault.unlock() }
            #expect(throws: VaultError.existingKey) { try vault.store(Secret("fixture"), for: binding) }
            #expect(!FileManager.default.fileExists(atPath: url.path))
        }
    }
}
