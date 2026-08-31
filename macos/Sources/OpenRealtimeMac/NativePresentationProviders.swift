import Foundation
import AppKit
import CoreGraphics
import OpenRealtimeClientCore

struct NativeMediaSnapshot: Equatable {
    let microphoneActive: Bool
    let microphoneMuted: Bool
    let outputStatus: String
    let permission: String
}

@MainActor
final class NativeMediaBoundary {
    private let audio = AudioIO()
    private let transport: RealtimeClient
    private let reducer: NativeReducerController
    private let protocolEvents: ValidatedProtocolEventService
    private var unsubscribeEvents: (() -> Void)?
    private var listeners: [UUID: (NativeMediaSnapshot) -> Void] = [:]
    private var mounted = false
    private var outputStatus = "idle"

    init(
        transport: RealtimeClient,
        reducer: NativeReducerController,
        protocolEvents: ValidatedProtocolEventService
    ) {
        self.transport = transport
        self.reducer = reducer
        self.protocolEvents = protocolEvents
    }

    var microphonePermission: String { audio.permission }

    func mount() throws {
        guard !mounted else { throw NativeProviderError("native media provider mounted twice") }
        mounted = true
        audio.onFrame = { [weak self] data in
            guard let self, self.mounted else { return }
            self.transport.sendAudio(data)
        }
        audio.onStatus = { [weak self] status in
            guard let self, self.mounted else { return }
            self.outputStatus = String(status.prefix(1_024))
            if status == "idle" || status == "interrupted" {
                self.reducer.setPlayout(speaking: false)
            }
            self.publish()
        }
        unsubscribeEvents = try protocolEvents.subscribe { [weak self] document in
            guard let self, self.mounted, let event = try? document.snapshot() else { return }
            self.consume(event)
        }
        publish()
    }

    func unmount() {
        guard mounted else { return }
        mounted = false
        unsubscribeEvents?()
        unsubscribeEvents = nil
        audio.onFrame = nil
        audio.onStatus = nil
        audio.shutdown()
        outputStatus = "idle"
        listeners.removeAll()
    }

    func snapshot() -> NativeMediaSnapshot {
        NativeMediaSnapshot(
            microphoneActive: audio.microphoneActive,
            microphoneMuted: audio.muted,
            outputStatus: outputStatus,
            permission: audio.permission
        )
    }

    @discardableResult
    func subscribe(_ listener: @escaping (NativeMediaSnapshot) -> Void) throws -> () -> Void {
        guard mounted else { throw NativeProviderError("native media provider is unavailable") }
        guard listeners.count < 128 else { throw NativeProviderError("native media listener limit reached") }
        let id = UUID()
        listeners[id] = listener
        listener(snapshot())
        return { [weak self] in self?.listeners.removeValue(forKey: id) }
    }

    func toggleMicrophone() async throws {
        guard mounted else { throw NativeProviderError("native media provider is unavailable") }
        if audio.microphoneActive {
            audio.toggleMute()
        } else {
            try await audio.startMicrophone()
        }
        publish()
    }

    func stopMicrophone() {
        audio.stopMicrophone()
        publish()
    }

    private func consume(_ event: [String: Any]) {
        switch event["type"] as? String {
        case "input_audio_buffer.speech_started":
            if let interruption = audio.interrupt() {
                reducer.setPlayout(
                    itemID: interruption.itemID,
                    playedMS: interruption.playedMS,
                    speaking: true
                )
            }
        case "response.output_audio.delta":
            if let item = event["item_id"] as? String, let delta = event["delta"] as? String {
                audio.enqueue(itemID: item, base64: delta)
                reducer.setPlayout(itemID: item, playedMS: 0, speaking: true)
            }
        case "response.output_audio.done":
            if let item = event["item_id"] as? String { audio.finish(itemID: item) }
        default: break
        }
    }

    private func publish() {
        let current = snapshot()
        for listener in Array(listeners.values) { listener(current) }
    }
}

struct NativeVideoGeometry: Equatable, Sendable {
    let width: Int
    let height: Int
}

struct NativeVideoSnapshot: Equatable {
    let enabled: Bool
    let active: [String]
    let limits: VideoLimits
    let geometry: [String: NativeVideoGeometry]
    let frameStatus: [String: String]
    let browserCaption: String
    let diagnostic: String
}

@MainActor
final class NativeVideoBoundary {
    private static let sources = Set(["camera", "screen", "browser"])

    private let transport: RealtimeClient
    private let reducer: NativeReducerController
    private let configuration: SessionConfigurationService
    private let capture = MediaCaptureController()
    private let browser = BrowserUseController()
    private var unsubscribeState: (() -> Void)?
    private var removeContribution: (() -> Void)?
    private var listeners: [UUID: (NativeVideoSnapshot) -> Void] = [:]
    private var active = Set<String>()
    private var geometry: [String: NativeVideoGeometry] = [:]
    private var frameStatus: [String: String] = [:]
    private var limits = VideoLimits()
    private var diagnostic = ""
    private var browserCaption = "waiting for a marked frame"
    private var negotiated = false
    private var enabled = false
    private var mounted = false
    private var generation = 0
    private var cleanupTask: Task<Void, Never>?
    private var browserCleanupTask: Task<Void, Never>?

