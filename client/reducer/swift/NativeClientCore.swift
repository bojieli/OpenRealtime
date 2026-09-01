import Foundation
#if canImport(FoundationNetworking)
import FoundationNetworking
#endif

public final class RealtimeReducerService {
    public static let maxEventBytes = 64 << 10
    public static let maxVirtualTimeMS: Int64 = 86_400_000

    private let reducer = PortableClientReducer()

    public init() {}

    public func apply(atMS: Int64, operation: [String: Any]) throws {
        try reducer.apply(atMS: atMS, operation: operation)
    }

    public func snapshot() throws -> [String: Any] {
        try reducer.snapshotObject()
    }

    public var outbound: [[String: Any]] { reducer.outbound }
}

public enum StrictRealtimeJSON {
    public static func object(from data: Data, maximumBytes: Int = RealtimeReducerService.maxEventBytes) throws -> [String: Any] {
        guard !data.isEmpty else { throw ReducerFailure("JSON message is empty") }
        guard data.count <= maximumBytes else {
            throw ReducerFailure("JSON message exceeds \(maximumBytes) bytes")
        }
        var parser = StrictJSONParser(data: data)
        return try objectValue(parser.parse(), "JSON message")
    }
}

/// Injectable, lifecycle-scoped face for the strict parser. Keeping this as a
/// service prevents views and effect providers from silently falling back to
/// Foundation's permissive JSON parser.
@MainActor
public final class StrictJSONService {
    private var disposed = false

    public init() {}

    public func parse(_ data: Data, maximumBytes: Int = RealtimeReducerService.maxEventBytes) throws -> [String: Any] {
        guard !disposed else { throw ReducerFailure("strict JSON service is disposed") }
        return try StrictRealtimeJSON.object(from: data, maximumBytes: maximumBytes)
    }

    public func stable(_ value: [String: Any], maximumBytes: Int = RealtimeReducerService.maxEventBytes) throws -> Data {
        guard !disposed else { throw ReducerFailure("strict JSON service is disposed") }
        guard try jsonSize(value) <= maximumBytes else {
            throw ReducerFailure("JSON value exceeds \(maximumBytes) bytes")
        }
        let encoded = try JSONSerialization.data(withJSONObject: value, options: [.sortedKeys])
        _ = try StrictRealtimeJSON.object(from: encoded, maximumBytes: maximumBytes)
        return encoded
    }

    public func dispose() { disposed = true }
}

public struct ValidatedProtocolEvent: Equatable, Sendable {
    public let type: String
    private let canonicalPayload: Data

    fileprivate init(type: String, canonicalPayload: Data) {
        self.type = type
        self.canonicalPayload = canonicalPayload
    }

    public var byteCount: Int { canonicalPayload.count }
    public func encoded() -> Data { canonicalPayload }
    public func snapshot() throws -> [String: Any] {
        try StrictRealtimeJSON.object(from: canonicalPayload)
    }
}

/// Read-only face published to protocol consumers. The corresponding
/// publisher is retained only by the canonical reducer adapter.
@MainActor
public final class ValidatedProtocolEventService {
    private static let maximumListeners = 128
    private var listeners: [UUID: (ValidatedProtocolEvent) -> Void] = [:]
    private var disposed = false

    public static func makeChannel() -> (
        service: ValidatedProtocolEventService,
        publisher: ValidatedProtocolEventPublisher
    ) {
        let service = ValidatedProtocolEventService()
        return (service, ValidatedProtocolEventPublisher(service: service))
    }

    private init() {}

    @discardableResult
    public func subscribe(_ listener: @escaping (ValidatedProtocolEvent) -> Void) throws -> () -> Void {
        guard !disposed else { throw ReducerFailure("protocol event service is disposed") }
        guard listeners.count < Self.maximumListeners else {
            throw ReducerFailure("protocol event listener limit reached")
        }
        let id = UUID()
        listeners[id] = listener
        return { [weak self] in self?.listeners.removeValue(forKey: id) }
    }

    public func dispose() {
        guard !disposed else { return }
        disposed = true
        listeners.removeAll()
    }

    public func suspend() {
        guard !disposed else { return }
        listeners.removeAll()
    }

    fileprivate func publish(_ event: [String: Any]) throws {
        guard !disposed else { throw ReducerFailure("protocol event service is disposed") }
        guard try jsonSize(event) <= RealtimeReducerService.maxEventBytes else {
            throw ReducerFailure("protocol event exceeds the byte limit")
        }
        let encoded = try JSONSerialization.data(withJSONObject: event, options: [.sortedKeys])
        let validated = try StrictRealtimeJSON.object(from: encoded)
        let type = try requiredString(validated, "type", ReducerLimits().maxStringBytes)
        let canonical = try JSONSerialization.data(withJSONObject: validated, options: [.sortedKeys])
        let document = ValidatedProtocolEvent(type: type, canonicalPayload: canonical)
        for listener in Array(listeners.values) { listener(document) }
    }
}

@MainActor
public final class ValidatedProtocolEventPublisher {
    private weak var service: ValidatedProtocolEventService?

    fileprivate init(service: ValidatedProtocolEventService) { self.service = service }

    public func publish(_ event: [String: Any]) throws {
        guard let service else { throw ReducerFailure("protocol event provider is unavailable") }
        try service.publish(event)
    }
}

/// Produces the bounded presentation/debug projection of a validated event.
/// Data-plane consumers still receive the exact event, while views can never
/// render scoped tokens or one-shot effect authority.
public enum ProtocolEventPresentation {
    public static func redacted(_ event: [String: Any]) -> [String: Any] {
        redact(event, key: "") as? [String: Any] ?? [:]
    }

    public static func redactedValue(_ value: Any) -> Any {
        redact(value, key: "")
    }

    private static func redact(_ value: Any, key: String) -> Any {
        let normalized = key.lowercased()
        if ["authority", "token", "access_token", "api_key", "authorization"].contains(normalized) {
            return "‹redacted secret›"
        }
        if let object = value as? [String: Any] {
            var result: [String: Any] = [:]
            for (name, child) in object { result[name] = redact(child, key: name) }
            return result
        }
        if let values = value as? [Any] {
            return values.map { redact($0, key: "") }
        }
        if ["audio", "delta", "frame"].contains(normalized),
           let text = value as? String, text.utf8.count > 96 {
            return "‹\(text.utf8.count) encoded bytes›"
        }
        return value
    }
}

/// One strictly validated client-effect call, encoded for immediate delivery
/// to the host-effects socket. The opaque authority has no public field and
/// never enters pending-call metadata; it exists only inside these bounded
/// wire bytes until the adapter sends and releases them.
public struct EncodedClientEffectCall: Sendable {
    public let id: String
    public let name: String
    private let canonicalArguments: Data
    private let wire: Data

    fileprivate init(id: String, name: String, canonicalArguments: Data, wire: Data) {
        self.id = id
        self.name = name
        self.canonicalArguments = canonicalArguments
        self.wire = wire
    }

    public func arguments() -> Data { canonicalArguments }
    public func encoded() -> Data { wire }
}

public enum ClientEffectInvocationEncoder {
    public static let maximumAuthorityBytes = 8_192

    @MainActor
    public static func encode(
        event: [String: Any], sessionID: String,
        declarationName: String, declarationDigest: String,
        maximumMessageBytes: Int, codec: StrictJSONService
    ) throws -> EncodedClientEffectCall {
        guard (1...(48 << 20)).contains(maximumMessageBytes),
              event["type"] as? String == "response.function_call_arguments.done",
              validSessionIdentity(sessionID),
              validEffectName(declarationName), validDigest(declarationDigest),
              let id = event["call_id"] as? String, validEffectCallID(id),
              event["name"] as? String == declarationName,
              let extensionRoot = event["openrealtime"] as? [String: Any],
              let authorityValue = extensionRoot["client_effect"] as? [String: Any],
              authorityValue.keys.allSatisfy(Set([
                  "version", "declaration_digest", "authority",
              ]).contains), authorityValue.count == 3,
              integerValue(authorityValue["version"]) == 1,
              authorityValue["declaration_digest"] as? String == declarationDigest,
              let authority = authorityValue["authority"] as? String,
              !authority.isEmpty, authority.utf8.count <= maximumAuthorityBytes,
              !authority.unicodeScalars.contains(where: CharacterSet.whitespacesAndNewlines.contains),
              let argumentText = event["arguments"] as? String,
              !argumentText.isEmpty, argumentText.utf8.count <= maximumMessageBytes else {
            throw ReducerFailure("effect call lacks exact bounded server authority")
        }
        let arguments = try codec.parse(
            Data(argumentText.utf8), maximumBytes: maximumMessageBytes
        )
        let canonicalArguments = try codec.stable(
            arguments, maximumBytes: maximumMessageBytes
        )
        let wire = try codec.stable([
            "type": "call", "session_id": sessionID, "id": id,
            "name": declarationName, "arguments": arguments,
            "authority": authority,
        ], maximumBytes: maximumMessageBytes)
        return EncodedClientEffectCall(
            id: id, name: declarationName,
            canonicalArguments: canonicalArguments, wire: wire
        )
    }
}

private func integerValue(_ value: Any?) -> Int? {
    guard !(value is Bool), let number = value as? NSNumber, number.doubleValue.isFinite,
          number.doubleValue.rounded(.towardZero) == number.doubleValue else { return nil }
    return number.intValue
}

private func validEffectCallID(_ value: String) -> Bool {
    let bytes = Array(value.utf8)
    return !bytes.isEmpty && bytes.count <= 128 && bytes.allSatisfy {
        (0x41...0x5a).contains($0) || (0x61...0x7a).contains($0)
            || (0x30...0x39).contains($0) || [0x5f, 0x2e, 0x3a, 0x2d].contains($0)
    }
}

private func validEffectName(_ value: String) -> Bool {
    let bytes = Array(value.utf8)
    guard !bytes.isEmpty, bytes.count <= 128,
          (0x41...0x5a).contains(bytes[0]) || (0x61...0x7a).contains(bytes[0]) else {
        return false
    }
    return bytes.dropFirst().allSatisfy {
        (0x41...0x5a).contains($0) || (0x61...0x7a).contains($0)
            || (0x30...0x39).contains($0) || [0x5f, 0x2e, 0x2d].contains($0)
    }
}

public struct SessionConfigurationSnapshot: Equatable, Sendable {
    public let contributions: Int
    public let sessionID: String
    public let diagnostic: String
    private let canonicalConfiguration: Data

    fileprivate init(
        contributions: Int,
        sessionID: String,
        diagnostic: String,
        canonicalConfiguration: Data
    ) {
        self.contributions = contributions
        self.sessionID = sessionID
        self.diagnostic = diagnostic
        self.canonicalConfiguration = canonicalConfiguration
    }

    public func configuration() throws -> [String: Any] {
        try StrictRealtimeJSON.object(from: canonicalConfiguration)
    }
}

private struct SessionConfigurationTool {
    let name: String
    let canonical: Data
    let value: [String: Any]
}

private struct SessionConfigurationFragment {
    let supports: [String]
    let observers: [String]
    let tools: [SessionConfigurationTool]
    let debugEnabled: Bool
    let debugCategories: [String]
}

/// Merges independently disposable session contributions and emits only the
/// canonical partial session.update understood by every client platform.
@MainActor
public final class SessionConfigurationService {
    public static let maximumContributions = 64
    public static let maximumListItems = 128
    public static let maximumConfigurationBytes = 64 << 10
    private static let maximumListeners = 128

    public typealias Apply = ([String: Any]) throws -> Void

    private let apply: Apply
    private var contributions: [Int: SessionConfigurationFragment] = [:]
    private var listeners: [UUID: (SessionConfigurationSnapshot) -> Void] = [:]
    private var serial = 0
    private var latestState: [String: Any] = [:]
    private var lastSession = ""
    private var lastPayload = Data()
    private var diagnostic = ""
    private var disposed = false
    private var reconciling = false

    public init(apply: @escaping Apply) { self.apply = apply }

    @discardableResult
    public func contribute(_ value: [String: Any]) throws -> () -> Void {
        guard !disposed else { throw ReducerFailure("session configuration service is disposed") }
        guard contributions.count < Self.maximumContributions else {
            throw ReducerFailure("session configuration contribution limit reached")
        }
        let fragment = try Self.validate(value)
        serial += 1
        let key = serial
        contributions[key] = fragment
        do { _ = try merge() }
        catch {
            contributions.removeValue(forKey: key)
            throw error
        }
        reconcile()
        var active = true
        return { [weak self] in
            guard active, let self else { return }
            active = false
            self.contributions.removeValue(forKey: key)
            self.reconcile()
        }
    }

    public func observe(state: [String: Any]) {
        guard !disposed else { return }
        latestState = state
        reconcile()
    }

    public func snapshot() throws -> SessionConfigurationSnapshot {
        let configuration = try merge()
        let encoded = try JSONSerialization.data(withJSONObject: configuration, options: [.sortedKeys])
        return SessionConfigurationSnapshot(
            contributions: contributions.count,
            sessionID: lastSession,
            diagnostic: diagnostic,
            canonicalConfiguration: encoded
        )
    }

    @discardableResult
    public func subscribe(
        _ listener: @escaping (SessionConfigurationSnapshot) -> Void
    ) throws -> () -> Void {
        guard !disposed else { throw ReducerFailure("session configuration service is disposed") }
        guard listeners.count < Self.maximumListeners else {
            throw ReducerFailure("session configuration listener limit reached")
        }
        let id = UUID()
        listeners[id] = listener
        listener(try snapshot())
        return { [weak self] in self?.listeners.removeValue(forKey: id) }
    }

    public func dispose() {
        guard !disposed else { return }
        disposed = true
        contributions.removeAll()
        listeners.removeAll()
        latestState.removeAll()
        lastSession = ""
        lastPayload.removeAll()
        diagnostic = ""
    }

    public func suspend() {
        guard !disposed else { return }
        contributions.removeAll()
        listeners.removeAll()
        latestState.removeAll()
        lastSession = ""
        lastPayload.removeAll()
        diagnostic = ""
    }

