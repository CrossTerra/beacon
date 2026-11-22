import BeaconSecurity
import Darwin
import Dispatch
import Foundation

/// A local client identity admitted at setup: a name for the record and a code requirement the connected
/// process must satisfy (13.00-D5). A path or user id alone is not an identity.
public struct AdmittedClient: Equatable, Sendable {
    public let name: String
    public let requirement: String

    public init(name: String, requirement: String) {
        self.name = name
        self.requirement = requirement
    }
}

/// The socket's limits (13.00, _The local API_).
public struct ServerLimits: Sendable {
    public var connections = 32
    public var waits = 16
    public var maxWait = 300
    public var pendingPerClient = 20
    public var proposalsPerClientPerHour = 60
    public var pendingTotal = 40
    /// One frame, including its newline.
    public var frameBytes = 256 * 1024
    /// An incomplete frame is dropped, with its connection, after this long.
    public var frameTimeout: TimeInterval = 10
    /// A connection with nothing in flight is closed after this long.
    public var idleTimeout: TimeInterval = 60

    public init() {}
}

public enum ServerError: Error, Equatable {
    case path
    case directory
    case inUse
    case system(Int32)
}

/// The proposal socket. One serial queue owns every connection and the book. Operations are `types`,
/// `propose`, `get`, `wait` and `withdraw`; there is no approve, list or history operation. Each connection is
/// admitted by the kernel's audit token for its peer before a byte is read.
public final class ProposalServer: @unchecked Sendable {
    public let path: String
    private let clients: [AdmittedClient]
    private let book: ProposalBook
    private let enabledLock = NSLock()
    private var enabledTypes: [ActionType]
    /// The types `types` lists and `propose` accepts. The app changes it when an account is added or removed.
    public var enabled: [ActionType] {
        get { enabledLock.withLock { enabledTypes } }
        set { enabledLock.withLock { enabledTypes = newValue } }
    }
    private let limits: ServerLimits
    private let now: @Sendable () -> Date
    private let queue: DispatchQueue
    private let uid = getuid()

    private var listener: Int32 = -1
    private var acceptSource: DispatchSourceRead?
    private var ticker: DispatchSourceTimer?
    private var connections: [ObjectIdentifier: Connection] = [:]
    private var proposed: [String: [Date]] = [:]

    public init(path: String, clients: [AdmittedClient], book: ProposalBook, enabled: [ActionType],
                limits: ServerLimits = ServerLimits(), now: @escaping @Sendable () -> Date = { Date() },
                queue: DispatchQueue = DispatchQueue(label: "dev.beacon.proposals")) {
        self.path = path
        self.clients = clients
        self.book = book
        enabledTypes = enabled
        self.limits = limits
        self.now = now
        self.queue = queue
        // A change can come from the window on another thread; waits are answered on this queue.
        book.changed = { [weak self] id in
            self?.queue.async { [weak self] in self?.answerWaits(for: id) }
        }
    }

    /// Opens the listener in a private directory: `0700`, owned by this user. A live listener at the path
    /// is refused, so a second app cannot take over the first's socket.
    public func start() throws {
        try queue.sync {
            let directory = (path as NSString).deletingLastPathComponent
            if mkdir(directory, 0o700) != 0 && errno != EEXIST { throw ServerError.directory }
            var info = stat()
            guard lstat(directory, &info) == 0, (info.st_mode & S_IFMT) == S_IFDIR, info.st_uid == uid,
                  info.st_mode & 0o077 == 0 else { throw ServerError.directory }
            var address = try ProposalServer.address(path)
            if ProposalServer.connectable(&address) { throw ServerError.inUse }
            unlink(path)
            let fd = socket(AF_UNIX, SOCK_STREAM, 0)
            guard fd >= 0 else { throw ServerError.system(errno) }
            let bound = withUnsafePointer(to: &address) {
                $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                    bind(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
                }
            }
            guard fcntl(fd, F_SETFD, FD_CLOEXEC) == 0, bound == 0, chmod(path, 0o600) == 0,
                  fcntl(fd, F_SETFL, O_NONBLOCK) == 0, listen(fd, 16) == 0 else {
                let failure = errno
                close(fd)
                throw ServerError.system(failure)
            }
            listener = fd
            let source = DispatchSource.makeReadSource(fileDescriptor: fd, queue: queue)
            source.setEventHandler { [weak self] in self?.accept() }
            source.activate()
            acceptSource = source
            let ticker = DispatchSource.makeTimerSource(queue: queue)
            ticker.schedule(deadline: .now() + 1, repeating: 1)
            ticker.setEventHandler { [weak self] in self?.tick() }
            ticker.activate()
            self.ticker = ticker
        }
    }

