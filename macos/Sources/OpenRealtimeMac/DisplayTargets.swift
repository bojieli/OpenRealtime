import AppKit
import CoreGraphics

@MainActor
func activeDisplays() -> [DisplayTarget] {
    var count: UInt32 = 0
    guard CGGetActiveDisplayList(0, nil, &count) == .success else { return [] }
    var ids = Array(repeating: CGDirectDisplayID(), count: Int(count))
    guard CGGetActiveDisplayList(count, &ids, &count) == .success else { return [] }
    return ids.prefix(Int(count)).enumerated().map { index, id in
        let screen = NSScreen.screens.first { screen in
            (screen.deviceDescription[NSDeviceDescriptionKey("NSScreenNumber")] as? NSNumber)?
                .uint32Value == id
        }
        return DisplayTarget(
            id: id,
            name: screen?.localizedName ?? "Display \(index + 1)",
            bounds: CGDisplayBounds(id),
            pixelsWide: Int(CGDisplayPixelsWide(id)),
            pixelsHigh: Int(CGDisplayPixelsHigh(id))
        )
    }
}
