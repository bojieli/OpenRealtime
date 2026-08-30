import Foundation
#if canImport(FoundationNetworking)
@preconcurrency import FoundationNetworking
#endif
import OpenRealtimeClientCore

struct NativeEffectLimits {
    let maxMessageBytes: Int
    let maxResultBytes: Int
    let maxInFlight: Int
    let maxCalls: Int
    let confirmationTimeoutMS: Int
    let executionTimeoutMS: Int
}

struct NativeEffectDeclaration {
    let name: String
    let summary: String
    let parameters: [String: Any]
    let hostConfirmation: String
    let target: String
    let mutating: Bool
    let channel: String
    let digest: String
}

private struct NativePendingEffect {
    let id: String
    let name: String
    let arguments: Data
    let channel: String
}

struct NativeEffectsSnapshot {
    let phase: String
    let sessionID: String
    let scopeID: String
    let declarationNames: [String]
    let pendingCount: Int
    let confirmations: [ConfirmationRequest]
    let records: [ChannelRecord]
    let debug: [DebugRecord]
    let diagnostic: String
    let calls: Int
    let results: Int
    let refused: Int
    let reconnects: Int
}

private final class HostEffectsSessionDelegate: NSObject, URLSessionTaskDelegate {
    func urlSession(
        _ session: URLSession,
        task: URLSessionTask,
        willPerformHTTPRedirection response: HTTPURLResponse,
        newRequest request: URLRequest,
        completionHandler: @escaping (URLRequest?) -> Void
    ) {
        completionHandler(nil)
    }
}

@MainActor
final class NativeEffectsBoundary {
    private static let maximumTools = 128
    private static let maximumNameBytes = 128
    private static let maximumDescriptionBytes = 4_096
    private static let maximumParametersBytes = 64 << 10
    private static let maximumProtocolErrorBytes = 4_096
    private static let maximumWireBytes = 48 << 20
    private static let maximumRecords = 2_000
    private static let maximumDebugRecords = 4_000
    private static let reconnectDelaysMS = [100, 250, 500, 1_000, 2_000]

    private let reducer: NativeReducerController
    private let strictJSON: StrictJSONService
    private let protocolEvents: ValidatedProtocolEventService
    private let sessionConfiguration: SessionConfigurationService
    private let declaredEndpoint: NativeManifestEndpoint
    private let selectedEndpoint: URL
    private var unsubscribeEvents: (() -> Void)?
    private var unsubscribeState: (() -> Void)?
    private var removeContribution: (() -> Void)?
    private var removeDebugContribution: (() -> Void)?
    private var listeners: [UUID: (NativeEffectsSnapshot, [String: Any]?) -> Void] = [:]
    private var configuredEndpoint: URL?
    private var desiredEndpoint: URL?
    private var session: URLSession?
    private var socket: URLSessionWebSocketTask?
    private var receiveTask: Task<Void, Never>?
    private var reconnectTask: Task<Void, Never>?
    private var generation = 0
    private var reconnectAttempt = 0
    private var activeSessionID = ""
    private var lastState: [String: Any] = [:]
    private var readyScopeID = ""
    private var declarations: [String: NativeEffectDeclaration] = [:]
    private var limits: NativeEffectLimits?
    private var pending: [String: NativePendingEffect] = [:]
    private var confirmations: [String: ConfirmationRequest] = [:]
    private var confirmationNonces: [String: String] = [:]
    private var contributionIdentity = Data()
    private var records: [ChannelRecord] = []
    private var debugRecords: [DebugRecord] = []
    private var phase = "idle"
    private var diagnostic = ""
    private var calls = 0
    private var results = 0
    private var refused = 0
    private var reconnects = 0
    private var mounted = false

    init(
        reducer: NativeReducerController,
        strictJSON: StrictJSONService,
        protocolEvents: ValidatedProtocolEventService,
        sessionConfiguration: SessionConfigurationService,
        declaration: NativeManifestEndpoint,
        endpoint: String
    ) throws {
        self.reducer = reducer
        self.strictJSON = strictJSON
        self.protocolEvents = protocolEvents
        self.sessionConfiguration = sessionConfiguration
        declaredEndpoint = declaration
        selectedEndpoint = try NativeEffectEndpointResolver.websocketURL(
            endpoint: endpoint, declaration: declaration
        )
    }

