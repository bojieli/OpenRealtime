import Foundation
import SwiftUI
import AppKit
import UniformTypeIdentifiers
import OpenRealtimeClientCore

@MainActor
final class DeveloperModel: ObservableObject {
    let endpoint: String
    /// Which transport this client was built against, for the view to state.
    let transportName: String
    /// The same transport under the reducer's own name, for the hosted proof.
    let transportKind: String
    let distribution: String
    let manifestFingerprint: String
    let endpointFingerprint: String
    @Published var token = ""
    @Published var systemPrompt = """
    You are a realtime development assistant. You can hear the microphone, receive typed text, and see explicitly shared screen, camera, and browser frames. When negotiated, use only the scoped client effects declared by the host. Answer briefly and use artifacts when information is better viewed than spoken.
    """
    @Published var cdpURL = "http://127.0.0.1:9222"
    @Published var displays: [DisplayTarget] = []
    @Published var selectedDisplayID: CGDirectDisplayID = 0

    @Published var connectionState: ConnectionState = .disconnected
    @Published private(set) var sessionID = ""
    @Published private(set) var updatedSessionID = ""
    @Published var statusText = "not connected"
    @Published var sessionErrorText = ""
    @Published var negotiationText = ""
    @Published var transportDiagnosticsText = "transport idle"
    @Published var effectsStatusText = "host effects idle"
    @Published var artifactDiagnostic = ""
    @Published var clientIdentity = ""
    @Published var preparingConnection = false
    @Published var composer = ""
    @Published var microphoneActive = false
    @Published var microphoneMuted = false
    @Published var speakerMuted = false
    @Published var agentTileHidden = false
    @Published var previews: [String: Data] = [:]
    @Published var recording = false
    @Published var selectedScenario = ""
    let roomScenarios = RoomScenario.load()
    private var handledRoomCalls = Set<String>()
    private var roomRecorder: AnyObject?
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
    @Published var inspectionAccessText = "session inspection unavailable"
    @Published var inspectionStatus = "waiting for a scoped capability"
    @Published var inspectionLive = ""
    @Published var inspectionDeltas = ""
    @Published var inspectionTrace = ""
    @Published var inspectionRefreshing = false
    @Published var authoringCapability = ""
    @Published var authoringPath = "agent.ortg"
    @Published var authoringSource = "graph agent {\n}\n"
    @Published var authoringStatus = "not analyzed"
    @Published var authoringPresentation: NativeConfigurationPresentation?
    @Published var authoringRunning = false
    @Published var artifacts: [ClientArtifactReference] = []
    @Published var selectedArtifact: ClientArtifactReference?
    @Published var selectedArtifactHTML = ""
    @Published var artifactLoading = false
    @Published var downloads: [ClientDownloadReference] = []
    @Published var pendingConfirmation: ConfirmationRequest?

    private let assembly: NativeClientAssembly
    private let reducer: NativeReducerController
    private let media: NativeMediaBoundary
    private let video: NativeVideoBoundary
    private let effects: NativeEffectsBoundary?
    private let artifactService: NativeArtifactsBoundary?
    private let inspection: NativeInspectionBoundary
    private let authoring: NativeAuthoringBoundary
    private var auxiliaryRecords: [ChannelRecord] = []
    private var effectRecords: [ChannelRecord] = []
    private var subscriptions: [() -> Void] = []
    private var inspectionEpoch = 0
    private var authoringEpoch = 0
    private var artifactEpoch = 0

