import Foundation
#if canImport(Darwin)
import Darwin
#elseif canImport(Glibc)
import Glibc
#endif

let maxCorpusBytes = 1 << 20

func onlyKeys(_ object: [String: Any], _ allowed: Set<String>, _ name: String) throws {
    for key in object.keys where !allowed.contains(key) {
        throw ReducerFailure("\(name) has unknown field \(quoted(key))")
    }
}

func stringArray(_ value: Any?, _ name: String, maximum: Int) throws -> [String] {
    guard let array = value as? [Any] else { throw ReducerFailure("\(name) must be an array") }
    return try array.map { try stringValue($0, name, maximum, allowEmpty: false) }
}

func validateSnapshot(_ snapshot: [String: Any], _ limits: ReducerLimits, _ name: String) throws {
    try onlyKeys(snapshot, [
        "now_ms", "connection", "session", "response", "conversation", "playout", "tools",
        "last_truncation", "last_error", "protocol_log",
    ], "\(name).snapshot")
    try onlyKeys(try objectValue(snapshot["connection"], "\(name).snapshot.connection"),
                 ["phase", "transport", "reason", "attempt", "next_retry_ms"],
                 "\(name).snapshot.connection")
    let session = try objectValue(snapshot["session"], "\(name).snapshot.session")
    try onlyKeys(session, ["id", "manual_turns", "openrealtime"], "\(name).snapshot.session")
    let negotiation = try objectValue(session["openrealtime"], "\(name).snapshot.session.openrealtime")
    try onlyKeys(negotiation, [
        "present", "version", "enabled", "observers", "available_observers", "debug_enabled", "video",
    ], "\(name).snapshot.session.openrealtime")
    try onlyKeys(try objectValue(negotiation["video"], "\(name).snapshot.session.openrealtime.video"),
                 ["format", "fps_cap", "max_dimension", "max_frame_bytes"],
                 "\(name).snapshot.session.openrealtime.video")
    try onlyKeys(try objectValue(snapshot["response"], "\(name).snapshot.response"),
                 ["id", "open", "status", "reason"], "\(name).snapshot.response")
    try onlyKeys(try objectValue(snapshot["playout"], "\(name).snapshot.playout"),
                 ["speaking", "item_id", "played_ms"], "\(name).snapshot.playout")
    try onlyKeys(try objectValue(snapshot["last_truncation"], "\(name).snapshot.last_truncation"),
                 ["item_id", "audio_end_ms"], "\(name).snapshot.last_truncation")

    guard let conversation = snapshot["conversation"] as? [Any],
          conversation.count <= limits.maxConversationItems else {
        throw ReducerFailure("\(name).snapshot conversation exceeds limit")
    }
    for (index, value) in conversation.enumerated() {
        try onlyKeys(try objectValue(value, "\(name).snapshot.conversation[\(index)]"),
                     ["item_id", "role", "channel", "text", "audio_deltas", "audio_done", "truncated_ms"],
                     "\(name).snapshot.conversation[\(index)]")
    }
    guard let tools = snapshot["tools"] as? [Any], tools.count <= limits.maxToolCalls else {
        throw ReducerFailure("\(name).snapshot tools exceed limit")
    }
    for (index, value) in tools.enumerated() {
        try onlyKeys(try objectValue(value, "\(name).snapshot.tools[\(index)]"),
                     ["call_id", "name", "arguments", "status", "output", "error"],
                     "\(name).snapshot.tools[\(index)]")
    }
    guard let protocolLog = snapshot["protocol_log"] as? [Any],
          protocolLog.count <= limits.maxProtocolLogEntries else {
        throw ReducerFailure("\(name).snapshot protocol log exceeds limit")
    }
    for (index, value) in protocolLog.enumerated() {
        try onlyKeys(try objectValue(value, "\(name).snapshot.protocol_log[\(index)]"),
                     ["at_ms", "direction", "type"], "\(name).snapshot.protocol_log[\(index)]")
    }
}

