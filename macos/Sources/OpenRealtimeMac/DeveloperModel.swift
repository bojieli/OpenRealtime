import Foundation
import SwiftUI
import AppKit

@MainActor
final class DeveloperModel: ObservableObject {
    @Published var endpoint = "ws://127.0.0.1:8765/v1/realtime"
    @Published var token = ""
    @Published var workspaceRoot = FileManager.default.currentDirectoryPath
    @Published var systemPrompt = """
    You are a realtime development assistant. You can hear the microphone, receive typed text, see explicitly shared screen, camera, and browser frames, use local tools within the selected workspace, display HTML artifacts, publish downloadable files, and act only on the bounded computer target declared by this client. Answer briefly. Use display_artifact for information that is better looked at than read aloud, and describe computer actions in a few words.
    """
    @Published var cdpURL = "http://127.0.0.1:9222"
    @Published var computerMode: ComputerMode = .browser
    @Published var displays: [DisplayTarget] = []
    @Published var selectedDisplayID: CGDirectDisplayID = 0

    @Published var connectionState: ConnectionState = .disconnected
    @Published var statusText = "not connected"
    @Published var negotiationText = ""
    @Published var composer = ""
    @Published var microphoneActive = false
    @Published var microphoneMuted = false
    @Published var screenActive = false
    @Published var cameraActive = false
    @Published var browserActive = false
    @Published var audioOutputStatus = "idle"
    @Published var browserCaption = "waiting for a marked frame"
    @Published var frameStatus: [String: String] = [:]
    @Published var conversation: [ChannelRecord] = []
    @Published var channelRecords: [ChannelRecord] = []
    @Published var protocolRecords: [ProtocolRecord] = []
    @Published var debugRecords: [DebugRecord] = []
    @Published var timelineFilter = ""
    @Published var artifacts: [ArtifactRecord] = []
    @Published var selectedArtifact: ArtifactRecord?
    @Published var downloads: [DownloadRecord] = []
    @Published var pendingConfirmation: ConfirmationRequest?

    private let client = RealtimeClient()
    private let audio = AudioIO()
    private let media = MediaCaptureController()
    private let browser = BrowserUseController()
    private var desktop: DesktopComputerController?
    private var toolHost: LocalToolHost?
    private var videoLimits = VideoLimits()
    private var videoNegotiated = false
    private var confirmationContinuation: CheckedContinuation<Bool, Never>?
    private var speechBuffer = ""
    private var textBuffer = ""
    private var speechRecordStarted = false
    private var textRecordStarted = false
    private var responseOpen = false

    var microphonePermission: String { audio.permission }
    var cameraPermission: String { MediaCaptureController.cameraPermission }
    var screenPermission: String { MediaCaptureController.screenPermission }
    var accessibilityPermission: String { DesktopComputerController.permission }

    var filteredDebugRecords: [DebugRecord] {
        let query = timelineFilter.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
        guard !query.isEmpty else { return debugRecords }
        return debugRecords.filter {
            [$0.category, $0.name, $0.phase, $0.correlationID, $0.detail]
                .joined(separator: " ").lowercased().contains(query)
        }
    }

    var latencySummary: String {
        let values = debugRecords.compactMap(\.durationMS).sorted()
        guard !values.isEmpty else { return "no timed events" }
        func percentile(_ p: Double) -> Double {
            values[min(values.count - 1, Int((Double(values.count - 1) * p).rounded()))]
        }
        return String(format: "%d timed · p50 %.1f ms · p95 %.1f ms", values.count,
                      percentile(0.5), percentile(0.95))
    }

