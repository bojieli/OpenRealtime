import Foundation
import CoreFoundation

struct ReducerLimits: Equatable {
    let maxVectors = 64
    let maxStepsPerVector = 512
    let maxEventBytes = 64 << 10
    let maxStringBytes = 16 << 10
    let maxConversationItems = 1_024
    let maxToolCalls = 256
    let maxOutboundEvents = 2_048
    let maxProtocolLogEntries = 4_096
    let maxErrorsPerVector = 128
    let maxReconnectAttempts = 3
    let maxVirtualTimeMS: Int64 = 86_400_000

    func matches(_ object: [String: Any]) -> Bool {
        let expected: [String: Int64] = [
            "max_vectors": Int64(maxVectors),
            "max_steps_per_vector": Int64(maxStepsPerVector),
            "max_event_bytes": Int64(maxEventBytes),
            "max_string_bytes": Int64(maxStringBytes),
            "max_conversation_items": Int64(maxConversationItems),
            "max_tool_calls": Int64(maxToolCalls),
            "max_outbound_events": Int64(maxOutboundEvents),
            "max_protocol_log_entries": Int64(maxProtocolLogEntries),
            "max_errors_per_vector": Int64(maxErrorsPerVector),
            "max_reconnect_attempts": Int64(maxReconnectAttempts),
            "max_virtual_time_ms": maxVirtualTimeMS,
        ]
        guard Set(object.keys) == Set(expected.keys) else { return false }
        return expected.allSatisfy { key, value in
            (try? integerValue(object[key], key, minimum: 0)) == value
        }
    }
}

struct ReducerFailure: LocalizedError {
    let message: String
    init(_ message: String) { self.message = message }
    var errorDescription: String? { message }
}

struct ClientConnectionProjection: Codable, Equatable {
    var phase = "disconnected"
    var transport = ""
    var reason = ""
    var attempt = 0
    var nextRetryMS: Int64 = 0

    enum CodingKeys: String, CodingKey {
        case phase, transport, reason, attempt
        case nextRetryMS = "next_retry_ms"
    }
}

struct ClientVideoProjection: Codable, Equatable {
    var format = ""
    var fpsCap = 0
    var maxDimension = 0
    var maxFrameBytes = 0

    enum CodingKeys: String, CodingKey {
        case format
        case fpsCap = "fps_cap"
        case maxDimension = "max_dimension"
        case maxFrameBytes = "max_frame_bytes"
    }
}

struct ClientNegotiationProjection: Codable, Equatable {
    var present = false
    var version = 0
    var enabled: [String] = []
    var observers: [String] = []
    var availableObservers: [String] = []
    var debugEnabled = false
    var video = ClientVideoProjection()

    enum CodingKeys: String, CodingKey {
        case present, version, enabled, observers, video
        case availableObservers = "available_observers"
        case debugEnabled = "debug_enabled"
    }
}

struct ClientSessionProjection: Codable, Equatable {
    var id = ""
    var manualTurns = false
    var openrealtime = ClientNegotiationProjection()

    enum CodingKeys: String, CodingKey {
        case id, openrealtime
        case manualTurns = "manual_turns"
    }
}

struct ClientResponseProjection: Codable, Equatable {
    var id = ""
    var open = false
    var status = ""
    var reason = ""
}

struct ClientConversationItem: Codable, Equatable {
    var itemID: String
    var role: String
    var channel: String
    var text: String
    var audioDeltas = 0
    var audioDone = false
    var truncatedMS: Int64 = 0

    enum CodingKeys: String, CodingKey {
        case role, channel, text
        case itemID = "item_id"
        case audioDeltas = "audio_deltas"
        case audioDone = "audio_done"
        case truncatedMS = "truncated_ms"
    }
}

struct ClientPlayoutProjection: Codable, Equatable {
    var speaking = false
    var itemID = ""
    var playedMS: Int64 = 0

