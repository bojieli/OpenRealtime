import Foundation
#if canImport(FoundationNetworking)
@preconcurrency import FoundationNetworking
#endif
import OpenRealtimeClientCore

private final class RealtimeWebSocketSessionDelegate: NSObject,
    URLSessionWebSocketDelegate, @unchecked Sendable {
    private let lock = NSLock()
    private var task: URLSessionWebSocketTask?
    private var outcome: Result<Void, Error>?
    private var continuation: CheckedContinuation<Void, Error>?
    private var timeoutWorkItem: DispatchWorkItem?

    func waitUntilOpen(
        _ webSocket: URLSessionWebSocketTask, timeout: TimeInterval
    ) async throws {
        try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation {
                (next: CheckedContinuation<Void, Error>) in
                register(next, for: webSocket, timeout: timeout)
            }
        } onCancel: {
            finish(
                webSocket,
                with: .failure(CancellationError())
            )
        }
    }

    func cancel(_ webSocket: URLSessionWebSocketTask, reason: String) {
        finish(webSocket, with: .failure(ClientError(reason)))
    }

    func urlSession(
        _ session: URLSession,
        webSocketTask: URLSessionWebSocketTask,
        didOpenWithProtocol protocol: String?
    ) {
        finish(webSocketTask, with: .success(()))
    }

    func urlSession(
        _ session: URLSession,
        webSocketTask: URLSessionWebSocketTask,
        didCloseWith closeCode: URLSessionWebSocketTask.CloseCode,
        reason: Data?
    ) {
        finish(
            webSocketTask,
            with: .failure(
                ClientError("WebSocket closed before opening (code \(closeCode.rawValue))")
            )
        )
    }

    func urlSession(
        _ session: URLSession,
        task: URLSessionTask,
        didCompleteWithError error: Error?
    ) {
        guard let webSocket = task as? URLSessionWebSocketTask, let error else { return }
        finish(webSocket, with: .failure(error))
    }

    private func register(
        _ next: CheckedContinuation<Void, Error>,
        for webSocket: URLSessionWebSocketTask,
        timeout: TimeInterval
    ) {
        var immediate: Result<Void, Error>?
        lock.lock()
        if let task, task !== webSocket {
            immediate = .failure(ClientError("WebSocket open delegate was rebound"))
        } else if continuation != nil {
            immediate = .failure(ClientError("WebSocket open wait was registered twice"))
        } else {
            task = webSocket
            if let outcome {
                immediate = outcome
            } else {
                continuation = next
                let workItem = DispatchWorkItem { [weak self, weak webSocket] in
                    guard let self, let webSocket else { return }
                    self.finish(
                        webSocket,
                        with: .failure(
                            ClientError("WebSocket opening handshake timed out")
                        )
                    )
                }
                timeoutWorkItem = workItem
                DispatchQueue.global(qos: .utility).asyncAfter(
                    deadline: .now() + timeout, execute: workItem
                )
            }
        }
        lock.unlock()
        if let immediate { next.resume(with: immediate) }
    }

    private func finish(
        _ webSocket: URLSessionWebSocketTask,
        with result: Result<Void, Error>
    ) {
        var waiter: CheckedContinuation<Void, Error>?
        lock.lock()
        if task == nil { task = webSocket }
        if task === webSocket, outcome == nil {
            outcome = result
            waiter = continuation
            continuation = nil
            timeoutWorkItem?.cancel()
            timeoutWorkItem = nil
        }
        lock.unlock()
        waiter?.resume(with: result)
    }
}

/// The WebSocket transport: every event, audio included, travels as a protocol
/// message on one socket.
@MainActor
final class RealtimeClient: RealtimeTransport {
    var onEvent: (([String: Any]) -> Void)?
    var onState: ((ConnectionState, String) -> Void)?
    var onProtocol: ((String, [String: Any]) -> Void)?

    private var session: URLSession?
    private var sessionDelegate: RealtimeWebSocketSessionDelegate?
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
        let delegate = RealtimeWebSocketSessionDelegate()
        let urlSession = URLSession(
            configuration: configuration, delegate: delegate, delegateQueue: nil
        )
        let webSocket = urlSession.webSocketTask(with: request)
        session = urlSession
        sessionDelegate = delegate
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
            try await delegate.waitUntilOpen(webSocket, timeout: 20)
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
        if let socket {
            sessionDelegate?.cancel(socket, reason: "WebSocket opening handshake cancelled")
            socket.cancel(with: .normalClosure, reason: nil)
        }
        socket = nil
        session?.invalidateAndCancel()
        session = nil
        sessionDelegate = nil
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