    func mount() throws {
        guard !mounted else { throw NativeProviderError("native effects provider mounted twice") }
        mounted = true
        generation += 1
        configuredEndpoint = desiredEndpoint
        removeDebugContribution = try sessionConfiguration.contribute([
            "debug": ["enabled": true, "categories": ["audio", "tools", "video"]],
        ])
        unsubscribeEvents = try protocolEvents.subscribe { [weak self] document in
            guard let self, self.mounted, let event = try? document.snapshot() else { return }
            self.consume(event)
        }
        unsubscribeState = try reducer.subscribe { [weak self] state in self?.observe(state) }
        publish()
    }

    func unmount() {
        guard mounted else { return }
        mounted = false
        unsubscribeEvents?()
        unsubscribeEvents = nil
        unsubscribeState?()
        unsubscribeState = nil
        removeDebugContribution?()
        removeDebugContribution = nil
        configuredEndpoint = nil
        activeSessionID = ""
        closeSocket(reason: "native effects provider was lost")
        listeners.removeAll()
        lastState.removeAll()
        records.removeAll()
        debugRecords.removeAll()
        diagnostic = ""
        calls = 0
        results = 0
        refused = 0
        reconnects = 0
        reconnectAttempt = 0
    }

    func configure() throws {
        guard mounted else { throw NativeProviderError("native effects provider is unavailable") }
        desiredEndpoint = selectedEndpoint
        configuredEndpoint = selectedEndpoint
        if !activeSessionID.isEmpty { openSocket() }
    }

    func deactivate() {
        desiredEndpoint = nil
        configuredEndpoint = nil
        activeSessionID = ""
        closeSocket(reason: "realtime session ended")
    }

    func dispose() {
        desiredEndpoint = nil
        configuredEndpoint = nil
    }

    func snapshot() -> NativeEffectsSnapshot {
        NativeEffectsSnapshot(
            phase: phase, sessionID: activeSessionID, scopeID: readyScopeID,
            declarationNames: declarations.keys.sorted(), pendingCount: pending.count,
            confirmations: confirmations.values.sorted { $0.id < $1.id },
            records: records, debug: debugRecords, diagnostic: diagnostic,
            calls: calls, results: results, refused: refused, reconnects: reconnects
        )
    }

    @discardableResult
    func subscribe(
        _ listener: @escaping (NativeEffectsSnapshot, [String: Any]?) -> Void
    ) throws -> () -> Void {
        guard mounted else { throw NativeProviderError("native effects provider is unavailable") }
        guard listeners.count < 128 else {
            throw NativeProviderError("native effects listener limit reached")
        }
        let id = UUID()
        listeners[id] = listener
        listener(snapshot(), nil)
        return { [weak self] in self?.listeners.removeValue(forKey: id) }
    }

    func decideConfirmation(id: String, approved: Bool) throws {
        guard mounted, confirmations[id] != nil,
              let nonce = confirmationNonces[id] else {
            throw NativeProviderError("effect decision does not match one pending confirmation")
        }
        try send(["type": "decide", "id": id, "nonce": nonce, "approved": approved])
        confirmations.removeValue(forKey: id)
        confirmationNonces.removeValue(forKey: id)
        publish(event: ["type": "decision", "id": id, "approved": approved])
    }

    private func observe(_ state: [String: Any]) {
        guard mounted else { return }
        lastState = state
        let connection = state["connection"] as? [String: Any]
        let sessionValue = state["session"] as? [String: Any]
        let nextSession = connection?["phase"] as? String == "connected"
            ? (sessionValue?["id"] as? String ?? "") : ""
        if nextSession != activeSessionID {
            closeSocket(reason: nextSession.isEmpty
                ? "realtime session unavailable" : "realtime session replaced")
            activeSessionID = nextSession
            if !activeSessionID.isEmpty { openSocket() }
        } else {
            do { try syncContribution() } catch { report(error) }
        }
    }

    private func enabled() -> Bool {
        let sessionValue = lastState["session"] as? [String: Any]
        let extensionValue = sessionValue?["openrealtime"] as? [String: Any]
        return (extensionValue?["enabled"] as? [String] ?? []).contains("client.effects")
    }

