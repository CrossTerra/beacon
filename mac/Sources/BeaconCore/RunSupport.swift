import Darwin
import Foundation
import TetherSupport

/// The environment a run gets: the person's login shell's, as their Terminal has it (plan 13.00, _Agent runs_;
/// the person chose this over a minimal one). Read by running the login shell once per launch; nothing from
/// Beacon's own environment or vault is added, and Beacon's own variables are dropped.
public enum LoginEnvironment {
    /// Variables Beacon or tether set for themselves; never passed to a run.
    static func excluded(_ name: String) -> Bool {
        name.hasPrefix("BEACON_") || name == "__CF_USER_TEXT_ENCODING" || name == "XPC_SERVICE_NAME"
            || name == "XPC_FLAGS"
    }

    /// `$SHELL -l -i -c` printing the environment NUL-separated after a marker, so anything the profile prints
    /// first is skipped. Interactive, because many people set `PATH` in `.zshrc`. Bounded by `timeout`.
    public static func read(timeout: TimeInterval = 10) -> [String: String]? {
        guard let entry = getpwuid(getuid()), let shellC = entry.pointee.pw_shell, let homeC = entry.pointee.pw_dir else {
            return nil
        }
        let shell = String(cString: shellC), home = String(cString: homeC)
        guard shell.hasPrefix("/") else { return nil }
        let marker = "__BEACON_ENVIRONMENT_\(randomHex(8))__"
        let output = run([shell, "-l", "-i", "-c", "printf '%s\\0' \(marker); /usr/bin/env -0"],
                         environment: ["HOME": home, "USER": NSUserName(), "LOGNAME": NSUserName(), "SHELL": shell,
                                       "PATH": "/usr/bin:/bin:/usr/sbin:/sbin", "TERM": "dumb",
                                       "TMPDIR": NSTemporaryDirectory()],
                         directory: home, timeout: timeout)
        return output.flatMap { parse($0, marker: marker) }
    }

    static func parse(_ data: Data, marker: String) -> [String: String]? {
        let fields = data.split(separator: 0, omittingEmptySubsequences: false).map { String(decoding: $0, as: UTF8.self) }
        guard let start = fields.lastIndex(of: marker) else { return nil }
        var environment: [String: String] = [:]
        for field in fields[(start + 1)...] {
            guard let equals = field.firstIndex(of: "="), equals != field.startIndex else { continue }
            let name = String(field[..<equals])
            if !excluded(name) { environment[name] = String(field[field.index(after: equals)...]) }
        }
        return environment["PATH"] == nil ? nil : environment
    }

    /// The first executable `name` on the environment's `PATH`.
    public static func which(_ name: String, in environment: [String: String]) -> String? {
        for directory in (environment["PATH"] ?? "").split(separator: ":") where directory.hasPrefix("/") {
            let path = "\(directory)/\(name)"
            var isDirectory: ObjCBool = false
            if FileManager.default.fileExists(atPath: path, isDirectory: &isDirectory), !isDirectory.boolValue,
               FileManager.default.isExecutableFile(atPath: path) {
                return path
            }
        }
        return nil
    }