func validateCorpus(_ root: Any, _ byteCount: Int) throws -> ([String: Any], ReducerLimits) {
    guard byteCount > 0 else { throw ReducerFailure("reducer corpus is empty") }
    guard byteCount <= maxCorpusBytes else {
        throw ReducerFailure("reducer corpus exceeds \(maxCorpusBytes) bytes")
    }
    let corpus = try objectValue(root, "corpus")
    try onlyKeys(corpus, ["format_version", "limits", "vectors"], "corpus")
    guard try integerValue(corpus["format_version"], "format_version", minimum: 0) == 1 else {
        throw ReducerFailure("unsupported reducer corpus format_version")
    }
    let limits = ReducerLimits()
    guard limits.matches(try objectValue(corpus["limits"], "corpus.limits")) else {
        throw ReducerFailure("reducer corpus limits do not match format 1")
    }
    guard let vectors = corpus["vectors"] as? [Any], !vectors.isEmpty,
          vectors.count <= limits.maxVectors else {
        throw ReducerFailure("reducer corpus vector count is outside limits")
    }
    var names = Set<String>()
    for (vectorIndex, value) in vectors.enumerated() {
        let label = "vectors[\(vectorIndex)]"
        let vector = try objectValue(value, label)
        try onlyKeys(vector, ["name", "description", "steps", "expect"], label)
        let name = try requiredString(vector, "name", limits.maxStringBytes)
        _ = try requiredString(vector, "description", limits.maxStringBytes)
        guard names.insert(name).inserted else { throw ReducerFailure("duplicate reducer vector name \(quoted(name))") }
        guard let steps = vector["steps"] as? [Any], !steps.isEmpty,
              steps.count <= limits.maxStepsPerVector else {
            throw ReducerFailure("\(label).steps count is outside limits")
        }
        var previous: Int64 = 0
        for (stepIndex, stepValue) in steps.enumerated() {
            let stepLabel = "\(label).steps[\(stepIndex)]"
            let step = try objectValue(stepValue, stepLabel)
            try onlyKeys(step, ["at_ms", "operation"], stepLabel)
            let atMS = try integerValue(step["at_ms"], "\(stepLabel).at_ms", minimum: 0)
            guard atMS >= previous, atMS <= limits.maxVirtualTimeMS else {
                throw ReducerFailure("\(stepLabel) has invalid virtual time")
            }
            previous = atMS
            let operation = try objectValue(step["operation"], "\(stepLabel).operation")
            let operationFields: [String: Set<String>] = [
                "connect": ["transport"], "connected": [], "transport_lost": ["reason"],
                "retry": [], "disconnect": ["reason"], "inbound": ["event"],
                "session_update": ["session"], "typed_text": ["item_id", "text"],
                "end_turn": [], "playout": ["speaking", "item_id", "played_ms"],
                "cancel_response": [], "tool_result": ["call_id", "status", "output", "error"],
            ]
            let kind = try requiredString(operation, "kind", limits.maxStringBytes)
            guard let allowed = operationFields[kind] else {
                throw ReducerFailure("unknown operation kind \(quoted(kind))")
            }
            try onlyKeys(operation, allowed.union(["kind"]), "\(stepLabel).operation")
        }
        let expect = try objectValue(vector["expect"], "\(label).expect")
        try onlyKeys(expect, ["snapshot", "outbound", "errors"], "\(label).expect")
        try validateSnapshot(try objectValue(expect["snapshot"], "\(label).expect.snapshot"), limits, "\(label).expect")
        guard let outbound = expect["outbound"] as? [Any], outbound.count <= limits.maxOutboundEvents else {
            throw ReducerFailure("\(label).expect.outbound exceeds limit")
        }
        for (index, event) in outbound.enumerated() {
            _ = try objectValue(event, "\(label).expect.outbound[\(index)]")
        }
        guard let errors = expect["errors"] as? [Any], errors.count <= limits.maxErrorsPerVector else {
            throw ReducerFailure("\(label).expect.errors exceeds limit")
        }
        _ = try errors.map { try stringValue($0, "expected error", limits.maxStringBytes) }
    }
    return (corpus, limits)
}