    init(
        transport: RealtimeClient,
        reducer: NativeReducerController,
        configuration: SessionConfigurationService
    ) {
        self.transport = transport
        self.reducer = reducer
        self.configuration = configuration
    }

    var cameraPermission: String { MediaCaptureController.cameraPermission }
    var screenPermission: String { MediaCaptureController.screenPermission }

    func mount() throws {
        guard !mounted else { throw NativeProviderError("native video provider mounted twice") }
        mounted = true
        generation += 1
        capture.onSource = { [weak self] source, state, width, height in
            self?.sourceChanged(source, state: state, width: width, height: height)
        }
        capture.onFrame = { [weak self] source, data, width, height, timestamp in
            self?.frame(source, data: data, width: width, height: height, timestampMS: timestamp)
        }
        capture.onFailure = { [weak self] source, message in self?.fail(source, message) }
        browser.onSource = { [weak self] source, state, width, height in
            self?.sourceChanged(source, state: state, width: width, height: height)
        }
        browser.onFrame = { [weak self] frame in
            guard let self, self.mounted else { return }
            let timestamp = Int64((Date().timeIntervalSince1970 * 1_000).rounded())
            self.frame(
                "browser", data: frame.data, width: frame.width,
                height: frame.height, timestampMS: timestamp
            )
            self.frameStatus["browser"] = "\(frame.width)×\(frame.height) · \(frame.data.count / 1_024) KiB · \(frame.elementCount) marks"
            self.publish()
        }
        browser.onCaption = { [weak self] value in
            guard let self, self.mounted else { return }
            self.browserCaption = String(value.prefix(2_048))
            self.publish()
        }
        browser.onFailure = { [weak self] value in self?.fail("browser", value) }
        removeContribution = try configuration.contribute([
            "supports": ["observations", "video.input"],
            "observers": ["audio", "video"],
        ])
        unsubscribeState = try reducer.subscribe { [weak self] state in self?.observe(state) }
        publish()
    }

    func unmount() {
        guard mounted else { return }
        mounted = false
        generation += 1
        unsubscribeState?()
        unsubscribeState = nil
        removeContribution?()
        removeContribution = nil
        capture.onSource = nil
        capture.onFrame = nil
        capture.onFailure = nil
        browser.onSource = nil
        browser.onFrame = nil
        browser.onCaption = nil
        browser.onFailure = nil
        active.removeAll()
        geometry.removeAll()
        frameStatus.removeAll()
        browserCaption = "waiting for a marked frame"
        diagnostic = ""
        limits = VideoLimits()
        negotiated = false
        enabled = false
        listeners.removeAll()
        let capture = self.capture
        cleanupTask?.cancel()
        cleanupTask = Task {
            await capture.stopAll()
        }
        browserCleanupTask?.cancel()
        let browser = self.browser
        browserCleanupTask = Task { await browser.stop() }
    }

    func snapshot() -> NativeVideoSnapshot {
        NativeVideoSnapshot(
            enabled: enabled, active: active.sorted(), limits: limits,
            geometry: geometry, frameStatus: frameStatus,
            browserCaption: browserCaption,
            diagnostic: diagnostic
        )
    }

    @discardableResult
    func subscribe(_ listener: @escaping (NativeVideoSnapshot) -> Void) throws -> () -> Void {
        guard mounted else { throw NativeProviderError("native video provider is unavailable") }
        guard listeners.count < 128 else { throw NativeProviderError("native video listener limit reached") }
        let id = UUID()
        listeners[id] = listener
        listener(snapshot())
        return { [weak self] in self?.listeners.removeValue(forKey: id) }
    }

    func startCamera() async throws { try await start("camera") }
    func startScreen(displayID: CGDirectDisplayID) async throws {
        try await start("screen", displayID: displayID)
    }

    func startBrowser(cdpURL: String) async throws {
        guard mounted, enabled else {
            throw NativeProviderError("video input is not negotiated for this session")
        }
        try Self.validateCDP(cdpURL)
        let requestGeneration = generation
        if let browserCleanupTask { await browserCleanupTask.value }
        guard mounted, generation == requestGeneration else {
            throw NativeProviderError("video provider was remounted while browser capture started")
        }
        await stop("browser")
        try await browser.start(cdpURL: cdpURL, limits: limits)
        guard mounted, generation == requestGeneration else {
            await browser.stop()
            throw NativeProviderError("video provider was lost while browser capture started")
        }
        diagnostic = ""
        publish()
    }

    func stop(_ source: String) async {
        guard Self.sources.contains(source) else { return }
        switch source {
        case "camera": capture.stopCamera()
        case "screen": await capture.stopScreen()
        case "browser": await browser.stop()
        default: break
        }
        active.remove(source)
        geometry.removeValue(forKey: source)
        frameStatus.removeValue(forKey: source)
        publish()
    }