    private func reconcile() {
        guard !disposed, !reconciling else { return }
        reconciling = true
        defer { reconciling = false }
        do {
            let connection = latestState["connection"] as? [String: Any]
            let session = latestState["session"] as? [String: Any]
            guard connection?["phase"] as? String == "connected",
                  let sessionID = session?["id"] as? String, !sessionID.isEmpty else {
                lastSession = ""
                lastPayload.removeAll()
                try publish()
                return
            }
            let configuration = try merge()
            let payload = try JSONSerialization.data(withJSONObject: configuration, options: [.sortedKeys])
            guard payload.count <= Self.maximumConfigurationBytes else {
                throw ReducerFailure("merged session configuration exceeds the byte limit")
            }
            guard lastSession != sessionID || lastPayload != payload else { return }
            try apply(configuration)
            lastSession = sessionID
            lastPayload = payload
            diagnostic = ""
        } catch {
            diagnostic = String(error.localizedDescription.prefix(ReducerLimits().maxStringBytes))
        }
        try? publish()
    }

    private func publish() throws {
        let value = try snapshot()
        for listener in Array(listeners.values) { listener(value) }
    }

    private func merge() throws -> [String: Any] {
        var supports = Set<String>()
        var observers = Set<String>()
        var categories = Set<String>()
        var tools: [String: SessionConfigurationTool] = [:]
        var debugEnabled = false
        for fragment in contributions.values {
            supports.formUnion(fragment.supports)
            observers.formUnion(fragment.observers)
            categories.formUnion(fragment.debugCategories)
            debugEnabled = debugEnabled || fragment.debugEnabled
            for tool in fragment.tools {
                if let prior = tools[tool.name], prior.canonical != tool.canonical {
                    throw ReducerFailure("conflicting declarations for tool \(quoted(tool.name))")
                }
                tools[tool.name] = tool
            }
        }
        var extensionObject: [String: Any] = [
            "version": 1,
            "supports": supports.sorted(),
            "observers": observers.sorted(),
        ]
        if debugEnabled || !categories.isEmpty {
            extensionObject["debug"] = [
                "enabled": debugEnabled,
                "categories": categories.sorted(),
            ]
        }
        return [
            "type": "realtime",
            "openrealtime": extensionObject,
            "tools": tools.values.sorted { $0.name < $1.name }.map(\.value),
        ]
    }

    private static func validate(_ fragment: [String: Any]) throws -> SessionConfigurationFragment {
        let allowed = Set(["supports", "observers", "tools", "debug"])
        guard fragment.keys.allSatisfy(allowed.contains) else {
            throw ReducerFailure("session configuration contribution has an unknown field")
        }
        guard try jsonSize(fragment) <= maximumConfigurationBytes else {
            throw ReducerFailure("session configuration contribution exceeds the byte limit")
        }
        let supports = try strings(fragment["supports"], "supports")
        let observers = try strings(fragment["observers"], "observers")
        let toolValues: [Any]
        if let rawTools = fragment["tools"] {
            guard let values = rawTools as? [Any] else {
                throw ReducerFailure("session configuration tools must be an array")
            }
            toolValues = values
        } else {
            toolValues = []
        }
        guard toolValues.count <= maximumListItems else {
            throw ReducerFailure("session configuration tools exceed the item limit")
        }
        var tools: [SessionConfigurationTool] = []
        for (index, raw) in toolValues.enumerated() {
            let tool = try objectValue(raw, "tools[\(index)]")
            guard tool["type"] as? String == "function" else {
                throw ReducerFailure("session configuration tool must be a function")
            }
            let name = try requiredString(tool, "name", 1_024)
            let canonical = try JSONSerialization.data(withJSONObject: tool, options: [.sortedKeys])
            let copied = try StrictRealtimeJSON.object(
                from: canonical,
                maximumBytes: maximumConfigurationBytes
            )
            tools.append(SessionConfigurationTool(name: name, canonical: canonical, value: copied))
        }
        var debugEnabled = false
        var debugCategories: [String] = []
        if let rawDebug = fragment["debug"], !(rawDebug is NSNull) {
            let debug = try objectValue(rawDebug, "debug")
            guard debug.keys.allSatisfy(Set(["enabled", "categories"]).contains) else {
                throw ReducerFailure("session configuration debug contribution has an unknown field")
            }
            if let value = debug["enabled"] {
                guard let enabled = value as? Bool else {
                    throw ReducerFailure("session configuration debug.enabled must be a boolean")
                }
                debugEnabled = enabled
            }
            debugCategories = try strings(debug["categories"], "debug categories")
        }
        return SessionConfigurationFragment(
            supports: supports,
            observers: observers,
            tools: tools,
            debugEnabled: debugEnabled,
            debugCategories: debugCategories
        )
    }

    private static func strings(_ value: Any?, _ label: String) throws -> [String] {
        guard let value else { return [] }
        guard let values = value as? [Any], values.count <= maximumListItems else {
            throw ReducerFailure("\(label) exceeds the item limit")
        }
        var result = Set<String>()
        for raw in values {
            let string = try stringValue(raw, label, 1_024)
            result.insert(string)
        }
        return result.sorted()
    }
}

public struct TransportDiagnosticsSnapshot: Equatable, Sendable {
    public let state: String
    public let inboundEvents: Int64
    public let outboundEvents: Int64
    public let inputAudioBytes: Int64
    public let outputAudioBytes: Int64
    public let inputVideoBytes: Int64
    public let queuedMessages: Int64
}

/// Payload-free counters are exposed separately from the transport. The
/// publisher is held by the transport provider; consumers can only snapshot
/// or subscribe and never gain access to wire payloads.
@MainActor
public final class TransportDiagnosticsService {
    private static let maximumListeners = 128
    private var state = "disconnected"
    private var inboundEvents: Int64 = 0
    private var outboundEvents: Int64 = 0
    private var inputAudioBytes: Int64 = 0
    private var outputAudioBytes: Int64 = 0
    private var inputVideoBytes: Int64 = 0
    private var queuedMessages: Int64 = 0
    private var listeners: [UUID: (TransportDiagnosticsSnapshot) -> Void] = [:]
    private var disposed = false

    public static func makeChannel() -> (
        service: TransportDiagnosticsService,
        publisher: TransportDiagnosticsPublisher
    ) {
        let service = TransportDiagnosticsService()
        return (service, TransportDiagnosticsPublisher(service: service))
    }

    private init() {}

    public func snapshot() throws -> TransportDiagnosticsSnapshot {
        guard !disposed else { throw ReducerFailure("transport diagnostics service is disposed") }
        return TransportDiagnosticsSnapshot(
            state: state, inboundEvents: inboundEvents, outboundEvents: outboundEvents,
            inputAudioBytes: inputAudioBytes, outputAudioBytes: outputAudioBytes,
            inputVideoBytes: inputVideoBytes, queuedMessages: queuedMessages
        )
    }

    @discardableResult
    public func subscribe(
        _ listener: @escaping (TransportDiagnosticsSnapshot) -> Void
    ) throws -> () -> Void {
        guard !disposed else { throw ReducerFailure("transport diagnostics service is disposed") }
        guard listeners.count < Self.maximumListeners else {
            throw ReducerFailure("transport diagnostics listener limit reached")
        }
        let id = UUID()
        listeners[id] = listener
        listener(try snapshot())
        return { [weak self] in self?.listeners.removeValue(forKey: id) }
    }

    public func dispose() {
        disposed = true
        listeners.removeAll()
    }

    public func suspend() {
        guard !disposed else { return }
        state = "disconnected"
        queuedMessages = 0
        listeners.removeAll()
    }

    fileprivate func updateState(_ value: String) {
        guard !disposed else { return }
        state = String(value.prefix(256))
        publish()
    }

    fileprivate func recordInbound(_ event: [String: Any]) {
        guard !disposed else { return }
        inboundEvents = saturatedAdd(inboundEvents, 1)
        if event["type"] as? String == "response.output_audio.delta",
           let value = event["delta"] as? String {
            outputAudioBytes = saturatedAdd(outputAudioBytes, Self.decodedByteEstimate(value))
        }
        publish()
    }

    fileprivate func recordOutbound(_ event: [String: Any], queued: Int) {
        guard !disposed else { return }
        outboundEvents = saturatedAdd(outboundEvents, 1)
        queuedMessages = Int64(max(0, queued))
        switch event["type"] as? String {
        case "input_audio_buffer.append":
            if let value = event["audio"] as? String {
                inputAudioBytes = saturatedAdd(inputAudioBytes, Self.decodedByteEstimate(value))
            }
        case "openrealtime.input_video_frame.append":
            if let value = event["frame"] as? String {
                inputVideoBytes = saturatedAdd(inputVideoBytes, Self.decodedByteEstimate(value))
            }
        default: break
        }
        publish()
    }

    fileprivate func updateQueue(_ value: Int) {
        guard !disposed else { return }
        queuedMessages = Int64(max(0, value))
        publish()
    }

    private func publish() {
        guard let current = try? snapshot() else { return }
        for listener in Array(listeners.values) { listener(current) }
    }

    private static func decodedByteEstimate(_ value: String) -> Int64 {
        let count = value.utf8.count
        guard count > 0 else { return 0 }
        let padding = value.hasSuffix("==") ? 2 : value.hasSuffix("=") ? 1 : 0
        return Int64(max(0, (count / 4) * 3 - padding))
    }
}

@MainActor
public final class TransportDiagnosticsPublisher {
    private weak var service: TransportDiagnosticsService?

    fileprivate init(service: TransportDiagnosticsService) { self.service = service }

    public func updateState(_ value: String) { service?.updateState(value) }
    public func recordInbound(_ event: [String: Any]) { service?.recordInbound(event) }
    public func recordOutbound(_ event: [String: Any], queuedMessages: Int = 0) {
        service?.recordOutbound(event, queued: queuedMessages)
    }
    public func updateQueue(_ value: Int) { service?.updateQueue(value) }
}

private func saturatedAdd(_ left: Int64, _ right: Int64) -> Int64 {
    let (sum, overflow) = left.addingReportingOverflow(right)
    return overflow ? Int64.max : sum
}

public struct ClientArtifactReference: Equatable, Sendable, Identifiable {
    public let id: String
    public let title: String
    public let path: String
    public let digest: String
    public let bytes: Int
    public let version: Int
    public let updatedAt: String
}

public struct ClientDownloadReference: Equatable, Sendable, Identifiable {
    public let id: String
    public let filename: String
    public let mediaType: String
    public let path: String
    public let digest: String
    public let bytes: Int
    public let version: Int
    public let updatedAt: String
}

public struct ClientArtifactsSnapshot: Equatable, Sendable {
    public let artifacts: [ClientArtifactReference]
    public let downloads: [ClientDownloadReference]
    public let diagnostic: String
}

/// Bounded immutable metadata for resources published by the host effects
/// provider. Bytes never enter this store; a platform adapter must bind the
/// reference path to its exact declared resource endpoint and verify size and
/// digest before viewing/exporting.
@MainActor
public final class ClientArtifactsService {
    public static let maximumReferences = 256
    public static let maximumArtifactBytes = 8 << 20
    public static let maximumDownloadBytes = 32 << 20
    private static let maximumListeners = 128

    private var artifacts: [String: ClientArtifactReference] = [:]
    private var downloads: [String: ClientDownloadReference] = [:]
    private var artifactOrder: [String] = []
    private var downloadOrder: [String] = []
    private var diagnostic = ""
    private var listeners: [UUID: (ClientArtifactsSnapshot) -> Void] = [:]
    private var disposed = false

    public static func makeChannel() -> (
        service: ClientArtifactsService,
        publisher: ClientArtifactsPublisher
    ) {
        let service = ClientArtifactsService()
        return (service, ClientArtifactsPublisher(service: service))
    }

    private init() {}

    public func snapshot() throws -> ClientArtifactsSnapshot {
        guard !disposed else { throw ReducerFailure("artifact service is disposed") }
        return ClientArtifactsSnapshot(
            artifacts: artifactOrder.compactMap { artifacts[$0] },
            downloads: downloadOrder.compactMap { downloads[$0] },
            diagnostic: diagnostic
        )
    }

    @discardableResult
    public func subscribe(_ listener: @escaping (ClientArtifactsSnapshot) -> Void) throws -> () -> Void {
        guard !disposed else { throw ReducerFailure("artifact service is disposed") }
        guard listeners.count < Self.maximumListeners else {
            throw ReducerFailure("artifact listener limit reached")
        }
        let id = UUID()
        listeners[id] = listener
        listener(try snapshot())
        return { [weak self] in self?.listeners.removeValue(forKey: id) }
    }

    public func artifact(id: String) throws -> ClientArtifactReference? {
        guard !disposed else { throw ReducerFailure("artifact service is disposed") }
        return artifacts[id]
    }

    public func download(id: String) throws -> ClientDownloadReference? {
        guard !disposed else { throw ReducerFailure("artifact service is disposed") }
        return downloads[id]
    }

    public func dispose() {
        disposed = true
        clear()
        listeners.removeAll()
    }

    public func suspend() {
        guard !disposed else { return }
        clear()
        listeners.removeAll()
    }

    fileprivate func publishArtifact(_ value: ClientArtifactReference) throws {
        guard !disposed else { throw ReducerFailure("artifact service is disposed") }
        try validateCommon(
            id: value.id, path: value.path, digest: value.digest,
            bytes: value.bytes, version: value.version,
            updatedAt: value.updatedAt, kind: "artifact",
            maximumBytes: Self.maximumArtifactBytes
        )
        guard canonicalText(value.title, maximum: 512) else {
            throw ReducerFailure("artifact title is invalid")
        }
        try revise(&artifacts, order: &artifactOrder, id: value.id, version: value.version, value: value)
        diagnostic = ""
        publish()
    }

