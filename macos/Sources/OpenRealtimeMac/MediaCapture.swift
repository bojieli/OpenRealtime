import Foundation
import AVFoundation
import ScreenCaptureKit
import CoreImage
import CoreGraphics
import AppKit

final class MediaCaptureController: NSObject, SCStreamOutput, AVCaptureVideoDataOutputSampleBufferDelegate {
    var onSource: ((String, String, Int, Int) -> Void)?
    var onFrame: ((String, Data, Int, Int, Int64) -> Void)?
    var onFailure: ((String, String) -> Void)?

    private let screenQueue = DispatchQueue(label: "ai.openrealtime.capture.screen", qos: .userInitiated)
    private let cameraQueue = DispatchQueue(label: "ai.openrealtime.capture.camera", qos: .userInitiated)
    private let imageContext = CIContext(options: [.cacheIntermediates: false])

    private var screenStream: SCStream?
    private var cameraSession: AVCaptureSession?
    private var limits = VideoLimits()
    private var screenGeometry: CGSize?
    private var cameraGeometry: CGSize?
    private var lastScreenFrame = Date.distantPast
    private var lastCameraFrame = Date.distantPast

    static var screenPermission: String {
        CGPreflightScreenCaptureAccess() ? "granted" : "not granted"
    }

    static var cameraPermission: String {
        switch AVCaptureDevice.authorizationStatus(for: .video) {
        case .authorized: return "granted"
        case .denied, .restricted: return "denied"
        case .notDetermined: return "not requested"
        @unknown default: return "unknown"
        }
    }

    func startScreen(displayID: CGDirectDisplayID, limits: VideoLimits) async throws {
        if !CGPreflightScreenCaptureAccess() { _ = CGRequestScreenCaptureAccess() }
        guard CGPreflightScreenCaptureAccess() else {
            throw CaptureError("Screen Recording permission was not granted")
        }
        await stopScreen()
        self.limits = limits
        let content = try await SCShareableContent.excludingDesktopWindows(false, onScreenWindowsOnly: true)
        guard let display = content.displays.first(where: { $0.displayID == displayID }) else {
            throw CaptureError("the selected display is no longer available")
        }

        let longEdge = max(display.width, display.height)
        let scale = longEdge > limits.maxDimension
            ? Double(limits.maxDimension) / Double(longEdge)
            : 1
        let configuration = SCStreamConfiguration()
        configuration.width = max(1, Int(Double(display.width) * scale))
        configuration.height = max(1, Int(Double(display.height) * scale))
        configuration.pixelFormat = kCVPixelFormatType_32BGRA
        configuration.minimumFrameInterval = CMTime(value: 1, timescale: CMTimeScale(max(1, limits.fpsCap)))
        configuration.queueDepth = 3
        configuration.showsCursor = true
        let filter = SCContentFilter(display: display, excludingWindows: [])
        let stream = SCStream(filter: filter, configuration: configuration, delegate: nil)
        try stream.addStreamOutput(self, type: .screen, sampleHandlerQueue: screenQueue)
        screenGeometry = nil
        lastScreenFrame = .distantPast
        screenStream = stream
        try await stream.startCapture()
    }

    func stopScreen() async {
        guard let stream = screenStream else { return }
        screenStream = nil
        try? await stream.stopCapture()
        screenGeometry = nil
        emitSource("screen", state: "closed", width: 0, height: 0)
    }

    func startCamera(limits: VideoLimits) async throws {
        if AVCaptureDevice.authorizationStatus(for: .video) == .notDetermined {
            _ = await AVCaptureDevice.requestAccess(for: .video)
        }
        guard AVCaptureDevice.authorizationStatus(for: .video) == .authorized else {
            throw CaptureError("camera permission was not granted")
        }
        stopCamera()
        self.limits = limits
        guard let device = AVCaptureDevice.default(.builtInWideAngleCamera, for: .video, position: .unspecified) else {
            throw CaptureError("no physical camera is available")
        }
        let input = try AVCaptureDeviceInput(device: device)
        let output = AVCaptureVideoDataOutput()
        output.alwaysDiscardsLateVideoFrames = true
        output.videoSettings = [kCVPixelBufferPixelFormatTypeKey as String: kCVPixelFormatType_32BGRA]
        output.setSampleBufferDelegate(self, queue: cameraQueue)

        let capture = AVCaptureSession()
        capture.beginConfiguration()
        if capture.canSetSessionPreset(.hd1280x720) { capture.sessionPreset = .hd1280x720 }
        guard capture.canAddInput(input), capture.canAddOutput(output) else {
            capture.commitConfiguration()
            throw CaptureError("the camera could not create a capture stream")
        }
        capture.addInput(input)
        capture.addOutput(output)
        capture.commitConfiguration()
        cameraGeometry = nil
        lastCameraFrame = .distantPast
        cameraSession = capture
        cameraQueue.async { capture.startRunning() }
    }