    func stopAll() async {
        await capture.stopAll()
        await browser.stop()
        active.removeAll()
        geometry.removeAll()
        frameStatus.removeAll()
        publish()
    }

    func isActive(_ source: String) -> Bool { active.contains(source) }

    private func start(
        _ source: String,
        displayID: CGDirectDisplayID = 0
    ) async throws {
        guard mounted, enabled, Self.sources.contains(source) else {
            throw NativeProviderError("video input is not negotiated for this session")
        }
        let requestGeneration = generation
        if let cleanupTask { await cleanupTask.value }
        guard mounted, generation == requestGeneration else {
            throw NativeProviderError("video provider was remounted while capture started")
        }
        await stop(source)
        switch source {
        case "camera": try await capture.startCamera(limits: limits)
        case "screen": try await capture.startScreen(displayID: displayID, limits: limits)
        default: throw NativeProviderError("unknown video source")
        }
        guard mounted, generation == requestGeneration else {
            await stop(source)
            throw NativeProviderError("video provider was lost while capture started")
        }
        active.insert(source)
        diagnostic = ""
        publish()
    }

    // The reducer always projects a session.openrealtime.video object, and
    // states it as zeroes when the capability was not enabled. Limits are
    // therefore only meaningful once video.input is actually in the negotiated
    // set: validating the unnegotiated zeroes would report a ceiling breach
    // that never happened and schedule a teardown on every single snapshot,
    // and each teardown emits a source update the session refuses, whose error
    // is another snapshot.
    private func observe(_ state: [String: Any]) {
        guard mounted else { return }
        let connection = state["connection"] as? [String: Any]
        let session = state["session"] as? [String: Any]
        let extensionObject = session?["openrealtime"] as? [String: Any]
        let features = extensionObject?["enabled"] as? [String] ?? []
        let offered = connection?["phase"] as? String == "connected"
            && features.contains("video.input")
        var nextEnabled = offered
        if offered, let raw = extensionObject?["video"] as? [String: Any] {
            let candidate = VideoLimits(dictionary: raw)
            if Self.valid(candidate) {
                limits = candidate
                diagnostic = ""
            } else {
                diagnostic = "negotiated video limits exceed the native ceiling"
                nextEnabled = false
            }
        }
        negotiated = offered
        enabled = nextEnabled
        if !enabled, !active.isEmpty { Task { [weak self] in await self?.stopAll() } }
        publish()
    }

    private func sourceChanged(_ source: String, state: String, width: Int, height: Int) {
        guard mounted, Self.sources.contains(source) else { return }
        if state == "active" {
            active.insert(source)
            geometry[source] = NativeVideoGeometry(width: width, height: height)
        } else if state == "closed" {
            active.remove(source)
            geometry.removeValue(forKey: source)
        }
        // A source declaration only exists on a session that negotiated
        // video.input. Local capture lifecycle is still projected into the
        // view, but it never becomes wire traffic the session must refuse.
        if negotiated {
            transport.updateVideoSource(source, state: state, width: width, height: height)
        }
        publish()
    }

    private func frame(_ source: String, data: Data, width: Int, height: Int, timestampMS: Int64) {
        guard mounted, enabled, active.contains(source), data.count <= limits.maxFrameBytes else { return }
        transport.sendVideo(source: source, data: data, timestampMS: timestampMS)
        geometry[source] = NativeVideoGeometry(width: width, height: height)
        frameStatus[source] = "\(width)×\(height) · \(data.count / 1_024) KiB · \(Self.clock(timestampMS))"
        publish()
    }

    private func fail(_ source: String, _ message: String) {
        guard mounted else { return }
        diagnostic = "\(source): \(String(message.prefix(4_096)))"
        active.remove(source)
        publish()
    }

    private func publish() {
        let current = snapshot()
        for listener in Array(listeners.values) { listener(current) }
    }

    private static func valid(_ value: VideoLimits) -> Bool {
        value.format == "jpeg" && (1...10).contains(value.fpsCap)
            && (64...4_096).contains(value.maxDimension)
            && (1_024...(4 << 20)).contains(value.maxFrameBytes)
    }

    private static func clock(_ milliseconds: Int64) -> String {
        let date = Date(timeIntervalSince1970: Double(milliseconds) / 1_000)
        let formatter = DateFormatter()
        formatter.dateFormat = "HH:mm:ss.SSS"
        return formatter.string(from: date)
    }

    private static func validateCDP(_ value: String) throws {
        guard let components = URLComponents(string: value),
              ["http", "https"].contains(components.scheme?.lowercased() ?? ""),
              let host = components.host?.lowercased(),
              ["localhost", "127.0.0.1", "::1"].contains(host),
              components.user == nil, components.password == nil,
              components.fragment == nil else {
            throw NativeProviderError("the browser CDP endpoint must be a loopback HTTP URL")
        }
    }
}


struct NativeProviderError: LocalizedError {
    let message: String
    init(_ message: String) { self.message = message }
    var errorDescription: String? { message }
}
