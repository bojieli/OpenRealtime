import Foundation

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

    var connected: Bool { socket != nil }

    func connect(endpoint: String, token: String, sessionConfiguration: [String: Any]) async throws {
        disconnect()
        guard let url = URL(string: endpoint), ["ws", "wss"].contains(url.scheme?.lowercased() ?? "") else {
            throw ClientError("the endpoint must be a ws:// or wss:// URL")
        }

        onState?(.connecting, "connecting")
        var request = URLRequest(url: url)
        request.timeoutInterval = 20
        if !token.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
            request.setValue("Bearer \(token.trimmingCharacters(in: .whitespacesAndNewlines))",
                             forHTTPHeaderField: "Authorization")
        }

        let urlSession = URLSession(configuration: .default)
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
                }
            } catch {
                self?.fail("send failed: \(error.localizedDescription)")
            }
        }

        webSocket.resume()
        receiveTask = Task { [weak self] in await self?.receiveLoop(webSocket) }
        onState?(.connected, "connected")
        send(["type": "session.update", "session": sessionConfiguration])
    }

    func disconnect(reason: String = "disconnected") {
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
        onState?(.disconnected, reason)
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

    func sendUserText(_ text: String) {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return }
        send([
            "type": "conversation.item.create",
            "item": [
                "type": "message", "role": "user",
                "content": [["type": "input_text", "text": trimmed]],
            ],
        ])
        send(["type": "response.create"])
    }

    func answerTool(callID: String, output: String) {
        send([
            "type": "conversation.item.create",
            "item": ["type": "function_call_output", "call_id": callID, "output": output],
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
                guard let object = try JSONSerialization.jsonObject(with: data) as? [String: Any] else {
                    fail("server sent a non-object JSON message")
                    continue
                }
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
    }
}

private struct ClientError: LocalizedError {
    let message: String
    init(_ message: String) { self.message = message }
    var errorDescription: String? { message }
}
