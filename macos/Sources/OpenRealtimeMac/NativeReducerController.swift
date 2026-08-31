import Foundation
import OpenRealtimeClientCore

@MainActor
final class NativeReducerController {
    var onSnapshot: (([String: Any]) -> Void)?
    var onDiagnostic: ((String) -> Void)?

    private struct ConnectionConfiguration {
        let token: String
        let session: [String: Any]
    }

    private let transport: any RealtimeTransport
    private let reducer: RealtimeReducerService
    private weak var inspectionAccess: SessionInspectionAccessService?
    private weak var inspectionClient: SessionInspectionClient?
    private weak var protocolEvents: ValidatedProtocolEventPublisher?
    private weak var sessionConfiguration: SessionConfigurationService?
    private let epoch = ProcessInfo.processInfo.systemUptime
    private var commandCursor = 0
    private var localItem = 0
    private var reconnectTask: Task<Void, Never>?
    private var configuration: ConnectionConfiguration?
    private var intentionalDisconnect = false
    private var listeners: [UUID: ([String: Any]) -> Void] = [:]

    init(
        transport: any RealtimeTransport,
        reducer: RealtimeReducerService = RealtimeReducerService()
    ) {
        self.transport = transport
        self.reducer = reducer
        transport.onState = { [weak self] state, message in self?.transportChanged(state, message: message) }
        transport.onEvent = { [weak self] event in self?.receive(event) }
    }

    func bindProtocolEvents(_ publisher: ValidatedProtocolEventPublisher) throws {
        guard protocolEvents == nil else {
            throw NativeReducerError("native protocol event provider is already bound")
        }
        protocolEvents = publisher
    }

    func unbindProtocolEvents(_ publisher: ValidatedProtocolEventPublisher) {
        guard protocolEvents === publisher else { return }
        protocolEvents = nil
    }

    func bindInspection(
        access: SessionInspectionAccessService, client: SessionInspectionClient
    ) throws {
        guard inspectionAccess == nil, inspectionClient == nil else {
            throw NativeReducerError("native inspection provider is already bound")
        }
        inspectionAccess = access
        inspectionClient = client
    }

    func unbindInspection(
        access: SessionInspectionAccessService, client: SessionInspectionClient
    ) {
        guard inspectionAccess === access, inspectionClient === client else { return }
        inspectionAccess = nil
        inspectionClient = nil
    }

    func bindSessionConfiguration(_ service: SessionConfigurationService) throws {
        guard sessionConfiguration == nil else {
            throw NativeReducerError("native session configuration provider is already bound")
        }
        sessionConfiguration = service
    }

    func unbindSessionConfiguration(_ service: SessionConfigurationService) {
        guard sessionConfiguration === service else { return }
        sessionConfiguration = nil
    }

    func observeProtocol(_ observer: ((String, [String: Any]) -> Void)?) {
        transport.onProtocol = observer
    }

    func publishCurrent() {
        do { publish(try reducer.snapshot()) }
        catch { report(error) }
    }

    @discardableResult
    func subscribe(_ listener: @escaping ([String: Any]) -> Void) throws -> () -> Void {
        guard listeners.count < 128 else { throw NativeReducerError("native reducer listener limit reached") }
        let id = UUID()
        listeners[id] = listener
        listener(try reducer.snapshot())
        return { [weak self] in self?.listeners.removeValue(forKey: id) }
    }

    func connect(token: String, session: [String: Any]) async throws {
        let phase = currentPhase()
        guard phase == "disconnected" || phase == "failed" else { return }
        reconnectTask?.cancel()
        configuration = ConnectionConfiguration(token: token, session: session)
        try apply(["kind": "connect", "transport": transport.transportKind])
        do {
            try await transport.connect(token: token)
            try apply(["kind": "connected"])
            try apply(["kind": "session_update", "session": session])
        } catch {
            recordTransportLoss(error.localizedDescription)
            throw error
        }
    }

    func disconnect(reason: String = "disconnected") {
        reconnectTask?.cancel()
        reconnectTask = nil
        configuration = nil
        inspectionAccess?.clear()
        if currentPhase() != "disconnected" {
            do { try apply(["kind": "disconnect", "reason": reason]) }
            catch { report(error) }
        }
        intentionalDisconnect = true
        transport.disconnect(reason: reason)
        intentionalDisconnect = false
    }

    func suspend(reason: String) {
        disconnect(reason: reason)
        listeners.removeAll()
        onSnapshot = nil
        onDiagnostic = nil
    }

    func sessionUpdate(_ session: [String: Any]) {
        do { try sessionUpdateThrowing(session) }
        catch { report(error) }
    }