    fileprivate func publishDownload(_ value: ClientDownloadReference) throws {
        guard !disposed else { throw ReducerFailure("artifact service is disposed") }
        try validateCommon(
            id: value.id, path: value.path, digest: value.digest,
            bytes: value.bytes, version: value.version,
            updatedAt: value.updatedAt, kind: "download",
            maximumBytes: Self.maximumDownloadBytes
        )
        guard canonicalText(value.filename, maximum: 255), value.filename != ".", value.filename != "..",
              !value.filename.contains("/"), !value.filename.contains("\\"),
              !value.filename.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }),
              canonicalText(value.mediaType, maximum: 255), validMediaType(value.mediaType) else {
            throw ReducerFailure("download metadata is invalid")
        }
        try revise(&downloads, order: &downloadOrder, id: value.id, version: value.version, value: value)
        diagnostic = ""
        publish()
    }

    fileprivate func report(_ message: String) {
        guard !disposed else { return }
        diagnostic = message.utf8.count <= 4_096
            ? message : "artifact diagnostic exceeds its byte limit"
        publish()
    }

    private func validateCommon(
        id: String, path: String, digest: String, bytes: Int,
        version: Int, updatedAt: String, kind: String, maximumBytes: Int
    ) throws {
        guard validResourceID(id), path == "/client/v1/\(kind)s/\(id)",
              validDigest(digest), (1...maximumBytes).contains(bytes), version > 0,
              updatedAt.utf8.count <= 64,
              validTimestamp(updatedAt) else {
            throw ReducerFailure("\(kind) reference is invalid")
        }
    }

    private func revise<Value>(
        _ values: inout [String: Value], order: inout [String],
        id: String, version: Int, value: Value
    ) throws {
        let priorVersion: Int?
        if let artifact = values[id] as? ClientArtifactReference { priorVersion = artifact.version }
        else if let download = values[id] as? ClientDownloadReference { priorVersion = download.version }
        else { priorVersion = nil }
        guard priorVersion.map({ version > $0 }) ?? true else {
            throw ReducerFailure("resource version must increase")
        }
        if values[id] == nil, values.count == Self.maximumReferences, let oldest = order.first {
            order.removeFirst()
            values.removeValue(forKey: oldest)
        }
        if values[id] != nil { order.removeAll { $0 == id } }
        values[id] = value
        order.append(id)
    }

    private func clear() {
        artifacts.removeAll()
        downloads.removeAll()
        artifactOrder.removeAll()
        downloadOrder.removeAll()
        diagnostic = ""
    }

    private func publish() {
        guard let current = try? snapshot() else { return }
        for listener in Array(listeners.values) { listener(current) }
    }

    private func validResourceID(_ value: String) -> Bool {
        let bytes = Array(value.utf8)
        return !bytes.isEmpty && bytes.count <= 64 && bytes.allSatisfy {
            (0x41...0x5a).contains($0) || (0x61...0x7a).contains($0)
                || (0x30...0x39).contains($0) || $0 == 0x5f || $0 == 0x2d
        }
    }

    private func validTimestamp(_ value: String) -> Bool {
        let basic = ISO8601DateFormatter()
        if basic.date(from: value) != nil { return true }
        let fractional = ISO8601DateFormatter()
        fractional.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return fractional.date(from: value) != nil
    }

    private func canonicalText(_ value: String, maximum: Int) -> Bool {
        !value.isEmpty && value.utf8.count <= maximum
            && value == value.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    private func validMediaType(_ value: String) -> Bool {
        let base = value.split(separator: ";", maxSplits: 1)[0]
        let pieces = base.split(separator: "/", omittingEmptySubsequences: false)
        guard pieces.count == 2, !pieces[0].contains("*"), !pieces[1].contains("*") else { return false }
        let allowed = CharacterSet(charactersIn: "!#$%&'*+-.^_`|~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz")
        return pieces.allSatisfy { !$0.isEmpty && $0.unicodeScalars.allSatisfy(allowed.contains) }
    }
}

@MainActor
public final class ClientArtifactsPublisher {
    private weak var service: ClientArtifactsService?

    fileprivate init(service: ClientArtifactsService) { self.service = service }

    public func publishArtifact(
        id: String, title: String, path: String, digest: String,
        bytes: Int, version: Int, updatedAt: String
    ) throws {
        guard let service else { throw ReducerFailure("artifact provider is unavailable") }
        try service.publishArtifact(ClientArtifactReference(
            id: id, title: title, path: path, digest: digest,
            bytes: bytes, version: version, updatedAt: updatedAt
        ))
    }

    public func publishDownload(
        id: String, filename: String, mediaType: String, path: String,
        digest: String, bytes: Int, version: Int, updatedAt: String
    ) throws {
        guard let service else { throw ReducerFailure("artifact provider is unavailable") }
        try service.publishDownload(ClientDownloadReference(
            id: id, filename: filename, mediaType: mediaType,
            path: path, digest: digest, bytes: bytes,
            version: version, updatedAt: updatedAt
        ))
    }

    public func report(_ message: String) { service?.report(message) }
}

public struct SessionInspectionAccessProjection: Codable, Equatable, Sendable {
    public let sessionID: String
    public let expiresAtMS: Int64

    enum CodingKeys: String, CodingKey {
        case sessionID = "session_id"
        case expiresAtMS = "expires_at_ms"
    }
}

public struct SessionInspectionFailure: LocalizedError, Equatable, Sendable {
    public let message: String

    public init(_ message: String) { self.message = message }
    public var errorDescription: String? { message }
}

private struct SessionInspectionCredential: Equatable {
    let sessionID: String
    let path: String
    let token: String
    let expiresAtMS: Int64

    var projection: SessionInspectionAccessProjection {
        SessionInspectionAccessProjection(sessionID: sessionID, expiresAtMS: expiresAtMS)
    }
}

private enum SessionInspectionProjection {
    case unchanged
    case clear
    case credential(SessionInspectionCredential)
}

/// Stores the short-lived management capability negotiated on the realtime
/// data plane. Its public face is deliberately redacted; only the sibling
/// management client in this module can read the bearer.
public final class SessionInspectionAccessService: @unchecked Sendable {
    public static let maxTokenBytes = 512

    private let lock = NSLock()
    private let clock: () -> Int64
    private var credentialValue: SessionInspectionCredential?
    private var listeners: [UUID: (SessionInspectionAccessProjection?) -> Void] = [:]
    private var disposed = false

    public init(clock: @escaping () -> Int64 = {
        Int64((Date().timeIntervalSince1970 * 1_000).rounded(.down))
    }) {
        self.clock = clock
    }

    /// Validates without mutating. The reducer adapter calls this before its
    /// canonical state transition and commits only after the complete inbound
    /// event has been accepted.
    public func validate(event: [String: Any]) throws {
        _ = try projection(event: event)
    }

    /// Captures only an already accepted session.updated debug capability.
    /// Ordinary updates which omit inspection leave the current rotated
    /// capability in place, while debug disablement and provider loss clear it.
    public func capture(event: [String: Any]) throws {
        switch try projection(event: event) {
        case .unchanged: return
        case .clear: try publish(nil)
        case .credential(let value): try publish(value)
        }
    }

    private func projection(event: [String: Any]) throws -> SessionInspectionProjection {
        guard event["type"] as? String == "session.updated" else { return .unchanged }
        guard try jsonSize(event) <= RealtimeReducerService.maxEventBytes else {
            throw SessionInspectionFailure("session inspection event exceeds the protocol limit")
        }
        let session = try objectValue(event["session"], "session")
        guard let extensionValue = session["openrealtime"], !(extensionValue is NSNull) else {
            return .unchanged
        }
        let extensionObject = try objectValue(extensionValue, "session.openrealtime")
        guard let debugValue = extensionObject["debug"], !(debugValue is NSNull) else {
            return .unchanged
        }
        let debug = try objectValue(debugValue, "session.openrealtime.debug")
        if let enabledValue = debug["enabled"] {
            guard let enabled = enabledValue as? Bool else {
                throw SessionInspectionFailure("session inspection debug.enabled must be a boolean")
            }
            if !enabled { return .clear }
        }
        guard let inspectionValue = debug["inspection"], !(inspectionValue is NSNull) else {
            return .unchanged
        }
        let inspection = try objectValue(inspectionValue, "session.openrealtime.debug.inspection")
        let allowed = Set(["session_id", "path", "token", "expires_at_ms"])
        guard inspection.keys.allSatisfy(allowed.contains), inspection.count == allowed.count else {
            throw SessionInspectionFailure("session inspection access has an unknown or missing field")
        }

        let sessionID = try stringValue(
            inspection["session_id"], "session inspection session_id", 256
        )
        guard validSessionIdentity(sessionID) else {
            throw SessionInspectionFailure("session inspection access has an invalid session identity")
        }
        if let eventSessionID = session["id"] {
            let value = try stringValue(eventSessionID, "session.id", 256)
            guard value == sessionID else {
                throw SessionInspectionFailure("session inspection access is bound to another session")
            }
        }
        let path = try stringValue(
            inspection["path"], "session inspection path", ReducerLimits().maxStringBytes
        )
        guard path == canonicalSessionInspectionPath(sessionID) else {
            throw SessionInspectionFailure("session inspection access has an invalid path")
        }
        let token = try stringValue(
            inspection["token"], "session inspection token", Self.maxTokenBytes
        )
        guard validManagementToken(token) else {
            throw SessionInspectionFailure("session inspection access has an invalid capability")
        }
        let expiresAtMS = try integerValue(
            inspection["expires_at_ms"], "session inspection expires_at_ms", minimum: 1
        )
        return .credential(SessionInspectionCredential(
            sessionID: sessionID, path: path, token: token, expiresAtMS: expiresAtMS
        ))
    }

    public func current() -> SessionInspectionAccessProjection? {
        usableCredential()?.projection
    }

    @discardableResult
    public func subscribe(
        _ listener: @escaping (SessionInspectionAccessProjection?) -> Void
    ) -> () -> Void {
        let id = UUID()
        lock.lock()
        let active = !disposed
        if active { listeners[id] = listener }
        lock.unlock()
        listener(active ? current() : nil)
        return { [weak self] in
            guard let self else { return }
            self.lock.lock()
            self.listeners.removeValue(forKey: id)
            self.lock.unlock()
        }
    }

    public func clear() {
        try? publish(nil)
    }

    public func dispose() {
        lock.lock()
        guard !disposed else {
            lock.unlock()
            return
        }
        disposed = true
        credentialValue = nil
        let callbacks = Array(listeners.values)
        listeners.removeAll()
        lock.unlock()
        callbacks.forEach { $0(nil) }
    }

    fileprivate func authorizedCredential() throws -> SessionInspectionCredential {
        guard let value = usableCredential() else {
            throw SessionInspectionFailure("session inspection is unavailable")
        }
        return value
    }

    private func usableCredential() -> SessionInspectionCredential? {
        let now = clock()
        lock.lock()
        guard !disposed else {
            lock.unlock()
            return nil
        }
        if let current = credentialValue, now >= current.expiresAtMS {
            credentialValue = nil
            let callbacks = Array(listeners.values)
            lock.unlock()
            callbacks.forEach { $0(nil) }
            return nil
        }
        let current = credentialValue
        lock.unlock()
        return current
    }

    private func publish(_ next: SessionInspectionCredential?) throws {
        lock.lock()
        guard !disposed else {
            lock.unlock()
            throw SessionInspectionFailure("session inspection access provider is disposed")
        }
        credentialValue = next
        let callbacks = Array(listeners.values)
        lock.unlock()
        let projection = next?.projection
        callbacks.forEach { $0(projection) }
    }
}

public enum SessionInspectionResource: String, Codable, Sendable {
    case live
    case deltas
    case trace
}

/// An immutable management response. Callers receive a freshly parsed object
/// for every projection, so no renderer can mutate another subscriber's state.
public struct SessionInspectionDocument: Equatable, Sendable {
    public let resource: SessionInspectionResource
    public let sessionID: String
    public let responseURL: String
    private let canonicalPayload: Data

    fileprivate init(
        resource: SessionInspectionResource, sessionID: String,
        responseURL: String, canonicalPayload: Data
    ) {
        self.resource = resource
        self.sessionID = sessionID
        self.responseURL = responseURL
        self.canonicalPayload = canonicalPayload
    }

    public var byteCount: Int { canonicalPayload.count }
    public func encoded() -> Data { canonicalPayload }
    public func snapshot() throws -> [String: Any] {
        try StrictRealtimeJSON.object(
            from: canonicalPayload,
            maximumBytes: SessionInspectionClient.maximumResponseBytes
        )
    }
}

private struct BoundedManagementHTTPResult {
    let response: HTTPURLResponse
    let data: Data
}

