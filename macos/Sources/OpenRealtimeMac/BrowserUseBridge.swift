import Foundation
import AppKit

actor BrowserUseProcess {
    private let cdpURL: String
    private var process: Process?
    private var input: FileHandle?
    private var output: FileHandle?
    private var readBuffer = Data()
    private var sequence = 0

    init(cdpURL: String) { self.cdpURL = cdpURL }

    func start() throws {
        if process?.isRunning == true { return }
        let project = try Self.bridgeProject()
        let task = Process()
        let stdinPipe = Pipe()
        let stdoutPipe = Pipe()
        task.executableURL = try Self.uvExecutable()
        task.arguments = [
            "run", "--offline", "--frozen", "--project", project.path, "python", "bridge.py",
            "--cdp-url", cdpURL,
        ]
        task.currentDirectoryURL = project
        let support = try FileManager.default.url(
            for: .applicationSupportDirectory, in: .userDomainMask,
            appropriateFor: nil, create: true
        ).appendingPathComponent("OpenRealtime/BrowserUse-0.12.6", isDirectory: true)
        try FileManager.default.createDirectory(at: support.deletingLastPathComponent(),
                                                withIntermediateDirectories: true)
        var environment = ProcessInfo.processInfo.environment
        environment["UV_PROJECT_ENVIRONMENT"] = support.path
        task.environment = environment
        task.standardInput = stdinPipe
        task.standardOutput = stdoutPipe
        task.standardError = FileHandle.standardError
        try task.run()
        process = task
        input = stdinPipe.fileHandleForWriting
        output = stdoutPipe.fileHandleForReading
        let ready: [String: Any]
        do {
            ready = try readObject()
        } catch {
            stop()
            throw BridgeError("browser-use is not prepared; run macos/prepare-browser-use.sh before Browser mode")
        }
        guard ready["status"] as? String == "ready" else {
            stop()
            throw BridgeError("browser-use did not complete its startup handshake")
        }
    }

    func command(_ command: [String: Any]) throws -> [String: Any] {
        guard process?.isRunning == true, let input else {
            throw BridgeError("the browser-use bridge is not running")
        }
        sequence += 1
        var request = command
        request["id"] = sequence
        guard JSONSerialization.isValidJSONObject(request) else {
            throw BridgeError("an invalid browser bridge request was created")
        }
        var data = try JSONSerialization.data(withJSONObject: request)
        data.append(0x0A)
        try input.write(contentsOf: data)
        let response = try readObject()
        guard response["id"] as? Int == sequence else {
            throw BridgeError("browser-use returned an out-of-order response")
        }
        if response["ok"] as? Bool != true {
            throw BridgeError((response["error"] as? String) ?? "browser-use rejected the command")
        }
        return response
    }

    func stop() {
        try? input?.close()
        input = nil
        try? output?.close()
        output = nil
        if process?.isRunning == true { process?.terminate() }
        process = nil
        readBuffer.removeAll(keepingCapacity: false)
    }

    private func readObject() throws -> [String: Any] {
        guard let output else { throw BridgeError("browser-use output is closed") }
        while true {
            if let newline = readBuffer.firstIndex(of: 0x0A) {
                let line = readBuffer.prefix(upTo: newline)
                readBuffer.removeSubrange(...newline)
                guard let object = try JSONSerialization.jsonObject(with: line) as? [String: Any] else {
                    throw BridgeError("browser-use returned invalid JSON")
                }
                return object
            }
            guard let chunk = try output.read(upToCount: 64 << 10), !chunk.isEmpty else {
                throw BridgeError("browser-use closed its response stream")
            }
            readBuffer.append(chunk)
        }
    }

    private static func bridgeProject() throws -> URL {
        let bundled = Bundle.main.resourceURL?.appendingPathComponent("BrowserUseBridge", isDirectory: true)
        if let bundled, FileManager.default.fileExists(atPath: bundled.appendingPathComponent("bridge.py").path) {
            return bundled
        }
        let source = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .appendingPathComponent("BrowserUseBridge", isDirectory: true)
        guard FileManager.default.fileExists(atPath: source.appendingPathComponent("bridge.py").path) else {
            throw BridgeError("BrowserUseBridge/bridge.py was not found in the app or source checkout")
        }
        return source
    }

    private static func uvExecutable() throws -> URL {
        let environment = ProcessInfo.processInfo.environment
        let home = FileManager.default.homeDirectoryForCurrentUser
        let candidates = [
            environment["OPENREALTIME_UV_BIN"],
            "/opt/homebrew/bin/uv", "/usr/local/bin/uv", "/usr/bin/uv",
            home.appendingPathComponent(".local/bin/uv").path,
            home.appendingPathComponent(".cargo/bin/uv").path,
        ].compactMap { $0 }
        for path in candidates where FileManager.default.isExecutableFile(atPath: path) {
            return URL(fileURLWithPath: path)
        }
        throw BridgeError("uv was not found; install it or set OPENREALTIME_UV_BIN")
    }
}