func canonicalJSON(_ value: Any) throws -> Data {
    guard JSONSerialization.isValidJSONObject(value) else {
        throw ReducerFailure("conformance value is not valid JSON")
    }
    return try JSONSerialization.data(withJSONObject: value, options: [.sortedKeys])
}

func runParserSelfTests() throws {
    for source in ["{\"a\":1,\"a\":2}", "{\"a\":1,\"\\u0061\":2}", "{} {}"] {
        var parser = StrictJSONParser(data: Data(source.utf8))
        do {
            _ = try parser.parse()
            throw ReducerFailure("strict Swift parser accepted ambiguous JSON")
        } catch is StrictJSONError {
            continue
        }
    }
    do {
        try onlyKeys(["known": true, "unknown": true], ["known"], "self-test")
        throw ReducerFailure("Swift corpus validator accepted an unknown field")
    } catch let error as ReducerFailure where error.message.contains("unknown field") {
        // Expected.
    }
}

func readBounded(_ path: String) throws -> Data {
    let handle = try FileHandle(forReadingFrom: URL(fileURLWithPath: path))
    defer { try? handle.close() }
    let data = try handle.read(upToCount: maxCorpusBytes + 1) ?? Data()
    guard data.count <= maxCorpusBytes else {
        throw ReducerFailure("reducer corpus exceeds \(maxCorpusBytes) bytes")
    }
    return data
}

func runConformance(_ path: String) throws -> Int {
    try runParserSelfTests()
    let data = try readBounded(path)
    var parser = StrictJSONParser(data: data)
    let parsed = try parser.parse()
    let (corpus, limits) = try validateCorpus(parsed, data.count)
    let vectors = corpus["vectors"] as? [Any] ?? []
    var failures: [String] = []
    for value in vectors {
        let vector = try objectValue(value, "vector")
        let name = try requiredString(vector, "name", limits.maxStringBytes)
        let machine = PortableClientReducer(limits: limits)
        var errors: [String] = []
        for (index, stepValue) in (vector["steps"] as? [Any] ?? []).enumerated() {
            let step = try objectValue(stepValue, "step")
            do {
                try machine.apply(
                    atMS: try integerValue(step["at_ms"], "at_ms", minimum: 0),
                    operation: try objectValue(step["operation"], "operation")
                )
            } catch {
                guard errors.count < limits.maxErrorsPerVector else {
                    throw ReducerFailure("vector \(quoted(name)) exceeded error limit")
                }
                errors.append("step \(index): \(error.localizedDescription)")
            }
        }
        let expect = try objectValue(vector["expect"], "expect")
        let actualSnapshot = try machine.snapshotObject()
        if try canonicalJSON(actualSnapshot) != canonicalJSON(expect["snapshot"] as Any) {
            failures.append("\(name): snapshot mismatch")
        }
        if try canonicalJSON(machine.outbound) != canonicalJSON(expect["outbound"] as Any) {
            failures.append("\(name): outbound mismatch")
        }
        if try canonicalJSON(errors) != canonicalJSON(expect["errors"] as Any) {
            failures.append("\(name): errors mismatch; actual \(errors)")
        }
    }
    guard failures.isEmpty else { throw ReducerFailure(failures.joined(separator: "\n")) }
    return vectors.count
}

do {
    guard CommandLine.arguments.count == 2 else {
        throw ReducerFailure("usage: swiftc StrictJSON.swift ClientReducer.swift main.swift -o reducer-conformance && reducer-conformance <corpus.json>")
    }
    let count = try runConformance(CommandLine.arguments[1])
    print("swift conformance: \(count) vectors passed")
} catch {
    let message = "swift conformance failed: \(error.localizedDescription)\n"
    FileHandle.standardError.write(Data(message.utf8))
    exit(1)
}