private final class BoundedManagementTransport:
    NSObject, URLSessionDataDelegate, URLSessionTaskDelegate, @unchecked Sendable
{
    private final class Pending {
        let continuation: CheckedContinuation<BoundedManagementHTTPResult, Error>
        var response: HTTPURLResponse?
        var data = Data()
        var failure: Error?

        init(_ continuation: CheckedContinuation<BoundedManagementHTTPResult, Error>) {
            self.continuation = continuation
        }
    }

    private let lock = NSLock()
    private let maximumBytes: Int
    private var pending: [Int: Pending] = [:]
    private var disposed = false
    private var urlSession: URLSession!

    init(configuration: URLSessionConfiguration, maximumBytes: Int) {
        self.maximumBytes = maximumBytes
        super.init()
        configuration.requestCachePolicy = .reloadIgnoringLocalCacheData
        configuration.urlCache = nil
        configuration.httpCookieStorage = nil
        configuration.httpShouldSetCookies = false
        configuration.urlCredentialStorage = nil
        configuration.httpAdditionalHeaders = [:]
        let queue = OperationQueue()
        queue.name = "openrealtime.native.management"
        queue.maxConcurrentOperationCount = 1
        urlSession = URLSession(configuration: configuration, delegate: self, delegateQueue: queue)
    }

    func execute(_ request: URLRequest) async throws -> BoundedManagementHTTPResult {
        try Task.checkCancellation()
        return try await withCheckedThrowingContinuation {
            (continuation: CheckedContinuation<BoundedManagementHTTPResult, Error>) in
            let task = urlSession.dataTask(with: request)
            lock.lock()
            guard !disposed else {
                lock.unlock()
                continuation.resume(throwing: SessionInspectionFailure("management client is disposed"))
                return
            }
            pending[task.taskIdentifier] = Pending(continuation)
            task.resume()
            lock.unlock()
        }
    }

    func dispose() {
        lock.lock()
        guard !disposed else {
            lock.unlock()
            return
        }
        disposed = true
        let abandoned = Array(pending.values)
        pending.removeAll()
        lock.unlock()
        urlSession.invalidateAndCancel()
        for request in abandoned {
            request.continuation.resume(
                throwing: SessionInspectionFailure("management client is disposed")
            )
        }
    }

    func urlSession(
        _ session: URLSession,
        dataTask: URLSessionDataTask,
        didReceive response: URLResponse,
        completionHandler: @escaping (URLSession.ResponseDisposition) -> Void
    ) {
        lock.lock()
        guard let request = pending[dataTask.taskIdentifier] else {
            lock.unlock()
            completionHandler(.cancel)
            return
        }
        guard let http = response as? HTTPURLResponse else {
            request.failure = SessionInspectionFailure("management endpoint returned a non-HTTP response")
            lock.unlock()
            completionHandler(.cancel)
            return
        }
        if http.expectedContentLength > Int64(maximumBytes) {
            request.failure = SessionInspectionFailure("management response exceeds the byte limit")
            lock.unlock()
            completionHandler(.cancel)
            return
        }
        if let header = http.value(forHTTPHeaderField: "Content-Length") {
            guard let declared = Int64(header), declared >= 0 else {
                request.failure = SessionInspectionFailure("management response has an invalid content length")
                lock.unlock()
                completionHandler(.cancel)
                return
            }
            guard declared <= Int64(maximumBytes) else {
                request.failure = SessionInspectionFailure("management response exceeds the byte limit")
                lock.unlock()
                completionHandler(.cancel)
                return
            }
        }
        request.response = http
        lock.unlock()
        completionHandler(.allow)
    }

    func urlSession(_ session: URLSession, dataTask: URLSessionDataTask, didReceive data: Data) {
        var cancel = false
        lock.lock()
        if let request = pending[dataTask.taskIdentifier], request.failure == nil {
            if data.count > maximumBytes - request.data.count {
                request.failure = SessionInspectionFailure("management response exceeds the byte limit")
                cancel = true
            } else {
                request.data.append(data)
            }
        }
        lock.unlock()
        if cancel { dataTask.cancel() }
    }

    func urlSession(
        _ session: URLSession,
        task: URLSessionTask,
        willPerformHTTPRedirection response: HTTPURLResponse,
        newRequest request: URLRequest,
        completionHandler: @escaping (URLRequest?) -> Void
    ) {
        lock.lock()
        pending[task.taskIdentifier]?.failure = SessionInspectionFailure(
            "management redirects are forbidden"
        )
        lock.unlock()
        completionHandler(nil)
    }

    func urlSession(
        _ session: URLSession,
        task: URLSessionTask,
        didCompleteWithError error: Error?
    ) {
        lock.lock()
        let request = pending.removeValue(forKey: task.taskIdentifier)
        lock.unlock()
        guard let request else { return }
        if let failure = request.failure {
            request.continuation.resume(throwing: failure)
        } else if let error {
            request.continuation.resume(throwing: error)
        } else if let response = request.response {
            request.continuation.resume(returning: BoundedManagementHTTPResult(
                response: response, data: request.data
            ))
        } else {
            request.continuation.resume(
                throwing: SessionInspectionFailure("management response is incomplete")
            )
        }
    }
}

/// Public, UI-independent client for the canonical OpenRealtime management
/// session resources. It never accepts a bearer argument: authority can only
/// arrive through SessionInspectionAccessService and is placed in one header.
public final class SessionInspectionClient: @unchecked Sendable {
    public static let maximumResponseBytes = 32 << 20
    public static let capabilityHeader = "OpenRealtime-Management-Token"

    private let lock = NSLock()
    private let accessSource: SessionInspectionAccessService
    private let transport: BoundedManagementTransport
    private var managementBase: URL?
    private var listeners: [UUID: (SessionInspectionAccessProjection?) -> Void] = [:]
    private var unsubscribeAccess: (() -> Void)?
    private var disposed = false

    public init(
        accessSource: SessionInspectionAccessService,
        configuration: URLSessionConfiguration = .ephemeral
    ) {
        self.accessSource = accessSource
        transport = BoundedManagementTransport(
            configuration: configuration, maximumBytes: Self.maximumResponseBytes
        )
        unsubscribeAccess = accessSource.subscribe { [weak self] _ in self?.notify() }
    }

    /// Pins management calls to one exact, credential-free HTTP(S) base URL.
    /// Changing endpoints first drops any capability minted by the old server.
    public func configure(managementEndpoint: String) throws {
        let base = try validNativeManagementBase(managementEndpoint)
        lock.lock()
        guard !disposed else {
            lock.unlock()
            throw SessionInspectionFailure("session inspection client is disposed")
        }
        let changed = managementBase != nil && managementBase != base
        managementBase = base
        lock.unlock()
        if changed { accessSource.clear() }
        notify()
    }

    public func unbind() {
        lock.lock()
        managementBase = nil
        lock.unlock()
        accessSource.clear()
        notify()
    }

    /// Ends one realtime session without discarding immutable deployment
    /// wiring, allowing the same selected provider to reconnect or remount.
    public func deactivate() {
        accessSource.clear()
    }

    public func available() -> Bool { access() != nil }

    public func access() -> SessionInspectionAccessProjection? {
        lock.lock()
        let active = !disposed && managementBase != nil
        lock.unlock()
        return active ? accessSource.current() : nil
    }

    @discardableResult
    public func subscribe(
        _ listener: @escaping (SessionInspectionAccessProjection?) -> Void
    ) -> () -> Void {
        let id = UUID()
        lock.lock()
        let active = !disposed
        if active { listeners[id] = listener }
        lock.unlock()
        listener(active ? access() : nil)
        return { [weak self] in
            guard let self else { return }
            self.lock.lock()
            self.listeners.removeValue(forKey: id)
            self.lock.unlock()
        }
    }

    public func live() async throws -> SessionInspectionDocument {
        try await read(.live)
    }

    public func deltas(after: Int = 0, limit: Int = 256) async throws -> SessionInspectionDocument {
        guard (0...1_000_000).contains(after), (0...1_000_000).contains(limit) else {
            throw SessionInspectionFailure("session inspection delta query exceeds its bounds")
        }
        return try await read(.deltas, query: [
            URLQueryItem(name: "after", value: String(after)),
            URLQueryItem(name: "limit", value: String(limit)),
        ])
    }

    public func trace() async throws -> SessionInspectionDocument {
        try await read(.trace)
    }

    public func dispose() {
        lock.lock()
        guard !disposed else {
            lock.unlock()
            return
        }
        disposed = true
        managementBase = nil
        let callbacks = Array(listeners.values)
        listeners.removeAll()
        let unsubscribe = unsubscribeAccess
        unsubscribeAccess = nil
        lock.unlock()
        unsubscribe?()
        callbacks.forEach { $0(nil) }
        transport.dispose()
    }

    private func read(
        _ resource: SessionInspectionResource,
        query: [URLQueryItem] = []
    ) async throws -> SessionInspectionDocument {
        let base = try configuredManagementBase()
        let credential = try accessSource.authorizedCredential()
        guard credential.path == canonicalSessionInspectionPath(credential.sessionID) else {
            throw SessionInspectionFailure("session inspection capability is bound to another management path")
        }
        let segment = credential.sessionID.addingPercentEncoding(
            withAllowedCharacters: Self.pathSegmentCharacters
        )
        guard let segment else {
            throw SessionInspectionFailure("session inspection has an invalid session identity")
        }
        var components = URLComponents(url: base, resolvingAgainstBaseURL: false)
        let prefix = components?.percentEncodedPath == "/"
            ? "" : (components?.percentEncodedPath ?? "")
        components?.percentEncodedPath = "\(prefix)/sessions/\(segment)/\(resource.rawValue)"
        components?.queryItems = query.isEmpty ? nil : query
        guard let url = components?.url else {
            throw SessionInspectionFailure("session inspection endpoint is invalid")
        }
        var request = URLRequest(url: url)
        request.httpMethod = "GET"
        request.timeoutInterval = 15
        request.cachePolicy = .reloadIgnoringLocalCacheData
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        request.setValue("no-store", forHTTPHeaderField: "Cache-Control")
        request.setValue(credential.token, forHTTPHeaderField: Self.capabilityHeader)
        let result = try await transport.execute(request)
        guard result.response.url == url else {
            throw SessionInspectionFailure("session inspection response changed origin or path")
        }
        guard (200...299).contains(result.response.statusCode) else {
            throw SessionInspectionFailure(
                "session inspection \(resource.rawValue) returned \(result.response.statusCode)"
            )
        }
        guard try accessSource.authorizedCredential() == credential else {
            throw SessionInspectionFailure("session inspection capability changed during request")
        }
        let object = try StrictRealtimeJSON.object(
            from: result.data, maximumBytes: Self.maximumResponseBytes
        )
        let canonical = try JSONSerialization.data(withJSONObject: object, options: [.sortedKeys])
        guard canonical.count <= Self.maximumResponseBytes else {
            throw SessionInspectionFailure("session inspection response exceeds the byte limit")
        }
        return SessionInspectionDocument(
            resource: resource, sessionID: credential.sessionID,
            responseURL: url.absoluteString, canonicalPayload: canonical
        )
    }

    // Keep synchronous state access outside the async method. Swift 6 rejects
    // direct NSLock operations from an asynchronous context, even when no
    // suspension can occur between lock and unlock.
    private func configuredManagementBase() throws -> URL {
        lock.lock()
        defer { lock.unlock() }
        guard !disposed, let base = managementBase else {
            throw SessionInspectionFailure("session inspection is unavailable")
        }
        return base
    }

    private func notify() {
        let projection = access()
        lock.lock()
        let callbacks = disposed ? [] : Array(listeners.values)
        lock.unlock()
        callbacks.forEach { $0(projection) }
    }

    private static var pathSegmentCharacters: CharacterSet {
        CharacterSet.alphanumerics.union(CharacterSet(charactersIn: "-._~"))
    }
}

private func validNativeManagementBase(_ endpoint: String) throws -> URL {
    guard endpoint == endpoint.trimmingCharacters(in: .whitespacesAndNewlines),
          endpoint.utf8.count <= 64 << 10,
          let components = URLComponents(string: endpoint),
          let scheme = components.scheme, ["http", "https"].contains(scheme),
          let host = components.host, !host.isEmpty,
          components.user == nil, components.password == nil,
          components.query == nil, components.fragment == nil,
          !components.percentEncodedPath.hasSuffix("/") || components.percentEncodedPath == "/",
          components.url?.absoluteString == endpoint else {
        throw SessionInspectionFailure("the declared management endpoint is invalid")
    }
    for segment in components.percentEncodedPath.split(separator: "/", omittingEmptySubsequences: false) {
        let decoded = String(segment).removingPercentEncoding
        guard decoded != ".", decoded != ".." else {
            throw SessionInspectionFailure("the declared management endpoint is invalid")
        }
    }
    guard let result = components.url else {
        throw SessionInspectionFailure("the declared management endpoint is invalid")
    }
    return result
}

public struct NativeConfigurationProperty: Equatable, Sendable, Identifiable {
    public var id: String { name }
    public let name: String
    public let pointer: String
    public let required: Bool
    public let types: [String]
    public let title: String?
    public let description: String?
    public let format: String?
    public let defaultJSON: String?
    public let enumJSON: [String]?
    public let schemaJSON: String
}

public struct NativeConfigurationContract: Equatable, Sendable, Identifiable {
    public var id: String { elementName }
    public let elementName: String
    public let elementRevision: Int
    public let elementDigest: String
    public let artifact: String
    public let resolved: Bool
    public let inlineTopologyValues: Bool
    public let emptyObjectOnly: Bool
    public let schemaStatus: String
    public let schemaReference: String?
    public let schemaID: String?
    public let schemaDigest: String?
    public let propertiesComplete: Bool
    public let additionalPropertiesJSON: String?
    public let properties: [NativeConfigurationProperty]
}

/// Immutable output for a native configuration editor. Text is a complete,
/// deterministic plaintext rendering; the typed contracts let a SwiftUI view
/// choose richer layout without reparsing untrusted management JSON.
public struct NativeConfigurationPresentation: Equatable, Sendable {
    public let sourceDigest: String
    public let total: Int
    public let incomplete: Bool
    public let contracts: [NativeConfigurationContract]
    public let plaintext: String
}

public struct NativeAuthoringCapabilityStatus: Equatable, Sendable {
    public let available: Bool
    public let expiresAtMS: Int64
    public let generation: UInt64
}

private struct NativeAuthoringCapabilityLease: Equatable {
    let token: String
    let generation: UInt64
}

/// Narrow native client for authoring analysis. It deliberately has no
/// compile, render, reconciliation, or file-write operation. The operator
/// bearer is private mutable service state and can never be passed to analyze,
/// serialized into the document, or confused with session-inspection access.
public final class NativeAuthoringClient: @unchecked Sendable {
    public static let maximumSourceBytes = 1 << 20
    public static let maximumResponseBytes = 64 << 20
    public static let capabilityHeader = "OpenRealtime-Management-Token"
    public static let identityHeader = "OpenRealtime-Management-Identity"

    private let lock = NSLock()
    private let clock: @Sendable () -> Int64
    private let transport: BoundedManagementTransport
    private var managementBase: URL?
    private var capability: String?
    private var expiresAtMS: Int64 = 0
    private var generation: UInt64 = 0
    private var disposed = false

    public init(
        configuration: URLSessionConfiguration = .ephemeral,
        clock: @escaping @Sendable () -> Int64 = {
            Int64((Date().timeIntervalSince1970 * 1_000).rounded(.down))
        }
    ) {
        self.clock = clock
        transport = BoundedManagementTransport(
            configuration: configuration, maximumBytes: Self.maximumResponseBytes
        )
    }

    public func configure(managementEndpoint: String) throws {
        let base = try validNativeManagementBase(managementEndpoint)
        lock.lock()
        guard !disposed else {
            lock.unlock()
            throw SessionInspectionFailure("native authoring client is disposed")
        }
        let changed = managementBase != nil && managementBase != base
        managementBase = base
        if changed { rotateCapabilityLocked(nil, expiresAtMS: 0) }
        lock.unlock()
    }

    @discardableResult
    public func replaceCapability(
        _ token: String, expiresAtMS: Int64 = 0
    ) throws -> NativeAuthoringCapabilityStatus {
        guard validNativeOperatorCapability(token) else {
            throw SessionInspectionFailure("native authoring capability is not canonical")
        }
        let now = clock()
        guard expiresAtMS == 0 || expiresAtMS > now else {
            throw SessionInspectionFailure("native authoring capability expiry is invalid")
        }
        lock.lock()
        defer { lock.unlock() }
        guard !disposed else {
            throw SessionInspectionFailure("native authoring client is disposed")
        }
        rotateCapabilityLocked(token, expiresAtMS: expiresAtMS)
        return statusLocked(now: now)
    }