@MainActor
final class BrowserUseController {
    var onSource: ((String, String, Int, Int) -> Void)?
    var onFrame: ((BrowserFrame) -> Void)?
    var onCaption: ((String) -> Void)?
    var onFailure: ((String) -> Void)?

    private var bridge: BrowserUseProcess?
    private var captureTask: Task<Void, Never>?
    private var limits = VideoLimits()
    private(set) var active = false
    private(set) var width = 1280
    private(set) var height = 720
    private var browserWidth = 1280
    private var browserHeight = 720
    private var declaredSize: CGSize?

    func start(cdpURL: String, limits: VideoLimits) async throws {
        await stop()
        self.limits = limits
        let bridge = BrowserUseProcess(cdpURL: cdpURL)
        try await bridge.start()
        self.bridge = bridge
        active = true
        try await captureOnce()
        let delay = UInt64(1_000_000_000 / max(1, limits.fpsCap))
        captureTask = Task { [weak self] in
            while let self, self.active, !Task.isCancelled {
                try? await Task.sleep(nanoseconds: delay)
                guard !Task.isCancelled else { return }
                do { try await self.captureOnce() }
                catch { self.onFailure?(error.localizedDescription) }
            }
        }
    }

    // Only a source that was declared active can be closed. The sibling
    // camera and screen paths already return early when nothing is running;
    // announcing an unconditional close here manufactured a source-lifecycle
    // event for a source that never existed.
    func stop() async {
        captureTask?.cancel()
        captureTask = nil
        active = false
        if let bridge { await bridge.stop() }
        bridge = nil
        let declared = declaredSize != nil
        declaredSize = nil
        if declared { onSource?("browser", "closed", 0, 0) }
    }

    func perform(name: String, arguments: [String: Any]) async throws -> String {
        guard active, let bridge else { throw BridgeError("the browser source is not active") }
        if name != "computer.wait" {
            guard arguments["source"] as? String == "browser" else {
                throw BridgeError("browser mode only owns the declared source \"browser\"")
            }
        }
        try validateCoordinates(name: name, arguments: arguments)
        var request = arguments
        request = scaledForBrowser(request)
        switch name {
        case "computer.click": request["command"] = "click"
        case "computer.click_element": request["command"] = "click_element"
        case "computer.double_click": request["command"] = "double_click"
        case "computer.move": request["command"] = "move"
        case "computer.drag": request["command"] = "drag"
        case "computer.type": request["command"] = "type"
        case "computer.key": request["command"] = "key"
        case "computer.scroll": request["command"] = "scroll"
        case "computer.screenshot":
            try await captureOnce()
            return "captured a fresh marked browser frame"
        case "computer.wait": request["command"] = "wait"
        default: throw BridgeError("unknown computer-use action \(name)")
        }
        _ = try await bridge.command(request)
        if name != "computer.wait" { try await captureOnce() }
        return [
            "computer.click": "clicked", "computer.click_element": "clicked marked element",
            "computer.double_click": "double-clicked", "computer.move": "moved",
            "computer.drag": "dragged", "computer.type": "typed", "computer.key": "pressed",
            "computer.scroll": "scrolled", "computer.wait": "waited",
        ][name] ?? "done"
    }

