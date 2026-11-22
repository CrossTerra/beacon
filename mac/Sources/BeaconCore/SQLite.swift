import Foundation
import SQLite3

public struct SQLiteError: Error, CustomStringConvertible, Sendable {
    public let code: Int32
    public let message: String
    public var description: String { "SQLite \(code): \(message)" }
}

/// A minimal SQLite connection: prepared statements, text, integers and blobs. Not thread-safe; the journal
/// serialises every call.
final class SQLite {
    private var db: OpaquePointer?
    private static let transient = unsafeBitCast(-1, to: sqlite3_destructor_type.self)

    init(path: String) throws {
        let flags = SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE | SQLITE_OPEN_FULLMUTEX | SQLITE_OPEN_NOFOLLOW
        let rc = sqlite3_open_v2(path, &db, flags, nil)
        guard rc == SQLITE_OK else {
            let error = SQLiteError(code: rc, message: db.map { String(cString: sqlite3_errmsg($0)) } ?? "open")
            sqlite3_close(db)
            throw error
        }
    }

    deinit { sqlite3_close(db) }

    func execute(_ sql: String) throws {
        guard sqlite3_exec(db, sql, nil, nil, nil) == SQLITE_OK else { throw error() }
    }

    /// Runs `sql` with `values` bound in order, and returns every row.
    @discardableResult
    func query(_ sql: String, _ values: [Value] = []) throws -> [[Value]] {
        var statement: OpaquePointer?
        guard sqlite3_prepare_v2(db, sql, -1, &statement, nil) == SQLITE_OK else { throw error() }
        defer { sqlite3_finalize(statement) }
        for (i, value) in values.enumerated() {
            let index = Int32(i + 1)
            switch value {
            case .null: sqlite3_bind_null(statement, index)
            case .int(let v): sqlite3_bind_int64(statement, index, v)
            case .text(let v): sqlite3_bind_text(statement, index, v, -1, SQLite.transient)
            case .blob(let v):
                _ = v.withUnsafeBytes { sqlite3_bind_blob(statement, index, $0.baseAddress, Int32($0.count), SQLite.transient) }
            }
        }
        var rows: [[Value]] = []
        while true {
            let rc = sqlite3_step(statement)
            if rc == SQLITE_DONE { return rows }
            guard rc == SQLITE_ROW else { throw error() }
            rows.append((0..<sqlite3_column_count(statement)).map { column in
                switch sqlite3_column_type(statement, column) {
                case SQLITE_INTEGER: return .int(sqlite3_column_int64(statement, column))
                case SQLITE_TEXT: return .text(String(cString: sqlite3_column_text(statement, column)))
                case SQLITE_BLOB:
                    let count = Int(sqlite3_column_bytes(statement, column))
                    return .blob(count == 0 ? Data() : Data(bytes: sqlite3_column_blob(statement, column), count: count))
                default: return .null
                }
            })
        }
    }

    /// Runs `body` in one IMMEDIATE transaction: all of it commits, or none of it.
    func transaction<T>(_ body: () throws -> T) throws -> T {
        try execute("BEGIN IMMEDIATE")
        do {
            let result = try body()
            try execute("COMMIT")
            return result
        } catch {
            try? execute("ROLLBACK")
            throw error
        }
    }

    private func error() -> SQLiteError {
        SQLiteError(code: sqlite3_errcode(db), message: String(cString: sqlite3_errmsg(db)))
    }

    enum Value: Equatable {
        case null
        case int(Int64)
        case text(String)
        case blob(Data)

        var int: Int64? { if case .int(let v) = self { return v } else { return nil } }
        var text: String? { if case .text(let v) = self { return v } else { return nil } }
        var blob: Data? { if case .blob(let v) = self { return v } else { return nil } }
    }
}
