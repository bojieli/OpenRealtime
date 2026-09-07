import Foundation
import AVFoundation

@MainActor
final class AudioIO {
    static let sessionRate: Double = 24_000

    var onFrame: ((Data) -> Void)?
    var onStatus: ((String) -> Void)?

    private let engine = AVAudioEngine()
    private let player = AVAudioPlayerNode()
    private var converter: AVAudioConverter?
    private var playbackFormat: AVAudioFormat?
    private var inputInstalled = false
    private(set) var microphoneActive = false
    private(set) var muted = false

    private var playbackItem: String?
    private var playbackSamples = 0
    private var playbackStartedAt: TimeInterval?
    private var playbackBuffers = 0
    private var playbackInputDone = false

    init() {
        engine.attach(player)
    }

    var permission: String {
        switch AVCaptureDevice.authorizationStatus(for: .audio) {
        case .authorized: return "granted"
        case .denied, .restricted: return "denied"
        case .notDetermined: return "not requested"
        @unknown default: return "unknown"
        }
    }

    func startMicrophone() async throws {
        if AVCaptureDevice.authorizationStatus(for: .audio) == .notDetermined {
            _ = await AVCaptureDevice.requestAccess(for: .audio)
        }
        guard AVCaptureDevice.authorizationStatus(for: .audio) == .authorized else {
            throw AudioError("microphone access was not granted")
        }
        guard !inputInstalled else {
            microphoneActive = true
            muted = false
            return
        }

        let input = engine.inputNode
        let hardwareFormat = input.outputFormat(forBus: 0)
        guard hardwareFormat.sampleRate > 0,
              let sessionFormat = AVAudioFormat(
                commonFormat: .pcmFormatInt16,
                sampleRate: Self.sessionRate,
                channels: 1,
                interleaved: false
              ),
              let converter = AVAudioConverter(from: hardwareFormat, to: sessionFormat) else {
            throw AudioError("the microphone format cannot be converted to PCM16 at 24 kHz")
        }
        self.converter = converter

        input.installTap(onBus: 0, bufferSize: 2048, format: hardwareFormat) { [weak self] buffer, _ in
            guard let self else { return }
            let ratio = Self.sessionRate / hardwareFormat.sampleRate
            let capacity = AVAudioFrameCount(ceil(Double(buffer.frameLength) * ratio) + 32)
            guard let output = AVAudioPCMBuffer(pcmFormat: sessionFormat, frameCapacity: capacity) else { return }
            var supplied = false
            var conversionError: NSError?
            let status = converter.convert(to: output, error: &conversionError) { _, outStatus in
                if supplied {
                    outStatus.pointee = .noDataNow
                    return nil
                }
                supplied = true
                outStatus.pointee = .haveData
                return buffer
            }
            guard status != .error, output.frameLength > 0, let channel = output.int16ChannelData?[0] else {
                return
            }
            var data = Data(bytes: channel, count: Int(output.frameLength) * MemoryLayout<Int16>.size)
            Task { @MainActor [weak self] in
                guard let self, self.microphoneActive else { return }
                if self.muted { data.resetBytes(in: data.startIndex..<data.endIndex) }
                self.onFrame?(data)
            }
        }
        inputInstalled = true
        do {
            try ensureEngineRunning()
        } catch {
            // A half-installed tap must not survive: the early return above
            // would otherwise report a live microphone on the next attempt
            // while the engine that feeds it never started.
            input.removeTap(onBus: 0)
            inputInstalled = false
            self.converter = nil
            throw error
        }
        microphoneActive = true
        muted = false
        onStatus?("microphone live · PCM16 24 kHz")
    }

    func setSpeakerMuted(_ muted: Bool) { player.volume = muted ? 0 : 1 }

    func toggleMute() {
        guard microphoneActive else { return }
        muted.toggle()
        onStatus?(muted ? "microphone muted · silence still streaming" : "microphone live · PCM16 24 kHz")
    }

    func stopMicrophone() {
        guard inputInstalled else { return }
        engine.inputNode.removeTap(onBus: 0)
        inputInstalled = false
        microphoneActive = false
        muted = false
        converter = nil
        onStatus?("microphone off")
    }

