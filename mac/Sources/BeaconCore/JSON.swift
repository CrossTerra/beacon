import CryptoKit
import Foundation

/// A JSON value as Signal's approval contract allows it: integers only, no floats, no duplicate keys, no lone
/// surrogates. Objects keep no order; canonical output sorts their keys.
public enum JSON: Equatable, Sendable {
    case null
    case bool(Bool)
    case int(Int64)
    case string(String)
    case array([JSON])
    case object([String: JSON])

    /// The largest integer `JSON.parse` keeps exactly.
    public static let maxSafeInteger: Int64 = 9_007_199_254_740_991

    public subscript(key: String) -> JSON? {
        if case .object(let fields) = self { return fields[key] }
        return nil
    }

    public var stringValue: String? {
        if case .string(let value) = self { return value }
        return nil
    }

    public var intValue: Int64? {
        if case .int(let value) = self { return value }
        return nil
    }

    public var objectValue: [String: JSON]? {
        if case .object(let value) = self { return value }
        return nil
    }
}

public struct JSONError: Error, Equatable, Sendable {
    public let offset: Int
    public let reason: String
}

extension JSON {
    /// Parses one JSON value from UTF-8 bytes, refusing anything the contract refuses.
    public static func parse(_ data: Data) throws -> JSON {
        var parser = Parser(bytes: [UInt8](data))
        parser.skipSpace()
        let value = try parser.value(depth: 0)
        parser.skipSpace()
        guard parser.index == parser.bytes.count else { throw parser.error("trailing bytes") }
        return value
    }

    /// Canonical JSON (13.00-D6): keys sorted by UTF-16 code unit, no insignificant whitespace, integers only,
    /// strings as `JSON.stringify` writes them.
    public var canonical: String {
        var out = ""
        write(to: &out)
        return out
    }

    public var canonicalData: Data { Data(canonical.utf8) }

    /// `sha256:` and the hex SHA-256 of the canonical bytes.
    public var hash: String {
        "sha256:" + SHA256.hash(data: canonicalData).map { String(format: "%02x", $0) }.joined()
    }

    private func write(to out: inout String) {
        switch self {
        case .null: out += "null"
        case .bool(let value): out += value ? "true" : "false"
        case .int(let value): out += String(value)
        case .string(let value): JSON.quote(value, into: &out)
        case .array(let values):
            out += "["
            for (i, value) in values.enumerated() {
                if i > 0 { out += "," }
                value.write(to: &out)
            }
            out += "]"
        case .object(let fields):
            out += "{"
            let keys = fields.keys.sorted { $0.utf16.lexicographicallyPrecedes($1.utf16) }
            for (i, key) in keys.enumerated() {
                if i > 0 { out += "," }
                JSON.quote(key, into: &out)
                out += ":"
                fields[key]!.write(to: &out)
            }
            out += "}"
        }
    }

    private static func quote(_ value: String, into out: inout String) {
        out += "\""
        for scalar in value.unicodeScalars {
            switch scalar {
            case "\"": out += "\\\""
            case "\\": out += "\\\\"
            case "\u{08}": out += "\\b"
            case "\u{0C}": out += "\\f"
            case "\n": out += "\\n"
            case "\r": out += "\\r"
            case "\t": out += "\\t"
            default:
                if scalar.value < 0x20 {
                    out += String(format: "\\u%04x", scalar.value)
                } else {
                    out.unicodeScalars.append(scalar)
                }
            }
        }
        out += "\""
    }
}

private struct Parser {
    let bytes: [UInt8]
    var index = 0
    static let maxDepth = 64

    init(bytes: [UInt8]) {
        self.bytes = bytes
    }

    func error(_ reason: String) -> JSONError {
        JSONError(offset: index, reason: reason)
    }

    mutating func skipSpace() {
        while index < bytes.count, [0x20, 0x09, 0x0A, 0x0D].contains(bytes[index]) { index += 1 }
    }

    mutating func value(depth: Int) throws -> JSON {
        guard depth < Parser.maxDepth else { throw error("too deep") }
        guard index < bytes.count else { throw error("unexpected end") }
        switch bytes[index] {
        case UInt8(ascii: "{"): return try object(depth: depth)
        case UInt8(ascii: "["): return try array(depth: depth)
        case UInt8(ascii: "\""): return .string(try string())
        case UInt8(ascii: "t"): try literal("true"); return .bool(true)
        case UInt8(ascii: "f"): try literal("false"); return .bool(false)
        case UInt8(ascii: "n"): try literal("null"); return .null
        default: return .int(try integer())
        }
    }

    mutating func literal(_ word: String) throws {
        let expected = Array(word.utf8)
        guard index + expected.count <= bytes.count, Array(bytes[index..<index + expected.count]) == expected else {
            throw error("invalid literal")
        }
        index += expected.count
    }

