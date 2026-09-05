package webrtc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bojieli/OpenRealtime/realtimeclient"
	"github.com/pion/opus"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// session is one bridged connection: a peer connection on one side, a protocol
// client on the other.
type session struct {
	adapter    *Adapter
	connection *webrtc.PeerConnection
	model      string

	client *realtimeclient.Client
	track  *webrtc.TrackLocalStaticSample

	eventsMu sync.Mutex
	events   *webrtc.DataChannel
	// pending holds events the endpoint produced before the data channel
	// finished opening. The adapter dials the endpoint as soon as the peer
	// connection is established, but SCTP negotiation completes some
	// milliseconds later, and an endpoint that greets a new session emits into
	// exactly that window. Dropping those would strand a client waiting for
	// session.created with no error to explain the wait.
	pending [][]byte
	// inbound rebuilds messages the client had to chunk, and outbound names
	// the ones this side chunks. Video is the reason both exist: a screen
	// frame does not fit in one SCTP message on every peer.
	inbound  *reassembler
	outbound atomic.Uint32

	closed atomic.Bool
	done   chan struct{}
	once   sync.Once

	// encoder is non-nil only when the adapter sends Opus. pending holds the
	// samples left over from a delta that did not divide into whole frames:
	// libopus takes exact frame sizes, and audio deltas do not arrive on
	// frame boundaries.
	encoder    opusEncoder
	pendingPCM []int16

	// video is what the server answered about video input, read from the
	// session events passing through to the client. The key-frame bridge in
	// video.go forwards nothing until the client has negotiated video and
	// conforms to the limits the server declared.
	videoMu    sync.Mutex
	video      videoNegotiation
	videoKnown bool
}

// prepare wires the peer connection before the offer is applied.
func (session *session) prepare() error {
	codec := session.adapter.config.AudioCodec
	track, err := webrtc.NewTrackLocalStaticSample(
		codec.trackCapability(), "audio", "openrealtime",
	)
	if err != nil {
		return fmt.Errorf("create audio track: %w", err)
	}
	if codec == AudioCodecOpus {
		encoder, err := newOpusEncoder(inboundRate, 1)
		if err != nil {
			return err
		}
		session.encoder = encoder
	}
	sender, err := session.connection.AddTrack(track)
	if err != nil {
		return fmt.Errorf("add audio track: %w", err)
	}
	session.track = track
	// Reading the sender drains RTCP. Without it the receiver's reports pile
	// up and the transport stops adapting, which shows up as audio that gets
	// steadily worse rather than as an error.
	go func() {
		buffer := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buffer); err != nil {
				return
			}
		}
	}()

	session.connection.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		switch remote.Kind() {
		case webrtc.RTPCodecTypeAudio:
			session.pumpInbound(remote)
		case webrtc.RTPCodecTypeVideo:
			session.pumpVideo(remote)
		}
	})

	session.connection.OnDataChannel(func(channel *webrtc.DataChannel) {
		if channel.Label() != EventChannel {
			return
		}
		session.eventsMu.Lock()
		session.events = channel
		session.eventsMu.Unlock()
		channel.OnOpen(func() { session.flushPending() })
		channel.OnMessage(func(message webrtc.DataChannelMessage) {
			session.receiveFromClient(message)
		})
	})
	session.connection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		session.adapter.config.Logf("webrtc connection state %s", state)
		switch state {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed,
			webrtc.PeerConnectionStateDisconnected:
			session.close()
		}
	})
	return nil
}

// connect opens the protocol connection and starts the two pumps.
func (session *session) connect(ctx context.Context) error {
	client, err := realtimeclient.Dial(ctx, realtimeclient.Config{
		URL:         session.adapter.config.Endpoint,
		Token:       session.adapter.config.Token,
		Model:       session.model,
		DialTimeout: session.adapter.config.ConnectTimeout,
	})
	if err != nil {
		return fmt.Errorf("connect to the protocol endpoint: %w", err)
	}
	session.client = client

	// The adapter owns the media format because it terminates media. This is
	// the one event it originates rather than forwards, and the reason is that
	// the client's audio never touches the protocol connection at all.
	//
	// The two directions differ because the codecs do. Inbound Opus is decoded
	// to 24 kHz PCM and sent as audio/pcm, so speech recognition sees the full
	// bandwidth the browser captured. Outbound stays mu-law, which the adapter
	// forwards without transcoding.
	if err := client.Send(ctx, map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime",
			"audio": map[string]any{
				"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
				"output": map[string]any{"format": session.adapter.config.AudioCodec.sessionOutputFormat()},
			},
		},
	}); err != nil {
		_ = client.Close()
		return fmt.Errorf("configure the protocol session: %w", err)
	}
	go session.pumpOutbound(ctx)
	if timeout := session.adapter.config.SessionTimeout; timeout > 0 {
		go func() {
			timer := time.NewTimer(timeout)
			defer timer.Stop()
			select {
			case <-timer.C:
				session.close()
			case <-session.done:
			}
		}()
	}
	return nil
}