    private func syncContribution() throws {
        guard mounted, limits != nil else {
            clearContribution()
            return
        }
        let tools: [[String: Any]] = enabled() ? declarations.values
            .sorted { $0.name < $1.name }.map(Self.sessionTool) : []
        let fragment: [String: Any] = ["supports": ["client.effects"], "tools": tools]
        let identity = try strictJSON.stable(
            fragment, maximumBytes: SessionConfigurationService.maximumConfigurationBytes
        )
        guard identity != contributionIdentity else { return }
        clearContribution()
        removeContribution = try sessionConfiguration.contribute(fragment)
        contributionIdentity = identity
    }

    private func clearContribution() {
        removeContribution?()
        removeContribution = nil
        contributionIdentity.removeAll()
    }

    private func openSocket() {
        guard mounted, !activeSessionID.isEmpty,
              let endpoint = configuredEndpoint, socket == nil else { return }
        phase = reconnectAttempt == 0 ? "connecting" : "reconnecting"
        let expectedGeneration = generation + 1
        generation = expectedGeneration
        let configuration = URLSessionConfiguration.ephemeral
        configuration.httpCookieStorage = nil
        configuration.urlCredentialStorage = nil
        configuration.httpShouldSetCookies = false
        configuration.requestCachePolicy = .reloadIgnoringLocalAndRemoteCacheData
        let delegate = HostEffectsSessionDelegate()
        let nextSession = URLSession(configuration: configuration, delegate: delegate, delegateQueue: nil)
        var request = URLRequest(url: endpoint)
        request.cachePolicy = .reloadIgnoringLocalAndRemoteCacheData
        request.timeoutInterval = 30
        let nextSocket = nextSession.webSocketTask(with: request)
        session = nextSession
        socket = nextSocket
        phase = "handshaking"
        nextSocket.resume()
        publish(event: ["type": "connecting"])
        receiveTask = Task { [weak self, weak nextSocket] in
            guard let self, let nextSocket else { return }
            do {
                while !Task.isCancelled {
                    let value = try await nextSocket.receive()
                    guard self.mounted, self.generation == expectedGeneration,
                          self.socket === nextSocket else { return }
                    try self.receive(value)
                }
            } catch {
                guard self.mounted, self.generation == expectedGeneration,
                      self.socket === nextSocket else { return }
                self.socketFailed(error)
            }
        }
    }

    private func receive(_ value: URLSessionWebSocketTask.Message) throws {
        let data: Data
        switch value {
        case .string(let text): data = Data(text.utf8)
        case .data: throw NativeProviderError("effect provider sent a binary message")
        @unknown default: throw NativeProviderError("effect provider sent an unknown message")
        }
        guard !data.isEmpty, data.count <= Self.maximumWireBytes else {
            throw NativeProviderError("effect provider sent an invalid or oversized text message")
        }
        let message = try strictJSON.parse(data, maximumBytes: Self.maximumWireBytes)
        guard let type = message["type"] as? String else {
            throw NativeProviderError("effect provider message requires a type")
        }
        if type == "ready" {
            guard limits == nil else { throw NativeProviderError("effect provider sent ready twice") }
            try acceptReady(message)
            return
        }
        guard let limits, data.count <= limits.maxMessageBytes else {
            throw NativeProviderError("effect provider sent a message before ready or above its declared bound")
        }
        switch type {
        case "confirm": try acceptConfirmation(message)
        case "result": try acceptResult(message)
        case "error": try acceptProtocolError(message)
        default: throw NativeProviderError("effect provider sent an unknown message type")
        }
    }

    private func acceptReady(_ message: [String: Any]) throws {
        try Self.onlyKeys(
            message, ["type", "version", "catalog_digest", "scope_id", "tools", "limits"],
            label: "effect ready message"
        )
        guard Self.integer(message["version"]) == 1,
              message["catalog_digest"] as? String == declaredEndpoint.catalogDigest,
              let scopeID = message["scope_id"] as? String, Self.validHex(scopeID, count: 32),
              let values = message["tools"] as? [Any], values.count <= Self.maximumTools,
              let rawLimits = message["limits"] as? [String: Any] else {
            throw NativeProviderError("effect ready message does not match the pinned catalog")
        }
        var next: [String: NativeEffectDeclaration] = [:]
        for value in values {
            guard let object = value as? [String: Any] else {
                throw NativeProviderError("effect declaration must be an object")
            }
            let declaration = try validateDeclaration(object)
            guard next[declaration.name] == nil else {
                throw NativeProviderError("effect ready message repeats a declaration")
            }
            next[declaration.name] = declaration
        }
        let nextLimits = try Self.validateLimits(rawLimits)
        readyScopeID = scopeID
        declarations = next
        limits = nextLimits
        phase = "ready"
        reconnectAttempt = 0
        diagnostic = ""
        try syncContribution()
        publish(event: ["type": "ready"])
    }

