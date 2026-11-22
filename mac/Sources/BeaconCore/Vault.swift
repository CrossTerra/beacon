import BeaconSecurity
import CryptoKit
import Foundation

/// Where the vault's encryption key comes from.
public protocol VaultKeySource: Sendable {
    /// Without `create`, reads the existing key. With `create` (no vault yet), replaces any existing key with a
    /// fresh one, never adopting it, and refuses if the existing one cannot be removed.
    func key(create: Bool) throws -> SymmetricKey
}

public enum VaultError: Error, Equatable, Sendable {
    /// The running process is not the app signed with a certificate, so it cannot meet the key's access list.
    case unsignedBuild
    case keychain(Int32)
    /// A vault key item exists before its vault and could not be removed: it is not used.
    case existingKey
    /// The vault file is not one this key opened.
    case unreadable
    /// The origin is not `https://host[:port]`.
    case origin
    /// No credential is bound to that service, origin and account.
    case notFound
}

/// The vault key in the login Keychain, readable without a prompt only by code meeting this app's designated
/// requirement. Any process that is not `dev.beacon.app` signed with a certificate (an ad-hoc build, a test host)
/// is refused before any keychain is opened. Tests use `FixtureVaultKey`; nothing exercises this in CI.
public struct KeychainVaultKey: VaultKeySource {
    public let service: String
    public let account: String

    static let app = "dev.beacon.app"

    public init(service: String = "dev.beacon.app", account: String = "vault-key") {
        self.service = service
        self.account = account
    }

    public func key(create: Bool) throws -> SymmetricKey {
        guard beacon_self_is(KeychainVaultKey.app) else { throw VaultError.unsignedBuild }
        var bytes = [UInt8](repeating: 0, count: 32)
        defer { bytes.withUnsafeMutableBytes { _ = memset($0.baseAddress!, 0, 32) } }
        let status = beacon_vault_key(KeychainVaultKey.app, service, account, create, &bytes)
        if status == BEACON_VAULT_KEY_EXISTS { throw VaultError.existingKey }
        guard status == 0 else { throw VaultError.keychain(status) }
        return SymmetricKey(data: bytes)
    }
}

/// A key held in memory. Tests and fixtures only.
public struct FixtureVaultKey: VaultKeySource {
    let bytes: SymmetricKey

    public init(_ key: SymmetricKey = SymmetricKey(size: .bits256)) {
        bytes = key
    }

    public func key(create: Bool) throws -> SymmetricKey { bytes }
}

/// A credential's secret. It never prints.
public struct Secret: Sendable, CustomStringConvertible, CustomDebugStringConvertible {
    public let value: String

    public init(_ value: String) {
        self.value = value
    }

    public var description: String { "[redacted]" }
    public var debugDescription: String { "[redacted]" }

    /// Slack's User OAuth Token, not a bot, app, cookie or configuration token. Also rules out header delimiters.
    var isSlackUserToken: Bool { Field.matches(value, "xoxp-[a-zA-Z0-9-]{1,1024}") }
}

/// What a credential is bound to (13.00-D31). A client asks by these, from Beacon's settings; an agent never
/// supplies them.
public struct CredentialBinding: Hashable, Codable, Sendable {
    public let service: String
    /// `https://host` or `https://host:port`, lowercase, without the default port.
    public let origin: String
    /// The provider's stable user id, e.g. `github:user:123`.
    public let account: String
    /// The Slack workspace id, for a Slack credential.
    public let workspace: String?

    public init(service: String, origin: String, account: String, workspace: String? = nil) throws {
        self.service = service
        self.origin = try CredentialBinding.normalize(origin)
        self.account = account
        self.workspace = workspace
    }

    /// `https://host[:port]` with nothing else: no user, path, query or fragment.
    public static func normalize(_ origin: String) throws -> String {
        guard let url = URLComponents(string: origin), url.scheme?.lowercased() == "https",
              let host = url.host?.lowercased(), !host.isEmpty, url.user == nil, url.password == nil,
              url.path.isEmpty || url.path == "/", url.query == nil, url.fragment == nil,
              Field.matches(host, "[a-z0-9.-]{1,253}") else { throw VaultError.origin }
        if let port = url.port, port != 443 {
            guard (1...65535).contains(port) else { throw VaultError.origin }
            return "https://\(host):\(port)"
        }
        return "https://\(host)"
    }
}

/// Service credentials, encrypted with AES-GCM under the vault key in one `0600` file. Beacon's typed clients
/// read a credential by its exact binding. Nothing lists secrets, and nothing hands them to a child process.
public final class Vault: @unchecked Sendable {
    private struct Entry: Codable {
        let binding: CredentialBinding
        let secret: String
    }