// pumpInbound decodes what the browser sent and forwards it to the protocol.
//
// Whichever codec was negotiated, what reaches the protocol is 24 kHz PCM:
// Opus is decoded, mu-law is expanded and upsampled. Doing the conversion here
// rather than pushing a second format onto the session keeps the engine
// working in one rate, and a rate that changes underneath an acoustic gate is
// a bug that looks like a slow model.
func (session *session) pumpInbound(remote *webrtc.TrackRemote) {
	codec := strings.ToLower(remote.Codec().MimeType)
	var decoder *opus.Decoder
	if strings.Contains(codec, "opus") {
		created, err := opus.NewDecoderWithOutput(inboundRate, 1)
		if err != nil {
			session.adapter.config.Logf("create the Opus decoder: %v", err)
			return
		}
		decoder = &created
	}
	session.adapter.config.Logf("receiving %s", remote.Codec().MimeType)

	samples := make([]int16, inboundRate/10)
	for {
		packet, _, err := remote.ReadRTP()
		if err != nil {
			return
		}
		if len(packet.Payload) == 0 || session.closed.Load() {
			continue
		}
		var pcm []byte
		if decoder != nil {
			count, decodeErr := decoder.DecodeToInt16(packet.Payload, samples)
			if decodeErr != nil || count == 0 {
				continue
			}
			pcm = encodePCM16(samples[:count])
		} else {
			pcm = expandMuLaw(packet.Payload)
		}
		if len(pcm) == 0 {
			continue
		}
		if err := session.client.Send(context.Background(), map[string]any{
			"type":  "input_audio_buffer.append",
			"audio": base64.StdEncoding.EncodeToString(pcm),
		}); err != nil {
			session.adapter.config.Logf("forward inbound audio: %v", err)
			return
		}
	}
}

// inboundRate is the rate everything inside the session works in.
const inboundRate = 24_000

func encodePCM16(samples []int16) []byte {
	encoded := make([]byte, len(samples)*2)
	for index, sample := range samples {
		encoded[index*2] = byte(uint16(sample))
		encoded[index*2+1] = byte(uint16(sample) >> 8)
	}
	return encoded
}

// expandMuLaw decodes G.711 and upsamples it to the session rate.
//
// Repeating each sample three times is not a good resampler, and it does not
// have to be: this path exists for a client that could not negotiate Opus, and
// the bandwidth it is missing was never in the signal to begin with.
func expandMuLaw(payload []byte) []byte {
	const ratio = inboundRate / 8000
	expanded := make([]byte, 0, len(payload)*2*ratio)
	for _, encoded := range payload {
		value := ^encoded
		mantissa := int(value & 0x0f)
		exponent := uint((value >> 4) & 0x07)
		sample := ((mantissa << 3) + 0x84) << exponent
		sample -= 0x84
		if value&0x80 != 0 {
			sample = -sample
		}
		low, high := byte(uint16(int16(sample))), byte(uint16(int16(sample))>>8)
		for repeat := 0; repeat < ratio; repeat++ {
			expanded = append(expanded, low, high)
		}
	}
	return expanded
}

// pumpOutbound routes protocol events: audio to the media track, everything
// else to the data channel, unchanged.
func (session *session) pumpOutbound(ctx context.Context) {
	defer session.close()
	for {
		select {
		case <-ctx.Done():
			return
		case event, open := <-session.client.Events():
			if !open {
				return
			}
			if event.Type == "response.output_audio.delta" {
				session.playAudio(event.Raw)
				continue
			}
			if event.Type == "session.created" || event.Type == "session.updated" {
				session.observeVideoNegotiation(event.Raw)
			}
			session.forwardToClient(event.Raw)
		}
	}
}