    private func validateDeclaration(_ value: [String: Any]) throws -> NativeEffectDeclaration {
        try Self.onlyKeys(value, [
            "name", "description", "parameters", "host_confirmation", "session_confirmation",
            "target", "mutating", "channel", "digest",
        ], label: "effect declaration")
        let name = try Self.bounded(value["name"], label: "effect name", maximum: Self.maximumNameBytes)
        guard Self.validToolName(name) else { throw NativeProviderError("effect name is not canonical") }
        let summary = try Self.bounded(
            value["description"], label: "effect description",
            maximum: Self.maximumDescriptionBytes
        )
        guard summary == summary.trimmingCharacters(in: .whitespacesAndNewlines),
              let parameters = value["parameters"] as? [String: Any],
              parameters["type"] as? String == "object",
              parameters["additionalProperties"] as? Bool == false else {
            throw NativeProviderError("effect parameters must be a bounded closed object schema")
        }
        let canonicalParameters = try strictJSON.stable(
            parameters, maximumBytes: Self.maximumParametersBytes
        )
        let copiedParameters = try strictJSON.parse(
            canonicalParameters, maximumBytes: Self.maximumParametersBytes
        )
        guard let hostConfirmation = value["host_confirmation"] as? String,
              ["never", "policy", "always"].contains(hostConfirmation),
              value["session_confirmation"] as? String == "never",
              let mutating = value["mutating"] as? Bool,
              let channel = value["channel"] as? String,
              ["tool", "computer", "artifact", "download"].contains(channel),
              let digest = value["digest"] as? String, Self.validDigest(digest) else {
            throw NativeProviderError("effect declaration metadata is invalid")
        }
        let target: String
        if let value = value["target"] {
            target = try Self.bounded(value, label: "effect target", maximum: 256)
            guard target == target.trimmingCharacters(in: .whitespacesAndNewlines),
                  !target.unicodeScalars.contains(where: CharacterSet.whitespacesAndNewlines.contains) else {
                throw NativeProviderError("effect target is not canonical")
            }
        } else { target = "" }
        return NativeEffectDeclaration(
            name: name, summary: summary, parameters: copiedParameters,
            hostConfirmation: hostConfirmation, target: target,
            mutating: mutating, channel: channel, digest: digest
        )
    }

    private func acceptConfirmation(_ message: [String: Any]) throws {
        try Self.onlyKeys(
            message, ["type", "id", "name", "arguments", "nonce", "confirm", "target", "channel"],
            label: "effect confirmation"
        )
        guard let id = message["id"] as? String, let call = pending[id],
              let name = message["name"] as? String, name == call.name,
              let declaration = declarations[name],
              let nonce = message["nonce"] as? String, Self.validHex(nonce, count: 32),
              message["confirm"] as? String == declaration.hostConfirmation,
              (message["target"] as? String ?? "") == declaration.target,
              message["channel"] as? String == declaration.channel,
              let arguments = message["arguments"] as? [String: Any],
              try strictJSON.stable(arguments, maximumBytes: limits!.maxMessageBytes) == call.arguments else {
            throw NativeProviderError("effect confirmation does not bind the admitted call")
        }
        let request = ConfirmationRequest(
            id: id, name: name,
            consequence: declaration.target.isEmpty
                ? "The host requests confirmation for this \(declaration.channel) effect."
                : "The host requests confirmation for target \(declaration.target).",
            arguments: prettyJSONString(arguments)
        )
        confirmations[id] = request
        confirmationNonces[id] = nonce
        publish(event: message)
    }