    enum CodingKeys: String, CodingKey {
        case speaking
        case itemID = "item_id"
        case playedMS = "played_ms"
    }
}

struct ClientToolProjection: Codable, Equatable {
    var callID: String
    var name: String
    var arguments: String
    var status: String
    var output = ""
    var error = ""

    enum CodingKeys: String, CodingKey {
        case name, arguments, status, output, error
        case callID = "call_id"
    }
}

struct ClientTruncationProjection: Codable, Equatable {
    var itemID = ""
    var audioEndMS: Int64 = 0

    enum CodingKeys: String, CodingKey {
        case itemID = "item_id"
        case audioEndMS = "audio_end_ms"
    }
}

struct ClientProtocolEntry: Codable, Equatable {
    var atMS: Int64
    var direction: String
    var type: String

    enum CodingKeys: String, CodingKey {
        case direction, type
        case atMS = "at_ms"
    }
}

struct ClientReducerState: Codable, Equatable {
    var nowMS: Int64 = 0
    var connection = ClientConnectionProjection()
    var session = ClientSessionProjection()
    var response = ClientResponseProjection()
    var conversation: [ClientConversationItem] = []
    var playout = ClientPlayoutProjection()
    var tools: [ClientToolProjection] = []
    var lastTruncation = ClientTruncationProjection()
    var lastError = ""
    var protocolLog: [ClientProtocolEntry] = []

    enum CodingKeys: String, CodingKey {
        case connection, session, response, conversation, playout, tools
        case nowMS = "now_ms"
        case lastTruncation = "last_truncation"
        case lastError = "last_error"
        case protocolLog = "protocol_log"
    }
}

final class PortableClientReducer {
    let limits: ReducerLimits
    private(set) var state = ClientReducerState()
    private(set) var outbound: [[String: Any]] = []

    init(limits: ReducerLimits = ReducerLimits()) {
        self.limits = limits
    }

    func apply(atMS: Int64, operation: [String: Any]) throws {
        guard atMS >= state.nowMS else {
            throw ReducerFailure("virtual time moved backwards from \(state.nowMS) to \(atMS)")
        }
        guard atMS <= limits.maxVirtualTimeMS else {
            throw ReducerFailure("virtual time \(atMS) exceeds limit \(limits.maxVirtualTimeMS)")
        }
        try validateOperation(operation)
        let priorState = state
        let priorOutbound = outbound
        state.nowMS = atMS
        do {
            try applyOperation(operation)
        } catch {
            state = priorState
            outbound = priorOutbound
            throw error
        }
    }

    func snapshotObject() throws -> [String: Any] {
        let data = try JSONEncoder().encode(state)
        guard let object = try JSONSerialization.jsonObject(with: data) as? [String: Any] else {
            throw ReducerFailure("encoded reducer snapshot is not an object")
        }
        return object
    }

    private func validateOperation(_ operation: [String: Any]) throws {
        let kind = try requiredString(operation, "kind", limits.maxStringBytes)
        for key in ["transport", "reason", "item_id", "text", "call_id", "status", "output", "error"]
        where operation[key] != nil {
            _ = try stringValue(operation[key], key, limits.maxStringBytes, allowEmpty: true)
        }
        if let played = operation["played_ms"] {
            _ = try integerValue(played, "played_ms", minimum: 0)
        }
        for key in ["event", "session"] where operation[key] != nil {
            guard JSONSerialization.isValidJSONObject(operation[key] as Any),
                  let data = try? JSONSerialization.data(withJSONObject: operation[key] as Any),
                  data.count <= limits.maxEventBytes else {
                throw ReducerFailure("operation JSON exceeds \(limits.maxEventBytes) bytes")
            }
        }
        let fields: [String: Set<String>] = [
            "connect": ["transport"], "connected": [], "transport_lost": ["reason"],
            "retry": [], "disconnect": ["reason"], "inbound": ["event"],
            "session_update": ["session"], "typed_text": ["item_id", "text"],
            "end_turn": [], "playout": ["speaking", "item_id", "played_ms"],
            "cancel_response": [], "tool_result": ["call_id", "status", "output", "error"],
        ]
        guard let allowed = fields[kind] else { throw ReducerFailure("unknown operation kind \(quoted(kind))") }
        for key in operation.keys where key != "kind" && !allowed.contains(key) {
            throw ReducerFailure("operation \(quoted(kind)) does not allow field \(quoted(key))")
        }
    }

