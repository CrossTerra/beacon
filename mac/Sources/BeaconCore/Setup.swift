import BeaconSecurity
import Foundation

/// Where the app keeps its files: `~/Library/Application Support/beacon`, the collector's directory.
public struct AppPaths: Sendable {
    public let root: URL

    public init(root: URL) {
        self.root = root
    }

    public static var standard: AppPaths {
        AppPaths(root: FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("beacon"))
    }

    /// The journal and vault, `0700`.
    public var data: URL { root.appendingPathComponent("app") }
    public var journal: URL { data.appendingPathComponent("journal.sqlite") }
    public var vault: URL { data.appendingPathComponent("vault") }
    /// What `beacon act` trusts: the app's own requirement.
    public var act: URL { root.appendingPathComponent("act.json") }
    public var socket: URL { root.appendingPathComponent("run/act.sock") }
}

/// The client's side of the socket's trust (13.00-D5), set up by the app instead of by hand. On the first launch
/// the app writes its own designated requirement to `act.json`. Later launches refuse to open the socket if the
/// file names any other requirement, for example after a rebuild signed with another certificate: the person
/// removes the file to accept the new signature.
public enum ClientTrust {
    public enum State: Equatable, Sendable {
        case written
        case matches
        /// `act.json` names another requirement, or is not one Beacon wrote.
        case mismatch
    }

    public static func ensure(_ url: URL, requirement: String) throws -> State {
        let data: Data
        do {
            data = try Data(contentsOf: url)
        } catch CocoaError.fileReadNoSuchFile {
            try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true,
                                                    attributes: [.posixPermissions: 0o700])
            try PrivateFile.replace(url, with: JSON.object(["requirement": .string(requirement)]).canonicalData)
            return .written
        }
        guard let fields = try? JSON.parse(data).objectValue, fields["requirement"]?.stringValue == requirement,
              fields.keys.allSatisfy({ ["requirement", "socket"].contains($0) }) else { return .mismatch }
        return .matches
    }

    /// The running app's designated requirement, or nil when it has none worth trusting: the app must be
    /// `dev.beacon.app` signed with a certificate (13.00-D15). An ad-hoc build never opens the socket.
    public static func ownRequirement() -> String? {
        guard beacon_self_is("dev.beacon.app") else { return nil }
        var buffer = [CChar](repeating: 0, count: 4096)
        guard beacon_self_designated_requirement(&buffer, buffer.count) else { return nil }
        return String(decoding: buffer.prefix { $0 != 0 }.map { UInt8(bitPattern: $0) }, as: UTF8.self)
    }

    /// The designated requirement of the signed code at `path`: the bundled `beacon` CLI, admitted as a client.
    public static func requirement(of path: String) -> String? {
        var buffer = [CChar](repeating: 0, count: 4096)
        guard beacon_designated_requirement(path, &buffer, buffer.count) else { return nil }
        return String(decoding: buffer.prefix { $0 != 0 }.map { UInt8(bitPattern: $0) }, as: UTF8.self)
    }
}

/// Makes invisible and bidirectional characters visible, so the window shows exactly what would be sent.
public func visibleText(_ text: String) -> String {
    var out = ""
    for scalar in text.unicodeScalars {
        let v = scalar.value
        let hidden = (v < 0x20 && scalar != "\n" && scalar != "\t") || (0x7F...0x9F).contains(v) ||
            [0x00AD, 0x034F, 0x061C, 0x115F, 0x1160, 0x17B4, 0x17B5, 0x180E, 0x2028, 0x2029, 0x2060, 0x2061,
             0x2062, 0x2063, 0x2064, 0x3164, 0xFEFF, 0xFFA0].contains(v) ||
            (0x200B...0x200F).contains(v) || (0x202A...0x202E).contains(v) || (0x2066...0x206F).contains(v) ||
            (0xFE00...0xFE0F).contains(v) || (0xE0000...0xE007F).contains(v)
        if hidden {
            out += String(format: "⟨U+%04X⟩", v)
        } else {
            out.unicodeScalars.append(scalar)
        }
    }
    return out
}