    @discardableResult
    public func clearCapability() -> NativeAuthoringCapabilityStatus {
        lock.lock()
        defer { lock.unlock() }
        if !disposed { rotateCapabilityLocked(nil, expiresAtMS: 0) }
        return statusLocked(now: clock())
    }

    public func capabilityStatus() -> NativeAuthoringCapabilityStatus {
        let now = clock()
        lock.lock()
        defer { lock.unlock() }
        expireLocked(now: now)
        return statusLocked(now: now)
    }

    public func analyze(
        path: String, source: String, revision: Int = 1
    ) async throws -> NativeConfigurationPresentation {
        let sourceBytes = Data(source.utf8)
        guard validNativeAuthoringPath(path),
              !sourceBytes.isEmpty, sourceBytes.count <= Self.maximumSourceBytes,
              revision > 0 else {
            throw SessionInspectionFailure("native authoring document is invalid")
        }
        let expectedDigest = "sha256:" + PortableNativeSHA256.hexDigest(sourceBytes)
        let body: [String: Any] = ["path": path, "source": source, "revision": revision]
        let encoded = try JSONSerialization.data(withJSONObject: body, options: [.sortedKeys])
        guard encoded.count <= 16 << 20 else {
            throw SessionInspectionFailure("native authoring request exceeds its byte limit")
        }
        let (base, lease) = try requestState()
        var components = URLComponents(url: base, resolvingAgainstBaseURL: false)
        let prefix = components?.percentEncodedPath == "/"
            ? "" : (components?.percentEncodedPath ?? "")
        components?.percentEncodedPath = "\(prefix)/authoring/analyze"
        components?.query = nil
        guard let url = components?.url else {
            throw SessionInspectionFailure("native authoring endpoint is invalid")
        }
        var request = URLRequest(url: url)
        request.httpMethod = "POST"
        request.httpBody = encoded
        request.timeoutInterval = 15
        request.cachePolicy = .reloadIgnoringLocalCacheData
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.setValue("no-store", forHTTPHeaderField: "Cache-Control")
        request.setValue(lease.token, forHTTPHeaderField: Self.capabilityHeader)
        let result = try await transport.execute(request)
        guard result.response.url == url else {
            throw SessionInspectionFailure("native authoring response changed origin or path")
        }
        guard (200...299).contains(result.response.statusCode) else {
            throw SessionInspectionFailure(
                "native authoring endpoint returned \(result.response.statusCode)"
            )
        }
        let mediaType = (result.response.value(forHTTPHeaderField: "Content-Type") ?? "")
            .split(separator: ";", maxSplits: 1).first?.trimmingCharacters(in: .whitespacesAndNewlines)
            .lowercased()
        guard mediaType == "application/json" else {
            throw SessionInspectionFailure("native authoring response is not JSON")
        }
        guard current(lease) else {
            throw SessionInspectionFailure("native authoring capability changed during request")
        }
        let identity = result.response.value(forHTTPHeaderField: Self.identityHeader)
        guard identity == "authoring:analyze:\(expectedDigest)" else {
            throw SessionInspectionFailure("native authoring response lacks exact source identity")
        }
        let object = try StrictRealtimeJSON.object(
            from: result.data, maximumBytes: Self.maximumResponseBytes
        )
        guard current(lease) else {
            throw SessionInspectionFailure("native authoring capability changed during response")
        }
        return try nativeConfigurationPresentation(
            analysis: object, expectedSourceDigest: expectedDigest
        )
    }

    public func suspend() {
        lock.lock()
        if !disposed {
            managementBase = nil
            rotateCapabilityLocked(nil, expiresAtMS: 0)
        }
        lock.unlock()
    }

    public func dispose() {
        lock.lock()
        guard !disposed else {
            lock.unlock()
            return
        }
        disposed = true
        managementBase = nil
        rotateCapabilityLocked(nil, expiresAtMS: 0)
        lock.unlock()
        transport.dispose()
    }

    private func requestState() throws -> (URL, NativeAuthoringCapabilityLease) {
        let now = clock()
        lock.lock()
        defer { lock.unlock() }
        expireLocked(now: now)
        guard !disposed, let base = managementBase, let token = capability else {
            throw SessionInspectionFailure("native authoring capability is unavailable")
        }
        return (base, NativeAuthoringCapabilityLease(token: token, generation: generation))
    }

    private func current(_ lease: NativeAuthoringCapabilityLease) -> Bool {
        let now = clock()
        lock.lock()
        defer { lock.unlock() }
        expireLocked(now: now)
        return !disposed && capability == lease.token && generation == lease.generation
    }

    private func expireLocked(now: Int64) {
        if capability != nil && expiresAtMS != 0 && now >= expiresAtMS {
            rotateCapabilityLocked(nil, expiresAtMS: 0)
        }
    }

    private func rotateCapabilityLocked(_ token: String?, expiresAtMS: Int64) {
        generation &+= 1
        capability = token
        self.expiresAtMS = expiresAtMS
    }

    private func statusLocked(now: Int64) -> NativeAuthoringCapabilityStatus {
        NativeAuthoringCapabilityStatus(
            available: !disposed && capability != nil &&
                (expiresAtMS == 0 || now < expiresAtMS),
            expiresAtMS: expiresAtMS, generation: generation
        )
    }
}

private func nativeConfigurationPresentation(
    analysis: [String: Any], expectedSourceDigest: String
) throws -> NativeConfigurationPresentation {
    try onlyNativeKeys(
        analysis,
        ["source_digest", "parsed", "recovered", "canonical", "diagnostics", "catalog", "formatting"],
        "native authoring analysis"
    )
    guard analysis["source_digest"] as? String == expectedSourceDigest,
          let parsed = analysis["parsed"] as? Bool,
          let recovered = analysis["recovered"] as? Bool,
          let canonical = analysis["canonical"] as? Bool,
          parsed != recovered, !canonical || parsed else {
        throw SessionInspectionFailure("native authoring analysis changed source identity")
    }
    let report = try objectValue(analysis["catalog"], "native configuration metadata report")
    try onlyNativeKeys(report, ["elements", "total", "incomplete"], "native configuration metadata report")
    guard let rows = report["elements"] as? [Any], rows.count <= 65_536,
          let totalValue = try? integerValue(
            report["total"], "native configuration metadata total", minimum: 0
          ), totalValue >= Int64(rows.count), totalValue <= 65_536 else {
        throw SessionInspectionFailure("native configuration metadata report exceeds its bounds")
    }
    let total = Int(totalValue)
    let incomplete: Bool
    if let value = report["incomplete"] {
        guard let exact = value as? Bool else {
            throw SessionInspectionFailure("native configuration metadata completeness is invalid")
        }
        incomplete = exact
    } else {
        incomplete = false
    }
    guard incomplete == (rows.count < total) else {
        throw SessionInspectionFailure("native configuration metadata completeness is inconsistent")
    }
    var contracts: [NativeConfigurationContract] = []
    contracts.reserveCapacity(rows.count)
    var previousElement = ""
    for (index, value) in rows.enumerated() {
        let row = try objectValue(value, "native configuration element[\(index)]")
        try onlyNativeKeys(
            row,
            ["identity", "topology_declaration", "generics", "ports", "reaction", "state_schema",
             "config", "dependencies", "effects", "composite_fingerprint"],
            "native configuration element[\(index)]"
        )
        let identity = try objectValue(row["identity"], "native configuration element identity")
        try onlyNativeKeys(identity, ["name", "revision", "digest"], "native configuration element identity")
        guard let name = identity["name"] as? String, validNativeSymbol(name), name > previousElement,
              let revisionValue = try? integerValue(
                identity["revision"], "native configuration element revision", minimum: 1
              ), revisionValue <= Int64(Int.max),
              let digest = identity["digest"] as? String, validDigest(digest) else {
            throw SessionInspectionFailure("native configuration element identity is invalid")
        }
        let revision = Int(revisionValue)
        previousElement = name
        let config = try objectValue(row["config"], "native configuration contract")
        contracts.append(try nativeConfigurationContract(
            elementName: name, revision: revision, digest: digest, config: config
        ))
    }
    let plaintext = nativeConfigurationPlaintext(contracts)
    guard plaintext.utf8.count <= NativeConfigurationLimits.maximumPresentationBytes else {
        throw SessionInspectionFailure("native configuration presentation exceeds its byte limit")
    }
    return NativeConfigurationPresentation(
        sourceDigest: expectedSourceDigest, total: total, incomplete: incomplete,
        contracts: contracts, plaintext: plaintext
    )
}

private func nativeConfigurationContract(
    elementName: String, revision: Int, digest: String, config: [String: Any]
) throws -> NativeConfigurationContract {
    try onlyNativeKeys(
        config,
        ["artifact", "resolved", "schema_reference", "inline_topology_values", "empty_object_only",
         "schema_status", "schema_id", "schema_digest", "properties_complete", "properties",
         "additional_properties"],
        "native configuration contract"
    )
    guard let artifact = config["artifact"] as? String, validNativeMetadataText(artifact, required: true),
          config["resolved"] as? Bool == true,
          config["inline_topology_values"] as? Bool == false,
          let emptyObjectOnly = config["empty_object_only"] as? Bool,
          let status = config["schema_status"] as? String,
          let propertiesComplete = config["properties_complete"] as? Bool,
          let rawProperties = config["properties"] as? [Any], rawProperties.count <= 65_536 else {
        throw SessionInspectionFailure("native configuration contract changed the topology/value boundary")
    }
    let schemaReference = try optionalNativeMetadataText(config, "schema_reference")
    let schemaID = try optionalNativeMetadataText(config, "schema_id")
    let schemaDigest = try optionalNativeMetadataText(config, "schema_digest")
    var properties: [NativeConfigurationProperty] = []
    properties.reserveCapacity(rawProperties.count)
    var previousProperty = ""
    for (index, value) in rawProperties.enumerated() {
        let property = try objectValue(value, "native configuration property[\(index)]")
        let rendered = try nativeConfigurationProperty(property, previous: previousProperty)
        previousProperty = rendered.name
        properties.append(rendered)
    }
    let additional = try optionalNativeJSON(config, "additional_properties")
    switch status {
    case "empty-object-only":
        guard emptyObjectOnly, propertiesComplete, properties.isEmpty,
              schemaReference == nil, schemaID == nil, schemaDigest == nil, additional == nil else {
            throw SessionInspectionFailure("native empty-object configuration contract is inconsistent")
        }
    case "unresolved", "invalid":
        guard !emptyObjectOnly, !propertiesComplete, properties.isEmpty,
              schemaReference != nil, schemaID == nil, schemaDigest == nil, additional == nil else {
            throw SessionInspectionFailure("native unresolved configuration contract invented fields")
        }
    case "resolved":
        guard !emptyObjectOnly, schemaReference != nil, schemaID != nil,
              let exactDigest = schemaDigest, validDigest(exactDigest) else {
            throw SessionInspectionFailure("native resolved configuration contract is incomplete")
        }
    default:
        throw SessionInspectionFailure("native configuration schema status is invalid")
    }
    return NativeConfigurationContract(
        elementName: elementName, elementRevision: revision, elementDigest: digest,
        artifact: artifact, resolved: true, inlineTopologyValues: false,
        emptyObjectOnly: emptyObjectOnly, schemaStatus: status,
        schemaReference: schemaReference, schemaID: schemaID, schemaDigest: schemaDigest,
        propertiesComplete: propertiesComplete, additionalPropertiesJSON: additional,
        properties: properties
    )
}

private func nativeConfigurationProperty(
    _ property: [String: Any], previous: String
) throws -> NativeConfigurationProperty {
    try onlyNativeKeys(
        property,
        ["name", "pointer", "required", "types", "title", "description", "format", "default", "enum", "schema"],
        "native configuration property"
    )
    guard let name = property["name"] as? String, validNativeMetadataText(name, required: true),
          name > previous,
          property["pointer"] as? String == "#/properties/\(escapeNativeJSONPointer(name))",
          property.keys.contains("schema") else {
        throw SessionInspectionFailure("native configuration property identity is invalid")
    }
    let required: Bool
    if let value = property["required"] {
        guard let exact = value as? Bool else {
            throw SessionInspectionFailure("native configuration property requiredness is invalid")
        }
        required = exact
    } else {
        required = false
    }
    let types: [String]
    if let value = property["types"] {
        guard let exact = value as? [String], exact.count <= 7 else {
            throw SessionInspectionFailure("native configuration property types are invalid")
        }
        let allowed = Set(["array", "boolean", "integer", "null", "number", "object", "string"])
        var prior = ""
        for type in exact {
            guard allowed.contains(type), prior.isEmpty || type > prior else {
                throw SessionInspectionFailure("native configuration property types are not canonical")
            }
            prior = type
        }
        types = exact
    } else {
        types = []
    }
    let title = try optionalNativeMetadataText(property, "title", allowEmpty: true)
    let description = try optionalNativeMetadataText(property, "description", allowEmpty: true)
    let format = try optionalNativeMetadataText(property, "format", allowEmpty: true)
    let defaultJSON = try optionalNativeJSON(property, "default")
    let enumeration: [String]?
    if let value = property["enum"] {
        guard let exact = value as? [Any], exact.count <= 65_536 else {
            throw SessionInspectionFailure("native configuration property enum exceeds its bound")
        }
        enumeration = try exact.map(nativeCanonicalJSON)
    } else {
        enumeration = nil
    }
    guard let schemaValue = property["schema"] else {
        throw SessionInspectionFailure("native configuration property schema is absent")
    }
    return NativeConfigurationProperty(
        name: name, pointer: "#/properties/\(escapeNativeJSONPointer(name))",
        required: required, types: types, title: title, description: description,
        format: format, defaultJSON: defaultJSON, enumJSON: enumeration,
        schemaJSON: try nativeCanonicalJSON(schemaValue)
    )
}

private enum NativeConfigurationLimits {
    static let maximumPresentationBytes = 64 << 20
}