    func sessionUpdateThrowing(_ session: [String: Any]) throws {
        try apply(["kind": "session_update", "session": session])
    }

    func sendText(_ text: String) {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return }
        localItem += 1
        do {
            try apply(["kind": "typed_text", "item_id": "native-\(localItem)", "text": trimmed])
        } catch { report(error) }
    }

    func endTurn() {
        do { try apply(["kind": "end_turn"]) }
        catch { report(error) }
    }

    func cancelResponse() {
        do { try apply(["kind": "cancel_response"]) }
        catch { report(error) }
    }

    func setPlayout(itemID: String = "", playedMS: Int = 0, speaking: Bool) {
        do {
            try apply([
                "kind": "playout", "speaking": speaking,
                "item_id": speaking ? itemID : "", "played_ms": speaking ? max(0, playedMS) : 0,
            ])
        } catch { report(error) }
    }

    func toolResult(callID: String, status: String, output: String = "", error: String = "") {
        do {
            try apply([
                "kind": "tool_result", "call_id": callID, "status": status,
                "output": output, "error": error,
            ])
        } catch { report(error) }
    }

    private func receive(_ event: [String: Any]) {
        do {
            try inspectionAccess?.validate(event: event)
            try apply(["kind": "inbound", "event": event])
            try inspectionAccess?.capture(event: event)
            try protocolEvents?.publish(event)
        } catch { report(error) }
    }

    private func transportChanged(_ state: ConnectionState, message: String) {
        switch state {
        case .failed:
            recordTransportLoss(message)
        case .disconnected where !intentionalDisconnect:
            recordTransportLoss(message)
        case .disconnected, .connecting, .connected, .reconnecting:
            break
        }
    }

    private func recordTransportLoss(_ reason: String) {
        inspectionAccess?.clear()
        let phase = currentPhase()
        if phase == "connected" || phase == "connecting" {
            do { try apply(["kind": "transport_lost", "reason": reason.isEmpty ? "transport lost" : reason]) }
            catch { report(error); return }
        }
        scheduleReconnect()
    }

    private func scheduleReconnect() {
        reconnectTask?.cancel()
        guard currentPhase() == "reconnecting",
              let snapshot = try? reducer.snapshot(),
              let connection = snapshot["connection"] as? [String: Any],
              let deadline = (connection["next_retry_ms"] as? NSNumber)?.int64Value else { return }
        guard deadline <= RealtimeReducerService.maxVirtualTimeMS else {
            disconnect(reason: "client reducer session lifetime exhausted")
            return
        }
        let delay = max(0, deadline - nowMS())
        reconnectTask = Task { [weak self] in
            do { try await Task.sleep(nanoseconds: UInt64(delay) * 1_000_000) }
            catch { return }
            guard let self else { return }
            await self.retry(atMS: deadline)
        }
    }

    private func retry(atMS deadline: Int64) async {
        guard currentPhase() == "reconnecting", let configuration else { return }
        do {
            try apply(["kind": "retry"], atMS: max(deadline, nowMS()))
            try await transport.connect(token: configuration.token)
            try apply(["kind": "connected"])
            try apply(["kind": "session_update", "session": configuration.session])
        } catch {
            recordTransportLoss(error.localizedDescription)
        }
    }

    private func apply(_ operation: [String: Any], atMS: Int64? = nil) throws {
        try reducer.apply(atMS: atMS ?? nowMS(), operation: operation)
        let commands = reducer.outbound
        while commandCursor < commands.count {
            transport.send(commands[commandCursor])
            commandCursor += 1
        }
        publish(try reducer.snapshot())
    }

    private func publish(_ snapshot: [String: Any]) {
        sessionConfiguration?.observe(state: snapshot)
        onSnapshot?(snapshot)
        for listener in Array(listeners.values) { listener(snapshot) }
    }

    private func currentPhase() -> String {
        guard let snapshot = try? reducer.snapshot(),
              let connection = snapshot["connection"] as? [String: Any] else { return "disconnected" }
        return (connection["phase"] as? String) ?? "disconnected"
    }

    private func nowMS() -> Int64 {
        let elapsed = Int64(max(0, (ProcessInfo.processInfo.systemUptime - epoch) * 1_000))
        let snapshot = try? reducer.snapshot()
        let current = (snapshot?["now_ms"] as? NSNumber)?.int64Value ?? 0
        return min(RealtimeReducerService.maxVirtualTimeMS, max(current, elapsed))
    }

    private func report(_ error: Error) {
        let message = String(error.localizedDescription.prefix(16 << 10))
        onDiagnostic?(message)
    }
}

private struct NativeReducerError: LocalizedError {
    let message: String
    init(_ message: String) { self.message = message }
    var errorDescription: String? { message }
}