    mutating func integer() throws -> Int64 {
        let start = index
        if index < bytes.count, bytes[index] == UInt8(ascii: "-") { index += 1 }
        let digits = index
        while index < bytes.count, (0x30...0x39).contains(bytes[index]) { index += 1 }
        guard index > digits else { throw error("invalid value") }
        guard !(bytes[digits] == 0x30 && index - digits > 1) else { throw error("leading zero") }
        if index < bytes.count, [UInt8(ascii: "."), UInt8(ascii: "e"), UInt8(ascii: "E")].contains(bytes[index]) {
            throw error("not an integer")
        }
        guard index - digits <= 16, let value = Int64(String(decoding: bytes[start..<index], as: UTF8.self)),
              abs(value) <= JSON.maxSafeInteger else { throw error("integer out of range") }
        return value
    }

    mutating func array(depth: Int) throws -> JSON {
        index += 1
        var values: [JSON] = []
        skipSpace()
        if index < bytes.count, bytes[index] == UInt8(ascii: "]") {
            index += 1
            return .array(values)
        }
        while true {
            skipSpace()
            values.append(try value(depth: depth + 1))
            skipSpace()
            guard index < bytes.count else { throw error("unexpected end") }
            if bytes[index] == UInt8(ascii: ",") { index += 1; continue }
            if bytes[index] == UInt8(ascii: "]") { index += 1; return .array(values) }
            throw error("expected , or ]")
        }
    }

    mutating func object(depth: Int) throws -> JSON {
        index += 1
        var fields: [String: JSON] = [:]
        skipSpace()
        if index < bytes.count, bytes[index] == UInt8(ascii: "}") {
            index += 1
            return .object(fields)
        }
        while true {
            skipSpace()
            guard index < bytes.count, bytes[index] == UInt8(ascii: "\"") else { throw error("expected a key") }
            let key = try string()
            guard fields[key] == nil else { throw error("duplicate key") }
            skipSpace()
            guard index < bytes.count, bytes[index] == UInt8(ascii: ":") else { throw error("expected :") }
            index += 1
            skipSpace()
            fields[key] = try value(depth: depth + 1)
            skipSpace()
            guard index < bytes.count else { throw error("unexpected end") }
            if bytes[index] == UInt8(ascii: ",") { index += 1; continue }
            if bytes[index] == UInt8(ascii: "}") { index += 1; return .object(fields) }
            throw error("expected , or }")
        }
    }

    mutating func hex4() throws -> UInt32 {
        guard index + 4 <= bytes.count,
              let value = UInt32(String(decoding: bytes[index..<index + 4], as: UTF8.self), radix: 16) else {
            throw error("invalid escape")
        }
        index += 4
        return value
    }

    mutating func string() throws -> String {
        index += 1
        var scalars = String.UnicodeScalarView()
        var run = index
        func flush(_ end: Int) throws {
            guard run < end else { return }
            guard let text = String(bytes: bytes[run..<end], encoding: .utf8) else { throw error("invalid UTF-8") }
            scalars.append(contentsOf: text.unicodeScalars)
        }
        while true {
            guard index < bytes.count else { throw error("unterminated string") }
            let byte = bytes[index]
            if byte == UInt8(ascii: "\"") {
                try flush(index)
                index += 1
                return String(scalars)
            }
            guard byte >= 0x20 else { throw error("control character in string") }
            guard byte == UInt8(ascii: "\\") else {
                index += 1
                continue
            }
            try flush(index)
            index += 1
            guard index < bytes.count else { throw error("unterminated string") }
            let escape = bytes[index]
            index += 1
            switch escape {
            case UInt8(ascii: "\""): scalars.append("\"")
            case UInt8(ascii: "\\"): scalars.append("\\")
            case UInt8(ascii: "/"): scalars.append("/")
            case UInt8(ascii: "b"): scalars.append("\u{08}")
            case UInt8(ascii: "f"): scalars.append("\u{0C}")
            case UInt8(ascii: "n"): scalars.append("\n")
            case UInt8(ascii: "r"): scalars.append("\r")
            case UInt8(ascii: "t"): scalars.append("\t")
            case UInt8(ascii: "u"):
                var code = try hex4()
                if (0xD800...0xDBFF).contains(code) {
                    guard index + 2 <= bytes.count, bytes[index] == UInt8(ascii: "\\"), bytes[index + 1] == UInt8(ascii: "u") else {
                        throw error("lone surrogate")
                    }
                    index += 2
                    let low = try hex4()
                    guard (0xDC00...0xDFFF).contains(low) else { throw error("lone surrogate") }
                    code = 0x10000 + ((code - 0xD800) << 10) + (low - 0xDC00)
                } else if (0xDC00...0xDFFF).contains(code) {
                    throw error("lone surrogate")
                }
                scalars.append(Unicode.Scalar(code)!)
            default:
                throw error("invalid escape")
            }
            run = index
        }
    }
}

extension JSON {
    /// Serialises the value for a socket reply or a record. Fields come out in canonical order.
    public static func encode(_ value: JSON) -> Data { value.canonicalData }
}
