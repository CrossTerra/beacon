import Foundation

/// The GitHub account Beacon merges as. The token itself is in the vault, bound to this origin and account.
public struct GitHubSettings: Codable, Equatable, Sendable {
    /// `https://api.github.com`, or a GitHub Enterprise API origin.
    public var origin: String
    /// `github:user:{id}`: the stable id, checked again before every merge.
    public var account: String
    /// The login at registration, for display only.
    public var login: String

    public var userID: Int64? { Int64(account.dropFirst("github:user:".count)) }

    public var binding: CredentialBinding {
        get throws { try CredentialBinding(service: "github", origin: origin, account: account) }
    }
}

/// Where Beacon.app sends its records (13.00-D16, D25): the Signal origin, the person they are about, and the app
/// actor whose key (in the vault) writes them.
public struct SignalSettings: Codable, Equatable, Sendable {
    /// `https://host[:port]`.
    public var origin: String
    /// The person the app works for, `act_…`: every record's `metadata.person`.
    public var person: String
    /// The app actor, `act_…`, the key belongs to.
    public var actor: String
    /// The app actor's handle, for display.
    public var handle: String

    public var binding: CredentialBinding {
        get throws { try CredentialBinding(service: "signal", origin: origin, account: "signal:actor:\(actor)") }
    }
}

/// `~/Library/Application Support/beacon/app/settings.json`: the person's choices, never a secret. `beacon
/// egress` reads it to list the hosts the app contacts.
public struct AppSettings: Codable, Equatable, Sendable {
    public var version = 1
    public var github: GitHubSettings?
    public var signal: SignalSettings?

    public init(github: GitHubSettings? = nil, signal: SignalSettings? = nil) {
        self.github = github
        self.signal = signal
    }

    public static func load(_ url: URL) throws -> AppSettings {
        do {
            return try JSONDecoder().decode(AppSettings.self, from: try Data(contentsOf: url))
        } catch CocoaError.fileReadNoSuchFile {
            return AppSettings()
        }
    }

    public func save(_ url: URL) throws {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys, .prettyPrinted]
        try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true,
                                                attributes: [.posixPermissions: 0o700])
        try PrivateFile.replace(url, with: try encoder.encode(self))
    }
}

extension AppPaths {
    public var settings: URL { data.appendingPathComponent("settings.json") }
}
