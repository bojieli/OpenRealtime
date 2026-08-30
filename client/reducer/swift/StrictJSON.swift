import Foundation

enum StrictJSONError: LocalizedError {
    case message(String)

    var errorDescription: String? {
        switch self {
        case .message(let message): return message
        }
    }
}

/// A bounded corpus must not inherit JSONSerialization's duplicate-key
/// behavior. This parser decodes JSON while rejecting duplicate keys (including
/// escape-equivalent keys), trailing values, control characters, and non-finite
/// numbers before the typed reducer sees the document.
struct StrictJSONParser {
    private let bytes: [UInt8]
    private let maxDepth: Int
    private var offset = 0

    init(data: Data, maxDepth: Int = 256) {
        bytes = Array(data)
        self.maxDepth = maxDepth
    }

    mutating func parse() throws -> Any {
        let result = try parseValue(depth: 0)
        skipWhitespace()
        guard offset == bytes.count else {
            throw StrictJSONError.message("trailing JSON token at offset \(offset)")
        }
        return result
    }

    private mutating func skipWhitespace() {
        while offset < bytes.count && [0x20, 0x09, 0x0a, 0x0d].contains(bytes[offset]) {
            offset += 1
        }
    }

    private mutating func parseValue(depth: Int) throws -> Any {
        skipWhitespace()
        guard offset < bytes.count else {
            throw StrictJSONError.message("expected JSON value at offset \(offset)")
        }
        switch bytes[offset] {
        case 0x7b:
            guard depth < maxDepth else { throw StrictJSONError.message("JSON nesting exceeds \(maxDepth)") }
            return try parseObject(depth: depth)
        case 0x5b:
            guard depth < maxDepth else { throw StrictJSONError.message("JSON nesting exceeds \(maxDepth)") }
            return try parseArray(depth: depth)
        case 0x22: return try parseString()
        case 0x74:
            try consumeLiteral("true")
            return true
        case 0x66:
            try consumeLiteral("false")
            return false
        case 0x6e:
            try consumeLiteral("null")
            return NSNull()
        default:
            return try parseNumber()
        }
    }

    private mutating func parseObject(depth: Int) throws -> [String: Any] {
        offset += 1
        skipWhitespace()
        var result: [String: Any] = [:]
        var keys = Set<String>()
        if consumeIf(0x7d) { return result }
        while true {
            skipWhitespace()
            guard offset < bytes.count, bytes[offset] == 0x22 else {
                throw StrictJSONError.message("expected object key at offset \(offset)")
            }
            let key = try parseString()
            guard keys.insert(key).inserted else {
                throw StrictJSONError.message("duplicate JSON key \(quoted(key))")
            }
            skipWhitespace()
            guard consumeIf(0x3a) else {
                throw StrictJSONError.message("expected ':' after object key at offset \(offset)")
            }
            result[key] = try parseValue(depth: depth + 1)
            skipWhitespace()
            if consumeIf(0x7d) { return result }
            guard consumeIf(0x2c) else {
                throw StrictJSONError.message("expected ',' or '}' at offset \(offset)")
            }
        }
    }

    private mutating func parseArray(depth: Int) throws -> [Any] {
        offset += 1
        skipWhitespace()
        var result: [Any] = []
        if consumeIf(0x5d) { return result }
        while true {
            result.append(try parseValue(depth: depth + 1))
            skipWhitespace()
            if consumeIf(0x5d) { return result }
            guard consumeIf(0x2c) else {
                throw StrictJSONError.message("expected ',' or ']' at offset \(offset)")
            }
        }
    }