    /// Runs `argv` with standard input from /dev/null and standard error discarded, as the leader of its own process
    /// group, and returns standard output (at most 8 MiB), or nil if it fails or outlasts `timeout`, when its whole
    /// group is killed.
    static func run(_ argv: [String], environment: [String: String], directory: String, timeout: TimeInterval)
        -> Data?
    {
        var fds: [Int32] = [-1, -1]
        guard pipe(&fds) == 0 else { return nil }
        _ = fcntl(fds[0], F_SETFD, FD_CLOEXEC)
        _ = fcntl(fds[1], F_SETFD, FD_CLOEXEC)
        let cArgs = argv.map { strdup($0) } + [nil]
        let cEnv = environment.map { strdup("\($0.key)=\($0.value)") } + [nil]
        defer {
            cArgs.forEach { free($0) }
            cEnv.forEach { free($0) }
        }
        let pid = tether_spawn_at(argv[0], cArgs, cEnv, -1, fds[1], -1, directory)
        close(fds[1])
        guard pid > 0 else {
            close(fds[0])
            return nil
        }
        let deadline = Date().addingTimeInterval(timeout)
        var output = Data(), buffer = [UInt8](repeating: 0, count: 65_536), timedOut = false
        while true {
            let left = deadline.timeIntervalSinceNow
            if left <= 0 {
                timedOut = true
                break
            }
            var fd = pollfd(fd: fds[0], events: Int16(POLLIN), revents: 0)
            let ready = poll(&fd, 1, Int32(min(left, 1) * 1000))
            if ready < 0 && errno == EINTR { continue }
            if ready == 0 { continue }
            let n = Darwin.read(fds[0], &buffer, buffer.count)
            if n < 0 && errno == EINTR { continue }
            if n <= 0 { break }
            if output.count < 8 << 20 { output.append(contentsOf: buffer[0..<min(n, (8 << 20) - output.count)]) }
        }
        close(fds[0])
        var status: Int32 = 0
        while !timedOut {
            let reaped = waitpid(pid, &status, WNOHANG)
            if reaped == pid { break }
            if reaped < 0 && errno != EINTR { return nil }
            if Date() >= deadline { timedOut = true } else { usleep(20_000) }
        }
        if timedOut {
            killpg(pid, SIGKILL)
            while waitpid(pid, &status, 0) < 0 && errno == EINTR {}
            return nil
        }
        // The shell is done; anything it left in its group goes with it.
        killpg(pid, SIGKILL)
        return status & 0x7f == 0 && (status >> 8) & 0xff == 0 ? output : nil
    }
}

/// The models opencode is configured with: `opencode models`, one `provider/model` id per line. Anything else on a
/// line is ignored. Uses a private server (`--standalone`), as runs do.
public enum OpencodeModels {
    public static func list(opencode: String, environment: [String: String], timeout: TimeInterval = 60) -> [String]? {
        let home = environment["HOME"] ?? NSHomeDirectory()
        guard let data = LoginEnvironment.run([opencode, "models", "--standalone"], environment: environment,
                                              directory: home, timeout: timeout) else { return nil }
        return parse(data)
    }

    static func parse(_ data: Data) -> [String] {
        var seen = Set<String>(), models: [String] = []
        for line in String(decoding: data, as: UTF8.self).split(whereSeparator: \.isNewline) {
            let id = line.trimmingCharacters(in: .whitespaces)
            if Field.matches(id, Field.model), seen.insert(id).inserted { models.append(id) }
        }
        return models
    }
}

/// A checkout as Beacon pins it for an approval (13.00, _Agent runs_): the directory's identity, so a replaced
/// checkout is refused, and the branch checked out.
public struct CheckoutIdentity: Equatable, Sendable {
    /// `vol:{device}/inode:{inode}`. Two names for one directory have the same id and share a claim.
    public let id: String
    public let branch: String
    /// The checkout has its own `bin/run`, which Beacon does not use.
    public let ownRunner: Bool

    public static func read(_ path: String) -> CheckoutIdentity? {
        var info = stat()
        guard lstat(path, &info) == 0, info.st_mode & S_IFMT == S_IFDIR else { return nil }
        let runner = path + "/bin/run"
        return CheckoutIdentity(id: "vol:\(info.st_dev)/inode:\(info.st_ino)", branch: branch(path),
                                ownRunner: FileManager.default.isExecutableFile(atPath: runner))
    }

    /// The branch in `.git/HEAD` (or a worktree's), `detached:{sha7}` for a detached head, `none` without Git.
    static func branch(_ path: String) -> String {
        var git = path + "/.git"
        if let text = try? String(contentsOfFile: git, encoding: .utf8), text.hasPrefix("gitdir: ") {
            let target = text.dropFirst("gitdir: ".count).trimmingCharacters(in: .whitespacesAndNewlines)
            git = target.hasPrefix("/") ? target : path + "/" + target
        }
        guard let head = try? String(contentsOfFile: git + "/HEAD", encoding: .utf8)
            .trimmingCharacters(in: .whitespacesAndNewlines) else { return "none" }
        let name: String
        if head.hasPrefix("ref: refs/heads/") {
            name = String(head.dropFirst("ref: refs/heads/".count))
        } else if Field.matches(head, "[0-9a-f]{40,64}") {
            name = "detached:" + head.prefix(7)
        } else {
            return "none"
        }
        return Field.matches(name, "[\\x21-\\x7e]{1,255}") ? name : "none"
    }
}

