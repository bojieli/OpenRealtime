import Foundation
import LiveKitWebRTC
import OpenRealtimeClientCore

/// The WebRTC transport: audio travels as negotiated media, every other
/// protocol event travels on the `oai-events` data channel.
///
/// The exchange is one-shot rather than trickle. The adapter answers a single
/// HTTP POST with a complete SDP answer and has nowhere to send a later
/// candidate, so the offer is only sent once ICE gathering has finished and
/// the local description already carries every candidate.
@MainActor
final class WebRTCTransport: NSObject, RealtimeTransport {
    /// The data channel a Realtime peer expects. It is the adapter's name and
    /// changing it silently produces a session that connects and never speaks.
    static let eventChannel = "oai-events"

    var onEvent: (([String: Any]) -> Void)?
    var onState: ((ConnectionState, String) -> Void)?
    var onProtocol: ((String, [String: Any]) -> Void)?

    /// WebRTC owns capture and playout through its own audio device module,
    /// so the media provider must not also open the microphone.
    var carriesAudioAsMedia: Bool { true }

    private static let factory: LKRTCPeerConnectionFactory = {
        LKRTCPeerConnectionFactory(
            encoderFactory: LKRTCDefaultVideoEncoderFactory(),
            decoderFactory: LKRTCDefaultVideoDecoderFactory()
        )
    }()

    private let strictJSON: StrictJSONService
    private let endpoint: URL
    private let iceServers: [LKRTCIceServer]
    private var connection: LKRTCPeerConnection?
    private var channel: LKRTCDataChannel?
    private var localAudio: LKRTCAudioTrack?
    private var speakerMuted = false
    private var diagnostics: TransportDiagnosticsPublisher?
    private var gathering: CheckedContinuation<Void, Error>?
    private var queuedMessages = 0
    private var microphoneEnabled = false

    init(
        strictJSON: StrictJSONService,
        endpoint: String,
        iceServers: [String] = []
    ) throws {
        guard let url = URL(string: endpoint),
              ["http", "https"].contains(url.scheme ?? ""),
              url.host?.isEmpty == false,
              url.user == nil, url.password == nil,
              url.query == nil, url.fragment == nil,
              url.absoluteString == endpoint else {
            throw WebRTCTransportError(
                "the declared WebRTC endpoint must be an exact credential-free HTTP URL"
            )
        }
        self.strictJSON = strictJSON
        self.endpoint = url
        self.iceServers = iceServers.isEmpty ? [] : [LKRTCIceServer(urlStrings: iceServers)]
        super.init()
    }

    func bindDiagnostics(_ publisher: TransportDiagnosticsPublisher) throws {
        guard diagnostics == nil else {
            throw WebRTCTransportError("transport diagnostics provider is already bound")
        }
        diagnostics = publisher
    }

    func unbindDiagnostics(_ publisher: TransportDiagnosticsPublisher) {
        guard diagnostics === publisher else { return }
        diagnostics = nil
    }

    var transportKind: String { "webrtc" }

    var connected: Bool { connection != nil }

    /// Enables or disables the outbound microphone track.
    ///
    /// The track stays attached either way. Removing it would renegotiate,
    /// and this transport has no channel to renegotiate over.
    func setMicrophone(enabled: Bool) {
        microphoneEnabled = enabled
        localAudio?.isEnabled = enabled
    }

    var microphoneActive: Bool { microphoneEnabled }

    func setSpeakerMuted(_ muted: Bool) {
        speakerMuted = muted
        for receiver in connection?.receivers ?? [] {
            (receiver.track as? LKRTCAudioTrack)?.isEnabled = !muted
        }
    }

    func connect(token: String) async throws {
        tearDown(notify: false, reason: "superseded")
        diagnostics?.updateState("connecting")
        onState?(.connecting, "connecting")
        do {
            try await establish(token: token)
        } catch {
            tearDown(notify: false, reason: "connection failed")
            diagnostics?.updateState("failed")
            onState?(.failed, "connection failed: \(error.localizedDescription)")
            throw error
        }
        diagnostics?.updateState("connected")
        onState?(.connected, "connected")
    }

