import Foundation
import AppKit
import ApplicationServices

@MainActor
final class DesktopComputerController {
    private(set) var display: DisplayTarget
    private(set) var coordinateSize: CGSize

    init(display: DisplayTarget, maxDimension: Int) {
        self.display = display
        self.coordinateSize = display.videoSize(maxDimension: maxDimension)
    }

    static var permission: String { AXIsProcessTrusted() ? "granted" : "not granted" }

    static func requestPermission() -> Bool {
        let options = [kAXTrustedCheckOptionPrompt.takeUnretainedValue() as String: true] as CFDictionary
        return AXIsProcessTrustedWithOptions(options)
    }

    func perform(name: String, arguments: [String: Any]) async throws -> String {
        if name != "computer.wait" {
            guard arguments["source"] as? String == "screen" else {
                throw DesktopError("desktop mode only owns the declared source \"screen\"")
            }
        }
        if name != "computer.wait", name != "computer.screenshot", !Self.requestPermission() {
            throw DesktopError("Accessibility permission is required for desktop computer use")
        }

        switch name {
        case "computer.click":
            let point = try point(arguments, x: "x", y: "y")
            let button = mouseButton(arguments["button"] as? String)
            try click(point, button: button, count: 1)
            return "clicked selected display"
        case "computer.double_click":
            try click(try point(arguments, x: "x", y: "y"), button: .left, count: 2)
            return "double-clicked selected display"
        case "computer.move":
            try postMouse(.mouseMoved, at: try point(arguments, x: "x", y: "y"), button: .left)
            return "moved pointer"
        case "computer.drag":
            let start = try point(arguments, x: "from_x", y: "from_y")
            let end = try point(arguments, x: "to_x", y: "to_y")
            try postMouse(.leftMouseDown, at: start, button: .left)
            for step in 1...8 {
                let amount = CGFloat(step) / 8
                let intermediate = CGPoint(
                    x: start.x + (end.x - start.x) * amount,
                    y: start.y + (end.y - start.y) * amount
                )
                try postMouse(.leftMouseDragged, at: intermediate, button: .left)
            }
            try postMouse(.leftMouseUp, at: end, button: .left)
            return "dragged on selected display"
        case "computer.type":
            guard let text = arguments["text"] as? String, !text.isEmpty else {
                throw DesktopError("typing requires non-empty text")
            }
            try type(text)
            return "typed text"
        case "computer.key":
            guard let keys = arguments["keys"] as? [String], !keys.isEmpty else {
                throw DesktopError("a key action requires at least one key")
            }
            try key(keys)
            return "pressed \(keys.joined(separator: "+"))"
        case "computer.scroll":
            let location = try point(arguments, x: "x", y: "y")
            let dx = (arguments["delta_x"] as? Int) ?? 0
            let dy = (arguments["delta_y"] as? Int) ?? 0
            guard let event = CGEvent(
                scrollWheelEvent2Source: nil, units: .pixel, wheelCount: 2,
                wheel1: Int32(-dy), wheel2: Int32(-dx), wheel3: 0
            ) else { throw DesktopError("macOS could not create a scroll event") }
            event.location = location
            event.post(tap: .cghidEventTap)
            return "scrolled selected display"
        case "computer.screenshot":
            return "requested a fresh selected-display frame"
        case "computer.wait":
            let requested = max(0, min(10_000, (arguments["duration_ms"] as? Int) ?? 0))
            try await Task.sleep(nanoseconds: UInt64(requested) * 1_000_000)
            return "waited \(requested) ms"
        case "computer.click_element":
            throw DesktopError("set-of-mark element grounding is available only in Browser mode")
        default:
            throw DesktopError("unknown computer-use action \(name)")
        }
    }

    private func point(_ arguments: [String: Any], x xName: String, y yName: String) throws -> CGPoint {
        guard let x = arguments[xName] as? Int, let y = arguments[yName] as? Int else {
            throw DesktopError("the action is missing integer coordinates")
        }
        let width = Int(coordinateSize.width)
        let height = Int(coordinateSize.height)
        guard x >= 0, y >= 0, x < width, y < height else {
            throw DesktopError("(\(x), \(y)) is outside the declared \(width)×\(height) selected display")
        }
        return CGPoint(
            x: display.bounds.minX + (CGFloat(x) + 0.5) * display.bounds.width / CGFloat(width),
            y: display.bounds.minY + (CGFloat(y) + 0.5) * display.bounds.height / CGFloat(height)
        )
    }

    private func mouseButton(_ name: String?) -> CGMouseButton {
        switch name?.lowercased() {
        case "right": return .right
        case "middle": return .center
        default: return .left
        }
    }