    private func captureOnce() async throws {
        guard active, let bridge else { return }
        let response = try await bridge.command(["command": "capture"])
        guard let encoded = response["frame"] as? String,
              let png = Data(base64Encoded: encoded),
              let jpeg = Self.makeJPEG(png, limits: limits) else {
            throw BridgeError("browser-use returned an image that could not be encoded as negotiated JPEG")
        }
        width = jpeg.width
        height = jpeg.height
        browserWidth = max(1, (response["width"] as? Int) ?? width)
        browserHeight = max(1, (response["height"] as? Int) ?? height)
        let geometry = CGSize(width: CGFloat(width), height: CGFloat(height))
        if declaredSize != geometry {
            declaredSize = geometry
            onSource?("browser", "active", width, height)
        }
        let elements = (response["elements"] as? [[String: Any]])?.count ?? 0
        let url = (response["url"] as? String) ?? ""
        let title = (response["title"] as? String) ?? ""
        onCaption?("\(width)×\(height) · \(elements) marks · \(title.isEmpty ? url : title)")
        onFrame?(BrowserFrame(data: jpeg.data, width: width, height: height,
                              url: url, title: title, elementCount: elements))
    }

    private func validateCoordinates(name: String, arguments: [String: Any]) throws {
        func inside(_ x: Int, _ y: Int) -> Bool { x >= 0 && y >= 0 && x < width && y < height }
        switch name {
        case "computer.click", "computer.double_click", "computer.move", "computer.scroll":
            guard let x = arguments["x"] as? Int, let y = arguments["y"] as? Int, inside(x, y) else {
                throw BridgeError("the browser action is outside its \(width)×\(height) marked frame")
            }
        case "computer.drag":
            guard let x1 = arguments["from_x"] as? Int, let y1 = arguments["from_y"] as? Int,
                  let x2 = arguments["to_x"] as? Int, let y2 = arguments["to_y"] as? Int,
                  inside(x1, y1), inside(x2, y2) else {
                throw BridgeError("the browser drag is outside its \(width)×\(height) marked frame")
            }
        default: break
        }
    }

    private func scaledForBrowser(_ arguments: [String: Any]) -> [String: Any] {
        var scaled = arguments
        func horizontal(_ name: String) {
            guard let value = arguments[name] as? Int else { return }
            scaled[name] = min(browserWidth - 1, max(0,
                Int((Double(value) * Double(browserWidth) / Double(max(1, width))).rounded())))
        }
        func vertical(_ name: String) {
            guard let value = arguments[name] as? Int else { return }
            scaled[name] = min(browserHeight - 1, max(0,
                Int((Double(value) * Double(browserHeight) / Double(max(1, height))).rounded())))
        }
        for name in ["x", "from_x", "to_x"] { horizontal(name) }
        for name in ["y", "from_y", "to_y"] { vertical(name) }
        if let deltaX = arguments["delta_x"] as? Int {
            scaled["delta_x"] = Int((Double(deltaX) * Double(browserWidth) / Double(max(1, width))).rounded())
        }
        if let deltaY = arguments["delta_y"] as? Int {
            scaled["delta_y"] = Int((Double(deltaY) * Double(browserHeight) / Double(max(1, height))).rounded())
        }
        return scaled
    }

    private static func makeJPEG(_ data: Data, limits: VideoLimits) -> (data: Data, width: Int, height: Int)? {
        guard let image = NSImage(data: data),
              let source = image.representations.first else { return nil }
        let originalWidth = max(1, source.pixelsWide)
        let originalHeight = max(1, source.pixelsHigh)
        let longEdge = max(originalWidth, originalHeight)
        let scale = longEdge > limits.maxDimension
            ? Double(limits.maxDimension) / Double(longEdge)
            : 1
        let width = max(1, Int((Double(originalWidth) * scale).rounded()))
        let height = max(1, Int((Double(originalHeight) * scale).rounded()))
        guard let bitmap = NSBitmapImageRep(
            bitmapDataPlanes: nil, pixelsWide: width, pixelsHigh: height,
            bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: false, isPlanar: false,
            colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0
        ), let graphics = NSGraphicsContext(bitmapImageRep: bitmap) else { return nil }
        NSGraphicsContext.saveGraphicsState()
        NSGraphicsContext.current = graphics
        image.draw(in: NSRect(x: 0, y: 0, width: width, height: height),
                   from: .zero, operation: .copy, fraction: 1)
        NSGraphicsContext.restoreGraphicsState()
        var quality = 0.84
        while quality >= 0.24 {
            if let encoded = bitmap.representation(using: .jpeg, properties: [.compressionFactor: quality]),
               encoded.count <= limits.maxFrameBytes {
                return (encoded, width, height)
            }
            quality -= 0.12
        }
        return nil
    }
}

private struct BridgeError: LocalizedError {
    let message: String
    init(_ message: String) { self.message = message }
    var errorDescription: String? { message }
}
