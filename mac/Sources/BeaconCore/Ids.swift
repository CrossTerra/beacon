import Foundation
import Security

/// Signal's public ids (DEC-32): a prefix and a UUIDv7 as 26 lowercase Crockford base32 characters, so ids sort as
/// their times do.
public enum PublicID {
    static let alphabet = Array("0123456789abcdefghjkmnpqrstvwxyz")

    /// A new id, `apr_…` for a proposal or `jrn_…` for a journal.
    public static func make(_ prefix: String, at date: Date = Date(), random: [UInt8]? = nil) -> String {
        let millis = UInt64(max(0, (date.timeIntervalSince1970 * 1000).rounded(.down)))
        let random = random ?? randomBytes(10)
        precondition(random.count == 10 && millis < 1 << 48)
        var bytes = [UInt8](repeating: 0, count: 16)
        for i in 0..<6 { bytes[i] = UInt8(truncatingIfNeeded: millis >> (40 - 8 * UInt64(i))) }
        bytes[6] = 0x70 | (random[0] & 0x0f)
        bytes[7] = random[1]
        bytes[8] = 0x80 | (random[2] & 0x3f)
        for i in 9..<16 { bytes[i] = random[i - 6] }
        return prefix + "_" + encode(bytes)
    }

    /// Whether `value` is `prefix_` and a UUIDv7 in Crockford base32.
    public static func isValid(_ value: String, prefix: String) -> Bool {
        guard value.hasPrefix(prefix + "_"), let bytes = decode(String(value.dropFirst(prefix.count + 1))) else { return false }
        return bytes[6] >> 4 == 7 && bytes[8] >> 6 == 2
    }

    /// The UUID bytes of an id body, compared to order two journals by time.
    public static func decode(_ body: String) -> [UInt8]? {
        let chars = Array(body)
        guard chars.count == 26 else { return nil }
        var high: UInt64 = 0, low: UInt64 = 0 // A 130-bit accumulator, as high:2 bits + 128.
        var top: UInt8 = 0
        for char in chars {
            guard let digit = alphabet.firstIndex(of: char) else { return nil }
            top = (top << 5) | UInt8(high >> 59)
            high = (high << 5) | (low >> 59)
            low = (low << 5) | UInt64(digit)
        }
        guard top == 0 else { return nil }
        var bytes = [UInt8](repeating: 0, count: 16)
        for i in 0..<8 {
            bytes[i] = UInt8(truncatingIfNeeded: high >> (56 - 8 * UInt64(i)))
            bytes[i + 8] = UInt8(truncatingIfNeeded: low >> (56 - 8 * UInt64(i)))
        }
        return bytes
    }

    static func encode(_ bytes: [UInt8]) -> String {
        var high: UInt64 = 0, low: UInt64 = 0
        for i in 0..<8 {
            high = (high << 8) | UInt64(bytes[i])
            low = (low << 8) | UInt64(bytes[i + 8])
        }
        var chars = [Character](repeating: "0", count: 26)
        for i in stride(from: 25, through: 0, by: -1) {
            chars[i] = alphabet[Int(low & 31)]
            low = (low >> 5) | ((high & 31) << 59)
            high >>= 5
        }
        return String(chars)
    }
}

/// Random bytes from the system generator.
public func randomBytes(_ count: Int) -> [UInt8] {
    var bytes = [UInt8](repeating: 0, count: count)
    precondition(SecRandomCopyBytes(kSecRandomDefault, count, &bytes) == errSecSuccess)
    return bytes
}

/// Lowercase hex of `count` random bytes: a proposal's salt (16 bytes, 13.00-D6) or a record's replay nonce.
public func randomHex(_ count: Int) -> String {
    randomBytes(count).map { String(format: "%02x", $0) }.joined()
}

/// The contract's hashes (docs/approval-app.md, _Hashes_). The salt is inside every hashed object.
public enum ApprovalHash {
    /// `params_hash`, or `approved_params_hash` with the final params.
    public static func params(type: String, params: JSON, salt: String) -> String {
        JSON.object(["type": .string(type), "params": params, "salt": .string(salt)]).hash
    }

    /// `approved_hash`: the proposal id and the final values the person approved. `origin` is null for a run.
    public static func approved(
        id: String, type: String, params: JSON, origin: String?, account: String, destination: JSON, salt: String
    ) -> String {
        JSON.object([
            "id": .string(id), "type": .string(type), "params": params,
            "origin": origin.map(JSON.string) ?? .null, "account": .string(account),
            "destination": destination, "salt": .string(salt),
        ]).hash
    }
}