/// What opencode's `run --format json` said: the final assistant message, as the run's report, and the last error.
public struct RunReport: Equatable, Sendable {
    public var report: String?
    public var error: String?

    /// The report's limit in Signal: 32,000 Unicode scalar values.
    static let limit = Limits.text

    /// Reads the output line by line, never more than one line in memory, and skips lines over 4 MiB.
    public static func read(_ url: URL) -> RunReport {
        guard let handle = try? FileHandle(forReadingFrom: url) else { return RunReport() }
        defer { try? handle.close() }
        var parser = Parser()
        var pending = Data(), skipping = false
        while let chunk = try? handle.read(upToCount: 1 << 20), !chunk.isEmpty {
            var start = chunk.startIndex
            while let newline = chunk[start...].firstIndex(of: 0x0a) {
                if !skipping { pending.append(chunk[start..<newline]); parser.line(pending) }
                pending.removeAll(keepingCapacity: true)
                skipping = false
                start = chunk.index(after: newline)
            }
            if !skipping { pending.append(chunk[start...]) }
            if pending.count > 4 << 20 {
                pending.removeAll()
                skipping = true
            }
        }
        if !skipping, !pending.isEmpty { parser.line(pending) }
        return parser.result
    }

    struct Parser {
        /// Text parts of each assistant message, in order: (part id, text).
        var messages: [String: [(String, String)]] = [:]
        var last: String?
        var error: String?

        mutating func line(_ data: Data) {
            guard let event = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
                  let type = event["type"] as? String else { return }
            if type == "error" {
                let error = event["error"] as? [String: Any]
                let data = error?["data"] as? [String: Any]
                self.error = (error?["message"] as? String) ?? (data?["message"] as? String) ?? (error?["name"] as? String)
                return
            }
            guard type == "text", let part = event["part"] as? [String: Any], let text = part["text"] as? String else {
                return
            }
            let message = part["messageID"] as? String ?? "", id = part["id"] as? String ?? ""
            var parts = messages[message, default: []]
            if let index = parts.firstIndex(where: { $0.0 == id }) {
                parts[index].1 = text
            } else {
                parts.append((id, text))
            }
            messages[message] = parts
            last = message
        }

        var result: RunReport {
            var report: String?
            if let last, let parts = messages[last] {
                var text = ""
                for (id, part) in parts {
                    // opencode completes a message's last part after the session goes idle with a projected part
                    // (`prt_{message}_text-N`) holding only the rest of the text: that continues the part before.
                    if !text.isEmpty, !(id.hasPrefix("prt_") && id.contains("_text-")) { text += "\n\n" }
                    text += part
                }
                report = text.trimmingCharacters(in: .whitespacesAndNewlines)
            }
            return RunReport(report: report.flatMap { $0.isEmpty ? nil : RunReport.capped($0) },
                             error: error.flatMap { $0.isEmpty ? nil : RunReport.capped($0) })
        }
    }

    /// At most 32,000 scalars, keeping the end, where a report usually concludes, and saying so.
    static func capped(_ text: String) -> String {
        let scalars = text.unicodeScalars
        guard scalars.count > limit else { return text }
        let note = "[Beacon kept the last part of a longer report: \(scalars.count) characters in all.]\n\n"
        var kept = String.UnicodeScalarView()
        kept.append(contentsOf: scalars.suffix(limit - note.unicodeScalars.count))
        return note + String(kept)
    }

    /// The last `limit` scalars of a file's text, for a failed run's detail when opencode reported no error.
    static func tail(_ url: URL, limit: Int = 4_000) -> String? {
        guard let handle = try? FileHandle(forReadingFrom: url) else { return nil }
        defer { try? handle.close() }
        let size = (try? handle.seekToEnd()) ?? 0
        try? handle.seek(toOffset: size > 64 << 10 ? size - (64 << 10) : 0)
        let text = String(decoding: (try? handle.readToEnd()) ?? Data(), as: UTF8.self)
            .trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty else { return nil }
        var kept = String.UnicodeScalarView()
        kept.append(contentsOf: text.unicodeScalars.suffix(limit))
        return String(kept)
    }
}