    private struct Contents: Codable {
        var version = 1
        var entries: [Entry] = []
    }

    private static let associated = Data("beacon-vault-v1".utf8)
    private let url: URL
    private let keySource: any VaultKeySource
    private let lock = NSLock()

    public init(url: URL, key: any VaultKeySource) {
        self.url = url
        keySource = key
    }

    /// Opens the vault. The first time, with no vault file, it creates the key and writes an empty vault (see
    /// `save`); afterwards it reads the existing key and checks the file opens.
    public func unlock() throws {
        try locked {
            if FileManager.default.fileExists(atPath: url.path) {
                _ = try load()
            } else {
                try save(Contents())
            }
        }
    }

    /// The bindings it holds, without secrets.
    public func bindings() throws -> [CredentialBinding] {
        try locked { try load().entries.map(\.binding) }
    }

    public func store(_ secret: Secret, for binding: CredentialBinding) throws {
        try locked {
            var contents = try load()
            contents.entries.removeAll { $0.binding == binding }
            contents.entries.append(Entry(binding: binding, secret: secret.value))
            try save(contents)
        }
    }

    public func remove(_ binding: CredentialBinding) throws {
        try locked {
            var contents = try load()
            contents.entries.removeAll { $0.binding == binding }
            try save(contents)
        }
    }

    /// The secret bound to exactly this service, origin, account and workspace.
    public func credential(for binding: CredentialBinding) throws -> Secret {
        try locked {
            guard let entry = try load().entries.first(where: { $0.binding == binding }) else { throw VaultError.notFound }
            return Secret(entry.secret)
        }
    }

    private func locked<T>(_ body: () throws -> T) throws -> T {
        lock.lock()
        defer { lock.unlock() }
        return try body()
    }

    private func load() throws -> Contents {
        let sealed: Data
        do {
            sealed = try Data(contentsOf: url)
        } catch CocoaError.fileReadNoSuchFile {
            return Contents()
        }
        let key = try keySource.key(create: false)
        do {
            let box = try AES.GCM.SealedBox(combined: sealed)
            let plain = try AES.GCM.open(box, using: key, authenticating: Vault.associated)
            let contents = try JSONDecoder().decode(Contents.self, from: plain)
            guard contents.version == 1 else { throw VaultError.unreadable }
            return contents
        } catch let error as VaultError {
            throw error
        } catch {
            throw VaultError.unreadable
        }
    }

    /// Without a vault file, nothing the Keychain holds can protect anything yet: the key is made afresh, and any
    /// existing item (left by a crash before the file was written, or planted by another program that may know its
    /// key) is deleted, never adopted. With a vault file, the existing key is used; a missing one means the vault
    /// cannot be read, and nothing replaces it.
    private func save(_ contents: Contents) throws {
        let plain = try JSONEncoder().encode(contents)
        let directory = url.deletingLastPathComponent()
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true,
                                                attributes: [.posixPermissions: 0o700])
        let key = try keySource.key(create: !FileManager.default.fileExists(atPath: url.path))
        let box = try AES.GCM.seal(plain, using: key, authenticating: Vault.associated)
        try PrivateFile.replace(url, with: box.combined!)
    }
}

/// Writes a `0600` file atomically: a temporary file, synced, then renamed over the old one.
enum PrivateFile {
    static func replace(_ url: URL, with data: Data) throws {
        let temporary = url.deletingLastPathComponent().appendingPathComponent(".\(url.lastPathComponent).\(randomHex(8))")
        let fd = open(temporary.path, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC, 0o600)
        guard fd >= 0 else { throw CocoaError(.fileWriteUnknown) }
        var ok = data.withUnsafeBytes { buffer in
            var offset = 0
            while offset < buffer.count {
                let n = write(fd, buffer.baseAddress! + offset, buffer.count - offset)
                if n <= 0 { return false }
                offset += n
            }
            return true
        }
        ok = ok && fullSync(fd)
        close(fd)
        guard ok, rename(temporary.path, url.path) == 0 else {
            unlink(temporary.path)
            throw CocoaError(.fileWriteUnknown)
        }
        let directory = open(url.deletingLastPathComponent().path, O_RDONLY | O_DIRECTORY | O_CLOEXEC)
        if directory >= 0 {
            _ = fullSync(directory)
            close(directory)
        }
    }

    /// Flushes to the drive, not only to its cache: plain fsync does not on macOS. A file system without
    /// F_FULLFSYNC falls back to fsync.
    static func fullSync(_ fd: Int32) -> Bool {
        fcntl(fd, F_FULLFSYNC) == 0 || fsync(fd) == 0
    }
}