    var microphonePermission: String { media.microphonePermission }
    var cameraPermission: String { video.cameraPermission }
    var screenPermission: String { video.screenPermission }
    var effectsAvailable: Bool { effects != nil }
    var artifactsAvailable: Bool { artifactService != nil }

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
        return String(
            format: "%d timed · p50 %.1f ms · p95 %.1f ms",
            values.count, percentile(0.5), percentile(0.95)
        )
    }

    init(
        distribution: NativeClientDistribution,
        endpointDirectoryData: Data? = nil
    ) throws {
        let assembly = try NativeClientAssembly(
            distribution: distribution, endpointDirectoryData: endpointDirectoryData
        )
        self.assembly = assembly
        self.distribution = distribution.rawValue
        manifestFingerprint = assembly.manifest.manifestFingerprint
        endpointFingerprint = assembly.endpointDirectory.fingerprint
        // Whichever realtime transport the deployment declared. The directory
        // carries exactly one, and it is the address this client is bound to.
        if let webSocket = try? assembly.endpointDirectory.endpoint(
            named: .realtimeWebSocket,
            protocol: NativeEndpoint.realtimeWebSocketProtocol
        ) {
            endpoint = webSocket.url
            transportName = "WebSocket"
            transportKind = "websocket"
        } else {
            endpoint = try assembly.endpointDirectory.endpoint(
                named: .realtimeWebRTC,
                protocol: NativeEndpoint.realtimeWebRTCProtocol
            ).url
            transportName = "WebRTC"
            transportKind = "webrtc"
        }
        let services = assembly.view.services
        reducer = services.reducer
        media = services.media
        video = services.video
        effects = services.effects
        artifactService = services.artifacts
        inspection = services.inspection
        authoring = services.authoring
        clientIdentity = "\(assembly.manifest.manifestFingerprint) · endpoints \(assembly.endpointDirectory.fingerprint)"
        if effects == nil {
            effectsStatusText = "host effects not installed in this profile"
            systemPrompt = """
            You are a realtime development assistant. You can hear the microphone, receive typed text, and see explicitly shared screen, camera, and browser frames. Answer briefly. This observer profile has no client-effect or artifact provider.
            """
        }
        if artifactService == nil {
            artifactDiagnostic = "artifact provider not installed in this profile"
        }
        displays = activeDisplays()
        selectedDisplayID = displays.first?.id ?? 0

        reducer.observeProtocol { [weak self] direction, event in
            self?.recordProtocol(direction, event)
            if direction == "IN", event["type"] as? String == "session.updated",
               let session = event["session"] as? [String: Any],
               let sessionID = session["id"] as? String, !sessionID.isEmpty {
                self?.updatedSessionID = sessionID
            }
        }
        assembly.view.attach(
            onMount: { [weak self] in self?.bindProjections() },
            onUnmount: { [weak self] in self?.unbindProjections() }
        )
        reducer.publishCurrent()
    }

    func connect() {
        guard !preparingConnection,
              connectionState == .disconnected || connectionState == .failed else { return }
        preparingConnection = true
        statusText = "binding pinned host providers"
        Task {
            defer {
                preparingConnection = false
                reducer.publishCurrent()
            }
            do {
                try effects?.configure()
                try artifactService?.configure()
                try await reducer.connect(
                    token: token, session: initialSession()
                )
            } catch {
                effects?.deactivate()
                artifactService?.deactivate()
                record("obs.tools", "connection setup", error.localizedDescription, failed: true)
            }
        }
    }

    func disconnect() {
        handledRoomCalls.removeAll()
        if #available(macOS 15.0, *), let recorder = roomRecorder as? RoomRecorder { Task { await recorder.stop() } }
        effects?.deactivate()
        artifactService?.deactivate()
        reducer.disconnect()
        media.stopMicrophone()
        inspectionChanged(nil)
        Task { await video.stopAll() }
    }

    func shutdown() {
        disconnect()
        assembly.view.detach()
        do { try assembly.composition.stop() }
        catch { record("obs.tools", "client disposal", error.localizedDescription, failed: true) }
    }

    func applySystemPrompt() {
        guard connectionState == .connected else { return }
        reducer.sessionUpdate(["type": "realtime", "instructions": systemPrompt])
        record("act.tools", "system prompt", "updated for the active session")
    }

    func submitText() {
        let text = composer.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty, connectionState == .connected else { return }
        composer = ""
        reducer.sendText(text)
    }

    func submitArtifactText(_ text: String) {
        guard connectionState == .connected else { return }
        reducer.sendText(text)
    }

    func endTurn() {
        guard connectionState == .connected else { return }
        reducer.endTurn()
    }

    func applyRoomScenario() {
        guard connectionState == .connected else { return }
        let preset = roomScenarios.first { $0.id == selectedScenario }
        if let preset { systemPrompt = preset.instructions }
        else { systemPrompt = "You are a realtime assistant. Follow the user's instructions about when to speak. Answer briefly." }
        reducer.sessionUpdate(["type": "realtime", "instructions": systemPrompt, "tools": preset?.tools ?? []])
    }

    func toggleSpeaker() { media.toggleSpeaker() }

    func attachImage() {
        let panel = NSOpenPanel()
        panel.allowedContentTypes = [.jpeg, .png, .webP]
        panel.allowsMultipleSelection = false
        guard panel.runModal() == .OK, let url = panel.url else { return }
        do {
            let values = try url.resourceValues(forKeys: [.fileSizeKey])
            guard (values.fileSize ?? 0) <= 10 * 1024 * 1024,
                  let image = NSImage(contentsOf: url), image.size.width > 0, image.size.height > 0 else {
                throw NativeProviderError("Choose an image smaller than 10 MB.")
            }
            let scale = min(1, 1280 / max(image.size.width, image.size.height))
            let size = NSSize(width: max(1, image.size.width * scale), height: max(1, image.size.height * scale))
            let resized = NSImage(size: size)
            resized.lockFocus()
            image.draw(in: NSRect(origin: .zero, size: size))
            resized.unlockFocus()
            guard let tiff = resized.tiffRepresentation, let bitmap = NSBitmapImageRep(data: tiff),
                  let data = bitmap.representation(using: .jpeg, properties: [.compressionFactor: 0.8]) else {
                throw NativeProviderError("Could not read this image.")
            }
            try video.sendImage(data)
            record("obs.image", "Image shared", url.lastPathComponent)
        } catch { sessionErrorText = error.localizedDescription }
    }

    func toggleRecording() {
        guard #available(macOS 15.0, *) else {
            sessionErrorText = "Room recording requires macOS 15 or later. Browser recording is also available."
            return
        }
        if let recorder = roomRecorder as? RoomRecorder {
            Task { await recorder.stop() }
            return
        }
        let panel = NSSavePanel()
        panel.allowedContentTypes = [.mpeg4Movie]
        panel.nameFieldStringValue = "OpenRealtime-room.mp4"
        guard panel.runModal() == .OK, let url = panel.url else { return }
        let recorder = RoomRecorder()
        roomRecorder = recorder
        recorder.setMicrophoneEnabled(microphoneActive && !microphoneMuted)
        recorder.onStatus = { [weak self] active, error in
            self?.recording = active
            if let error { self?.sessionErrorText = error }
            if !active { self?.roomRecorder = nil }
        }
        Task {
            do { try await recorder.start(url: url) }
            catch { roomRecorder = nil; sessionErrorText = error.localizedDescription }
        }
    }

    func toggleMicrophone() {
        guard connectionState == .connected else { return }
        Task {
            do { try await media.toggleMicrophone() }
            catch { record("obs.audio", "microphone failed", error.localizedDescription, failed: true) }
        }
    }

    func toggleScreen() {
        guard connectionState == .connected else { return }
        Task {
            if screenActive {
                await video.stop("screen")
            } else {
                do {
                    try await video.startScreen(displayID: selectedDisplayID)
                    record("obs.screen", "screen", "selected display capture started")
                } catch { record("obs.screen", "screen failed", error.localizedDescription, failed: true) }
            }
        }
    }

    func toggleCamera() {
        guard connectionState == .connected else { return }
        Task {
            if cameraActive {
                await video.stop("camera")
            } else {
                do {
                    try await video.startCamera()
                    record("obs.camera", "camera", "physical camera capture started")
                } catch { record("obs.camera", "camera failed", error.localizedDescription, failed: true) }
            }
        }
    }

    func toggleBrowser() {
        guard connectionState == .connected else { return }
        Task {
            if browserActive {
                await video.stop("browser")
            } else {
                do {
                    try await video.startBrowser(cdpURL: cdpURL)
                    record("obs.browser", "browser-use", "marked browser capture started")
                } catch { record("obs.browser", "browser-use failed", error.localizedDescription, failed: true) }
            }
        }
    }

    func saveDownload(_ download: ClientDownloadReference) {
        guard let artifactService else { return }
        let panel = NSSavePanel()
        panel.nameFieldStringValue = download.filename
        guard panel.runModal() == .OK, let destination = panel.url else { return }
        Task {
            do { try await artifactService.export(download, to: destination) }
            catch { record("act.download", "save failed", error.localizedDescription, failed: true) }
        }
    }

    func openDownload(_ download: ClientDownloadReference) {
        guard let artifactService else { return }
        Task {
            do { try await artifactService.open(download) }
            catch { record("act.download", "open failed", error.localizedDescription, failed: true) }
        }
    }

    func revealDownload(_ download: ClientDownloadReference) {
        guard let artifactService else { return }
        Task {
            do { try await artifactService.reveal(download) }
            catch { record("act.download", "reveal failed", error.localizedDescription, failed: true) }
        }
    }

    func exportProtocolLog() {
        let panel = NSSavePanel()
        panel.nameFieldStringValue = "openrealtime-macos-\(Int(Date().timeIntervalSince1970)).json"
        guard panel.runModal() == .OK, let url = panel.url else { return }
        let records: [[String: Any]] = protocolRecords.map {
            [
                "timestamp_ms": Int64($0.timestamp.timeIntervalSince1970 * 1_000),
                "direction": $0.direction, "type": $0.type, "payload": $0.payload,
            ]
        }
        do {
            let data = try JSONSerialization.data(
                withJSONObject: ["events": records], options: [.prettyPrinted, .sortedKeys]
            )
            try data.write(to: url, options: .atomic)
        } catch { record("act.download", "export failed", error.localizedDescription, failed: true) }
    }

    func refreshInspection() {
        guard inspection.available else {
            inspectionChanged(nil)
            return
        }
        inspectionEpoch += 1
        let epoch = inspectionEpoch
        inspectionRefreshing = true
        inspectionStatus = "reading canonical live, delta, and trace resources"
        Task { [weak self] in
            guard let self else { return }
            var liveText = ""
            var deltaText = ""
            var traceText = ""
            var failures: [String] = []
            do {
                let document = try await self.inspection.live()
                liveText = prettyJSONString(try document.snapshot())
            }
            catch { failures.append("live: \(error.localizedDescription)") }
            do {
                deltaText = prettyJSONString(
                    try await self.inspection.deltas(after: 0, limit: 256).snapshot()
                )
            } catch { failures.append("deltas: \(error.localizedDescription)") }
            do { traceText = prettyJSONString(try await self.inspection.trace().snapshot()) }
            catch { failures.append("trace: \(error.localizedDescription)") }
            guard self.inspectionEpoch == epoch, self.inspection.available else { return }
            self.inspectionLive = liveText
            self.inspectionDeltas = deltaText
            self.inspectionTrace = traceText
            self.inspectionRefreshing = false
            self.inspectionStatus = failures.isEmpty
                ? "canonical management snapshot loaded"
                : failures.joined(separator: " · ")
        }
    }

    func authoringDocumentChanged() {
        authoringEpoch += 1
        authoringRunning = false
        authoringPresentation = nil
        authoringStatus = "document changed; analyze again"
    }

    func analyzeConfigurationContracts() {
        guard !authoringRunning else { return }
        authoringRunning = true
        authoringEpoch += 1
        let epoch = authoringEpoch
        let path = authoringPath
        let source = authoringSource
        authoringStatus = "analyzing exact in-memory source"
        do {
            _ = try authoring.replaceCapability(authoringCapability)
        } catch {
            authoringRunning = false
            authoringPresentation = nil
            authoringStatus = error.localizedDescription
            return
        }
        Task {
            do {
                let presentation = try await authoring.analyze(path: path, source: source)
                guard authoringEpoch == epoch else { return }
                authoringPresentation = presentation
                authoringStatus = "\(presentation.contracts.count) of \(presentation.total) exact contracts"
            } catch {
                guard authoringEpoch == epoch else { return }
                authoringPresentation = nil
                authoringStatus = error.localizedDescription
            }
            if authoringEpoch == epoch { authoringRunning = false }
        }
    }

    func clearAuthoringCapability() {
        authoringEpoch += 1
        _ = authoring.clearCapability()
        authoringCapability = ""
        authoringPresentation = nil
        authoringRunning = false
        authoringStatus = "operator capability cleared"
    }

    /// Hosted release-gate access to the UI-independent management client.
    /// The returned document is rebound to the same scoped capability before
    /// and after the network request; rendered presentation state is not part
    /// of this proof path.
    func hostedManagementSnapshot(
        expectedSessionID: String
    ) async throws -> SessionInspectionDocument {
        guard !expectedSessionID.isEmpty,
              let before = inspection.access,
              before.sessionID == expectedSessionID else {
            throw SessionInspectionFailure(
                "hosted management capability is unavailable or bound to another session"
            )
        }
        let document = try await inspection.live()
        let pathCharacters = CharacterSet.alphanumerics.union(
            CharacterSet(charactersIn: "-._~")
        )
        guard let encodedSession = expectedSessionID.addingPercentEncoding(
            withAllowedCharacters: pathCharacters
        ) else {
            throw SessionInspectionFailure("hosted management session is not URL-safe")
        }
        guard document.resource == .live,
              document.sessionID == expectedSessionID,
              inspection.access == before,
              let components = URLComponents(string: document.responseURL),
              components.user == nil, components.password == nil,
              components.query == nil, components.fragment == nil,
              components.percentEncodedPath.hasSuffix(
                "/sessions/\(encodedSession)/live"
              ) else {
            throw SessionInspectionFailure(
                "hosted management response is not bound to the exact active session"
            )
        }
        return document
    }

    func decideConfirmation(_ approved: Bool) {
        guard let request = pendingConfirmation, let effects else { return }
        do { try effects.decideConfirmation(id: request.id, approved: approved) }
        catch { record("obs.tools", "confirmation failed", error.localizedDescription, failed: true) }
    }

    func selectArtifact(_ reference: ClientArtifactReference) {
        selectedArtifact = reference
        loadSelectedArtifact()
    }

    private func bindProjections() {
        unbindProjections()
        do {
            subscriptions.append(try reducer.subscribe { [weak self] snapshot in self?.project(snapshot) })
            subscriptions.append(try media.subscribe { [weak self] snapshot in self?.mediaChanged(snapshot) })
            subscriptions.append(try video.subscribe { [weak self] snapshot in self?.videoChanged(snapshot) })
            if let effects {
                subscriptions.append(try effects.subscribe { [weak self] snapshot, _ in
                    self?.effectsChanged(snapshot)
                })
            }
            if let artifactService {
                subscriptions.append(try artifactService.subscribe { [weak self] snapshot in
                    self?.artifactsChanged(snapshot)
                })
            }
            subscriptions.append(try assembly.view.services.transportDiagnostics.subscribe { [weak self] snapshot in
                self?.transportChanged(snapshot)
            })
            subscriptions.append(inspection.subscribe { [weak self] access in
                Task { @MainActor in self?.inspectionChanged(access) }
            })
            reducer.onDiagnostic = { [weak self] message in
                self?.record("obs.tools", "protocol adapter", message, failed: true)
            }
        } catch {
            unbindProjections()
            record("obs.tools", "view projection", error.localizedDescription, failed: true)
        }
    }

    private func unbindProjections() {
        for unsubscribe in subscriptions.reversed() { unsubscribe() }
        subscriptions.removeAll()
        reducer.onDiagnostic = nil
    }

    private func mediaChanged(_ snapshot: NativeMediaSnapshot) {
        microphoneActive = snapshot.microphoneActive
        microphoneMuted = snapshot.microphoneMuted
        speakerMuted = snapshot.speakerMuted
        if #available(macOS 15.0, *), let recorder = roomRecorder as? RoomRecorder {
            recorder.setMicrophoneEnabled(microphoneActive && !microphoneMuted)
        }
        audioOutputStatus = snapshot.outputStatus
    }

    private func videoChanged(_ snapshot: NativeVideoSnapshot) {
        screenActive = snapshot.active.contains("screen")
        cameraActive = snapshot.active.contains("camera")
        browserActive = snapshot.active.contains("browser")
        browserCaption = snapshot.browserCaption
        frameStatus = snapshot.frameStatus
        previews = snapshot.previews
    }

    private func effectsChanged(_ snapshot: NativeEffectsSnapshot) {
        pendingConfirmation = snapshot.confirmations.first
        effectRecords = snapshot.records
        debugRecords = snapshot.debug
        let catalog = snapshot.declarationNames.isEmpty
            ? "no declarations" : "\(snapshot.declarationNames.count) declarations"
        effectsStatusText = "\(snapshot.phase) · \(catalog) · pending \(snapshot.pendingCount) · calls \(snapshot.calls)/results \(snapshot.results)/refused \(snapshot.refused)/reconnects \(snapshot.reconnects)"
        if !snapshot.diagnostic.isEmpty {
            effectsStatusText += " · \(snapshot.diagnostic)"
        }
        rebuildChannels()
    }

    private func artifactsChanged(_ snapshot: ClientArtifactsSnapshot) {
        artifacts = snapshot.artifacts
        downloads = snapshot.downloads
        artifactDiagnostic = snapshot.diagnostic
        if let selectedArtifact,
           let replacement = artifacts.first(where: { $0.id == selectedArtifact.id }) {
            if replacement != selectedArtifact {
                self.selectedArtifact = replacement
                loadSelectedArtifact()
            }
        } else {
            selectedArtifact = artifacts.last
            loadSelectedArtifact()
        }
    }

    private func transportChanged(_ snapshot: TransportDiagnosticsSnapshot) {
        transportDiagnosticsText = "\(snapshot.state) · in \(snapshot.inboundEvents) · out \(snapshot.outboundEvents) · audio \(snapshot.inputAudioBytes)/\(snapshot.outputAudioBytes) B · video \(snapshot.inputVideoBytes) B · queue \(snapshot.queuedMessages)"
    }

    private func inspectionChanged(_ access: SessionInspectionAccessProjection?) {
        inspectionEpoch += 1
        guard let access else {
            inspectionAccessText = "session inspection unavailable"
            inspectionStatus = "waiting for a scoped capability"
            inspectionLive = ""
            inspectionDeltas = ""
            inspectionTrace = ""
            inspectionRefreshing = false
            return
        }
        let expiration = Date(timeIntervalSince1970: Double(access.expiresAtMS) / 1_000)
        inspectionAccessText = "session \(access.sessionID) · expires \(expiration.formatted())"
        inspectionStatus = "reading canonical management snapshot"
        inspectionLive = ""
        inspectionDeltas = ""
        inspectionTrace = ""
        refreshInspection()
    }

    private func initialSession() -> [String: Any] {
        [
            "type": "realtime",
            "instructions": systemPrompt,
            "audio": [
                "input": ["format": ["type": "audio/pcm", "rate": 24_000]],
                "output": ["format": ["type": "audio/pcm", "rate": 24_000]],
            ],
        ]
    }

    private func loadSelectedArtifact() {
        artifactEpoch += 1
        let epoch = artifactEpoch
        guard let reference = selectedArtifact, let artifactService else {
            selectedArtifactHTML = ""
            artifactLoading = false
            return
        }
        selectedArtifactHTML = ""
        artifactLoading = true
        Task { [weak self] in
            guard let self else { return }
            do {
                let html = try await artifactService.artifactHTML(reference)
                guard self.artifactEpoch == epoch, self.selectedArtifact == reference else { return }
                self.selectedArtifactHTML = html
                self.artifactLoading = false
            } catch {
                guard self.artifactEpoch == epoch, self.selectedArtifact == reference else { return }
                self.selectedArtifactHTML = ""
                self.artifactLoading = false
                self.record("act.artifact", "view failed", error.localizedDescription, failed: true)
            }
        }
    }

    private func project(_ snapshot: [String: Any]) {
        for event in snapshot["tools"] as? [[String: Any]] ?? [] {
            if selectedScenario == "a recorded menu", event["status"] as? String == "pending",
               event["name"] as? String == "press_key", let callID = event["call_id"] as? String,
               !handledRoomCalls.contains(callID) {
                handledRoomCalls.insert(callID)
                let raw = (event["arguments"] as? String ?? "{}").data(using: .utf8) ?? Data()
                let args = (try? JSONSerialization.jsonObject(with: raw)) as? [String: Any]
                let digit = args?["digit"] as? String ?? "invalid"
                let destination = digit == "2" ? "order-status" : "other-option"
                let result = "{\"ok\":\(digit == "2" ? "true" : "false"),\"destination\":\"\(destination)\"}"
                Task { @MainActor [weak self] in
                    self?.reducer.toolResult(callID: callID, status: "done", output: result)
                }
            }

        }
        let connection = snapshot["connection"] as? [String: Any] ?? [:]
        let phase = connection["phase"] as? String ?? "disconnected"
        let reason = connection["reason"] as? String ?? ""
        let attempt = (connection["attempt"] as? NSNumber)?.intValue ?? 0
        let session = snapshot["session"] as? [String: Any] ?? [:]
        sessionID = (session["id"] as? String) ?? ""
        let response = snapshot["response"] as? [String: Any] ?? [:]
        switch phase {
        case "connecting":
            connectionState = .connecting
            statusText = "connecting"
        case "reconnecting":
            connectionState = .reconnecting
            statusText = "reconnecting · attempt \(attempt)"
        case "connected":
            connectionState = .connected
            if response["open"] as? Bool == true {
                statusText = "responding · \((response["status"] as? String) ?? "in progress")"
            } else if let id = session["id"] as? String, !id.isEmpty {
                statusText = "connected · \(id)"
            } else {
                statusText = "connected"
            }
        case "failed":
            connectionState = .failed
            statusText = reason.isEmpty ? ((snapshot["last_error"] as? String) ?? "connection failed") : reason
        default:
            connectionState = .disconnected
            statusText = reason.isEmpty ? "not connected" : reason
        }

        // The reducer records every server `error` event, not only the ones
        // that end the connection. Reading it in the failed phase alone left a
        // refused event or a provider failure with no visible trace outside the
        // raw protocol log.
        sessionErrorText = String(((snapshot["last_error"] as? String) ?? "").prefix(1_024))

        if let extensionObject = session["openrealtime"] as? [String: Any],
           extensionObject["present"] as? Bool == true {
            let enabled = (extensionObject["enabled"] as? [String]) ?? []
            let observers = (extensionObject["observers"] as? [String]) ?? []
            negotiationText = "\(enabled.joined(separator: ", ")) · observers \(observers.joined(separator: ", ")) · debug \(extensionObject["debug_enabled"] as? Bool == true ? "on" : "off")"
        } else {
            negotiationText = "base Realtime only · no OpenRealtime extensions"
        }
        projectConversation(snapshot)
    }

    private func projectConversation(_ snapshot: [String: Any]) {
        let items = snapshot["conversation"] as? [[String: Any]] ?? []
        conversation = items.compactMap { item in
            guard let role = item["role"] as? String,
                  let channel = item["channel"] as? String,
                  let text = item["text"] as? String else { return nil }
            if role == "observation" && (text == "The user attached an image." || text.hasPrefix("Current shared ")) { return nil }
            if role == "observation" && items.contains(where: { $0["role"] as? String == "user" && $0["text"] as? String == text }) { return nil }
            let presentationChannel: String
            let title: String
            switch (role, channel) {
            case ("user", "input_text"):
                presentationChannel = "obs.text"; title = "you · typed"
            case ("user", "input_audio_transcript"):
                presentationChannel = "obs.audio"; title = "you · speech"
            case ("assistant", "output_audio_transcript"):
                presentationChannel = "act.speech"; title = "assistant · speech"
            case ("assistant", "output_text"):
                presentationChannel = "act.text"; title = "assistant · text"
            default:
                presentationChannel = channel.hasPrefix("observation.")
                    ? "obs." + String(channel.dropFirst("observation.".count)) : channel
                title = role == "observation"
                    ? "observed · \(presentationChannel)" : "\(role) · \(channel)"
            }
            return ChannelRecord(
                channel: presentationChannel, title: title, body: text, failed: false
            )
        }
        rebuildChannels()
    }

    private func record(_ channel: String, _ title: String, _ body: String, failed: Bool = false) {
        auxiliaryRecords.append(ChannelRecord(
            channel: channel, title: title,
            body: String(body.prefix(64 << 10)), failed: failed
        ))
        if auxiliaryRecords.count > 2_000 {
            auxiliaryRecords.removeFirst(auxiliaryRecords.count - 2_000)
        }
        rebuildChannels()
    }

    private func rebuildChannels() {
        channelRecords = conversation + auxiliaryRecords + effectRecords
    }

    private func recordProtocol(_ direction: String, _ event: [String: Any]) {
        protocolRecords.append(ProtocolRecord(
            direction: direction, type: (event["type"] as? String) ?? "",
            payload: inspection.protocolPayload(event)
        ))
        if protocolRecords.count > 2_000 {
            protocolRecords.removeFirst(protocolRecords.count - 2_000)
        }
    }
}
