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

/// A stand-in for the Keychain item. It keeps one key, may refuse to delete it (another program's access list, a
/// denied prompt), and can fail just after making a new item, as a crash before the vault file would.
final class FakeKeychain: VaultKeySource, @unchecked Sendable {
    struct Crash: Error {}

    private let lock = NSLock()
    var item: SymmetricKey?
    var deletable = true
    var crashAfterCreate = false
    private(set) var requests: [Bool] = []

    func key(create: Bool) throws -> SymmetricKey {
        try lock.withLock {
            requests.append(create)
            guard create else {
                guard let item else { throw VaultError.keychain(-25300) }
                return item
            }
            if item != nil {
                guard deletable else { throw VaultError.existingKey }
                item = nil
            }
            let made = SymmetricKey(size: .bits256)
            item = made
            if crashAfterCreate { throw Crash() }
            return made
        }
    }
}

func same(_ a: SymmetricKey?, _ b: SymmetricKey?) -> Bool {
    a.map { $0.withUnsafeBytes { Data($0) } } == b.map { $0.withUnsafeBytes { Data($0) } }
}

struct VaultKeyCreationTests {
    let binding = try! CredentialBinding(service: "github", origin: "https://api.github.com", account: "github:user:1")

    /// With a vault file the existing key is used and never replaced.
    @Test func anExistingVaultUsesItsKey() throws {
        let url = try VaultTests().scratch()
        let keychain = FakeKeychain()
        try Vault(url: url, key: keychain).unlock()
        let key = keychain.item
        try Vault(url: url, key: keychain).unlock()
        try Vault(url: url, key: keychain).store(Secret("fixture"), for: binding)
        #expect(try Vault(url: url, key: keychain).credential(for: binding).value == "fixture")
        // One creation, with the first vault; every later request only reads.
        #expect(keychain.requests.first == true && keychain.requests.dropFirst().allSatisfy { !$0 })
        #expect(keychain.requests.count == 5 && same(keychain.item, key))
    }

    /// A vault whose key is gone is unreadable and says so; nothing makes a new key over it.
    @Test func anExistingVaultWithoutItsKeyIsUnreadable() throws {
        let url = try VaultTests().scratch()
        let keychain = FakeKeychain()
        try Vault(url: url, key: keychain).store(Secret("fixture"), for: binding)
        keychain.item = nil
        #expect(throws: VaultError.keychain(-25300)) { try Vault(url: url, key: keychain).unlock() }
        #expect(throws: VaultError.keychain(-25300)) { try Vault(url: url, key: keychain).credential(for: binding) }
        #expect(keychain.item == nil && keychain.requests.dropFirst().allSatisfy { !$0 })
    }

    /// The item was made, then the app died before the vault file: the next launch replaces the orphan.
    @Test func anOrphanAfterACrashIsReplaced() throws {
        let url = try VaultTests().scratch()
        let keychain = FakeKeychain()
        keychain.crashAfterCreate = true
        #expect(throws: FakeKeychain.Crash.self) { try Vault(url: url, key: keychain).unlock() }
        let orphan = try #require(keychain.item)
        #expect(!FileManager.default.fileExists(atPath: url.path))

        keychain.crashAfterCreate = false
        try Vault(url: url, key: keychain).unlock()
        #expect(!same(keychain.item, orphan))
        try Vault(url: url, key: keychain).store(Secret("fixture"), for: binding)
        #expect(try Vault(url: url, key: keychain).credential(for: binding).value == "fixture")
    }

    /// An item another program planted before the vault existed is deleted, and its key never encrypts anything.
    @Test func aPlantedItemIsReplacedAndNeverUsed() throws {
        let url = try VaultTests().scratch()
        let keychain = FakeKeychain()
        let planted = SymmetricKey(size: .bits256)
        keychain.item = planted
        try Vault(url: url, key: keychain).store(Secret("fixture"), for: binding)
        #expect(!same(keychain.item, planted))
        #expect(throws: VaultError.unreadable) { try Vault(url: url, key: FixtureVaultKey(planted)).credential(for: binding) }
        #expect(try Vault(url: url, key: keychain).credential(for: binding).value == "fixture")
    }

    /// If the existing item cannot be deleted, the vault is not opened and nothing is written.
    @Test func anItemThatCannotBeDeletedIsRefused() throws {
        let url = try VaultTests().scratch()
        let keychain = FakeKeychain()
        let planted = SymmetricKey(size: .bits256)
        keychain.item = planted
        keychain.deletable = false
        #expect(throws: VaultError.existingKey) { try Vault(url: url, key: keychain).unlock() }
        #expect(throws: VaultError.existingKey) { try Vault(url: url, key: keychain).store(Secret("fixture"), for: binding) }
        #expect(!FileManager.default.fileExists(atPath: url.path) && same(keychain.item, planted))
    }
}