    func enqueue(itemID: String, base64: String) {
        guard let data = Data(base64Encoded: base64), !data.isEmpty else { return }
        let count = data.count / MemoryLayout<Int16>.size
        guard count > 0,
              let format = playbackPCMFormat(),
              let buffer = AVAudioPCMBuffer(pcmFormat: format, frameCapacity: AVAudioFrameCount(count)),
              let target = buffer.floatChannelData?[0] else { return }

        data.withUnsafeBytes { raw in
            let source = raw.bindMemory(to: Int16.self)
            for index in 0..<count { target[index] = Float(source[index]) / 32_768 }
        }
        buffer.frameLength = AVAudioFrameCount(count)
        if playbackItem != itemID {
            if playbackItem != nil {
                player.stop()
                player.reset()
            }
            playbackItem = itemID
            playbackSamples = 0
            playbackStartedAt = ProcessInfo.processInfo.systemUptime
            playbackBuffers = 0
            playbackInputDone = false
        }
        playbackSamples += count
        playbackBuffers += 1
        player.scheduleBuffer(buffer, completionCallbackType: .dataPlayedBack) { [weak self] _ in
            Task { @MainActor in self?.playedBuffer(itemID: itemID) }
        }
        do {
            try ensureEngineRunning()
            if !player.isPlaying { player.play() }
            onStatus?("playing PCM16 24 kHz")
        } catch {
            stopPlayback()
            onStatus?("audio output failed: \(error.localizedDescription)")
        }
    }

    func finish(itemID: String) {
        guard playbackItem == itemID else { return }
        playbackInputDone = true
        if playbackBuffers == 0 { completePlayback(itemID: itemID) }
        else { onStatus?("draining output") }
    }

    func interrupt() -> (itemID: String, playedMS: Int)? {
        guard let itemID = playbackItem else { return nil }
        let elapsed = max(0, (ProcessInfo.processInfo.systemUptime - (playbackStartedAt ?? 0)) * 1000)
        let queued = Double(playbackSamples) / Self.sessionRate * 1000
        guard elapsed < queued else {
            playbackItem = nil
            playbackSamples = 0
            playbackStartedAt = nil
            playbackBuffers = 0
            playbackInputDone = false
            onStatus?("idle")
            return nil
        }
        player.stop()
        player.reset()
        playbackItem = nil
        playbackSamples = 0
        playbackStartedAt = nil
        playbackBuffers = 0
        playbackInputDone = false
        onStatus?("interrupted")
        return (itemID, Int(min(elapsed, queued).rounded()))
    }

    func stopPlayback() {
        player.stop()
        player.reset()
        playbackItem = nil
        playbackSamples = 0
        playbackStartedAt = nil
        playbackBuffers = 0
        playbackInputDone = false
        onStatus?("idle")
    }

    private func playedBuffer(itemID: String) {
        guard playbackItem == itemID else { return }
        playbackBuffers = max(0, playbackBuffers - 1)
        if playbackInputDone, playbackBuffers == 0 { completePlayback(itemID: itemID) }
    }

    private func completePlayback(itemID: String) {
        guard playbackItem == itemID else { return }
        playbackItem = nil
        playbackSamples = 0
        playbackStartedAt = nil
        playbackBuffers = 0
        playbackInputDone = false
        onStatus?("idle")
    }

    func shutdown() {
        stopMicrophone()
        stopPlayback()
        engine.stop()
    }

    private func playbackPCMFormat() -> AVAudioFormat? {
        if let playbackFormat { return playbackFormat }
        guard let format = AVAudioFormat(
            commonFormat: .pcmFormatFloat32,
            sampleRate: Self.sessionRate,
            channels: 1,
            interleaved: false
        ) else { return nil }
        engine.connect(player, to: engine.mainMixerNode, format: format)
        playbackFormat = format
        return format
    }

    private func ensureEngineRunning() throws {
        _ = playbackPCMFormat()
        if !engine.isRunning {
            engine.prepare()
            try engine.start()
        }
    }
}

private struct AudioError: LocalizedError {
    let message: String
    init(_ message: String) { self.message = message }
    var errorDescription: String? { message }
}
