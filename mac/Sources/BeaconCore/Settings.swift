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

/// The one Slack workspace and user allowed to post. The API origin is fixed; only the vault holds the token.
public struct SlackSettings: Codable, Equatable, Sendable {
    public static let origin = "https://slack.com"
    public let workspace: String
    public let userID: String
    public let team: String
    public let user: String

    public var account: String { "slack:user:\(userID)" }
    public var connected: String { "Posts to \(team) (\(workspace)) as \(user) (\(userID))" }

    public var binding: CredentialBinding {
        get throws {
            try CredentialBinding(service: "slack", origin: Self.origin, account: account, workspace: workspace)
        }
    }
}

/// Where Beacon.app sends its records (13.00-D16, D25): the Signal origin, the person they are about, and the app
/// actor whose key (in the vault) writes them.
public struct SignalSettings: Codable, Equatable, Sendable {
    /// `https://host[:port]`.
    public var origin: String
    /// The person the app works for, `act_…`, as Signal's `works_for` named it: every record's `metadata.person`.
    public var person: String
    /// The app actor, `act_…`, the key belongs to.
    public var actor: String
    /// The app actor's handle, for display.
    public var handle: String
    /// The person's handle and name, for display.
    public var personHandle: String?
    public var personName: String?

    public init(origin: String, person: String, actor: String, handle: String, personHandle: String? = nil,
                personName: String? = nil) {
        self.origin = origin
        self.person = person
        self.actor = actor
        self.handle = handle
        self.personHandle = personHandle
        self.personName = personName
    }

    /// The person as Settings shows them: "Winton (@winton)", or whatever part Signal gave.
    public var personLabel: String {
        switch (personName, personHandle) {
        case let (name?, handle?): "\(name) (@\(handle))"
        case let (name?, nil): name
        case let (nil, handle?): "@\(handle)"
        default: person
        }
    }

    /// "Connected to https://signal.example as Winton (@winton)".
    public var connected: String { "Connected to \(origin) as \(personLabel)" }

    /// Records can go out under these settings: the person is an `act_…` other than the app itself. Settings saved
    /// before Beacon took the person from Signal could name the app; those are set up again in Settings.
    public var usable: Bool { PublicID.isValid(person, prefix: "act") && person != actor }

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
    public var slack: SlackSettings?

    public init(github: GitHubSettings? = nil, signal: SignalSettings? = nil, slack: SlackSettings? = nil) {
        self.github = github
        self.signal = signal
        self.slack = slack
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