    private func establish(token: String) async throws {
        let configuration = LKRTCConfiguration()
        configuration.iceServers = iceServers
        configuration.sdpSemantics = .unifiedPlan
        // The adapter answers once and cannot receive a later candidate, so
        // the whole candidate set has to be in the offer.
        configuration.continualGatheringPolicy = .gatherOnce
        let constraints = LKRTCMediaConstraints(
            mandatoryConstraints: nil, optionalConstraints: nil
        )
        guard let peer = Self.factory.peerConnection(
            with: configuration, constraints: constraints, delegate: self
        ) else {
            throw WebRTCTransportError("the WebRTC peer connection could not be created")
        }
        connection = peer

        // Audio is bidirectional from the start: the session's whole point is
        // speech in both directions, and this transport has no way to add a
        // track later without renegotiating.
        let source = Self.factory.audioSource(with: constraints)
        let track = Self.factory.audioTrack(with: source, trackId: "openrealtime-microphone")
        track.isEnabled = microphoneEnabled
        peer.add(track, streamIds: ["openrealtime"])
        localAudio = track

        let channelConfiguration = LKRTCDataChannelConfiguration()
        channelConfiguration.isOrdered = true
        guard let events = peer.dataChannel(
            forLabel: Self.eventChannel, configuration: channelConfiguration
        ) else {
            throw WebRTCTransportError("the \(Self.eventChannel) data channel could not be created")
        }
        events.delegate = self
        channel = events

        let offer = try await peer.offer(for: constraints)
        try await peer.setLocalDescription(offer)
        try await waitForGathering(peer)
        guard let complete = peer.localDescription else {
            throw WebRTCTransportError("the local description was lost during ICE gathering")
        }
        let answer = try await exchange(offer: complete.sdp, token: token)
        try await peer.setRemoteDescription(
            LKRTCSessionDescription(type: .answer, sdp: answer)
        )
    }

    /// Waits for ICE gathering to finish, bounded, so a network that never
    /// completes gathering fails as a connection error rather than hanging.
    private func waitForGathering(_ peer: LKRTCPeerConnection) async throws {
        if peer.iceGatheringState == .complete { return }
        let timeout = Task { [weak self] in
            try? await Task.sleep(nanoseconds: 10_000_000_000)
            guard !Task.isCancelled else { return }
            await MainActor.run { self?.finishGathering(.failure(
                WebRTCTransportError("ICE gathering did not complete")
            )) }
        }
        defer { timeout.cancel() }
        try await withCheckedThrowingContinuation { continuation in
            gathering = continuation
        }
    }

    fileprivate func finishGathering(_ result: Result<Void, Error>) {
        guard let continuation = gathering else { return }
        gathering = nil
        continuation.resume(with: result)
    }

    /// Posts the offer and reads the answer.
    ///
    /// Redirect-free and credential-free by construction: the bearer token is
    /// a header on exactly this request and never reaches a URL.
    private func exchange(offer: String, token: String) async throws -> String {
        var request = URLRequest(url: endpoint)
        request.httpMethod = "POST"
        request.timeoutInterval = 20
        request.setValue("application/sdp", forHTTPHeaderField: "Content-Type")
        request.setValue("application/sdp", forHTTPHeaderField: "Accept")
        let trimmed = token.trimmingCharacters(in: .whitespacesAndNewlines)
        if !trimmed.isEmpty {
            request.setValue("Bearer \(trimmed)", forHTTPHeaderField: "Authorization")
        }
        request.httpBody = Data(offer.utf8)

        let configuration = URLSessionConfiguration.ephemeral
        configuration.httpCookieStorage = nil
        configuration.urlCredentialStorage = nil
        configuration.httpShouldSetCookies = false
        let session = URLSession(configuration: configuration)
        defer { session.invalidateAndCancel() }
        let (data, response) = try await session.data(for: request)
        guard let http = response as? HTTPURLResponse else {
            throw WebRTCTransportError("the WebRTC endpoint did not answer with HTTP")
        }
        guard (200...299).contains(http.statusCode) else {
            throw WebRTCTransportError(
                "the WebRTC endpoint refused the offer with status \(http.statusCode)"
            )
        }
        guard data.count <= 1 << 20, let answer = String(data: data, encoding: .utf8),
              answer.hasPrefix("v=") else {
            throw WebRTCTransportError("the WebRTC endpoint did not answer with an SDP answer")
        }
        return answer
    }

    func disconnect(reason: String = "disconnected") {
        tearDown(notify: true, reason: reason)
    }

    private func tearDown(notify: Bool, reason: String) {
        finishGathering(.failure(WebRTCTransportError("the transport was torn down")))
        channel?.delegate = nil
        channel?.close()
        channel = nil
        localAudio = nil
        connection?.delegate = nil
        connection?.close()
        connection = nil
        queuedMessages = 0
        diagnostics?.updateQueue(0)
        diagnostics?.updateState("disconnected")
        if notify { onState?(.disconnected, reason) }
    }