    private func acceptResult(_ message: [String: Any]) throws {
        try Self.onlyKeys(
            message, ["type", "id", "channel", "output", "error", "artifact", "download"],
            label: "effect result"
        )
        guard let id = message["id"] as? String, Self.validID(id),
              let call = pending[id],
              !(message.keys.contains("artifact") && message.keys.contains("download")) else {
            throw NativeProviderError("effect result is invalid")
        }
        guard !message.keys.contains("artifact") || call.channel == "artifact",
              !message.keys.contains("download") || call.channel == "download" else {
            throw NativeProviderError("effect result resource does not match the admitted declaration")
        }
        if message.keys.contains("error") {
            guard message["error"] is [String: Any] else {
                throw NativeProviderError("effect result error is invalid")
            }
        } else if message["channel"] as? String != call.channel {
            throw NativeProviderError("effect result channel does not match the admitted declaration")
        }
        pending.removeValue(forKey: id)
        confirmations.removeValue(forKey: id)
        confirmationNonces.removeValue(forKey: id)
        results += 1
        if let errorValue = message["error"] as? [String: Any] {
            try Self.onlyKeys(errorValue, ["code", "message"], label: "effect result error")
            let code = try Self.bounded(errorValue["code"], label: "effect error code", maximum: 128)
            let detail = try Self.bounded(
                errorValue["message"], label: "effect error message",
                maximum: Self.maximumProtocolErrorBytes
            )
            refused += 1
            reducer.toolResult(
                callID: id, status: code == "confirmation_declined" ? "declined" : "failed",
                error: detail
            )
            record("obs.tools", "\(call.name) refused", detail, failed: true)
        } else {
            let output = try Self.bounded(
                message["output"] ?? "", label: "effect output",
                maximum: limits!.maxResultBytes, allowEmpty: true
            )
            reducer.toolResult(callID: id, status: "done", output: output)
            record("obs.tools", "\(call.name) returned", String(output.prefix(600)))
        }
        publish(event: message)
    }

    private func acceptProtocolError(_ message: [String: Any]) throws {
        try Self.onlyKeys(message, ["type", "id", "error"], label: "effect protocol error")
        if let id = message["id"] as? String, pending[id] != nil {
            var result: [String: Any] = ["type": "result", "id": id]
            result["error"] = message["error"]
            try acceptResult(result)
            return
        }
        guard let errorValue = message["error"] as? [String: Any] else {
            throw NativeProviderError("effect protocol error is invalid")
        }
        try Self.onlyKeys(errorValue, ["code", "message"], label: "effect protocol error detail")
        throw NativeProviderError(try Self.bounded(
            errorValue["message"], label: "effect protocol error",
            maximum: Self.maximumProtocolErrorBytes
        ))
    }

    private func consume(_ event: [String: Any]) {
        if event["type"] as? String == "openrealtime.debug.event" {
            handleDebug(event)
            return
        }
        guard event["type"] as? String == "response.function_call_arguments.done",
              limits != nil, let name = event["name"] as? String,
              let declaration = declarations[name] else { return }
        let callID = event["call_id"] as? String ?? ""
        do {
            guard enabled() else {
                throw NativeProviderError("client effects were not negotiated by the realtime server")
            }
            guard Self.validID(callID), pending[callID] == nil else {
                throw NativeProviderError("effect call ID is not canonical or unique")
            }
            let call = try ClientEffectInvocationEncoder.encode(
                event: event, sessionID: activeSessionID,
                declarationName: declaration.name, declarationDigest: declaration.digest,
                maximumMessageBytes: limits!.maxMessageBytes, codec: strictJSON
            )
            guard pending.count < limits!.maxInFlight, calls < limits!.maxCalls else {
                throw NativeProviderError("effect client reached its declared call bound")
            }
            pending[callID] = NativePendingEffect(
                id: callID, name: name, arguments: call.arguments(), channel: declaration.channel
            )
            calls += 1
            try sendEncoded(call.encoded())
            record("act.\(declaration.channel)", name, "admitted bounded host effect call")
            publish(event: ["type": "call", "id": callID, "name": name])
        } catch {
            pending.removeValue(forKey: callID)
            refused += 1
            if Self.validID(callID) {
                reducer.toolResult(callID: callID, status: "failed", error: error.localizedDescription)
            }
            report(error)
        }
    }

    private func send(_ value: [String: Any]) throws {
        guard let socket, let limits else {
            throw NativeProviderError("effect provider is not ready")
        }
        let encoded = try strictJSON.stable(value, maximumBytes: limits.maxMessageBytes)
        guard let text = String(data: encoded, encoding: .utf8) else {
            throw NativeProviderError("effect request is not UTF-8")
        }
        let expectedGeneration = generation
        Task { [weak self, weak socket] in
            guard let self, let socket else { return }
            do { try await socket.send(.string(text)) }
            catch {
                guard self.mounted, self.generation == expectedGeneration,
                      self.socket === socket else { return }
                self.socketFailed(error)
            }
        }
    }