    init() {
        displays = activeDisplays()
        selectedDisplayID = displays.first?.id ?? 0

        client.onState = { [weak self] state, message in
            guard let self else { return }
            self.connectionState = state
            self.statusText = message
            if state == .failed { self.record("obs.tools", "connection", message, failed: true) }
        }
        client.onProtocol = { [weak self] direction, event in self?.recordProtocol(direction, event) }
        client.onEvent = { [weak self] event in self?.handle(event) }

        audio.onFrame = { [weak self] data in self?.client.sendAudio(data) }
        audio.onStatus = { [weak self] status in
            guard let self else { return }
            if status.contains("output") || status == "idle" || status == "interrupted" || status == "draining output" {
                self.audioOutputStatus = status
            }
        }

        media.onSource = { [weak self] source, state, width, height in
            guard let self else { return }
            self.client.updateVideoSource(source, state: state, width: width, height: height)
        }
        media.onFrame = { [weak self] source, data, width, height, timestamp in
            guard let self else { return }
            self.client.sendVideo(source: source, data: data, timestampMS: timestamp)
            self.frameStatus[source] = "\(width)×\(height) · \(data.count / 1024) KiB · \(Self.clock(timestamp))"
        }
        media.onFailure = { [weak self] source, message in
            self?.record("obs.\(source)", "capture failed", message, failed: true)
        }

        browser.onSource = { [weak self] source, state, width, height in
            guard let self else { return }
            self.client.updateVideoSource(source, state: state, width: width, height: height)
            if state == "active", self.computerMode == .browser { self.refreshToolGeometry(width: width, height: height) }
        }
        browser.onFrame = { [weak self] frame in
            guard let self else { return }
            let now = Int64((Date().timeIntervalSince1970 * 1000).rounded())
            self.client.sendVideo(source: "browser", data: frame.data, timestampMS: now)
            self.frameStatus["browser"] = "\(frame.width)×\(frame.height) · \(frame.data.count / 1024) KiB · \(frame.elementCount) marks"
        }
        browser.onCaption = { [weak self] caption in self?.browserCaption = caption }
        browser.onFailure = { [weak self] message in
            self?.record("obs.browser", "browser-use", message, failed: true)
        }
    }

    func connect() {
        guard connectionState == .disconnected || connectionState == .failed else { return }
        connectionState = .connecting
        statusText = "preparing local tool host"
        Task {
            do {
                let geometry = initialComputerGeometry()
                if computerMode == .desktop {
                    guard let display = selectedDisplay else { throw ModelError("select an active display") }
                    desktop = DesktopComputerController(display: display, maxDimension: videoLimits.maxDimension)
                }
                let host = try makeToolHost(width: geometry.width, height: geometry.height)
                toolHost = host
                let tools = await host.declarations
                try await client.connect(
                    endpoint: endpoint, token: token,
                    sessionConfiguration: initialSession(tools: tools)
                )
            } catch {
                connectionState = .failed
                statusText = error.localizedDescription
                record("obs.tools", "connection setup", error.localizedDescription, failed: true)
            }
        }
    }

    func disconnect() {
        decideConfirmation(false)
        client.disconnect()
        audio.shutdown()
        microphoneActive = false
        microphoneMuted = false
        screenActive = false
        cameraActive = false
        browserActive = false
        videoNegotiated = false
        responseOpen = false
        toolHost = nil
        desktop = nil
        Task {
            await media.stopAll()
            await browser.stop()
        }
    }

    func applySystemPrompt() {
        guard connectionState == .connected else { return }
        client.send(["type": "session.update", "session": ["type": "realtime", "instructions": systemPrompt]])
        record("act.tools", "system prompt", "updated for the active session")
    }

    func submitText() {
        let text = composer.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty, connectionState == .connected else { return }
        composer = ""
        client.sendUserText(text)
        let entry = ChannelRecord(channel: "obs.text", title: "you · typed", body: text, failed: false)
        conversation.append(entry)
        channelRecords.append(entry)
    }

    func submitArtifactText(_ text: String) {
        guard connectionState == .connected else { return }
        client.sendUserText(text)
        record("obs.text", "artifact interaction", text)
    }