    /// Closes the listener and every connection. A pending wait gets no answer.
    public func stop() {
        queue.sync {
            acceptSource?.cancel()
            acceptSource = nil
            ticker?.cancel()
            ticker = nil
            if listener >= 0 {
                close(listener)
                listener = -1
                unlink(path)
            }
            for connection in connections.values { connection.close() }
            connections.removeAll()
        }
    }

    static func address(_ path: String) throws -> sockaddr_un {
        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        let bytes = Array(path.utf8)
        guard !bytes.isEmpty, bytes.count < MemoryLayout.size(ofValue: address.sun_path) else { throw ServerError.path }
        withUnsafeMutableBytes(of: &address.sun_path) { buffer in
            buffer.copyBytes(from: bytes)
        }
        return address
    }

    private static func connectable(_ address: inout sockaddr_un) -> Bool {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { return false }
        defer { close(fd) }
        return withUnsafePointer(to: &address) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) == 0
            }
        }
    }

    private func accept() {
        while true {
            let fd = Darwin.accept(listener, nil, nil)
            guard fd >= 0 else { return }
            guard connections.count < limits.connections, let (client, pid, executable) = admit(fd) else {
                close(fd)
                continue
            }
            let connection = Connection(fd: fd, client: client, pid: pid, executable: executable, queue: queue, now: now)
            connection.onFrame = { [weak self, weak connection] frame in
                guard let self, let connection else { return }
                self.handle(frame, on: connection)
            }
            connection.onClose = { [weak self, weak connection] in
                guard let self, let connection else { return }
                self.connections[ObjectIdentifier(connection)] = nil
            }
            connections[ObjectIdentifier(connection)] = connection
            connection.start(limit: limits.frameBytes)
        }
    }

    /// The peer must run as this user and satisfy an admitted requirement. Nothing has been read yet.
    private func admit(_ fd: Int32) -> (AdmittedClient, pid_t, String?)? {
        guard fcntl(fd, F_SETFD, FD_CLOEXEC) == 0, fcntl(fd, F_SETFL, 0) == 0 else { return nil }
        var pid: pid_t = -1
        var path = [CChar](repeating: 0, count: Int(PATH_MAX))
        let index = clients.map { strdup($0.requirement) }.withUnsafeBufferPointer { pointers -> Int32 in
            defer { pointers.forEach { free($0) } }
            let requirements = pointers.map { UnsafePointer<CChar>($0) }
            return requirements.withUnsafeBufferPointer {
                beacon_peer_match(fd, uid, $0.baseAddress, Int32(clients.count), &pid, &path, path.count)
            }
        }
        guard index >= 0 else { return nil }
        // Replies go out with a bounded wait, so a client that stops reading cannot hold the queue.
        var timeout = timeval(tv_sec: 5, tv_usec: 0)
        setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, socklen_t(MemoryLayout<timeval>.size))
        var one: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))
        let executable = String(decoding: path.prefix { $0 != 0 }.map { UInt8(bitPattern: $0) }, as: UTF8.self)
        return (clients[Int(index)], pid, executable.isEmpty ? nil : executable)
    }

    private func tick() {
        let date = now()
        for connection in Array(connections.values) {
            if let wait = connection.wait, date >= wait.deadline {
                connection.wait = nil
                if let proposal = book.proposal(wait.id, at: date) { connection.reply(status(proposal)) }
                connection.resume()
            } else if connection.expired(at: date, frame: limits.frameTimeout, idle: limits.idleTimeout) {
                connection.close()
            }
        }
    }

    private func answerWaits(for id: String) {
        let date = now()
        for connection in connections.values {
            guard let wait = connection.wait, wait.id == id, let proposal = book.proposal(id, at: date),
                  proposal.version > wait.after else { continue }
            connection.wait = nil
            connection.reply(status(proposal))
            connection.resume()
        }
    }

    private func handle(_ frame: Data, on connection: Connection) {
        let request: [String: JSON]
        do {
            guard let fields = try JSON.parse(frame).objectValue else { throw Invalid(field: "") }
            request = fields
        } catch {
            connection.reply(ProposalServer.error("invalid_frame"))
            return
        }
        let date = now()
        connection.lastActive = date
        switch request["op"]?.stringValue {
        case "types":
            guard request.count == 1 else { return connection.reply(ProposalServer.invalid("")) }
            connection.reply(.object(["types": .array(enabled.map {
                .object(["type": .string($0.rawValue), "params": $0.paramsSchema])
            })]))
        case "propose":
            propose(request, on: connection, at: date)
        case "get":
            guard let id = owned(request, keys: ["op", "id"], by: connection, at: date) else { return }
            connection.reply(status(id))
        case "withdraw":
            guard let proposal = owned(request, keys: ["op", "id"], by: connection, at: date) else { return }
            do {
                connection.reply(status(try book.withdraw(proposal.id, at: date)))
            } catch {
                connection.reply(ProposalServer.error("not_pending"))
            }
        case "wait":
            wait(request, on: connection, at: date)
        default:
            connection.reply(ProposalServer.invalid("/op"))
        }
    }

    private func propose(_ request: [String: JSON], on connection: Connection, at date: Date) {
        let parsed: ProposalRequest
        do {
            parsed = try ProposalRequest(request)
        } catch let invalid as Invalid {
            return connection.reply(ProposalServer.invalid(invalid.field))
        } catch {
            return connection.reply(ProposalServer.invalid(""))
        }
        guard enabled.contains(parsed.type) else { return connection.reply(ProposalServer.error("type_disabled")) }
        let client = connection.client.name
        let recent = (proposed[client] ?? []).filter { date.timeIntervalSince($0) < 3600 }
        proposed[client] = recent
        guard recent.count < limits.proposalsPerClientPerHour,
              book.pendingCount(client: client, at: date) < limits.pendingPerClient,
              book.pendingCount(client: nil, at: date) < limits.pendingTotal else {
            return connection.reply(ProposalServer.error("busy"))
        }
        let proposal = Proposal(request: parsed, client: client, at: date, clientPath: connection.executable,
                                clientRequirement: connection.client.requirement)
        do {
            try book.add(proposal)
        } catch {
            return connection.reply(ProposalServer.error("unavailable"))
        }
        proposed[client, default: []].append(date)
        var reply: [String: JSON] = [
            "id": .string(proposal.id), "params_hash": .string(proposal.paramsHash),
            "status": .string(proposal.status.rawValue), "version": .int(proposal.version),
        ]
        // A run's reports reach the session that asked by this id (docs/approval-app.md, _Delivering a run's report_).
        reply["runner_id"] = book.runnerID(proposal.id).map(JSON.string)
        connection.reply(.object(reply))
    }

    private func wait(_ request: [String: JSON], on connection: Connection, at date: Date) {
        guard let proposal = owned(request, keys: ["op", "id", "after", "timeout"], by: connection, at: date) else { return }
        guard let after = request["after"]?.intValue, after >= 0 else { return connection.reply(ProposalServer.invalid("/after")) }
        guard let timeout = request["timeout"]?.intValue, (1...Int64(limits.maxWait)).contains(timeout) else {
            return connection.reply(ProposalServer.invalid("/timeout"))
        }
        if proposal.version > after { return connection.reply(status(proposal)) }
        guard connections.values.filter({ $0.wait != nil }).count < limits.waits else {
            return connection.reply(ProposalServer.error("busy"))
        }
        connection.wait = Connection.Wait(id: proposal.id, after: after, deadline: date.addingTimeInterval(TimeInterval(timeout)))
        connection.pause()
    }

    /// The proposal named by `id`, only if this connection's client identity proposed it. Any other id reads
    /// as not found, so one client cannot learn another's ids.
    private func owned(_ request: [String: JSON], keys: Set<String>, by connection: Connection, at date: Date) -> Proposal? {
        if let extra = request.keys.first(where: { !keys.contains($0) }) {
            connection.reply(ProposalServer.invalid("/" + extra))
            return nil
        }
        guard let id = request["id"]?.stringValue, PublicID.isValid(id, prefix: "apr") else {
            connection.reply(ProposalServer.invalid("/id"))
            return nil
        }
        guard let proposal = book.proposal(id, at: date), proposal.client == connection.client.name else {
            connection.reply(ProposalServer.error("not_found"))
            return nil
        }
        return proposal
    }

    private func status(_ id: Proposal) -> JSON {
        .object(["id": .string(id.id), "status": .string(id.status.rawValue), "version": .int(id.version)])
    }

    static func error(_ code: String) -> JSON {
        .object(["error": .string(code)])
    }

    static func invalid(_ field: String) -> JSON {
        .object(["error": .string("invalid"), "field": .string(field)])
    }
}