private func nativeConfigurationPlaintext(_ contracts: [NativeConfigurationContract]) -> String {
    var lines: [String] = []
    for contract in contracts {
        lines.append("element: \(nativeQuoted(contract.elementName))")
        lines.append("element revision: \(contract.elementRevision)")
        lines.append("element digest: \(nativeQuoted(contract.elementDigest))")
        lines.append("artifact: \(nativeQuoted(contract.artifact))")
        lines.append("descriptor resolved: \(contract.resolved)")
        lines.append("inline topology values: \(contract.inlineTopologyValues)")
        lines.append("empty object only: \(contract.emptyObjectOnly)")
        lines.append("schema status: \(nativeQuoted(contract.schemaStatus))")
        lines.append("schema reference: \(contract.schemaReference.map(nativeQuoted) ?? "absent")")
        lines.append("schema identity: \(contract.schemaID.map(nativeQuoted) ?? "absent")")
        lines.append("schema digest: \(contract.schemaDigest.map(nativeQuoted) ?? "absent")")
        lines.append("properties complete: \(contract.propertiesComplete)")
        lines.append("additional properties: \(contract.additionalPropertiesJSON ?? "absent")")
        lines.append("property count: \(contract.properties.count)")
        for (index, property) in contract.properties.enumerated() {
            let prefix = "property[\(index)]."
            lines.append("\(prefix)name: \(nativeQuoted(property.name))")
            lines.append("\(prefix)pointer: \(nativeQuoted(property.pointer))")
            lines.append("\(prefix)required: \(property.required)")
            lines.append("\(prefix)types: \(property.types.isEmpty ? "absent" : nativeStringArray(property.types))")
            lines.append("\(prefix)title: \(property.title.map(nativeQuoted) ?? "absent")")
            lines.append("\(prefix)description: \(property.description.map(nativeQuoted) ?? "absent")")
            lines.append("\(prefix)format: \(property.format.map(nativeQuoted) ?? "absent")")
            lines.append("\(prefix)default: \(property.defaultJSON ?? "absent")")
            lines.append("\(prefix)enum: \(property.enumJSON.map { "[" + $0.joined(separator: ",") + "]" } ?? "absent")")
            lines.append("\(prefix)schema: \(property.schemaJSON)")
        }
    }
    return lines.joined(separator: "\n")
}

private func nativeStringArray(_ values: [String]) -> String {
    "[" + values.map(nativeQuoted).joined(separator: ",") + "]"
}

private func nativeQuoted(_ value: String) -> String {
    guard let encoded = try? JSONSerialization.data(
        withJSONObject: value, options: [.fragmentsAllowed, .withoutEscapingSlashes]
    ), let result = String(data: encoded, encoding: .utf8) else { return value }
    return result
}

private func optionalNativeMetadataText(
    _ object: [String: Any], _ key: String, allowEmpty: Bool = false
) throws -> String? {
    guard let value = object[key] else { return nil }
    guard let text = value as? String,
          validNativeMetadataText(text, required: !allowEmpty) else {
        throw SessionInspectionFailure("native configuration metadata field \(quoted(key)) is invalid")
    }
    return text
}

private func optionalNativeJSON(_ object: [String: Any], _ key: String) throws -> String? {
    guard object.keys.contains(key), let value = object[key] else { return nil }
    return try nativeCanonicalJSON(value)
}

private func nativeCanonicalJSON(_ value: Any) throws -> String {
    let encoded = try JSONSerialization.data(
        withJSONObject: value,
        options: [.fragmentsAllowed, .sortedKeys, .withoutEscapingSlashes]
    )
    guard encoded.count <= 64 << 20, let result = String(data: encoded, encoding: .utf8) else {
        throw SessionInspectionFailure("native configuration JSON exceeds its bound")
    }
    return result
}

private func validNativeMetadataText(_ value: String, required: Bool) -> Bool {
    (!required || !value.isEmpty) && value.utf8.count <= 64 << 20
}

private func validNativeAuthoringPath(_ value: String) -> Bool {
    !value.isEmpty && value == value.trimmingCharacters(in: .whitespacesAndNewlines) &&
        value.utf8.count <= 4_096 && !value.contains("\0") &&
        !value.contains("\r") && !value.contains("\n") && value.lowercased().hasSuffix(".ortg")
}

private func validNativeOperatorCapability(_ value: String) -> Bool {
    !value.isEmpty && value == value.trimmingCharacters(in: .whitespacesAndNewlines) &&
        value.utf8.count <= 512 && !value.contains("\0") &&
        !value.contains("\r") && !value.contains("\n")
}

private func validNativeSymbol(_ value: String) -> Bool {
    let parts = value.split(separator: ".", omittingEmptySubsequences: false)
    guard parts.count > 1 else { return false }
    return parts.allSatisfy { part in
        let bytes = Array(part.utf8)
        guard let first = bytes.first,
              (0x41...0x5a).contains(first) || (0x61...0x7a).contains(first) else { return false }
        return bytes.dropFirst().allSatisfy {
            (0x41...0x5a).contains($0) || (0x61...0x7a).contains($0) ||
                (0x30...0x39).contains($0) || $0 == 0x5f || $0 == 0x2d
        }
    }
}

private func escapeNativeJSONPointer(_ value: String) -> String {
    value.replacingOccurrences(of: "~", with: "~0").replacingOccurrences(of: "/", with: "~1")
}

private func validSessionIdentity(_ value: String) -> Bool {
    let bytes = Array(value.utf8)
    return !bytes.isEmpty && bytes.count <= 256 && bytes.allSatisfy {
        (0x41...0x5a).contains($0) || (0x61...0x7a).contains($0) ||
            (0x30...0x39).contains($0) || [0x2e, 0x5f, 0x3a, 0x2d].contains($0)
    }
}

private func canonicalSessionInspectionPath(_ sessionID: String) -> String {
    "/openrealtime/v1/sessions/\(sessionID)/live"
}

private func validManagementToken(_ value: String) -> Bool {
    let bytes = Array(value.utf8)
    let prefix = Array("mgmt_".utf8)
    guard bytes.count > prefix.count, Array(bytes.prefix(prefix.count)) == prefix else { return false }
    return bytes.dropFirst(prefix.count).allSatisfy {
        (0x41...0x5a).contains($0) || (0x61...0x7a).contains($0) ||
            (0x30...0x39).contains($0) || $0 == 0x5f || $0 == 0x2d
    }
}

public enum NativeClientService: String, CaseIterable, Codable, Sendable {
    case slots = "presentation.client.slots"
    case strictJSON = "presentation.client.strict_json"
    case connection = "presentation.client.connection"
    case transportDiagnostics = "presentation.client.transport_diagnostics"
    case reducer = "presentation.client.session_state"
    case protocolEvents = "presentation.client.protocol_events"
    case inspectionAccess = "presentation.client.inspection_access"
    case sessionConfiguration = "presentation.client.session_configuration"
    case authoring = "presentation.client.management_authoring"
    case media = "presentation.client.media"
    case video = "presentation.client.video"
    case effects = "presentation.client.tools_effects"
    case artifacts = "presentation.client.artifacts"
    case inspection = "presentation.client.inspection"
    case view = "presentation.client.view"
}

public enum NativeClientDistribution: String, Codable, Sendable {
    case effectsDeveloper = "effects-developer"
    case observerDeveloper = "observer-developer"

    /// Empty or absent configuration selects the authority-free observer
    /// distribution. Every explicit value must match exactly so a misspelling
    /// cannot silently mount a different permission profile.
    public static func parse(_ value: String?) throws -> NativeClientDistribution {
        guard let value, !value.isEmpty else { return .observerDeveloper }
        guard let distribution = Self(rawValue: value) else {
            throw ReducerFailure("unsupported native client distribution \(quoted(value))")
        }
        return distribution
    }
}

public struct NativeClientPermission: Codable, Equatable, Sendable {
    public let kind: String
    public let resource: String
    public let operations: [String]

    public init(kind: String, resource: String, operations: [String]) {
        self.kind = kind
        self.resource = resource
        self.operations = operations
    }
}

public struct NativeProviderSelection: Codable, Equatable, Sendable {
    public let id: String
    public let service: NativeClientService
    public let implementation: String
    public let requires: [NativeClientService]
    public let permissions: [NativeClientPermission]

    public init(
        id: String,
        service: NativeClientService,
        implementation: String,
        requires: [NativeClientService] = [],
        permissions: [NativeClientPermission] = []
    ) {
        self.id = id
        self.service = service
        self.implementation = implementation
        self.requires = requires
        self.permissions = permissions
    }
}

public struct NativeManifestEndpoint: Codable, Equatable, Sendable {
    public static let clientEffectsProtocol = "openrealtime.client-effects.v1"
    public static let defaultEffectsCatalogDigest = "sha256:10601cffd46c69eb64dc65906209c1a008c0c7ebd823c69cf02329bda2a9c3ec"
    public let name: String
    public let method: String
    public let path: String
    public let protocolName: String
    public let catalogDigest: String

    enum CodingKeys: String, CodingKey {
        case name, method, path
        case protocolName = "protocol"
        case catalogDigest = "catalog_digest"
    }

    public init(
        name: String, method: String, path: String,
        protocolName: String, catalogDigest: String
    ) {
        self.name = name
        self.method = method
        self.path = path
        self.protocolName = protocolName
        self.catalogDigest = catalogDigest
    }
}

/// Resolves the exact credential-free host-effects socket selected by the
/// endpoint directory against the manifest's capability declaration.
public enum NativeEffectEndpointResolver {
    public static func websocketURL(
        endpoint: String, declaration: NativeManifestEndpoint
    ) throws -> URL {
        guard declaration.name == "effects.local", declaration.method == "GET",
              declaration.path == "/client/v1/effects",
              declaration.protocolName == NativeManifestEndpoint.clientEffectsProtocol,
              validDigest(declaration.catalogDigest),
              let components = URLComponents(string: endpoint),
              let scheme = components.scheme, ["ws", "wss"].contains(scheme),
              components.host != nil, components.user == nil, components.password == nil,
              components.query == nil, components.fragment == nil,
              components.percentEncodedPath == declaration.path,
              components.url?.absoluteString == endpoint else {
            throw ReducerFailure("the pinned host effect endpoint is invalid")
        }
        return components.url!
    }
}

public enum NativeEndpointName: String, CaseIterable, Codable, Sendable {
    case realtimeWebSocket = "realtime.websocket"
    case realtimeWebRTC = "realtime.webrtc"
    case management = "management.canonical"
    case effects = "effects.local"
    case artifacts = "resources.artifacts"
    case downloads = "resources.downloads"
}

/// One exact public-wire destination. Credentials and session values belong
/// to their separate providers and cannot appear in this immutable value.
public struct NativeEndpoint: Codable, Equatable, Sendable {
    public static let realtimeWebSocketProtocol = "openai.realtime.websocket.v1"
    public static let realtimeWebRTCProtocol = "openai.realtime.webrtc.v1"
    public static let managementProtocol = "openrealtime.management.v1"
    public static let effectsProtocol = "openrealtime.client-effects.v1"
    public static let artifactsProtocol = "openrealtime.host-artifacts.v1"
    public static let downloadsProtocol = "openrealtime.host-downloads.v1"

    public let name: NativeEndpointName
    public let protocolName: String
    public let url: String

    enum CodingKeys: String, CodingKey {
        case name, url
        case protocolName = "protocol"
    }

    public init(name: NativeEndpointName, protocolName: String, url: String) {
        self.name = name
        self.protocolName = protocolName
        self.url = url
    }
}

/// Versioned, canonical deployment wiring shared by native transports. The
/// fingerprint uses the same canonical JSON field order as the Go endpoint
/// directory. Presence is capability; lookup never rewrites another origin.
public struct NativeEndpointDirectory: Codable, Equatable, Sendable {
    public static let formatVersion = 1
    public let formatVersion: Int
    public let fingerprint: String
    public let endpoints: [NativeEndpoint]

    enum CodingKeys: String, CodingKey {
        case formatVersion = "format_version"
        case fingerprint, endpoints
    }

    public init(formatVersion: Int, fingerprint: String, endpoints: [NativeEndpoint]) {
        self.formatVersion = formatVersion
        self.fingerprint = fingerprint
        self.endpoints = endpoints
    }

    public static func freeze(_ source: [NativeEndpoint]) throws -> NativeEndpointDirectory {
        let endpoints = source.sorted { $0.name.rawValue < $1.name.rawValue }
        let directory = NativeEndpointDirectory(
            formatVersion: Self.formatVersion,
            fingerprint: fingerprint(for: endpoints), endpoints: endpoints
        )
        try directory.validate()
        return directory
    }

    public static func decodeStrict(_ data: Data) throws -> NativeEndpointDirectory {
        let root = try StrictRealtimeJSON.object(from: data, maximumBytes: 1 << 20)
        try onlyNativeKeys(
            root, ["format_version", "fingerprint", "endpoints"],
            "native endpoint directory"
        )
        guard let endpoints = root["endpoints"] as? [Any] else {
            throw ReducerFailure("native endpoint directory endpoints must be an array")
        }
        for (index, value) in endpoints.enumerated() {
            let endpoint = try objectValue(value, "native endpoint directory endpoints[\(index)]")
            try onlyNativeKeys(
                endpoint, ["name", "protocol", "url"],
                "native endpoint directory endpoints[\(index)]"
            )
        }
        let normalized = try JSONSerialization.data(withJSONObject: root, options: [.sortedKeys])
        let directory = try JSONDecoder().decode(NativeEndpointDirectory.self, from: normalized)
        try directory.validate()
        return directory
    }

    public func validate() throws {
        guard formatVersion == Self.formatVersion else {
            throw ReducerFailure("unsupported native endpoint directory format \(formatVersion)")
        }
        guard endpoints.count <= NativeEndpointName.allCases.count else {
            throw ReducerFailure("native endpoint directory exceeds its endpoint bound")
        }
        var names = Set<NativeEndpointName>()
        var prior = ""
        for endpoint in endpoints {
            guard names.insert(endpoint.name).inserted else {
                throw ReducerFailure("native endpoint directory repeats \(quoted(endpoint.name.rawValue))")
            }
            guard prior.isEmpty || prior < endpoint.name.rawValue else {
                throw ReducerFailure("native endpoint directory is not in canonical order")
            }
            prior = endpoint.name.rawValue
            try Self.validate(endpoint)
        }
        guard fingerprint == Self.fingerprint(for: endpoints) else {
            throw ReducerFailure("native endpoint directory fingerprint does not match its contents")
        }
    }

