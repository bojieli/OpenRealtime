import Foundation
import OpenRealtimeClientCore

/// Which server the client is pointed at, and therefore which paths its
/// endpoints live under.
///
/// These are the only two layouts this repository serves. A remote deployment
/// normally uses `.realtimeServer`: the Realtime server is the one process
/// that binds a public address, and the native client needs exactly one origin
/// from it. `.presentationHost` is the loopback companion layout, where a
/// local presentation host relays to the server on the client's behalf.
enum NativeDeploymentLayout: String, CaseIterable, Identifiable, Sendable {
    case realtimeServer
    case presentationHost

    var id: String { rawValue }

    var title: String {
        switch self {
        case .realtimeServer: return "Realtime server"
        case .presentationHost: return "Presentation host"
        }
    }

    var detail: String {
        switch self {
        case .realtimeServer: return "/v1/realtime · /openrealtime/v1"
        case .presentationHost: return "/client/v1/*"
        }
    }

    var realtimePath: String {
        self == .realtimeServer ? "/v1/realtime" : "/client/v1/realtime"
    }

    var managementPath: String {
        self == .realtimeServer ? "/openrealtime/v1" : "/client/v1/management"
    }
}

/// Which realtime transport the client uses.
///
/// Both carry the same session. They differ in what the network has to allow:
/// the WebSocket is one HTTP origin and traverses any HTTP proxy or tunnel,
/// while WebRTC needs its media plane, which is UDP and does not.
enum NativeTransportKind: String, CaseIterable, Identifiable, Sendable {
    case webSocket
    case webRTC

    var id: String { rawValue }

    var title: String {
        switch self {
        case .webSocket: return "OpenRealtime over WebSocket"
        case .webRTC: return "WebRTC"
        }
    }

    var detail: String {
        switch self {
        case .webSocket: return "one HTTP origin; works through an HTTP tunnel"
        case .webRTC: return "needs UDP media; will not traverse an HTTP tunnel"
        }
    }
}

/// An operator-entered deployment: one base URL plus the layout it serves.
///
/// This is the only value the view may edit. It is deployment wiring the
/// person running the client chose, never anything a server told the client;
/// no endpoint is ever derived from another endpoint's URL at runtime.
struct NativeDeploymentSelection: Equatable, Sendable {
    var base: String
    var layout: NativeDeploymentLayout
    var transport: NativeTransportKind = .webSocket
    /// The WebRTC adapter is a separate listener from the Realtime server, so
    /// it cannot be derived from the same base.
    var webRTCBase: String = "http://127.0.0.1:8766"

    static let presentationHostDefault = NativeDeploymentSelection(
        base: "http://127.0.0.1:8767", layout: .presentationHost
    )
}

enum NativeDeploymentError: LocalizedError {
    case base(String)

    var errorDescription: String? {
        switch self {
        case .base(let message): return message
        }
    }
}

/// Builds a complete, frozen endpoint directory from one operator-entered
/// base URL.
///
/// Derivation happens once, here, over a value a person typed, and the result
/// is immediately frozen and fingerprinted by the portable core. Everything
/// downstream still receives one immutable directory and still refuses a
/// directory whose fingerprint does not cover its contents.
enum NativeDeploymentDirectory {
    static let maximumBaseBytes = 2_048

    static func freeze(
        _ selection: NativeDeploymentSelection,
        distribution: NativeClientDistribution
    ) throws -> NativeEndpointDirectory {
        let base = try canonicalBase(selection.base)
        if distribution == .effectsDeveloper, selection.layout == .realtimeServer {
            throw NativeDeploymentError.base(
                "the effects distribution needs a presentation host: the Realtime server serves no effect or resource endpoints"
            )
        }
        let realtime: NativeEndpoint
        switch selection.transport {
        case .webSocket:
            realtime = NativeEndpoint(
                name: .realtimeWebSocket,
                protocolName: NativeEndpoint.realtimeWebSocketProtocol,
                url: base.websocket + selection.layout.realtimePath
            )
        case .webRTC:
            // The adapter is its own listener and answers a single SDP POST
            // at the path the Realtime API publishes.
            let adapter = try canonicalBase(selection.webRTCBase)
            realtime = NativeEndpoint(
                name: .realtimeWebRTC,
                protocolName: NativeEndpoint.realtimeWebRTCProtocol,
                url: adapter.http + "/v1/realtime/calls"
            )
        }
        var endpoints: [NativeEndpoint] = [
            realtime,
            NativeEndpoint(
                name: .management,
                protocolName: NativeEndpoint.managementProtocol,
                url: base.http + selection.layout.managementPath
            ),
        ]
        if distribution == .effectsDeveloper {
            endpoints.append(NativeEndpoint(
                name: .effects, protocolName: NativeEndpoint.effectsProtocol,
                url: base.websocket + "/client/v1/effects"
            ))
            endpoints.append(NativeEndpoint(
                name: .artifacts, protocolName: NativeEndpoint.artifactsProtocol,
                url: base.http + "/client/v1/artifacts"
            ))
            endpoints.append(NativeEndpoint(
                name: .downloads, protocolName: NativeEndpoint.downloadsProtocol,
                url: base.http + "/client/v1/downloads"
            ))
        }
        return try NativeEndpointDirectory.freeze(endpoints)
    }

