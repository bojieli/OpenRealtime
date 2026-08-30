import Foundation
#if canImport(FoundationNetworking)
@preconcurrency import FoundationNetworking
#endif
import OpenRealtimeClientCore

@MainActor
final class RealtimeClient {
    var onEvent: (([String: Any]) -> Void)?
    var onState: ((ConnectionState, String) -> Void)?
    var onProtocol: ((String, [String: Any]) -> Void)?

    private var session: URLSession?
    private var socket: URLSessionWebSocketTask?
    private var receiveTask: Task<Void, Never>?
    private var writerTask: Task<Void, Never>?
    private var sendContinuation: AsyncStream<String>.Continuation?
    private let strictJSON: StrictJSONService
    private let endpoint: URL
    private var diagnostics: TransportDiagnosticsPublisher?
    private var queuedMessages = 0

    init(strictJSON: StrictJSONService, endpoint: String) throws {
        guard let url = URL(string: endpoint),
              ["ws", "wss"].contains(url.scheme ?? ""),
              url.host?.isEmpty == false,
              url.user == nil, url.password == nil,
              url.query == nil, url.fragment == nil,
              url.absoluteString == endpoint else {
            throw ClientError("the declared realtime endpoint must be an exact credential-free WebSocket URL")
        }
        self.strictJSON = strictJSON
        self.endpoint = url
    }

    func bindDiagnostics(_ publisher: TransportDiagnosticsPublisher) throws {
        guard diagnostics == nil else {
            throw ClientError("transport diagnostics provider is already bound")
        }
        diagnostics = publisher
    }

    func unbindDiagnostics(_ publisher: TransportDiagnosticsPublisher) {
        guard diagnostics === publisher else { return }
        diagnostics = nil
    }

    var connected: Bool { socket != nil }

    func connect(token: String) async throws {
        tearDown(notify: false, reason: "superseded")

        diagnostics?.updateState("connecting")
        onState?(.connecting, "connecting")
        var request = URLRequest(url: endpoint)
        request.timeoutInterval = 20
        if !token.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
            request.setValue("Bearer \(token.trimmingCharacters(in: .whitespacesAndNewlines))",
                             forHTTPHeaderField: "Authorization")
        }

        let configuration = URLSessionConfiguration.ephemeral
        configuration.httpCookieStorage = nil
        configuration.urlCredentialStorage = nil
        configuration.httpShouldSetCookies = false
        configuration.requestCachePolicy = .reloadIgnoringLocalAndRemoteCacheData
        let urlSession = URLSession(configuration: configuration)
        let webSocket = urlSession.webSocketTask(with: request)
        session = urlSession
        socket = webSocket

        var continuation: AsyncStream<String>.Continuation?
        let stream = AsyncStream<String> { continuation = $0 }
        sendContinuation = continuation
        writerTask = Task { [weak self, weak webSocket] in
            do {
                for await payload in stream {
                    guard let webSocket else { return }
                    try await webSocket.send(.string(payload))
                    self?.sentQueuedMessage()
                }
            } catch {
                self?.fail("send failed: \(error.localizedDescription)")
            }
        }

        webSocket.resume()
        do {
            try await waitForOpen(webSocket)
        } catch {
            tearDown(notify: false, reason: "connection failed")
            diagnostics?.updateState("failed")
            onState?(.failed, "connection failed: \(error.localizedDescription)")
            throw error
        }
        receiveTask = Task { [weak self] in await self?.receiveLoop(webSocket) }
        diagnostics?.updateState("connected")
        onState?(.connected, "connected")
    }

    func disconnect(reason: String = "disconnected") {
        tearDown(notify: true, reason: reason)
    }

    private func tearDown(notify: Bool, reason: String) {
        receiveTask?.cancel()
        receiveTask = nil
        sendContinuation?.finish()
        sendContinuation = nil
        writerTask?.cancel()
        writerTask = nil
        socket?.cancel(with: .normalClosure, reason: nil)
        socket = nil
        session?.invalidateAndCancel()
        session = nil
        queuedMessages = 0
        diagnostics?.updateQueue(0)
        diagnostics?.updateState("disconnected")
        if notify { onState?(.disconnected, reason) }
    }

    func send(_ event: [String: Any]) {
        guard socket != nil else { return }
        guard JSONSerialization.isValidJSONObject(event),
              let data = try? JSONSerialization.data(withJSONObject: event),
              let text = String(data: data, encoding: .utf8) else {
            fail("client attempted to send invalid JSON")
            return
        }
        onProtocol?("OUT", event)
        queuedMessages += 1
        diagnostics?.recordOutbound(event, queuedMessages: queuedMessages)
        sendContinuation?.yield(text)
    }

    func sendAudio(_ pcm16LE: Data) {
        send(["type": "input_audio_buffer.append", "audio": pcm16LE.base64EncodedString()])
    }

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

    private func receiveLoop(_ webSocket: URLSessionWebSocketTask) async {
        do {
            while !Task.isCancelled {
                let message = try await webSocket.receive()
                let data: Data
                switch message {
                case .string(let text): data = Data(text.utf8)
                case .data(let binary): data = binary
                @unknown default: continue
                }
                let object = try strictJSON.parse(data)
                diagnostics?.recordInbound(object)
                onProtocol?("IN", object)
                onEvent?(object)
            }
        } catch is CancellationError {
            return
        } catch {
            guard socket === webSocket else { return }
            fail("connection closed: \(error.localizedDescription)")
        }
    }

    private nonisolated func waitForOpen(_ webSocket: URLSessionWebSocketTask) async throws {
        try await withThrowingTaskGroup(of: Void.self) { group in
            group.addTask { try await webSocket.sendPing() }
            group.addTask {
                try await Task.sleep(nanoseconds: 20_000_000_000)
                throw ClientError("WebSocket opening handshake timed out")
            }
            defer { group.cancelAll() }
            _ = try await group.next()
        }
    }

    private func fail(_ message: String) {
        diagnostics?.updateState("failed")
        onState?(.failed, message)
        receiveTask?.cancel()
        receiveTask = nil
        sendContinuation?.finish()
        sendContinuation = nil
        writerTask?.cancel()
        writerTask = nil
        socket?.cancel(with: .goingAway, reason: nil)
        socket = nil
        session?.invalidateAndCancel()
        session = nil
        queuedMessages = 0
        diagnostics?.updateQueue(0)
    }

    private func sentQueuedMessage() {
        queuedMessages = max(0, queuedMessages - 1)
        diagnostics?.updateQueue(queuedMessages)
    }
}

private struct ClientError: LocalizedError {
    let message: String
    init(_ message: String) { self.message = message }
    var errorDescription: String? { message }
}