    private func sendEncoded(_ encoded: Data) throws {
        guard let socket, let limits, encoded.count <= limits.maxMessageBytes,
              let text = String(data: encoded, encoding: .utf8) else {
            throw NativeProviderError("effect provider is not ready for a bounded request")
        }
        let expectedGeneration = generation
        Task { [weak self, weak socket] in
            guard let self, let socket else { return }
            do { try await socket.send(.string(text)) }
            catch {
                guard self.mounted, self.generation == expectedGeneration,
                      self.socket === socket else { return }
                self.socketFailed(error)
            }
        }
    }

    private func socketFailed(_ error: Error) {
        report(error)
        closeSocket(reason: "effect provider disconnected", scheduleReconnect: true)
    }

    private func closeSocket(reason: String, scheduleReconnect: Bool = false) {
        reconnectTask?.cancel()
        reconnectTask = nil
        generation += 1
        receiveTask?.cancel()
        receiveTask = nil
        socket?.cancel(with: .normalClosure, reason: Data(reason.prefix(120).utf8))
        socket = nil
        session?.invalidateAndCancel()
        session = nil
        readyScopeID = ""
        declarations.removeAll()
        limits = nil
        phase = mounted ? "idle" : "closed"
        clearContribution()
        failPending(reason)
        if scheduleReconnect { scheduleReconnectTask() }
    }

    private func failPending(_ reason: String) {
        for call in pending.values {
            reducer.toolResult(callID: call.id, status: "failed", error: reason)
        }
        pending.removeAll()
        confirmations.removeAll()
        confirmationNonces.removeAll()
        publish(event: ["type": "provider_lost", "reason": String(reason.prefix(4_096))])
    }

    private func scheduleReconnectTask() {
        guard mounted, !activeSessionID.isEmpty, configuredEndpoint != nil else { return }
        let delay = Self.reconnectDelaysMS[min(reconnectAttempt, Self.reconnectDelaysMS.count - 1)]
        reconnectAttempt += 1
        let expectedGeneration = generation
        reconnectTask = Task { [weak self] in
            do { try await Task.sleep(nanoseconds: UInt64(delay) * 1_000_000) }
            catch { return }
            guard let self, self.mounted, self.generation == expectedGeneration,
                  !self.activeSessionID.isEmpty else { return }
            self.reconnectTask = nil
            self.reconnects += 1
            self.openSocket()
        }
    }

    private func report(_ error: Error) {
        let value = error.localizedDescription
        diagnostic = value.utf8.count <= Self.maximumProtocolErrorBytes
            ? value : "effect client diagnostic exceeds its bound"
        publish(event: ["type": "diagnostic", "message": diagnostic])
    }

    private func handleDebug(_ event: [String: Any]) {
        let timestamp = (event["timestamp_ms"] as? NSNumber)?.int64Value
            ?? Int64((Date().timeIntervalSince1970 * 1_000).rounded())
        let duration = (event["duration_ms"] as? NSNumber)?.doubleValue
        var detail: [String: Any] = [:]
        if let attributes = event["attributes"] as? [String: Any] {
            detail["attributes"] = redactedProtocolValue(attributes)
        }
        if let message = event["message"] as? String, !message.isEmpty { detail["message"] = message }
        if let payload = event["payload"], !(payload is NSNull) {
            detail["payload"] = redactedProtocolValue(payload)
        }
        debugRecords.append(DebugRecord(
            timestampMS: timestamp, category: (event["category"] as? String) ?? "",
            name: (event["name"] as? String) ?? "", phase: (event["phase"] as? String) ?? "",
            durationMS: duration, correlationID: (event["correlation_id"] as? String) ?? "",
            detail: detail.isEmpty ? "" : compactJSONString(detail)
        ))
        if debugRecords.count > Self.maximumDebugRecords {
            debugRecords.removeFirst(debugRecords.count - Self.maximumDebugRecords)
        }
        publish()
    }

    private func record(_ channel: String, _ title: String, _ body: String, failed: Bool = false) {
        records.append(ChannelRecord(
            channel: channel, title: title,
            body: String(body.prefix(64 << 10)), failed: failed
        ))
        if records.count > Self.maximumRecords {
            records.removeFirst(records.count - Self.maximumRecords)
        }
        publish()
    }