    func endTurn() {
        guard connectionState == .connected else { return }
        client.send(["type": "input_audio_buffer.commit"])
        client.send(["type": "response.create"])
    }

    func toggleMicrophone() {
        guard connectionState == .connected else { return }
        if microphoneActive {
            audio.toggleMute()
            microphoneMuted = audio.muted
            return
        }
        Task {
            do {
                try await audio.startMicrophone()
                microphoneActive = true
                microphoneMuted = false
                record("obs.audio", "microphone", "PCM16 24 kHz stream started")
            } catch { record("obs.audio", "microphone failed", error.localizedDescription, failed: true) }
        }
    }

    func toggleScreen() {
        guard requireVideo() else { return }
        Task {
            if screenActive {
                await media.stopScreen()
                screenActive = false
            } else {
                do {
                    try await media.startScreen(displayID: selectedDisplayID, limits: videoLimits)
                    screenActive = true
                    record("obs.screen", "screen", "selected display capture started")
                } catch { record("obs.screen", "screen failed", error.localizedDescription, failed: true) }
            }
        }
    }

    func toggleCamera() {
        guard requireVideo() else { return }
        Task {
            if cameraActive {
                media.stopCamera()
                cameraActive = false
            } else {
                do {
                    try await media.startCamera(limits: videoLimits)
                    cameraActive = true
                    record("obs.camera", "camera", "physical camera capture started")
                } catch { record("obs.camera", "camera failed", error.localizedDescription, failed: true) }
            }
        }
    }

    func toggleBrowser() {
        guard requireVideo() else { return }
        Task {
            if browserActive {
                await browser.stop()
                browserActive = false
            } else {
                do {
                    try await browser.start(cdpURL: cdpURL, limits: videoLimits)
                    browserActive = true
                    record("obs.browser", "browser-use", "marked browser capture started")
                } catch { record("obs.browser", "browser-use failed", error.localizedDescription, failed: true) }
            }
        }
    }