    /// Normalizes what a person is likely to type into one exact origin plus
    /// optional path prefix, in both the HTTP and the WebSocket scheme.
    ///
    /// A bare authority is read as plain HTTP rather than guessed at: the
    /// caller is shown the exact URLs this produced before anything connects.
    static func canonicalBase(_ value: String) throws -> (http: String, websocket: String) {
        let trimmed = value.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty, trimmed.utf8.count <= maximumBaseBytes else {
            throw NativeDeploymentError.base("enter the server's base URL")
        }
        var text = trimmed
        if !text.contains("://") { text = "http://" + text }
        while text.hasSuffix("/") { text.removeLast() }
        guard let components = URLComponents(string: text) else {
            throw NativeDeploymentError.base("the base URL is not a valid URL")
        }
        let scheme = (components.scheme ?? "").lowercased()
        let secure: Bool
        switch scheme {
        case "http", "ws": secure = false
        case "https", "wss": secure = true
        default:
            throw NativeDeploymentError.base(
                "the base URL scheme must be http, https, ws, or wss"
            )
        }
        guard let host = components.host, !host.isEmpty else {
            throw NativeDeploymentError.base("the base URL needs a host")
        }
        guard components.user == nil, components.password == nil else {
            throw NativeDeploymentError.base(
                "the base URL must not carry credentials; use the bearer token field"
            )
        }
        guard components.query == nil, components.fragment == nil else {
            throw NativeDeploymentError.base("the base URL must not carry a query or fragment")
        }
        let prefix = components.percentEncodedPath
        for segment in prefix.split(separator: "/", omittingEmptySubsequences: false) {
            guard segment != ".", segment != ".." else {
                throw NativeDeploymentError.base("the base URL must not contain a relative path segment")
            }
        }
        var authority = host
        if host.contains(":"), !host.hasPrefix("[") { authority = "[\(host)]" }
        if let port = components.port { authority += ":\(port)" }
        return (
            http: "\(secure ? "https" : "http")://\(authority)\(prefix)",
            websocket: "\(secure ? "wss" : "ws")://\(authority)\(prefix)"
        )
    }
}

/// Remembers the last deployment the operator applied, so a client that is
/// pointed at a remote host stays pointed at it across launches.
enum NativeDeploymentStore {
    private static let baseKey = "ai.openrealtime.developer.deployment.base"
    private static let layoutKey = "ai.openrealtime.developer.deployment.layout"
    private static let transportKey = "ai.openrealtime.developer.deployment.transport"
    private static let webRTCKey = "ai.openrealtime.developer.deployment.webrtc"

    static func load() -> NativeDeploymentSelection? {
        let defaults = UserDefaults.standard
        guard let base = defaults.string(forKey: baseKey), !base.isEmpty,
              let raw = defaults.string(forKey: layoutKey),
              let layout = NativeDeploymentLayout(rawValue: raw) else { return nil }
        var selection = NativeDeploymentSelection(base: base, layout: layout)
        if let raw = defaults.string(forKey: transportKey),
           let transport = NativeTransportKind(rawValue: raw) {
            selection.transport = transport
        }
        if let webRTC = defaults.string(forKey: webRTCKey), !webRTC.isEmpty {
            selection.webRTCBase = webRTC
        }
        return selection
    }

    static func save(_ selection: NativeDeploymentSelection) {
        let defaults = UserDefaults.standard
        defaults.set(selection.base, forKey: baseKey)
        defaults.set(selection.layout.rawValue, forKey: layoutKey)
        defaults.set(selection.transport.rawValue, forKey: transportKey)
        defaults.set(selection.webRTCBase, forKey: webRTCKey)
    }

    static func clear() {
        let defaults = UserDefaults.standard
        defaults.removeObject(forKey: baseKey)
        defaults.removeObject(forKey: layoutKey)
        defaults.removeObject(forKey: transportKey)
        defaults.removeObject(forKey: webRTCKey)
    }
}