    func stopCamera() {
        guard let capture = cameraSession else { return }
        cameraSession = nil
        cameraQueue.async { capture.stopRunning() }
        cameraGeometry = nil
        emitSource("camera", state: "closed", width: 0, height: 0)
    }

    func stopAll() async {
        await stopScreen()
        stopCamera()
    }

    func stream(_ stream: SCStream, didOutputSampleBuffer sampleBuffer: CMSampleBuffer,
                of outputType: SCStreamOutputType) {
        guard outputType == .screen, stream === screenStream else { return }
        process(sampleBuffer, source: "screen")
    }

    func captureOutput(_ output: AVCaptureOutput, didOutput sampleBuffer: CMSampleBuffer,
                       from connection: AVCaptureConnection) {
        guard cameraSession != nil else { return }
        process(sampleBuffer, source: "camera")
    }

    private func process(_ sampleBuffer: CMSampleBuffer, source: String) {
        let now = Date()
        let interval = 1.0 / Double(max(1, limits.fpsCap))
        if source == "screen" {
            guard now.timeIntervalSince(lastScreenFrame) >= interval else { return }
            lastScreenFrame = now
        } else {
            guard now.timeIntervalSince(lastCameraFrame) >= interval else { return }
            lastCameraFrame = now
        }
        guard let pixelBuffer = sampleBuffer.imageBuffer,
              let encoded = encode(pixelBuffer, limits: limits) else { return }
        let timestamp = Int64((Date().timeIntervalSince1970 * 1000).rounded())
        var announce = false
        let geometry = CGSize(width: CGFloat(encoded.width), height: CGFloat(encoded.height))
        if source == "screen", screenGeometry != geometry {
            screenGeometry = geometry
            announce = true
        } else if source == "camera", cameraGeometry != geometry {
            cameraGeometry = geometry
            announce = true
        }
        DispatchQueue.main.async { [weak self] in
            guard let self else { return }
            if announce { self.onSource?(source, "active", encoded.width, encoded.height) }
            self.onFrame?(source, encoded.data, encoded.width, encoded.height, timestamp)
        }
    }

    private func encode(_ pixelBuffer: CVPixelBuffer, limits: VideoLimits) -> (data: Data, width: Int, height: Int)? {
        var image = CIImage(cvPixelBuffer: pixelBuffer)
        let originalWidth = max(1, Int(image.extent.width.rounded()))
        let originalHeight = max(1, Int(image.extent.height.rounded()))
        let longEdge = max(originalWidth, originalHeight)
        var width = originalWidth
        var height = originalHeight
        if longEdge > limits.maxDimension, limits.maxDimension > 0 {
            let scale = Double(limits.maxDimension) / Double(longEdge)
            image = image.transformed(by: CGAffineTransform(scaleX: scale, y: scale))
            width = max(1, Int((Double(originalWidth) * scale).rounded()))
            height = max(1, Int((Double(originalHeight) * scale).rounded()))
        }
        guard let cgImage = imageContext.createCGImage(image, from: image.extent) else {
            emitFailure(source: "video", "a frame could not be rendered for JPEG encoding")
            return nil
        }
        let bitmap = NSBitmapImageRep(cgImage: cgImage)
        var quality = 0.82
        while quality >= 0.24 {
            if let data = bitmap.representation(
                using: .jpeg, properties: [.compressionFactor: quality]
            ), data.count <= limits.maxFrameBytes {
                return (data, width, height)
            }
            quality -= 0.12
        }
        emitFailure(source: "video", "a frame could not fit the negotiated \(limits.maxFrameBytes)-byte limit")
        return nil
    }

    private func emitSource(_ source: String, state: String, width: Int, height: Int) {
        DispatchQueue.main.async { [weak self] in self?.onSource?(source, state, width, height) }
    }

    private func emitFailure(source: String, _ message: String) {
        DispatchQueue.main.async { [weak self] in self?.onFailure?(source, message) }
    }
}

private struct CaptureError: LocalizedError {
    let message: String
    init(_ message: String) { self.message = message }
    var errorDescription: String? { message }
}