    private func click(_ point: CGPoint, button: CGMouseButton, count: Int) throws {
        let down: CGEventType = button == .right ? .rightMouseDown : button == .center ? .otherMouseDown : .leftMouseDown
        let up: CGEventType = button == .right ? .rightMouseUp : button == .center ? .otherMouseUp : .leftMouseUp
        for index in 1...count {
            guard let press = CGEvent(mouseEventSource: nil, mouseType: down, mouseCursorPosition: point,
                                      mouseButton: button),
                  let release = CGEvent(mouseEventSource: nil, mouseType: up, mouseCursorPosition: point,
                                        mouseButton: button) else {
                throw DesktopError("macOS could not create a mouse event")
            }
            press.setIntegerValueField(.mouseEventClickState, value: Int64(index))
            release.setIntegerValueField(.mouseEventClickState, value: Int64(index))
            press.post(tap: .cghidEventTap)
            release.post(tap: .cghidEventTap)
        }
    }

    private func postMouse(_ type: CGEventType, at point: CGPoint, button: CGMouseButton) throws {
        guard let event = CGEvent(mouseEventSource: nil, mouseType: type,
                                  mouseCursorPosition: point, mouseButton: button) else {
            throw DesktopError("macOS could not create a pointer event")
        }
        event.post(tap: .cghidEventTap)
    }

    private func type(_ text: String) throws {
        let utf16 = Array(text.utf16)
        for start in stride(from: 0, to: utf16.count, by: 20) {
            let chunk = Array(utf16[start..<min(start + 20, utf16.count)])
            guard let down = CGEvent(keyboardEventSource: nil, virtualKey: 0, keyDown: true),
                  let up = CGEvent(keyboardEventSource: nil, virtualKey: 0, keyDown: false) else {
                throw DesktopError("macOS could not create a keyboard event")
            }
            down.keyboardSetUnicodeString(stringLength: chunk.count, unicodeString: chunk)
            up.keyboardSetUnicodeString(stringLength: chunk.count, unicodeString: chunk)
            down.post(tap: .cghidEventTap)
            up.post(tap: .cghidEventTap)
        }
    }

    private func key(_ keys: [String]) throws {
        var flags: CGEventFlags = []
        var primary: String?
        for raw in keys {
            switch raw.lowercased() {
            case "cmd", "command", "meta": flags.insert(.maskCommand)
            case "ctrl", "control": flags.insert(.maskControl)
            case "alt", "option": flags.insert(.maskAlternate)
            case "shift": flags.insert(.maskShift)
            default: primary = raw.lowercased()
            }
        }
        guard let primary, let code = keyCode(primary) else {
            throw DesktopError("unsupported key combination \(keys.joined(separator: "+"))")
        }
        guard let down = CGEvent(keyboardEventSource: nil, virtualKey: code, keyDown: true),
              let up = CGEvent(keyboardEventSource: nil, virtualKey: code, keyDown: false) else {
            throw DesktopError("macOS could not create a key event")
        }
        down.flags = flags
        up.flags = flags
        down.post(tap: .cghidEventTap)
        up.post(tap: .cghidEventTap)
    }

    private func keyCode(_ name: String) -> CGKeyCode? {
        let named: [String: CGKeyCode] = [
            "return": 36, "enter": 36, "tab": 48, "space": 49, "delete": 51,
            "backspace": 51, "escape": 53, "esc": 53, "left": 123, "right": 124,
            "down": 125, "up": 126, "home": 115, "end": 119, "pageup": 116,
            "pagedown": 121, "f1": 122, "f2": 120, "f3": 99, "f4": 118,
            "f5": 96, "f6": 97, "f7": 98, "f8": 100, "f9": 101, "f10": 109,
            "f11": 103, "f12": 111,
        ]
        if let code = named[name] { return code }
        let letters = "abcdefghijklmnopqrstuvwxyz"
        let codes: [CGKeyCode] = [0,11,8,2,14,3,5,4,34,38,40,37,46,45,31,35,12,15,1,17,32,9,13,7,16,6]
        if name.count == 1, let character = name.first, let index = letters.firstIndex(of: character) {
            return codes[letters.distance(from: letters.startIndex, to: index)]
        }
        let digits: [String: CGKeyCode] = ["0":29,"1":18,"2":19,"3":20,"4":21,"5":23,"6":22,"7":26,"8":28,"9":25]
        return digits[name]
    }
}

@MainActor
func activeDisplays() -> [DisplayTarget] {
    var count: UInt32 = 0
    guard CGGetActiveDisplayList(0, nil, &count) == .success else { return [] }
    var ids = Array(repeating: CGDirectDisplayID(), count: Int(count))
    guard CGGetActiveDisplayList(count, &ids, &count) == .success else { return [] }
    return ids.prefix(Int(count)).enumerated().map { index, id in
        let screen = NSScreen.screens.first { screen in
            (screen.deviceDescription[NSDeviceDescriptionKey("NSScreenNumber")] as? NSNumber)?.uint32Value == id
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

private struct DesktopError: LocalizedError {
    let message: String
    init(_ message: String) { self.message = message }
    var errorDescription: String? { message }
}