    public func endpoint(
        named name: NativeEndpointName, protocol protocolName: String
    ) throws -> NativeEndpoint {
        guard let endpoint = endpoints.first(where: { $0.name == name }) else {
            throw ReducerFailure("native endpoint directory does not declare \(quoted(name.rawValue))")
        }
        guard endpoint.protocolName == protocolName else {
            throw ReducerFailure("native endpoint directory protocol does not match \(quoted(name.rawValue))")
        }
        return endpoint
    }

    /// Cross-checks deployment wiring against the exact providers selected by
    /// a native client manifest before any provider factory is invoked.
    public func validate(selectedBy manifest: NativeClientManifest) throws {
        try validate()
        try manifest.validate()
        let services = Set(manifest.providers.map(\.service))
        // Exactly one realtime endpoint, and the transport is whichever one
        // the deployment declared. The two carry the same session and a
        // directory that named both would leave the choice to whichever
        // provider looked first.
        let realtime: (NativeEndpointName, String)
        switch (
            endpoints.contains { $0.name == .realtimeWebSocket },
            endpoints.contains { $0.name == .realtimeWebRTC }
        ) {
        case (true, false):
            realtime = (.realtimeWebSocket, NativeEndpoint.realtimeWebSocketProtocol)
        case (false, true):
            realtime = (.realtimeWebRTC, NativeEndpoint.realtimeWebRTCProtocol)
        case (true, true):
            throw ReducerFailure("native endpoint directory declares two realtime transports")
        default:
            throw ReducerFailure("native endpoint directory declares no realtime transport")
        }
        var selected: [(NativeEndpointName, String)] = [realtime]
        if services.contains(.inspection) || services.contains(.authoring) {
            selected.append((.management, NativeEndpoint.managementProtocol))
        }
        if services.contains(.effects) {
            selected.append((.effects, NativeEndpoint.effectsProtocol))
        }
        if services.contains(.artifacts) {
            selected.append((.artifacts, NativeEndpoint.artifactsProtocol))
            selected.append((.downloads, NativeEndpoint.downloadsProtocol))
        }
        for (name, protocolName) in selected {
            _ = try endpoint(named: name, protocol: protocolName)
        }
        guard endpoints.count == selected.count else {
            throw ReducerFailure("native endpoint directory does not exactly match selected providers")
        }
        if services.contains(.effects) {
            guard let declaration = manifest.endpoints.first(where: { $0.name == "effects.local" }) else {
                throw ReducerFailure("native effects endpoint declaration is unavailable")
            }
            let selectedEffects = try endpoint(
                named: .effects, protocol: NativeEndpoint.effectsProtocol
            )
            _ = try NativeEffectEndpointResolver.websocketURL(
                endpoint: selectedEffects.url, declaration: declaration
            )
        }
        if services.contains(.artifacts) {
            for (name, protocolName, path) in [
                (NativeEndpointName.artifacts, NativeEndpoint.artifactsProtocol, "/client/v1/artifacts"),
                (NativeEndpointName.downloads, NativeEndpoint.downloadsProtocol, "/client/v1/downloads"),
            ] {
                let selectedResource = try endpoint(named: name, protocol: protocolName)
                guard URLComponents(string: selectedResource.url)?.percentEncodedPath == path else {
                    throw ReducerFailure("native resource endpoint does not match its protocol path")
                }
            }
        }
    }
    private static func validate(_ endpoint: NativeEndpoint) throws {
        let expected: (String, Set<String>)
        switch endpoint.name {
        case .realtimeWebSocket:
            expected = (NativeEndpoint.realtimeWebSocketProtocol, ["ws", "wss"])
        case .realtimeWebRTC:
            expected = (NativeEndpoint.realtimeWebRTCProtocol, ["http", "https"])
        case .management:
            expected = (NativeEndpoint.managementProtocol, ["http", "https"])
        case .effects:
            expected = (NativeEndpoint.effectsProtocol, ["ws", "wss"])
        case .artifacts:
            expected = (NativeEndpoint.artifactsProtocol, ["http", "https"])
        case .downloads:
            expected = (NativeEndpoint.downloadsProtocol, ["http", "https"])
        }
        let bytes = Array(endpoint.url.utf8)
        guard endpoint.protocolName == expected.0,
              !bytes.isEmpty, bytes.count <= 64 << 10,
              bytes.allSatisfy({ (0x21...0x7e).contains($0) && ![0x22, 0x26, 0x3c, 0x3e, 0x5c].contains($0) }),
              let components = URLComponents(string: endpoint.url),
              let scheme = components.scheme, expected.1.contains(scheme),
              let host = components.host, !host.isEmpty,
              components.user == nil, components.password == nil,
              components.query == nil, components.fragment == nil,
              components.url?.absoluteString == endpoint.url else {
            throw ReducerFailure("native endpoint \(quoted(endpoint.name.rawValue)) is invalid")
        }
        for segment in components.percentEncodedPath.split(separator: "/", omittingEmptySubsequences: false) {
            let decoded = String(segment).removingPercentEncoding
            guard decoded != ".", decoded != ".." else {
                throw ReducerFailure("native endpoint \(quoted(endpoint.name.rawValue)) is invalid")
            }
        }
        if [.management, .effects, .artifacts, .downloads].contains(endpoint.name) {
            guard !components.percentEncodedPath.hasSuffix("/") || components.percentEncodedPath == "/" else {
                throw ReducerFailure("native endpoint \(quoted(endpoint.name.rawValue)) is not a canonical base")
            }
        }
    }

    private static func fingerprint(for endpoints: [NativeEndpoint]) -> String {
        var payload = Data("{\"format_version\":1,\"fingerprint\":\"\",\"endpoints\":[".utf8)
        for (index, endpoint) in endpoints.enumerated() {
            if index != 0 { payload.append(0x2c) }
            payload.append(Data("{\"name\":\"\(endpoint.name.rawValue)\",\"protocol\":\"\(endpoint.protocolName)\",\"url\":\"\(endpoint.url)\"}".utf8))
        }
        payload.append(Data("]}".utf8))
        return "sha256:" + PortableNativeSHA256.hexDigest(payload)
    }
}

private enum PortableNativeSHA256 {
    private static let initial: [UInt32] = [
        0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
        0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
    ]
    private static let constants: [UInt32] = [
        0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
        0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
        0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
        0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
        0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
        0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
        0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
        0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
    ]

    static func hexDigest(_ data: Data) -> String {
        var message = Array(data)
        let bitCount = UInt64(message.count) &* 8
        message.append(0x80)
        while message.count % 64 != 56 { message.append(0) }
        for shift in stride(from: 56, through: 0, by: -8) {
            message.append(UInt8(truncatingIfNeeded: bitCount >> UInt64(shift)))
        }
        var hash = initial
        var schedule = [UInt32](repeating: 0, count: 64)
        for offset in stride(from: 0, to: message.count, by: 64) {
            for index in 0..<16 {
                let start = offset + index * 4
                schedule[index] = UInt32(message[start]) << 24
                    | UInt32(message[start + 1]) << 16
                    | UInt32(message[start + 2]) << 8
                    | UInt32(message[start + 3])
            }
            for index in 16..<64 {
                let left = rotate(schedule[index - 15], 7) ^ rotate(schedule[index - 15], 18) ^ (schedule[index - 15] >> 3)
                let right = rotate(schedule[index - 2], 17) ^ rotate(schedule[index - 2], 19) ^ (schedule[index - 2] >> 10)
                schedule[index] = schedule[index - 16] &+ left &+ schedule[index - 7] &+ right
            }
            var a = hash[0], b = hash[1], c = hash[2], d = hash[3]
            var e = hash[4], f = hash[5], g = hash[6], h = hash[7]
            for index in 0..<64 {
                let sigma1 = rotate(e, 6) ^ rotate(e, 11) ^ rotate(e, 25)
                let choice = (e & f) ^ ((~e) & g)
                let first = h &+ sigma1 &+ choice &+ constants[index] &+ schedule[index]
                let sigma0 = rotate(a, 2) ^ rotate(a, 13) ^ rotate(a, 22)
                let majority = (a & b) ^ (a & c) ^ (b & c)
                let second = sigma0 &+ majority
                h = g; g = f; f = e; e = d &+ first
                d = c; c = b; b = a; a = first &+ second
            }
            hash[0] &+= a; hash[1] &+= b; hash[2] &+= c; hash[3] &+= d
            hash[4] &+= e; hash[5] &+= f; hash[6] &+= g; hash[7] &+= h
        }
        return hash.map { String(format: "%08x", $0) }.joined()
    }

    private static func rotate(_ value: UInt32, _ amount: UInt32) -> UInt32 {
        (value >> amount) | (value << (32 - amount))
    }
}

public struct NativeClientManifest: Codable, Equatable, Sendable {
    public let formatVersion: Int
    public let platform: String
    public let profileFingerprint: String
    public let lockFingerprint: String
    public let planFingerprint: String
    public let manifestFingerprint: String
    public let endpoints: [NativeManifestEndpoint]
    public let providers: [NativeProviderSelection]

    enum CodingKeys: String, CodingKey {
        case formatVersion = "format_version"
        case platform, endpoints, providers
        case profileFingerprint = "profile_fingerprint"
        case lockFingerprint = "lock_fingerprint"
        case planFingerprint = "plan_fingerprint"
        case manifestFingerprint = "manifest_fingerprint"
    }

    public init(
        formatVersion: Int = 1,
        platform: String = "macos",
        profileFingerprint: String = "sha256:" + String(repeating: "0", count: 64),
        lockFingerprint: String = "sha256:" + String(repeating: "1", count: 64),
        planFingerprint: String = "sha256:" + String(repeating: "2", count: 64),
        manifestFingerprint: String = "sha256:" + String(repeating: "3", count: 64),
        endpoints: [NativeManifestEndpoint] = [NativeManifestEndpoint(
            name: "effects.local", method: "GET", path: "/client/v1/effects",
            protocolName: NativeManifestEndpoint.clientEffectsProtocol,
            catalogDigest: NativeManifestEndpoint.defaultEffectsCatalogDigest
        )],
        providers: [NativeProviderSelection]
    ) {
        self.formatVersion = formatVersion
        self.platform = platform
        self.profileFingerprint = profileFingerprint
        self.lockFingerprint = lockFingerprint
        self.planFingerprint = planFingerprint
        self.manifestFingerprint = manifestFingerprint
        self.endpoints = endpoints
        self.providers = providers
    }

    public static func decodeStrict(_ data: Data) throws -> NativeClientManifest {
        let root = try StrictRealtimeJSON.object(from: data, maximumBytes: 1 << 20)
        try onlyNativeKeys(root, [
            "format_version", "platform", "profile_fingerprint", "lock_fingerprint",
            "plan_fingerprint", "manifest_fingerprint", "endpoints", "providers",
        ], "native manifest")
        guard let endpoints = root["endpoints"] as? [Any] else {
            throw ReducerFailure("native manifest endpoints must be an array")
        }
        for (index, value) in endpoints.enumerated() {
            let endpoint = try objectValue(value, "native manifest endpoints[\(index)]")
            try onlyNativeKeys(
                endpoint, ["name", "method", "path", "protocol", "catalog_digest"],
                "native manifest endpoints[\(index)]"
            )
        }
        guard let rows = root["providers"] as? [Any] else {
            throw ReducerFailure("native manifest providers must be an array")
        }
        for (index, value) in rows.enumerated() {
            let row = try objectValue(value, "native manifest providers[\(index)]")
            try onlyNativeKeys(row, ["id", "service", "implementation", "requires", "permissions"], "native manifest providers[\(index)]")
            guard let permissions = row["permissions"] as? [Any] else {
                throw ReducerFailure("native manifest providers[\(index)].permissions must be an array")
            }
            for (permissionIndex, permissionValue) in permissions.enumerated() {
                let permission = try objectValue(permissionValue, "native permission")
                try onlyNativeKeys(permission, ["kind", "resource", "operations"], "native manifest providers[\(index)].permissions[\(permissionIndex)]")
            }
        }
        let normalized = try JSONSerialization.data(withJSONObject: root, options: [.sortedKeys])
        let manifest = try JSONDecoder().decode(NativeClientManifest.self, from: normalized)
        try manifest.validate()
        return manifest
    }

    public func validate() throws {
        guard formatVersion == 1 else { throw ReducerFailure("unsupported native manifest format \(formatVersion)") }
        guard platform == "macos" else { throw ReducerFailure("native manifest platform must be macos") }
        for fingerprint in [profileFingerprint, lockFingerprint, planFingerprint, manifestFingerprint]
        where !validDigest(fingerprint) {
            throw ReducerFailure("native manifest contains an invalid composition fingerprint")
        }
        guard !providers.isEmpty else { throw ReducerFailure("native manifest selects no providers") }
        var ids = Set<String>()
        var services = Set<NativeClientService>()
        var mounted = Set<NativeClientService>()
        for row in providers {
            guard validIdentifier(row.id), validIdentifier(row.implementation) else {
                throw ReducerFailure("native provider has an invalid identity")
            }
            guard ids.insert(row.id).inserted else { throw ReducerFailure("duplicate native provider id \(quoted(row.id))") }
            guard services.insert(row.service).inserted else { throw ReducerFailure("duplicate native service \(quoted(row.service.rawValue))") }
            guard Set(row.requires).count == row.requires.count, !row.requires.contains(row.service) else {
                throw ReducerFailure("native provider \(quoted(row.id)) has invalid dependencies")
            }
            for dependency in row.requires where !mounted.contains(dependency) {
                throw ReducerFailure("native provider \(quoted(row.id)) dependency \(quoted(dependency.rawValue)) is not mounted earlier")
            }
            try validatePermissions(row)
            mounted.insert(row.service)
        }
        if services.contains(.effects) {
            guard endpoints.count == 1, let endpoint = endpoints.first,
                  endpoint.name == "effects.local", endpoint.method == "GET",
                  endpoint.path == "/client/v1/effects",
                  endpoint.protocolName == NativeManifestEndpoint.clientEffectsProtocol,
                  validDigest(endpoint.catalogDigest) else {
                throw ReducerFailure("native effects provider requires the exact host effect endpoint and catalog")
            }
        } else if !endpoints.isEmpty {
            throw ReducerFailure("native manifest cannot publish an effects endpoint without an effects provider")
        }
    }
}