    private mutating func parseString() throws -> String {
        let start = offset
        offset += 1
        while offset < bytes.count {
            let byte = bytes[offset]
            offset += 1
            if byte == 0x22 {
                let token = Data(bytes[start..<offset])
                do {
                    guard let value = try JSONSerialization.jsonObject(
                        with: token, options: [.fragmentsAllowed]
                    ) as? String else {
                        throw StrictJSONError.message("invalid JSON string at offset \(start)")
                    }
                    return value
                } catch let error as StrictJSONError {
                    throw error
                } catch {
                    throw StrictJSONError.message(
                        "invalid JSON string at offset \(start): \(error.localizedDescription)"
                    )
                }
            }
            guard byte >= 0x20 else {
                throw StrictJSONError.message("control character in JSON string at offset \(offset - 1)")
            }
            if byte == 0x5c {
                guard offset < bytes.count else {
                    throw StrictJSONError.message("unterminated JSON escape at offset \(offset)")
                }
                if bytes[offset] == 0x75 {
                    guard offset + 4 < bytes.count else {
                        throw StrictJSONError.message("short Unicode escape at offset \(offset - 1)")
                    }
                    for digit in bytes[(offset + 1)...(offset + 4)] where !Self.isHex(digit) {
                        _ = digit
                        throw StrictJSONError.message("invalid Unicode escape at offset \(offset - 1)")
                    }
                    offset += 5
                } else {
                    guard [0x22, 0x5c, 0x2f, 0x62, 0x66, 0x6e, 0x72, 0x74].contains(bytes[offset]) else {
                        throw StrictJSONError.message("invalid JSON escape at offset \(offset - 1)")
                    }
                    offset += 1
                }
            }
        }
        throw StrictJSONError.message("unterminated JSON string at offset \(start)")
    }

    private mutating func parseNumber() throws -> NSNumber {
        let start = offset
        if consumeIf(0x2d), offset >= bytes.count {
            throw StrictJSONError.message("invalid JSON number at offset \(start)")
        }
        if consumeIf(0x30) {
            if offset < bytes.count, Self.isDigit(bytes[offset]) {
                throw StrictJSONError.message("leading zero in JSON number at offset \(start)")
            }
        } else {
            guard offset < bytes.count, (0x31...0x39).contains(bytes[offset]) else {
                throw StrictJSONError.message("invalid JSON value at offset \(start)")
            }
            while offset < bytes.count, Self.isDigit(bytes[offset]) { offset += 1 }
        }
        if consumeIf(0x2e) {
            guard offset < bytes.count, Self.isDigit(bytes[offset]) else {
                throw StrictJSONError.message("invalid JSON fraction at offset \(start)")
            }
            while offset < bytes.count, Self.isDigit(bytes[offset]) { offset += 1 }
        }
        if offset < bytes.count, bytes[offset] == 0x65 || bytes[offset] == 0x45 {
            offset += 1
            if offset < bytes.count, bytes[offset] == 0x2b || bytes[offset] == 0x2d { offset += 1 }
            guard offset < bytes.count, Self.isDigit(bytes[offset]) else {
                throw StrictJSONError.message("invalid JSON exponent at offset \(start)")
            }
            while offset < bytes.count, Self.isDigit(bytes[offset]) { offset += 1 }
        }
        let token = Data(bytes[start..<offset])
        do {
            guard let number = try JSONSerialization.jsonObject(
                with: token, options: [.fragmentsAllowed]
            ) as? NSNumber, number.doubleValue.isFinite else {
                throw StrictJSONError.message("non-finite JSON number at offset \(start)")
            }
            return number
        } catch let error as StrictJSONError {
            throw error
        } catch {
            throw StrictJSONError.message("invalid JSON number at offset \(start)")
        }
    }

    private mutating func consumeLiteral(_ literal: String) throws {
        let expected = Array(literal.utf8)
        guard offset + expected.count <= bytes.count,
              Array(bytes[offset..<(offset + expected.count)]) == expected else {
            throw StrictJSONError.message("invalid JSON literal at offset \(offset)")
        }
        offset += expected.count
    }

    private mutating func consumeIf(_ byte: UInt8) -> Bool {
        guard offset < bytes.count, bytes[offset] == byte else { return false }
        offset += 1
        return true
    }

    private static func isDigit(_ byte: UInt8) -> Bool { (0x30...0x39).contains(byte) }
    private static func isHex(_ byte: UInt8) -> Bool {
        (0x30...0x39).contains(byte) || (0x41...0x46).contains(byte) || (0x61...0x66).contains(byte)
    }
}

func quoted(_ value: String) -> String {
    guard let data = try? JSONSerialization.data(withJSONObject: [value]),
          let encoded = String(data: data, encoding: .utf8) else { return value }
    return String(encoded.dropFirst().dropLast())
}