// playAudio repacketises a protocol audio delta onto the track.
//
// The protocol delivers audio already paced in wall-clock terms; RTP needs it
// in packets of a fixed duration. Handing the whole delta to the track as one
// sample would make the pacer emit a burst, so it is split here at the
// packetisation interval.
func (session *session) playAudio(raw []byte) {
	var decoded struct {
		Delta string `json:"delta"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return
	}
	payload, err := base64.StdEncoding.DecodeString(decoded.Delta)
	if err != nil || len(payload) == 0 {
		return
	}
	if session.encoder != nil {
		session.playOpus(payload)
		return
	}
	// mu-law is one byte per sample at 8 kHz.
	samplesPerPacket := int(8000 * session.adapter.config.PacketDuration / time.Second)
	if samplesPerPacket <= 0 {
		samplesPerPacket = 160
	}
	for offset := 0; offset < len(payload); offset += samplesPerPacket {
		end := min(offset+samplesPerPacket, len(payload))
		chunk := payload[offset:end]
		duration := time.Duration(len(chunk)) * time.Second / 8000
		if err := session.track.WriteSample(media.Sample{Data: chunk, Duration: duration}); err != nil {
			session.adapter.config.Logf("write audio sample: %v", err)
			return
		}
	}
}

// playOpus encodes the session's own 24 kHz PCM onto the track.
//
// libopus accepts only frame sizes it recognises, and an audio delta is
// whatever length the session produced, so samples that do not fill a frame
// are carried to the next delta rather than padded with silence. Padding
// would insert a gap into continuous speech on every delta boundary.
func (session *session) playOpus(payload []byte) {
	session.pendingPCM = append(session.pendingPCM, decodePCM16(payload)...)
	for len(session.pendingPCM) >= opusSamplesPerFrame {
		frame, err := session.encoder.EncodeFrame(session.pendingPCM[:opusSamplesPerFrame])
		session.pendingPCM = session.pendingPCM[opusSamplesPerFrame:]
		if err != nil {
			session.adapter.config.Logf("encode audio: %v", err)
			return
		}
		if len(frame) == 0 {
			continue
		}
		if err := session.track.WriteSample(media.Sample{
			Data: frame, Duration: opusFrameDuration,
		}); err != nil {
			session.adapter.config.Logf("write audio sample: %v", err)
			return
		}
	}
	// Left-over samples must not accumulate without bound if the track stops
	// draining; one frame is the most that can ever legitimately be held.
	if len(session.pendingPCM) > opusSamplesPerFrame {
		session.pendingPCM = session.pendingPCM[:0]
	}
}

// maxPendingEvents bounds what is held for a data channel that has not opened.
//
// The window this covers is milliseconds, so the limit is generous by design;
// reaching it means the channel is never going to open, and at that point the
// session is over and holding more events helps nobody.
const maxPendingEvents = 256

// forwardToClient sends one protocol event to the browser verbatim.
//
// Order is preserved across the not-yet-open window: an event queued before
// the channel opened is sent before anything that arrives after, because the
// protocol's meaning depends on its sequence.
func (session *session) forwardToClient(raw []byte) {
	session.eventsMu.Lock()
	channel := session.events
	if channel == nil || channel.ReadyState() != webrtc.DataChannelStateOpen ||
		len(session.pending) > 0 {
		queued := len(session.pending) < maxPendingEvents
		if queued {
			session.pending = append(session.pending, append([]byte(nil), raw...))
		}
		session.eventsMu.Unlock()
		if !queued {
			session.adapter.config.Logf(
				"dropping event: the data channel has not opened after %d queued events",
				maxPendingEvents)
		}
		// The channel may have opened between the check and the append, which
		// would leave the queue with nobody to flush it.
		if channel != nil && channel.ReadyState() == webrtc.DataChannelStateOpen {
			session.flushPending()
		}
		return
	}
	session.eventsMu.Unlock()
	if err := session.sendToClient(channel, raw); err != nil {
		session.adapter.config.Logf("forward event to client: %v", err)
	}
}

// flushPending sends everything queued before the data channel opened.
func (session *session) flushPending() {
	for {
		session.eventsMu.Lock()
		channel := session.events
		if channel == nil || channel.ReadyState() != webrtc.DataChannelStateOpen ||
			len(session.pending) == 0 {
			session.eventsMu.Unlock()
			return
		}
		raw := session.pending[0]
		session.pending = session.pending[1:]
		session.eventsMu.Unlock()
		if err := session.sendToClient(channel, raw); err != nil {
			session.adapter.config.Logf("forward queued event to client: %v", err)
			return
		}
	}
}

// forwardToProtocol sends one client event to the endpoint.
//
// Audio format changes are dropped rather than forwarded: the client's audio
// is on the RTP track, so its opinion about the protocol connection's audio
// format would break the media path without meaning anything. Everything else
// crosses unchanged, which is what keeps the adapter from being able to
// express anything a plain WebSocket client cannot.
func (session *session) forwardToProtocol(raw []byte) {
	if session.closed.Load() || session.client == nil {
		return
	}
	sanitised, err := stripAudioFormat(raw)
	if err != nil {
		session.adapter.config.Logf("client event: %v", err)
		return
	}
	if err := session.client.Send(context.Background(), json.RawMessage(sanitised)); err != nil {
		session.adapter.config.Logf("forward client event: %v", err)
	}
}

func stripAudioFormat(raw []byte) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, errors.New("client event is not a JSON object")
	}
	var eventType string
	if err := json.Unmarshal(envelope["type"], &eventType); err != nil {
		return nil, errors.New("client event has no type")
	}
	if eventType != "session.update" {
		return raw, nil
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(envelope["session"], &body); err != nil {
		return raw, nil
	}
	audio, present := body["audio"]
	if !present {
		return raw, nil
	}
	var audioBody map[string]json.RawMessage
	if err := json.Unmarshal(audio, &audioBody); err != nil {
		return raw, nil
	}
	for _, direction := range []string{"input", "output"} {
		section, exists := audioBody[direction]
		if !exists {
			continue
		}
		var sectionBody map[string]json.RawMessage
		if err := json.Unmarshal(section, &sectionBody); err != nil {
			continue
		}
		delete(sectionBody, "format")
		encoded, err := json.Marshal(sectionBody)
		if err != nil {
			continue
		}
		audioBody[direction] = encoded
	}
	encodedAudio, err := json.Marshal(audioBody)
	if err != nil {
		return raw, nil
	}
	body["audio"] = encodedAudio
	encodedBody, err := json.Marshal(body)
	if err != nil {
		return raw, nil
	}
	envelope["session"] = encodedBody
	return json.Marshal(envelope)
}

func (session *session) close() {
	session.once.Do(func() {
		session.closed.Store(true)
		close(session.done)
		if session.client != nil {
			_ = session.client.Close()
		}
		_ = session.connection.Close()
	})
}

// receiveFromClient routes one data channel message.
//
// Text is a protocol event and binary is a chunk of one. Keeping the two
// apart by message kind rather than by inspecting content means a client that
// never chunks is unaffected by any of this, and a malformed chunk cannot be
// mistaken for an event.
func (session *session) receiveFromClient(message webrtc.DataChannelMessage) {
	if message.IsString {
		session.forwardToProtocol(message.Data)
		return
	}
	complete, err := session.inbound.accept(message.Data)
	if err != nil {
		session.adapter.config.Logf("client chunk: %v", err)
		return
	}
	if complete == nil {
		return
	}
	session.forwardToProtocol(complete)
}

// sendToClient writes one protocol event to the data channel, chunking it only
// when the peer could not take it whole.
//
// The threshold is the size SCTP actually negotiated rather than a constant,
// so nothing a client can already receive changes shape: a message that fits
// is still one text message. What changes is that a message that does not fit
// now has a way across instead of being refused by the write.
func (session *session) sendToClient(channel *webrtc.DataChannel, raw []byte) error {
	negotiated := session.negotiatedMessageBytes()
	if wholeMessageFits(len(raw), negotiated) {
		return channel.SendText(string(raw))
	}
	identifier := session.outbound.Add(1)
	for _, frame := range splitChunks(identifier, raw, chunkPayloadFor(negotiated)) {
		if err := channel.Send(frame); err != nil {
			return err
		}
	}
	return nil
}

// negotiatedMessageBytes is the largest message this peer will accept.
func (session *session) negotiatedMessageBytes() uint32 {
	transport := session.connection.SCTP()
	if transport == nil {
		return 0
	}
	return transport.GetCapabilities().MaxMessageSize
}
