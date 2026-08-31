import Foundation
import OpenRealtimeClientCore

/// What every realtime transport owes the rest of the client.
///
/// The reducer, the media provider, and the video provider are written against
/// this rather than against a WebSocket, because a WebRTC transport carries
/// the same session differently: audio as negotiated media and the remaining
/// events over a data channel. Whatever the two end up needing to differ on
/// belongs here, stated, rather than being discovered by a provider whose
/// audio quietly goes nowhere.
@MainActor
protocol RealtimeTransport: AnyObject {
    var onEvent: (([String: Any]) -> Void)? { get set }
    var onState: ((ConnectionState, String) -> Void)? { get set }
    var onProtocol: ((String, [String: Any]) -> Void)? { get set }

    /// Whether this transport carries microphone audio and assistant playout
    /// itself, as negotiated media.
    ///
    /// A transport that does owns capture and playout end to end, and the
    /// media provider must not also open the microphone: two capture paths on
    /// one device is a permission prompt for nothing and a second copy of the
    /// user's voice on the wire.
    var carriesAudioAsMedia: Bool { get }

    /// The reducer's own name for this transport. It is not a label: a
    /// barge-in on a media transport has to clear the server's audio buffer
    /// because that audio is already in flight, and the reducer decides that
    /// from this value.
    var transportKind: String { get }

    var connected: Bool { get }

    func connect(token: String) async throws
    func disconnect(reason: String)

    func send(_ event: [String: Any])
    func sendAudio(_ pcm16LE: Data)
    func sendVideo(source: String, data: Data, timestampMS: Int64)
    func updateVideoSource(_ source: String, state: String, width: Int, height: Int)

    func bindDiagnostics(_ publisher: TransportDiagnosticsPublisher) throws
    func unbindDiagnostics(_ publisher: TransportDiagnosticsPublisher)
}

extension RealtimeTransport {
    /// A transport sends audio as protocol events unless it says otherwise.
    var carriesAudioAsMedia: Bool { false }
}
