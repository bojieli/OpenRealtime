import Foundation
import CoreGraphics
import OpenRealtimeClientCore

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

struct ConfirmationRequest: Identifiable {
    let id: String
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
    compactJSONString(ProtocolEventPresentation.redacted(event))
}

func redactedProtocolValue(_ value: Any) -> Any {
    ProtocolEventPresentation.redactedValue(value)
}