/// One admitted connection. Frames are processed one at a time. During a wait, bytes are still read (and
/// bounded), so a client that disconnects frees its wait at once.
final class Connection {
    struct Wait {
        let id: String
        let after: Int64
        let deadline: Date
    }

    let fd: Int32
    let client: AdmittedClient
    let pid: pid_t
    /// The executable the kernel's code reference for the peer names.
    let executable: String?
    var onFrame: ((Data) -> Void)?
    var onClose: (() -> Void)?
    var wait: Wait?
    var lastActive: Date

    private let source: DispatchSourceRead
    private let now: () -> Date
    private var buffer = Data()
    private var partialSince: Date?
    private var paused = false
    private var closed = false
    private var limit = 0

    init(fd: Int32, client: AdmittedClient, pid: pid_t, executable: String?, queue: DispatchQueue,
         now: @escaping () -> Date) {
        self.fd = fd
        self.client = client
        self.pid = pid
        self.executable = executable
        self.now = now
        lastActive = now()
        source = DispatchSource.makeReadSource(fileDescriptor: fd, queue: queue)
    }

    func start(limit: Int) {
        self.limit = limit
        source.setEventHandler { [weak self] in self?.readable() }
        let fd = self.fd
        source.setCancelHandler { Darwin.close(fd) }
        source.activate()
    }