    func send(_ event: [String: Any]) {
        guard let channel, channel.readyState == .open else { return }
        guard JSONSerialization.isValidJSONObject(event),
              let data = try? JSONSerialization.data(withJSONObject: event) else {
            fail("client attempted to send invalid JSON")
            return
        }
        onProtocol?("OUT", event)
        queuedMessages += 1
        diagnostics?.recordOutbound(event, queuedMessages: queuedMessages)
        channel.sendData(LKRTCDataBuffer(data: data, isBinary: false))
        queuedMessages = max(0, queuedMessages - 1)
        diagnostics?.updateQueue(queuedMessages)
    }

    /// Microphone audio is carried as RTP by this transport, so a protocol
    /// audio append would be a second copy of the same speech.
    func sendAudio(_ pcm16LE: Data) {}

    func sendVideo(source: String, data: Data, timestampMS: Int64) {
        send([
            "type": "openrealtime.input_video_frame.append",
            "source": source,
            "frame": data.base64EncodedString(),
            "timestamp_ms": timestampMS,
        ])
    }

    func updateVideoSource(_ source: String, state: String, width: Int = 0, height: Int = 0) {
        send([
            "type": "openrealtime.input_video_source.update",
            "source": source,
            "state": state,
            "width": width,
            "height": height,
        ])
    }

    fileprivate func receive(_ data: Data) {
        guard let object = try? strictJSON.parse(data) else { return }
        diagnostics?.recordInbound(object)
        onProtocol?("IN", object)
        onEvent?(object)
    }

    fileprivate func fail(_ message: String) {
        diagnostics?.updateState("failed")
        onState?(.failed, message)
        tearDown(notify: false, reason: message)
    }
}

extension WebRTCTransport: LKRTCPeerConnectionDelegate {
    nonisolated func peerConnection(
        _ peerConnection: LKRTCPeerConnection,
        didChange newState: LKRTCIceGatheringState
    ) {
        guard newState == .complete else { return }
        Task { @MainActor [weak self] in self?.finishGathering(.success(())) }
    }

    nonisolated func peerConnection(
        _ peerConnection: LKRTCPeerConnection,
        didChange newState: LKRTCPeerConnectionState
    ) {
        guard newState == .failed || newState == .closed else { return }
        Task { @MainActor [weak self] in
            guard let self, self.connection === peerConnection else { return }
            self.fail("the WebRTC connection was lost")
        }
    }

    nonisolated func peerConnection(
        _ peerConnection: LKRTCPeerConnection,
        didOpen dataChannel: LKRTCDataChannel
    ) {
        Task { @MainActor [weak self] in
            guard let self, dataChannel.label == Self.eventChannel else { return }
            dataChannel.delegate = self
            self.channel = dataChannel
        }
    }

    nonisolated func peerConnectionShouldNegotiate(_ peerConnection: LKRTCPeerConnection) {}
    nonisolated func peerConnection(
        _ peerConnection: LKRTCPeerConnection, didChange stateChanged: LKRTCSignalingState
    ) {}
    nonisolated func peerConnection(
        _ peerConnection: LKRTCPeerConnection, didAdd stream: LKRTCMediaStream
    ) {
        Task { @MainActor [weak self] in
            guard let self else { return }
            for track in stream.audioTracks { track.isEnabled = !self.speakerMuted }
        }
    }
    nonisolated func peerConnection(
        _ peerConnection: LKRTCPeerConnection, didRemove stream: LKRTCMediaStream
    ) {}
    nonisolated func peerConnection(
        _ peerConnection: LKRTCPeerConnection, didChange newState: LKRTCIceConnectionState
    ) {}
    nonisolated func peerConnection(
        _ peerConnection: LKRTCPeerConnection, didGenerate candidate: LKRTCIceCandidate
    ) {}
    nonisolated func peerConnection(
        _ peerConnection: LKRTCPeerConnection, didRemove candidates: [LKRTCIceCandidate]
    ) {}
}

extension WebRTCTransport: LKRTCDataChannelDelegate {
    nonisolated func dataChannel(
        _ dataChannel: LKRTCDataChannel, didReceiveMessageWith buffer: LKRTCDataBuffer
    ) {
        let data = buffer.data
        Task { @MainActor [weak self] in self?.receive(data) }
    }

    nonisolated func dataChannelDidChangeState(_ dataChannel: LKRTCDataChannel) {}
}

struct WebRTCTransportError: LocalizedError {
    let message: String
    init(_ message: String) { self.message = message }
    var errorDescription: String? { message }
}
