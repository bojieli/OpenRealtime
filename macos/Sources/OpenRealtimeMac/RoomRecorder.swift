import AppKit
import ScreenCaptureKit

/// Records this application's room window, agent audio and the enabled microphone.
/// ScreenCaptureKit owns encoding and file finalization; capture never enters the server.
@available(macOS 15.0, *)
@MainActor
final class RoomRecorder: NSObject, SCRecordingOutputDelegate, SCStreamDelegate {
    var onStatus: ((Bool, String?) -> Void)?
    private var stream: SCStream?
    private var output: SCRecordingOutput?
    private var configuration: SCStreamConfiguration?
    private var microphoneEnabled = true
    private var cancelled = false

    func start(url: URL) async throws {
        let content = try await SCShareableContent.excludingDesktopWindows(false, onScreenWindowsOnly: true)
        guard !cancelled else { throw NativeProviderError("Recording was cancelled.") }
        let windowID = NSApp.keyWindow?.windowNumber
        guard let window = content.windows.first(where: {
            $0.owningApplication?.processID == ProcessInfo.processInfo.processIdentifier && Int($0.windowID) == windowID
        }) ?? content.windows.first(where: {
            $0.owningApplication?.processID == ProcessInfo.processInfo.processIdentifier && $0.frame.width > 400
        }) else { throw NativeProviderError("The room window could not be found for recording.") }
        let filter = SCContentFilter(desktopIndependentWindow: window)
        let settings = SCStreamConfiguration()
        settings.width = max(2, Int(window.frame.width) / 2 * 2)
        settings.height = max(2, Int(window.frame.height) / 2 * 2)
        settings.minimumFrameInterval = CMTime(value: 1, timescale: 30)
        settings.capturesAudio = true
        settings.excludesCurrentProcessAudio = false
        settings.captureMicrophone = microphoneEnabled
        settings.showsCursor = true
        configuration = settings
        let recording = SCRecordingOutputConfiguration()
        recording.outputURL = url
        recording.outputFileType = .mp4
        recording.videoCodecType = .h264
        let output = SCRecordingOutput(configuration: recording, delegate: self)
        let stream = SCStream(filter: filter, configuration: settings, delegate: self)
        try stream.addRecordingOutput(output)
        self.output = output
        self.stream = stream
        do { try await stream.startCapture() }
        catch { self.stream = nil; self.output = nil; throw error }
    }

    func setMicrophoneEnabled(_ enabled: Bool) {
        guard microphoneEnabled != enabled else { return }
        microphoneEnabled = enabled
        guard let configuration, let stream else { return }
        configuration.captureMicrophone = enabled
        Task {
            do { try await stream.updateConfiguration(configuration) }
            catch { onStatus?(true, "Could not update the recording microphone: \(error.localizedDescription)") }
        }
    }

    func stop() async {
        cancelled = true
        guard let stream else { onStatus?(false, nil); return }
        self.stream = nil
        do { try await stream.stopCapture() }
        catch { onStatus?(false, error.localizedDescription) }
    }

    nonisolated func recordingOutputDidStartRecording(_ recordingOutput: SCRecordingOutput) {
        Task { @MainActor in self.onStatus?(true, nil) }
    }
    nonisolated func recordingOutputDidFinishRecording(_ recordingOutput: SCRecordingOutput) {
        Task { @MainActor in self.output = nil; self.onStatus?(false, nil) }
    }
    nonisolated func recordingOutput(_ recordingOutput: SCRecordingOutput, didFailWithError error: Error) {
        Task { @MainActor in await self.stop(); self.output = nil; self.onStatus?(false, error.localizedDescription) }
    }
    nonisolated func stream(_ stream: SCStream, didStopWithError error: Error) {
        Task { @MainActor in self.stream = nil; self.output = nil; self.onStatus?(false, error.localizedDescription) }
    }
}