    private func readable() {
        var chunk = [UInt8](repeating: 0, count: 64 * 1024)
        let n = read(fd, &chunk, chunk.count)
        guard n > 0 else { return close() }
        if buffer.isEmpty { partialSince = now() }
        buffer.append(contentsOf: chunk[0..<n])
        process()
        // While paused in a wait, frames queue up unread. At most one frame and a little more may wait here,
        // paused or not; past that the connection closes.
        if !closed && buffer.count > limit + Connection.margin { tooLarge() }
    }

    /// Bytes allowed beyond one frame in a connection's buffer.
    static let margin = 4096

    private func process() {
        while !closed {
            guard let newline = buffer.firstIndex(of: UInt8(ascii: "\n")) else {
                // Without a newline the frame cannot fit once the buffer reaches the limit.
                if buffer.count >= limit { tooLarge() }
                return
            }
            guard newline - buffer.startIndex + 1 <= limit else { return tooLarge() }
            if paused { return }
            let frame = Data(buffer[buffer.startIndex..<newline])
            buffer = Data(buffer[(newline + 1)...])
            partialSince = buffer.isEmpty ? nil : now()
            onFrame?(frame)
        }
    }

    private func tooLarge() {
        reply(ProposalServer.error("frame_too_large"))
        close()
    }

    func pause() {
        paused = true
    }

    func resume() {
        paused = false
        process()
    }

    func expired(at date: Date, frame: TimeInterval, idle: TimeInterval) -> Bool {
        if let since = partialSince, wait == nil, date.timeIntervalSince(since) >= frame { return true }
        return wait == nil && buffer.isEmpty && date.timeIntervalSince(lastActive) >= idle
    }

    func reply(_ value: JSON) {
        guard !closed else { return }
        var data = JSON.encode(value)
        data.append(UInt8(ascii: "\n"))
        let ok = data.withUnsafeBytes { bytes -> Bool in
            var offset = 0
            while offset < bytes.count {
                let n = write(fd, bytes.baseAddress! + offset, bytes.count - offset)
                if n <= 0 { return false }
                offset += n
            }
            return true
        }
        if !ok { close() }
    }

    func close() {
        guard !closed else { return }
        closed = true
        wait = nil
        source.cancel()
        onClose?()
    }
}
