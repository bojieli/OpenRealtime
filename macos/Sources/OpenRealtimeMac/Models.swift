import Foundation
import CoreGraphics

enum ConnectionState: String {
    case disconnected
    case connecting
    case connected
    case failed
}

enum ComputerMode: String, CaseIterable, Identifiable {
    case browser = "Browser · set of mark"
    case desktop = "Desktop · selected display"

    var id: String { rawValue }
    var source: String { self == .browser ? "browser" : "screen" }
    var target: String { self == .browser ? "browser-use-cdp" : "selected-display" }
}

struct DisplayTarget: Identifiable, Hashable {
    let id: CGDirectDisplayID
    let name: String
    let bounds: CGRect
    let pixelsWide: Int
    let pixelsHigh: Int

    var label: String { "\(name) · \(pixelsWide)×\(pixelsHigh)" }

    func videoSize(maxDimension: Int) -> CGSize {
        let longEdge = max(pixelsWide, pixelsHigh)
        guard longEdge > maxDimension, maxDimension > 0 else {
            return CGSize(width: CGFloat(pixelsWide), height: CGFloat(pixelsHigh))
        }
        let scale = Double(maxDimension) / Double(longEdge)
        return CGSize(
            width: CGFloat(max(1, Int((Double(pixelsWide) * scale).rounded()))),
            height: CGFloat(max(1, Int((Double(pixelsHigh) * scale).rounded())))
        )
    }
}

struct VideoLimits: Equatable, Sendable {
    var format = "jpeg"
    var fpsCap = 3
    var maxDimension = 1280
    var maxFrameBytes = 4 << 20

    init(dictionary: [String: Any]? = nil) {
        guard let dictionary else { return }
        format = (dictionary["format"] as? String) ?? format
        fpsCap = (dictionary["fps_cap"] as? Int) ?? fpsCap
        maxDimension = (dictionary["max_dimension"] as? Int) ?? maxDimension
        maxFrameBytes = (dictionary["max_frame_bytes"] as? Int) ?? maxFrameBytes
    }
}

struct ChannelRecord: Identifiable {
    let id = UUID()
    let timestamp = Date()
    let channel: String
    let title: String
    let body: String
    let failed: Bool
}

struct ProtocolRecord: Identifiable {
    let id = UUID()
    let timestamp = Date()
    let direction: String
    let type: String
    let payload: String
}

struct DebugRecord: Identifiable {
    let id = UUID()
    let timestampMS: Int64
    let category: String
    let name: String
    let phase: String
    let durationMS: Double?
    let correlationID: String
    let detail: String

    var date: Date { Date(timeIntervalSince1970: Double(timestampMS) / 1000) }
}

struct ArtifactRecord: Identifiable, Sendable {
    let id: String
    let title: String
    let html: String
    let version: Int
}

struct DownloadRecord: Identifiable, Sendable {
    let id: String
    let filename: String
    let mediaType: String
    let bytes: Int
    let version: Int
    let url: URL
}

struct ToolExecutionResult: Sendable {
    let output: String
    var artifact: ArtifactRecord?
    var download: DownloadRecord?
}

struct ConfirmationRequest: Identifiable {
    let id = UUID()
    let name: String
    let consequence: String
    let arguments: String
}

struct BrowserFrame: Sendable {
    let data: Data
    let width: Int
    let height: Int
    let url: String
    let title: String
    let elementCount: Int
}

func compactJSONString(_ value: Any) -> String {
    guard JSONSerialization.isValidJSONObject(value),
          let data = try? JSONSerialization.data(withJSONObject: value, options: [.sortedKeys]),
          let string = String(data: data, encoding: .utf8) else {
        return String(describing: value)
    }
    return string
}

func prettyJSONString(_ value: Any) -> String {
    guard JSONSerialization.isValidJSONObject(value),
          let data = try? JSONSerialization.data(withJSONObject: value, options: [.prettyPrinted, .sortedKeys]),
          let string = String(data: data, encoding: .utf8) else {
        return String(describing: value)
    }
    return string
}

func safeProtocolPayload(_ event: [String: Any]) -> String {
    var copy = event
    for key in ["audio", "delta", "frame"] {
        if let value = copy[key] as? String, value.count > 96 {
            copy[key] = "‹\(value.count) base64 characters›"
        }
    }
    return compactJSONString(copy)
}