@MainActor
public protocol NativeClientProvider: AnyObject {
    var service: NativeClientService { get }
    var implementation: String { get }
    var permissions: [NativeClientPermission] { get }
    var instance: AnyObject { get }
    func start(dependencies: NativeProviderDependencies) throws
    func stop() throws
    func dispose() throws
}

public extension NativeClientProvider {
    func dispose() throws { try stop() }
}

public struct NativeProviderDependencies {
    private let declared: Set<NativeClientService>
    private let instances: [NativeClientService: AnyObject]
    public let permissions: [NativeClientPermission]

    init(
        declared: Set<NativeClientService>,
        instances: [NativeClientService: AnyObject],
        permissions: [NativeClientPermission]
    ) {
        self.declared = declared
        self.instances = instances
        self.permissions = permissions
    }

    public func service(_ service: NativeClientService) throws -> AnyObject {
        guard declared.contains(service) else {
            throw ReducerFailure("native provider requested undeclared dependency \(quoted(service.rawValue))")
        }
        guard let instance = instances[service] else {
            throw ReducerFailure("native provider dependency \(quoted(service.rawValue)) is unavailable")
        }
        return instance
    }

    public func allows(kind: String, resource: String, operation: String) -> Bool {
        permissions.contains {
            $0.kind == kind && $0.resource == resource && $0.operations.contains(operation)
        }
    }
}

/// One installed native implementation and its exact manifest contract. A
/// registry entry is deliberately more specific than a service-to-closure
/// map: changing the entry id, dependency set, permission boundary, or
/// implementation identity requires a different registration.
@MainActor
public struct NativeClientProviderFactoryRegistration {
    public typealias Factory = @MainActor (
        NativeProviderFactoryContext
    ) throws -> NativeClientProvider

    public let selection: NativeProviderSelection
    fileprivate let factory: Factory

    public init(selection: NativeProviderSelection, factory: @escaping Factory) {
        self.selection = selection
        self.factory = factory
    }
}

/// Resource-free construction context for a selected native provider. It can
/// resolve only dependencies declared by that provider's manifest row.
@MainActor
public struct NativeProviderFactoryContext {
    public let manifest: NativeClientManifest
    public let selection: NativeProviderSelection
    private let instances: [NativeClientService: AnyObject]

    fileprivate init(
        manifest: NativeClientManifest,
        selection: NativeProviderSelection,
        instances: [NativeClientService: AnyObject]
    ) {
        self.manifest = manifest
        self.selection = selection
        self.instances = instances
    }

    public func service(_ service: NativeClientService) throws -> AnyObject {
        guard selection.requires.contains(service) else {
            throw ReducerFailure("native factory requested undeclared dependency \(quoted(service.rawValue))")
        }
        guard let instance = instances[service] else {
            throw ReducerFailure("native factory dependency \(quoted(service.rawValue)) is unavailable")
        }
        return instance
    }

    public func service<T: AnyObject>(
        _ service: NativeClientService, as type: T.Type
    ) throws -> T {
        guard let value = try self.service(service) as? T else {
            throw ReducerFailure("native factory dependency \(quoted(service.rawValue)) has an unexpected implementation type")
        }
        return value
    }

    public func endpoint(named name: String) throws -> NativeManifestEndpoint {
        guard let endpoint = manifest.endpoints.first(where: { $0.name == name }) else {
            throw ReducerFailure("native manifest endpoint \(quoted(name)) is unavailable")
        }
        return endpoint
    }
}

/// Catalog of native implementations installed in the application. Assembly
/// is manifest driven: the registry preflights every exact selection before
/// invoking any factory, then constructs only selected providers in manifest
/// order. A partial factory failure disposes everything constructed so far.
@MainActor
public final class NativeClientProviderRegistry {
    private var registrations: [String: NativeClientProviderFactoryRegistration] = [:]

    public init() {}

    public func register(_ registration: NativeClientProviderFactoryRegistration) throws {
        let row = registration.selection
        guard validIdentifier(row.id), validIdentifier(row.implementation) else {
            throw ReducerFailure("native factory registration has an invalid identity")
        }
        guard registrations[row.implementation] == nil else {
            throw ReducerFailure("duplicate native implementation registration \(quoted(row.implementation))")
        }
        guard Set(row.requires).count == row.requires.count,
              !row.requires.contains(row.service) else {
            throw ReducerFailure("native factory registration \(quoted(row.id)) has invalid dependencies")
        }
        try validatePermissions(row)
        registrations[row.implementation] = registration
    }

    public func providers(for manifest: NativeClientManifest) throws -> [NativeClientProvider] {
        try manifest.validate()

        // Complete exact-match preflight happens before a factory can allocate
        // a platform object or acquire a resource.
        for row in manifest.providers {
            guard let registration = registrations[row.implementation],
                  exactNativeSelection(row, registration.selection) else {
                throw ReducerFailure("native implementation registration does not match manifest for \(quoted(row.service.rawValue))")
            }
        }

        var instances: [NativeClientService: AnyObject] = [:]
        var providers: [NativeClientProvider] = []
        do {
            for row in manifest.providers {
                guard let registration = registrations[row.implementation] else {
                    throw ReducerFailure("native implementation disappeared for \(quoted(row.service.rawValue))")
                }
                let context = NativeProviderFactoryContext(
                    manifest: manifest, selection: row, instances: instances
                )
                let provider = try registration.factory(context)
                guard provider.service == row.service,
                      provider.implementation == row.implementation,
                      permissionSignatures(provider.permissions) == permissionSignatures(row.permissions) else {
                    try? provider.dispose()
                    throw ReducerFailure("native factory returned a substituted implementation for \(quoted(row.service.rawValue))")
                }
                providers.append(provider)
                instances[row.service] = provider.instance
            }
            return providers
        } catch {
            for provider in providers.reversed() { try? provider.dispose() }
            throw error
        }
    }
}

@MainActor
public final class NativeClientComposition {
    public enum State: Equatable { case idle, active, degraded, stopped }

    public let manifest: NativeClientManifest
    public private(set) var state: State = .idle
    private let selected: [NativeClientService: NativeClientProvider]
    private var instances: [NativeClientService: AnyObject] = [:]
    private var started: [NativeClientProvider] = []

    public init(manifest: NativeClientManifest, providers: [NativeClientProvider]) throws {
        try manifest.validate()
        var selected: [NativeClientService: NativeClientProvider] = [:]
        for provider in providers {
            guard selected[provider.service] == nil else {
                throw ReducerFailure("multiple native implementations supplied for \(quoted(provider.service.rawValue))")
            }
            selected[provider.service] = provider
        }
        guard providers.count == manifest.providers.count else {
            throw ReducerFailure("native provider set does not exactly match manifest")
        }
        for row in manifest.providers {
            guard let provider = selected[row.service], provider.implementation == row.implementation,
                  permissionSignatures(provider.permissions) == permissionSignatures(row.permissions) else {
                throw ReducerFailure("native implementation does not match manifest for \(quoted(row.service.rawValue))")
            }
        }
        self.manifest = manifest
        self.selected = selected
    }

    public convenience init(
        manifest: NativeClientManifest, registry: NativeClientProviderRegistry
    ) throws {
        try self.init(manifest: manifest, providers: registry.providers(for: manifest))
    }

    public func start() throws {
        guard state == .idle else { throw ReducerFailure("native composition can only start once") }
        do {
            for row in manifest.providers {
                guard let provider = selected[row.service] else {
                    throw ReducerFailure("native provider disappeared for \(quoted(row.service.rawValue))")
                }
                let dependencies = NativeProviderDependencies(
                    declared: Set(row.requires), instances: instances, permissions: row.permissions
                )
                try provider.start(dependencies: dependencies)
                started.append(provider)
                instances[row.service] = provider.instance
            }
            state = .active
        } catch {
            for provider in started.reversed() { try? provider.dispose() }
            started.removeAll()
            instances.removeAll()
            state = .stopped
            throw error
        }
    }

    public func service(_ service: NativeClientService) throws -> AnyObject {
        guard state == .active || state == .degraded, let instance = instances[service] else {
            throw ReducerFailure("native service \(quoted(service.rawValue)) is not active")
        }
        return instance
    }

    /// Quiesces a lost provider and every transitive consumer in reverse mount
    /// order. Unrelated services remain available while the composition is
    /// degraded, which mirrors the browser host's scoped provider-loss model.
    public func providerLost(_ service: NativeClientService) throws {
        guard state == .active || state == .degraded, instances[service] != nil else {
            throw ReducerFailure("native provider \(quoted(service.rawValue)) is not mounted")
        }
        var affected: Set<NativeClientService> = [service]
        for row in manifest.providers where row.requires.contains(where: affected.contains) {
            affected.insert(row.service)
        }
        var failures: [Error] = []
        for provider in started.reversed() where affected.contains(provider.service) {
            do { try provider.stop() } catch { failures.append(error) }
            instances.removeValue(forKey: provider.service)
        }
        started.removeAll { affected.contains($0.service) }
        state = .degraded
        if !failures.isEmpty { throw ReducerFailure("one or more lost native providers failed to stop") }
    }

    /// Remounts all missing providers in manifest order. A failed remount is
    /// rolled back to the prior degraded service set and may be retried.
    public func remount() throws {
        guard state == .degraded else {
            throw ReducerFailure("native composition is not awaiting a remount")
        }
        var mountedNow: [NativeClientProvider] = []
        do {
            for row in manifest.providers where instances[row.service] == nil {
                guard let provider = selected[row.service] else {
                    throw ReducerFailure("native provider disappeared for \(quoted(row.service.rawValue))")
                }
                let dependencies = NativeProviderDependencies(
                    declared: Set(row.requires), instances: instances, permissions: row.permissions
                )
                try provider.start(dependencies: dependencies)
                mountedNow.append(provider)
                started.append(provider)
                instances[row.service] = provider.instance
            }
            guard instances.count == manifest.providers.count else {
                throw ReducerFailure("native remount left services unavailable")
            }
            state = .active
        } catch {
            for provider in mountedNow.reversed() {
                try? provider.stop()
                instances.removeValue(forKey: provider.service)
            }
            let mountedServices = Set(mountedNow.map(\.service))
            started.removeAll { mountedServices.contains($0.service) }
            state = .degraded
            throw error
        }
    }

    public func stop() throws {
        guard state != .stopped else { return }
        var failures: [Error] = []
        for provider in started.reversed() {
            do { try provider.dispose() } catch { failures.append(error) }
        }
        started.removeAll()
        instances.removeAll()
        state = .stopped
        if !failures.isEmpty { throw ReducerFailure("one or more native providers failed to stop") }
    }
}

private func onlyNativeKeys(_ object: [String: Any], _ allowed: Set<String>, _ name: String) throws {
    for key in object.keys where !allowed.contains(key) {
        throw ReducerFailure("\(name) has unknown field \(quoted(key))")
    }
}

private func validIdentifier(_ value: String) -> Bool {
    let bytes = Array(value.utf8)
    return !bytes.isEmpty && bytes.count <= 256 && bytes.allSatisfy {
        (0x41...0x5a).contains($0) || (0x61...0x7a).contains($0) ||
            (0x30...0x39).contains($0) || [0x2e, 0x2d, 0x5f].contains($0)
    }
}

private func validDigest(_ value: String) -> Bool {
    let bytes = Array(value.utf8)
    guard bytes.count == 71, Array(bytes.prefix(7)) == Array("sha256:".utf8) else { return false }
    return bytes.dropFirst(7).allSatisfy {
        (0x30...0x39).contains($0) || (0x61...0x66).contains($0)
    }
}

private func validatePermissions(_ row: NativeProviderSelection) throws {
    let allowed = nativePermissionCeiling(row.service)
    var keys = Set<String>()
    for permission in row.permissions {
        let key = permission.kind + "\u{0}" + permission.resource
        guard keys.insert(key).inserted,
              let ceiling = allowed.first(where: { $0.kind == permission.kind && $0.resource == permission.resource }),
              !permission.operations.isEmpty,
              Set(permission.operations).count == permission.operations.count,
              permission.operations.allSatisfy({ ceiling.operations.contains($0) }) else {
            throw ReducerFailure("native provider \(quoted(row.id)) exceeds its permission ceiling")
        }
    }
    guard row.permissions.count == allowed.count else {
        throw ReducerFailure("native provider \(quoted(row.id)) does not declare its exact permission boundary")
    }
}

private func exactNativeSelection(
    _ selected: NativeProviderSelection, _ installed: NativeProviderSelection
) -> Bool {
    selected.id == installed.id && selected.service == installed.service &&
        selected.implementation == installed.implementation &&
        Set(selected.requires) == Set(installed.requires) &&
        permissionSignatures(selected.permissions) == permissionSignatures(installed.permissions)
}

public func nativePermissionCeiling(_ service: NativeClientService) -> [NativeClientPermission] {
    switch service {
    case .connection:
        // One provider builds either transport, so it is granted both. Which
        // one it builds is decided by the deployment's endpoint directory,
        // not by widening a permission at connect time.
        return [NativeClientPermission(
            kind: "network.connect", resource: "realtime-endpoint",
            operations: ["websocket", "webrtc"]
        )]
    case .media:
        return [NativeClientPermission(
            kind: "device.media", resource: "native-audio",
            operations: ["microphone", "playout"]
        )]
    case .video:
        return [
            NativeClientPermission(
                kind: "device.media", resource: "native-video",
                operations: ["camera", "screen"]
            ),
            NativeClientPermission(
                kind: "device.media", resource: "native-browser",
                operations: ["capture"]
            ),
        ]
    case .effects:
        return [NativeClientPermission(
            kind: "network.connect", resource: "host-effects", operations: ["websocket"]
        )]
    case .artifacts:
        return [NativeClientPermission(
            kind: "network.connect", resource: "host-resources", operations: ["http"]
        )]
    case .inspection, .authoring:
        return [NativeClientPermission(kind: "network.connect", resource: "management-endpoint", operations: ["http"])]
    case .slots, .strictJSON, .transportDiagnostics, .reducer, .protocolEvents,
         .inspectionAccess, .sessionConfiguration, .view:
        return []
    }
}

private func permissionSignatures(_ permissions: [NativeClientPermission]) -> [String] {
    permissions.map { permission in
        permission.kind + "\u{0}" + permission.resource + "\u{0}" + permission.operations.sorted().joined(separator: "\u{0}")
    }.sorted()
}