    func chooseWorkspace() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.allowsMultipleSelection = false
        if panel.runModal() == .OK, let url = panel.url { workspaceRoot = url.path }
    }

    func saveDownload(_ download: DownloadRecord) {
        let panel = NSSavePanel()
        panel.nameFieldStringValue = download.filename
        if panel.runModal() == .OK, let destination = panel.url {
            do {
                try Data(contentsOf: download.url).write(to: destination, options: .atomic)
            } catch { record("act.download", "save failed", error.localizedDescription, failed: true) }
        }
    }

    func exportProtocolLog() {
        let panel = NSSavePanel()
        panel.nameFieldStringValue = "openrealtime-macos-\(Int(Date().timeIntervalSince1970)).json"
        guard panel.runModal() == .OK, let url = panel.url else { return }
        let records: [[String: Any]] = protocolRecords.map {
            ["timestamp_ms": Int64($0.timestamp.timeIntervalSince1970 * 1000),
             "direction": $0.direction, "type": $0.type, "payload": $0.payload]
        }
        do {
            let data = try JSONSerialization.data(withJSONObject: ["events": records], options: [.prettyPrinted, .sortedKeys])
            try data.write(to: url, options: .atomic)
        } catch { record("act.download", "export failed", error.localizedDescription, failed: true) }
    }

    func decideConfirmation(_ approved: Bool) {
        guard let continuation = confirmationContinuation else {
            pendingConfirmation = nil
            return
        }
        confirmationContinuation = nil
        pendingConfirmation = nil
        continuation.resume(returning: approved)
    }

    private func requestConfirmation(name: String, consequence: String,
                                     arguments: [String: Any]) async -> Bool {
        if confirmationContinuation != nil { return false }
        return await withCheckedContinuation { continuation in
            confirmationContinuation = continuation
            pendingConfirmation = ConfirmationRequest(
                name: name, consequence: consequence, arguments: prettyJSONString(arguments)
            )
        }
    }

    private func makeToolHost(width: Int, height: Int) throws -> LocalToolHost {
        try LocalToolHost(
            rootPath: workspaceRoot, mode: computerMode, width: width, height: height,
            confirm: { [weak self] name, consequence, arguments in
                guard let self else { return false }
                return await self.requestConfirmation(name: name, consequence: consequence, arguments: arguments)
            },
            computerAction: { [weak self] name, arguments in
                guard let self else { throw ModelError("the native client disconnected") }
                return try await self.performComputer(name: name, arguments: arguments)
            }
        )
    }

    private func performComputer(name: String, arguments: [String: Any]) async throws -> String {
        let started = Date()
        let output: String
        if computerMode == .browser {
            output = try await browser.perform(name: name, arguments: arguments)
        } else {
            guard screenActive else { throw ModelError("Desktop mode requires the selected screen source to be live") }
            guard let desktop else { throw ModelError("the selected-display controller is unavailable") }
            output = try await desktop.perform(name: name, arguments: arguments)
        }
        let elapsed = Int(Date().timeIntervalSince(started) * 1000)
        record("act.computer", name, "\(output) · \(elapsed) ms")
        return output
    }

    private func initialComputerGeometry() -> (width: Int, height: Int) {
        if computerMode == .desktop, let display = selectedDisplay {
            let size = display.videoSize(maxDimension: videoLimits.maxDimension)
            return (Int(size.width), Int(size.height))
        }
        return (1280, 720)
    }

    private var selectedDisplay: DisplayTarget? {
        displays.first(where: { $0.id == selectedDisplayID })
    }

    private func initialSession(tools: [[String: Any]]) -> [String: Any] {
        [
            "type": "realtime",
            "instructions": systemPrompt,
            "audio": [
                "input": ["format": ["type": "audio/pcm", "rate": 24_000]],
                "output": ["format": ["type": "audio/pcm", "rate": 24_000]],
            ],
            "tools": tools,
            "openrealtime": [
                "version": 1,
                "supports": ["video.input", "observations", "computer_use"],
                "observers": ["audio", "video"],
                "debug": ["enabled": true, "include_payloads": false],
            ],
        ]
    }

    private func requireVideo() -> Bool {
        guard connectionState == .connected else { return false }
        guard videoNegotiated else {
            record("obs.video", "video unavailable", "the server did not negotiate video.input", failed: true)
            return false
        }
        return true
    }

    private func refreshToolGeometry(width: Int, height: Int) {
        guard let toolHost, connectionState == .connected else { return }
        Task {
            let declarations = await toolHost.declarationsFor(width: width, height: height)
            client.send(["type": "session.update", "session": ["type": "realtime", "tools": declarations]])
        }
    }

    private func handle(_ event: [String: Any]) {
        let type = (event["type"] as? String) ?? ""
        switch type {
        case "session.created":
            if let session = event["session"] as? [String: Any], let id = session["id"] as? String {
                statusText = "connected · \(id)"
            }
        case "session.updated":
            handleSessionUpdated(event)
        case "input_audio_buffer.speech_started":
            statusText = "listening"
            if let interruption = audio.interrupt() {
                client.send(["type": "output_audio_buffer.clear"])
                client.send([
                    "type": "conversation.item.truncate", "item_id": interruption.itemID,
                    "content_index": 0, "audio_end_ms": interruption.playedMS,
                ])
            }
        case "input_audio_buffer.speech_stopped": statusText = "thinking"
        case "conversation.item.input_audio_transcription.completed":
            let text = (event["transcript"] as? String) ?? ""
            let entry = ChannelRecord(channel: "obs.audio", title: "you · speech", body: text, failed: false)
            conversation.append(entry); channelRecords.append(entry)
        case "response.created":
            responseOpen = true; speechBuffer = ""; textBuffer = ""
            speechRecordStarted = false; textRecordStarted = false; statusText = "responding"
        case "response.output_audio_transcript.delta":
            speechBuffer += (event["delta"] as? String) ?? ""
            replaceAssistant(channel: "act.speech", title: "assistant · speech", body: speechBuffer,
                             started: speechRecordStarted)
            speechRecordStarted = true
        case "response.output_text.delta":
            textBuffer += (event["delta"] as? String) ?? ""
            replaceAssistant(channel: "act.text", title: "assistant · text", body: textBuffer,
                             started: textRecordStarted)
            textRecordStarted = true
        case "response.output_audio.delta":
            if let item = event["item_id"] as? String, let delta = event["delta"] as? String {
                audio.enqueue(itemID: item, base64: delta)
            }
        case "response.output_audio.done":
            if let item = event["item_id"] as? String { audio.finish(itemID: item) }
        case "openrealtime.observation.added":
            let observer = (event["observer"] as? String) ?? "observer"
            let source = (event["source"] as? String) ?? ""
            let channel = source.isEmpty ? "obs.\(observer)" : "obs.\(source)"
            record(channel, "observed · \(observer)\(source.isEmpty ? "" : " · \(source)")",
                   (event["text"] as? String) ?? "")
        case "openrealtime.debug.event": handleDebug(event)
        case "response.function_call_arguments.done": executeTool(event)
        case "response.done":
            responseOpen = false; statusText = "connected"
            if let response = event["response"] as? [String: Any],
               let status = response["status"] as? String, ["failed", "incomplete"].contains(status) {
                record("act.tools", "response \(status)", prettyJSONString(response), failed: true)
            }
        case "error":
            let error = event["error"] as? [String: Any]
            record("obs.tools", "server error", (error?["message"] as? String) ?? prettyJSONString(event), failed: true)
            statusText = "server error"
        default: break
        }
    }

    private func handleSessionUpdated(_ event: [String: Any]) {
        guard let session = event["session"] as? [String: Any],
              let extensionObject = session["openrealtime"] as? [String: Any] else {
            negotiationText = "base Realtime only · no OpenRealtime extensions"
            videoNegotiated = false
            return
        }
        let enabled = (extensionObject["enabled"] as? [String]) ?? []
        videoNegotiated = enabled.contains("video.input")
        if let video = extensionObject["video"] as? [String: Any] {
            let negotiated = VideoLimits(dictionary: video)
            if negotiated != videoLimits {
                videoLimits = negotiated
                if computerMode == .desktop, let display = selectedDisplay {
                    desktop = DesktopComputerController(display: display, maxDimension: negotiated.maxDimension)
                    let size = display.videoSize(maxDimension: negotiated.maxDimension)
                    refreshToolGeometry(width: Int(size.width), height: Int(size.height))
                }
            }
        }
        let observers = (extensionObject["observers"] as? [String]) ?? []
        let debug = extensionObject["debug"] as? [String: Any]
        negotiationText = "\(enabled.joined(separator: ", ")) · observers \(observers.joined(separator: ", ")) · debug \((debug?["enabled"] as? Bool) == true ? "on" : "off")"
    }

    private func handleDebug(_ event: [String: Any]) {
        let timestamp = (event["timestamp_ms"] as? NSNumber)?.int64Value
            ?? Int64((Date().timeIntervalSince1970 * 1000).rounded())
        let duration = (event["duration_ms"] as? NSNumber)?.doubleValue
        var detail: [String: Any] = [:]
        if let attributes = event["attributes"] as? [String: Any] { detail["attributes"] = attributes }
        if let message = event["message"] as? String, !message.isEmpty { detail["message"] = message }
        if let payload = event["payload"], !(payload is NSNull) { detail["payload"] = payload }
        debugRecords.append(DebugRecord(
            timestampMS: timestamp, category: (event["category"] as? String) ?? "",
            name: (event["name"] as? String) ?? "", phase: (event["phase"] as? String) ?? "",
            durationMS: duration, correlationID: (event["correlation_id"] as? String) ?? "",
            detail: detail.isEmpty ? "" : compactJSONString(detail)
        ))
        if debugRecords.count > 4_000 { debugRecords.removeFirst(debugRecords.count - 4_000) }
    }

    private func executeTool(_ event: [String: Any]) {
        let callID = (event["call_id"] as? String) ?? ""
        let name = (event["name"] as? String) ?? ""
        let encoded = (event["arguments"] as? String) ?? "{}"
        let arguments: [String: Any]
        if let data = encoded.data(using: .utf8),
           let object = try? JSONSerialization.jsonObject(with: data),
           let parsed = object as? [String: Any] {
            arguments = parsed
        } else {
            client.answerTool(callID: callID, output: compactJSONString(["error": "tool arguments were not a JSON object"]))
            return
        }
        record(name.hasPrefix("computer.") ? "act.computer" : "act.tools", name, compactJSONString(arguments))
        guard let toolHost else {
            client.answerTool(callID: callID, output: compactJSONString(["error": "local tool host is unavailable"]))
            return
        }
        Task {
            let result = await toolHost.execute(name: name, arguments: arguments)
            switch result {
            case .success(let execution):
                if let artifact = execution.artifact {
                    artifacts.removeAll { $0.id == artifact.id }
                    artifacts.append(artifact)
                    selectedArtifact = artifact
                    record("act.artifact", name, "\(artifact.title) · revision \(artifact.version)")
                }
                if let download = execution.download {
                    downloads.removeAll { $0.id == download.id }
                    downloads.append(download)
                    record("act.download", name, "\(download.filename) · \(download.bytes) bytes")
                }
                client.answerTool(callID: callID, output: execution.output)
                record("obs.tools", "\(name) returned", String(execution.output.prefix(600)))
            case .failure(let error):
                let output = compactJSONString(["error": error.localizedDescription])
                client.answerTool(callID: callID, output: output)
                record("obs.tools", "\(name) refused", error.localizedDescription, failed: true)
            }
        }
    }

    private func replaceAssistant(channel: String, title: String, body: String, started: Bool) {
        if started, let index = conversation.lastIndex(where: { $0.channel == channel && $0.title == title }) {
            conversation[index] = ChannelRecord(channel: channel, title: title, body: body, failed: false)
        } else {
            conversation.append(ChannelRecord(channel: channel, title: title, body: body, failed: false))
        }
        if started, let index = channelRecords.lastIndex(where: { $0.channel == channel && $0.title == title }) {
            channelRecords[index] = ChannelRecord(channel: channel, title: title, body: body, failed: false)
        } else {
            channelRecords.append(ChannelRecord(channel: channel, title: title, body: body, failed: false))
        }
    }

    private func record(_ channel: String, _ title: String, _ body: String, failed: Bool = false) {
        let entry = ChannelRecord(channel: channel, title: title, body: body, failed: failed)
        channelRecords.append(entry)
        if channel == "obs.audio" || channel == "obs.text" || channel == "act.speech" || channel == "act.text" {
            conversation.append(entry)
        }
        if channelRecords.count > 2_000 { channelRecords.removeFirst(channelRecords.count - 2_000) }
    }

    private func recordProtocol(_ direction: String, _ event: [String: Any]) {
        protocolRecords.append(ProtocolRecord(
            direction: direction, type: (event["type"] as? String) ?? "", payload: safeProtocolPayload(event)
        ))
        if protocolRecords.count > 2_000 { protocolRecords.removeFirst(protocolRecords.count - 2_000) }
    }

    private static func clock(_ milliseconds: Int64) -> String {
        let date = Date(timeIntervalSince1970: Double(milliseconds) / 1000)
        let formatter = DateFormatter()
        formatter.dateFormat = "HH:mm:ss.SSS"
        return formatter.string(from: date)
    }
}

private struct ModelError: LocalizedError {
    let message: String
    init(_ message: String) { self.message = message }
    var errorDescription: String? { message }
}