    private func applyOperation(_ operation: [String: Any]) throws {
        let kind = try requiredString(operation, "kind", limits.maxStringBytes)
        switch kind {
        case "connect": try connect(try requiredString(operation, "transport", limits.maxStringBytes))
        case "connected": try connected()
        case "transport_lost": try transportLost(try requiredString(operation, "reason", limits.maxStringBytes))
        case "retry": try retry()
        case "disconnect": try disconnect((operation["reason"] as? String) ?? "")
        case "inbound": try inbound(try objectValue(operation["event"], "inbound event"))
        case "session_update": try sessionUpdate(try objectValue(operation["session"], "session update"))
        case "typed_text": try typedText(
            try requiredString(operation, "item_id", limits.maxStringBytes),
            try requiredString(operation, "text", limits.maxStringBytes)
        )
        case "end_turn": try endTurn()
        case "playout":
            state.playout = ClientPlayoutProjection(
                speaking: (operation["speaking"] as? Bool) ?? false,
                itemID: (operation["item_id"] as? String) ?? "",
                playedMS: try optionalInteger(operation["played_ms"], "played_ms") ?? 0
            )
        case "cancel_response": try cancelResponse()
        case "tool_result": try toolResult(operation)
        default: throw ReducerFailure("unknown operation kind \(quoted(kind))")
        }
    }

    private func requireConnected() throws {
        guard state.connection.phase == "connected" else {
            throw ReducerFailure("operation requires connected state, got \(state.connection.phase)")
        }
    }

    private func connect(_ transport: String) throws {
        guard ["disconnected", "failed"].contains(state.connection.phase) else {
            throw ReducerFailure("cannot connect while connection is \(state.connection.phase)")
        }
        guard ["websocket", "webrtc"].contains(transport) else {
            throw ReducerFailure("unsupported transport \(quoted(transport))")
        }
        state.connection = ClientConnectionProjection(phase: "connecting", transport: transport)
        state.lastError = ""
    }

    private func connected() throws {
        guard state.connection.phase == "connecting" else {
            throw ReducerFailure("cannot become connected while connection is \(state.connection.phase)")
        }
        state.connection.phase = "connected"
        state.connection.reason = ""
        state.connection.attempt = 0
        state.connection.nextRetryMS = 0
        state.lastError = ""
    }

    private func cleanupSession(_ reason: String) {
        if state.response.open {
            state.response.open = false
            state.response.status = "abandoned"
            state.response.reason = reason
        }
        state.playout = ClientPlayoutProjection()
        for index in state.tools.indices where state.tools[index].status == "pending" {
            state.tools[index].status = "failed"
            state.tools[index].error = reason
        }
        state.session = ClientSessionProjection()
    }