    private func publish(event: [String: Any]? = nil) {
        let current = snapshot()
        for listener in Array(listeners.values) { listener(current, event) }
    }

    private static func sessionTool(_ declaration: NativeEffectDeclaration) -> [String: Any] {
        var extensionValue: [String: Any] = [
            "confirm": "never",
            "client_effect": ["version": 1, "declaration_digest": declaration.digest],
        ]
        if !declaration.target.isEmpty { extensionValue["target"] = declaration.target }
        return [
            "type": "function", "name": declaration.name,
            "description": declaration.summary, "parameters": declaration.parameters,
            "openrealtime": extensionValue,
        ]
    }

    private static func validateLimits(_ value: [String: Any]) throws -> NativeEffectLimits {
        try onlyKeys(value, [
            "max_message_bytes", "max_result_bytes", "max_in_flight", "max_calls",
            "confirmation_timeout_ms", "execution_timeout_ms",
        ], label: "effect limits")
        let result = NativeEffectLimits(
            maxMessageBytes: try positive(value["max_message_bytes"], "max_message_bytes", maximumWireBytes),
            maxResultBytes: try positive(value["max_result_bytes"], "max_result_bytes", 1 << 20),
            maxInFlight: try positive(value["max_in_flight"], "max_in_flight", 256),
            maxCalls: try positive(value["max_calls"], "max_calls", 16_384),
            confirmationTimeoutMS: try positive(value["confirmation_timeout_ms"], "confirmation_timeout_ms", 300_000),
            executionTimeoutMS: try positive(value["execution_timeout_ms"], "execution_timeout_ms", 300_000)
        )
        guard result.maxResultBytes < result.maxMessageBytes,
              result.maxCalls >= result.maxInFlight,
              result.confirmationTimeoutMS >= 100, result.executionTimeoutMS >= 100 else {
            throw NativeProviderError("effect limits are internally inconsistent")
        }
        return result
    }

    private static func onlyKeys(_ value: [String: Any], _ allowed: Set<String>, label: String) throws {
        guard value.keys.allSatisfy(allowed.contains) else {
            throw NativeProviderError("\(label) contains an unknown field")
        }
    }

    private static func bounded(
        _ value: Any?, label: String, maximum: Int, allowEmpty: Bool = false
    ) throws -> String {
        guard let value = value as? String,
              (allowEmpty || !value.isEmpty), value.utf8.count <= maximum else {
            throw NativeProviderError("\(label) is not a bounded string")
        }
        return value
    }

    private static func integer(_ value: Any?) -> Int? {
        guard !(value is Bool), let value = value as? NSNumber,
              value.doubleValue.isFinite,
              value.doubleValue.rounded(.towardZero) == value.doubleValue else { return nil }
        return value.intValue
    }

    private static func positive(_ value: Any?, _ label: String, _ maximum: Int) throws -> Int {
        guard let value = integer(value), (1...maximum).contains(value) else {
            throw NativeProviderError("\(label) is outside its declared bound")
        }
        return value
    }

    private static func validID(_ value: String) -> Bool {
        let bytes = Array(value.utf8)
        return !bytes.isEmpty && bytes.count <= 128 && bytes.allSatisfy {
            (0x41...0x5a).contains($0) || (0x61...0x7a).contains($0)
                || (0x30...0x39).contains($0) || [0x5f, 0x2e, 0x3a, 0x2d].contains($0)
        }
    }

    private static func validToolName(_ value: String) -> Bool {
        let bytes = Array(value.utf8)
        guard !bytes.isEmpty, bytes.count <= 128,
              (0x41...0x5a).contains(bytes[0]) || (0x61...0x7a).contains(bytes[0]) else { return false }
        return bytes.dropFirst().allSatisfy {
            (0x41...0x5a).contains($0) || (0x61...0x7a).contains($0)
                || (0x30...0x39).contains($0) || [0x5f, 0x2e, 0x2d].contains($0)
        }
    }

    private static func validDigest(_ value: String) -> Bool {
        value.hasPrefix("sha256:") && validHex(String(value.dropFirst(7)), count: 64)
    }

    private static func validHex(_ value: String, count: Int) -> Bool {
        let bytes = Array(value.utf8)
        return bytes.count == count && bytes.allSatisfy {
            (0x30...0x39).contains($0) || (0x61...0x66).contains($0)
        }
    }

}