    private func transportLost(_ reason: String) throws {
        let phase = state.connection.phase
        guard ["connected", "connecting"].contains(phase) else {
            throw ReducerFailure("transport cannot be lost while connection is \(phase)")
        }
        guard !reason.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
            throw ReducerFailure("transport loss requires a reason")
        }
        cleanupSession(reason)
        var attempt = state.connection.attempt
        if attempt == 0 {
            attempt = 1
        } else if phase == "connecting" {
            if attempt >= limits.maxReconnectAttempts {
                state.connection.phase = "failed"
                state.connection.reason = reason
                state.connection.nextRetryMS = 0
                state.lastError = reason
                return
            }
            attempt += 1
        }
        let delays: [Int64] = [250, 1_000, 4_000]
        state.connection.phase = "reconnecting"
        state.connection.reason = reason
        state.connection.attempt = attempt
        state.connection.nextRetryMS = state.nowMS + delays[min(attempt - 1, delays.count - 1)]
    }

    private func retry() throws {
        guard state.connection.phase == "reconnecting" else {
            throw ReducerFailure("cannot retry while connection is \(state.connection.phase)")
        }
        guard state.nowMS >= state.connection.nextRetryMS else {
            throw ReducerFailure("retry at \(state.nowMS) precedes deadline \(state.connection.nextRetryMS)")
        }
        state.connection.phase = "connecting"
        state.connection.nextRetryMS = 0
    }

    private func disconnect(_ suppliedReason: String) throws {
        let reason = suppliedReason.isEmpty ? "disconnected" : suppliedReason
        cleanupSession(reason)
        state.connection = ClientConnectionProjection(phase: "disconnected", reason: reason)
    }

    private func sessionUpdate(_ session: [String: Any]) throws {
        try requireConnected()
        if let type = session["type"], try stringValue(type, "session.type", limits.maxStringBytes) != "realtime" {
            throw ReducerFailure("session.type must be \"realtime\"")
        }
        try emit([["type": "session.update", "session": session]])
    }

    private func typedText(_ itemIDValue: String, _ textValue: String) throws {
        try requireConnected()
        let itemID = itemIDValue.trimmingCharacters(in: .whitespacesAndNewlines)
        let text = textValue.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !itemID.isEmpty, !text.isEmpty else {
            throw ReducerFailure("typed_text requires non-empty item_id and text")
        }
        guard conversationIndex(itemID, "input_text") == nil else {
            throw ReducerFailure("conversation item \(quoted(itemID)) already exists")
        }
        guard state.conversation.count < limits.maxConversationItems else {
            throw ReducerFailure("conversation item limit reached")
        }
        try emit([
            [
                "type": "conversation.item.create",
                "item": [
                    "type": "message", "role": "user",
                    "content": [["type": "input_text", "text": text]],
                ],
            ],
            ["type": "response.create"],
        ])
        state.conversation.append(ClientConversationItem(
            itemID: itemID, role: "user", channel: "input_text", text: text
        ))
    }

    private func endTurn() throws {
        try requireConnected()
        guard state.session.manualTurns else {
            throw ReducerFailure("end_turn requires negotiated manual turn detection")
        }
        try emit([["type": "input_audio_buffer.commit"], ["type": "response.create"]])
    }

    private func cancelResponse() throws {
        try requireConnected()
        guard state.response.open else { throw ReducerFailure("no response is open") }
        guard state.response.status != "cancelling" else {
            throw ReducerFailure("response cancellation is already pending")
        }
        try emit([responseCancel()])
        state.response.status = "cancelling"
    }

    private func responseCancel() -> [String: Any] {
        var event: [String: Any] = ["type": "response.cancel"]
        if !state.response.id.isEmpty { event["response_id"] = state.response.id }
        return event
    }

    private func inbound(_ event: [String: Any]) throws {
        try requireConnected()
        let type = try requiredString(event, "type", limits.maxStringBytes)
        guard try jsonSize(event) <= limits.maxEventBytes else {
            throw ReducerFailure("inbound event exceeds \(limits.maxEventBytes) bytes")
        }
        guard state.protocolLog.count < limits.maxProtocolLogEntries else {
            throw ReducerFailure("protocol log limit reached")
        }
        state.protocolLog.append(ClientProtocolEntry(atMS: state.nowMS, direction: "in", type: type))
        switch type {
        case "session.created":
            let session = try objectValue(event["session"], "session")
            guard session["id"] != nil else { throw ReducerFailure("session.created: missing id") }
            state.session.id = try requiredString(session, "id", limits.maxStringBytes)
        case "session.updated": try sessionUpdated(event)
        case "input_audio_buffer.speech_started": try speechStarted()
        case "input_audio_buffer.speech_stopped": return
        case "conversation.item.input_audio_transcription.completed":
            try setConversation(
                try requiredString(event, "item_id", limits.maxStringBytes),
                "user", "input_audio_transcript",
                try requiredString(event, "transcript", limits.maxStringBytes), false
            )
        case "response.created": try responseCreated(event)
        case "response.output_audio_transcript.delta": try responseTextDelta(event, "output_audio_transcript")
        case "response.output_text.delta": try responseTextDelta(event, "output_text")
        case "response.output_audio.delta": try responseAudioDelta(event)
        case "response.output_audio.done": try responseAudioDone(event)
        case "response.function_call_arguments.done": try functionCall(event)
        case "response.done": try responseDone(event)
        case "conversation.item.truncated": try itemTruncated(event)
        case "openrealtime.observation.added": try observationAdded(event)
        case "openrealtime.debug.event": return
        case "error":
            let error = try objectValue(event["error"], "error")
            state.lastError = try requiredString(error, "message", limits.maxStringBytes)
        default: return
        }
    }

    private func sessionUpdated(_ event: [String: Any]) throws {
        let session = try objectValue(event["session"], "session")
        if let id = session["id"] {
            state.session.id = try stringValue(id, "session.id", limits.maxStringBytes, allowEmpty: true)
        }
        state.session.manualTurns = false
        if let audioValue = session["audio"],
           let inputValue = try? objectValue(try objectValue(audioValue, "session.audio")["input"], "session.audio.input"),
           inputValue["turn_detection"] is NSNull {
            state.session.manualTurns = true
        }
        guard let extensionValue = session["openrealtime"], !(extensionValue is NSNull) else {
            state.session.openrealtime = ClientNegotiationProjection()
            return
        }
        let extensionObject = try objectValue(extensionValue, "session.openrealtime")
        var projection = ClientNegotiationProjection(present: true)
        if let version = extensionObject["version"] {
            projection.version = Int(try integerValue(version, "session.openrealtime.version", minimum: 0))
        }
        projection.enabled = try stringList(extensionObject["enabled"], "enabled")
        projection.observers = try stringList(extensionObject["observers"], "observers")
        projection.availableObservers = try stringList(extensionObject["available_observers"], "available_observers")
        if let debugValue = extensionObject["debug"], !(debugValue is NSNull) {
            let debug = try objectValue(debugValue, "session.openrealtime.debug")
            if let enabled = debug["enabled"] {
                guard let value = enabled as? Bool else {
                    throw ReducerFailure("session.openrealtime.debug.enabled must be a boolean")
                }
                projection.debugEnabled = value
            }
        }
        if let videoValue = extensionObject["video"], !(videoValue is NSNull) {
            let video = try objectValue(videoValue, "session.openrealtime.video")
            if let format = video["format"] {
                projection.video.format = try stringValue(format, "session.openrealtime.video.format", limits.maxStringBytes, allowEmpty: true)
            }
            if let value = video["fps_cap"] { projection.video.fpsCap = Int(try integerValue(value, "session.openrealtime.video.fps_cap", minimum: 0)) }
            if let value = video["max_dimension"] { projection.video.maxDimension = Int(try integerValue(value, "session.openrealtime.video.max_dimension", minimum: 0)) }
            if let value = video["max_frame_bytes"] { projection.video.maxFrameBytes = Int(try integerValue(value, "session.openrealtime.video.max_frame_bytes", minimum: 0)) }
        }
        state.session.openrealtime = projection
    }

    private func stringList(_ value: Any?, _ name: String) throws -> [String] {
        guard let value, !(value is NSNull) else { return [] }
        guard let list = value as? [Any], list.count <= limits.maxToolCalls else {
            throw ReducerFailure("\(name) must be an array of strings")
        }
        return try list.map { try stringValue($0, "\(name) value", limits.maxStringBytes) }
    }

    private func speechStarted() throws {
        guard state.playout.speaking else { return }
        guard !state.playout.itemID.isEmpty else { throw ReducerFailure("active playout has no item_id") }
        var commands: [[String: Any]] = []
        if state.response.open, state.response.status != "cancelling" { commands.append(responseCancel()) }
        if state.response.open, state.connection.transport == "webrtc" {
            commands.append(["type": "output_audio_buffer.clear"])
        }
        commands.append([
            "type": "conversation.item.truncate", "item_id": state.playout.itemID,
            "content_index": 0, "audio_end_ms": state.playout.playedMS,
        ])
        try emit(commands)
        if state.response.open { state.response.status = "cancelling" }
        state.playout = ClientPlayoutProjection()
    }

    private func responseCreated(_ event: [String: Any]) throws {
        guard !state.response.open else {
            throw ReducerFailure("response \(quoted(state.response.id)) is already open")
        }
        let response = try objectValue(event["response"], "response")
        state.response = ClientResponseProjection(
            id: try requiredString(response, "id", limits.maxStringBytes),
            open: true,
            status: try optionalString(response["status"], "response.status") ?? "in_progress"
        )
    }

    private func responseTextDelta(_ event: [String: Any], _ channel: String) throws {
        guard state.response.open else { throw ReducerFailure("response delta arrived without an open response") }
        try appendConversation(
            try requiredString(event, "item_id", limits.maxStringBytes), "assistant", channel,
            try stringValue(event["delta"], "delta", limits.maxStringBytes, allowEmpty: true)
        )
    }

    private func responseAudioDelta(_ event: [String: Any]) throws {
        guard state.response.open else { throw ReducerFailure("audio delta arrived without an open response") }
        let itemID = try requiredString(event, "item_id", limits.maxStringBytes)
        let delta = try requiredString(event, "delta", limits.maxStringBytes)
        guard Data(base64Encoded: delta, options: []) != nil else {
            throw ReducerFailure("response audio delta is not base64")
        }
        if let index = conversationIndex(itemID, "output_audio_transcript") {
            state.conversation[index].audioDeltas += 1
        } else {
            guard state.conversation.count < limits.maxConversationItems else {
                throw ReducerFailure("conversation item limit reached")
            }
            state.conversation.append(ClientConversationItem(
                itemID: itemID, role: "assistant", channel: "output_audio_transcript", text: "",
                audioDeltas: 1
            ))
        }
    }

    private func responseAudioDone(_ event: [String: Any]) throws {
        let itemID = try requiredString(event, "item_id", limits.maxStringBytes)
        if let index = conversationIndex(itemID, "output_audio_transcript") {
            state.conversation[index].audioDone = true
        } else {
            guard state.conversation.count < limits.maxConversationItems else {
                throw ReducerFailure("conversation item limit reached")
            }
            state.conversation.append(ClientConversationItem(
                itemID: itemID, role: "assistant", channel: "output_audio_transcript", text: "",
                audioDone: true
            ))
        }
    }

    private func functionCall(_ event: [String: Any]) throws {
        let callID = try requiredString(event, "call_id", limits.maxStringBytes)
        guard toolIndex(callID) == nil else { throw ReducerFailure("tool call \(quoted(callID)) already exists") }
        guard state.tools.count < limits.maxToolCalls else { throw ReducerFailure("tool call limit reached") }
        state.tools.append(ClientToolProjection(
            callID: callID,
            name: try requiredString(event, "name", limits.maxStringBytes),
            arguments: try stringValue(event["arguments"], "arguments", limits.maxStringBytes, allowEmpty: true),
            status: "pending"
        ))
    }

    private func toolResult(_ operation: [String: Any]) throws {
        try requireConnected()
        let callID = (operation["call_id"] as? String) ?? ""
        guard let index = toolIndex(callID) else { throw ReducerFailure("unknown tool call \(quoted(callID))") }
        guard state.tools[index].status == "pending" else {
            throw ReducerFailure("tool call \(quoted(callID)) is already \(state.tools[index].status)")
        }
        let status = (operation["status"] as? String) ?? ""
        let output = (operation["output"] as? String) ?? ""
        let error = (operation["error"] as? String) ?? ""
        let wireOutput: String
        switch status {
        case "done":
            guard error.isEmpty else { throw ReducerFailure("a completed tool result cannot contain error") }
            wireOutput = output
        case "failed", "declined":
            guard !error.isEmpty else { throw ReducerFailure("\(status) tool result requires error") }
            let data = try JSONSerialization.data(withJSONObject: ["error": error], options: [.sortedKeys])
            wireOutput = String(data: data, encoding: .utf8) ?? ""
        default: throw ReducerFailure("unknown tool result status \(quoted(status))")
        }
        try emit([
            [
                "type": "conversation.item.create",
                "item": ["type": "function_call_output", "call_id": callID, "output": wireOutput],
            ],
            ["type": "response.create"],
        ])
        state.tools[index].status = status
        state.tools[index].output = output
        state.tools[index].error = error
    }

    private func responseDone(_ event: [String: Any]) throws {
        let response = try objectValue(event["response"], "response")
        let id = try requiredString(response, "id", limits.maxStringBytes)
        let status = try requiredString(response, "status", limits.maxStringBytes)
        if state.response.open, !state.response.id.isEmpty, state.response.id != id {
            throw ReducerFailure("response.done for \(quoted(id)) while \(quoted(state.response.id)) is open")
        }
        var reason = ""
        if let detailsValue = response["status_details"], !(detailsValue is NSNull) {
            let details = try objectValue(detailsValue, "response.status_details")
            if let value = details["reason"], !(value is NSNull) {
                reason = try stringValue(value, "response.status_details.reason", limits.maxStringBytes, allowEmpty: true)
            }
        }
        state.response = ClientResponseProjection(id: id, status: status, reason: reason)
    }

    private func itemTruncated(_ event: [String: Any]) throws {
        let itemID = try requiredString(event, "item_id", limits.maxStringBytes)
        let audioEndMS = try integerValue(event["audio_end_ms"], "audio_end_ms", minimum: 0)
        state.lastTruncation = ClientTruncationProjection(itemID: itemID, audioEndMS: audioEndMS)
        for index in state.conversation.indices where state.conversation[index].itemID == itemID {
            state.conversation[index].truncatedMS = audioEndMS
        }
    }

    private func observationAdded(_ event: [String: Any]) throws {
        let itemID = try requiredString(event, "observation_id", limits.maxStringBytes)
        let observer = try requiredString(event, "observer", limits.maxStringBytes)
        let text = try stringValue(event["text"], "text", limits.maxStringBytes, allowEmpty: true)
        var channel = "observation.\(observer)"
        if let value = event["source"], !(value is NSNull) {
            let source = try stringValue(value, "source", limits.maxStringBytes, allowEmpty: true)
            if !source.isEmpty { channel = "observation.\(source)" }
        }
        try setConversation(itemID, "observation", channel, text, true)
    }

    private func setConversation(
        _ itemID: String, _ role: String, _ channel: String, _ text: String, _ replace: Bool
    ) throws {
        if let index = conversationIndex(itemID, channel) {
            guard replace else {
                throw ReducerFailure("conversation item \(quoted(itemID)) channel \(quoted(channel)) already exists")
            }
            state.conversation[index].text = text
            return
        }
        guard state.conversation.count < limits.maxConversationItems else {
            throw ReducerFailure("conversation item limit reached")
        }
        state.conversation.append(ClientConversationItem(
            itemID: itemID, role: role, channel: channel, text: text
        ))
    }

    private func appendConversation(
        _ itemID: String, _ role: String, _ channel: String, _ delta: String
    ) throws {
        if let index = conversationIndex(itemID, channel) {
            guard (state.conversation[index].text + delta).utf8.count <= limits.maxStringBytes else {
                throw ReducerFailure("conversation text exceeds \(limits.maxStringBytes) bytes")
            }
            state.conversation[index].text += delta
            return
        }
        guard state.conversation.count < limits.maxConversationItems else {
            throw ReducerFailure("conversation item limit reached")
        }
        state.conversation.append(ClientConversationItem(
            itemID: itemID, role: role, channel: channel, text: delta
        ))
    }

    private func conversationIndex(_ itemID: String, _ channel: String) -> Int? {
        state.conversation.firstIndex { $0.itemID == itemID && $0.channel == channel }
    }

    private func toolIndex(_ callID: String) -> Int? {
        state.tools.firstIndex { $0.callID == callID }
    }

    private func emit(_ events: [[String: Any]]) throws {
        guard outbound.count + events.count <= limits.maxOutboundEvents else {
            throw ReducerFailure("outbound event limit reached")
        }
        guard state.protocolLog.count + events.count <= limits.maxProtocolLogEntries else {
            throw ReducerFailure("protocol log limit reached")
        }
        for event in events {
            _ = try requiredString(event, "type", limits.maxStringBytes)
            guard try jsonSize(event) <= limits.maxEventBytes else {
                throw ReducerFailure("outbound event exceeds \(limits.maxEventBytes) bytes")
            }
        }
        for event in events {
            outbound.append(event)
            state.protocolLog.append(ClientProtocolEntry(
                atMS: state.nowMS, direction: "out", type: event["type"] as? String ?? ""
            ))
        }
    }

    private func optionalString(_ value: Any?, _ name: String) throws -> String? {
        guard let value else { return nil }
        return try stringValue(value, name, limits.maxStringBytes)
    }

    private func optionalInteger(_ value: Any?, _ name: String) throws -> Int64? {
        guard let value else { return nil }
        return try integerValue(value, name, minimum: 0)
    }
}

func objectValue(_ value: Any?, _ name: String) throws -> [String: Any] {
    guard let object = value as? [String: Any] else { throw ReducerFailure("\(name) must be a JSON object") }
    return object
}

func requiredString(_ object: [String: Any], _ key: String, _ maximum: Int) throws -> String {
    guard let value = object[key] else { throw ReducerFailure("missing \(key)") }
    return try stringValue(value, key, maximum)
}

func stringValue(
    _ value: Any?, _ name: String, _ maximum: Int, allowEmpty: Bool = false
) throws -> String {
    guard let string = value as? String else { throw ReducerFailure("\(name) must be a string") }
    guard allowEmpty || !string.isEmpty else { throw ReducerFailure("\(name) must not be empty") }
    guard string.utf8.count <= maximum else { throw ReducerFailure("\(name) exceeds \(maximum) bytes") }
    return string
}

func integerValue(_ value: Any?, _ name: String, minimum: Int64) throws -> Int64 {
    guard let value, let number = value as? NSNumber,
          CFGetTypeID(number) != CFBooleanGetTypeID() else {
        throw ReducerFailure("\(name) must be an integer")
    }
    let double = number.doubleValue
    let integer = number.int64Value
    guard double.isFinite, Double(integer) == double else {
        throw ReducerFailure("\(name) must be an integer")
    }
    guard integer >= minimum else { throw ReducerFailure("\(name) must be at least \(minimum)") }
    return integer
}

func jsonSize(_ object: Any) throws -> Int {
    guard JSONSerialization.isValidJSONObject(object) else {
        throw ReducerFailure("value is not a JSON object")
    }
    return try JSONSerialization.data(withJSONObject: object).count
}
