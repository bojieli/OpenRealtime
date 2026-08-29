package bench

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bojieli/OpenRealtime/realtimeclient"
	rtcadapter "github.com/bojieli/OpenRealtime/transport/webrtc"
	pion "github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

func startLoopbackWebRTC(upstream, token, model string) (string, func(), error) {
	adapter, err := rtcadapter.New(rtcadapter.Config{
		Endpoint: upstream, Token: token, Model: model,
	})
	if err != nil {
		return "", nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("listen for benchmark WebRTC adapter: %w", err)
	}
	server := &http.Server{Handler: adapter.Handler()}
	go func() { _ = server.Serve(listener) }()
	closeAdapter := func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}
	return "http://" + listener.Addr().String() + "/v1/realtime", closeAdapter, nil
}

const (
	// TransportWebSocket sends protocol audio and events over one WebSocket.
	TransportWebSocket = "websocket"
	// TransportWebRTC sends audio over RTP and protocol/video/tool events over
	// the WebRTC data channel, matching the repository browser surface.
	TransportWebRTC = "webrtc"
)

// realtimeSession is the part of realtimeclient.Client the benchmark uses.
// A WebRTC session implements the same event boundary even though its audio
// travels on RTP rather than as input_audio_buffer.append messages.
type realtimeSession interface {
	Events() <-chan realtimeclient.Event
	Send(context.Context, any) error
	Err() error
	Close() error
}

// pcmInput is implemented by a transport that owns an input media track.
type pcmInput interface {
	SendPCM24k(context.Context, []int16) error
}

type webRTCSession struct {
	connection *pion.PeerConnection
	track      *pion.TrackLocalStaticSample
	channel    *pion.DataChannel
	codec      *rtcadapter.EventCodec
	events     chan realtimeclient.Event

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	eventsMu sync.RWMutex
	writeMu  sync.Mutex
	closed   atomic.Bool
	readErr  atomic.Pointer[error]
}

func dialWebRTC(ctx context.Context, endpoint, token, model string) (*webRTCSession, error) {
	if strings.TrimSpace(endpoint) == "" {
		return nil, errors.New("a WebRTC session requires an SDP endpoint")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("a WebRTC endpoint must be an absolute http or https URL")
	}
	if strings.TrimSpace(model) != "" && parsed.Query().Get("model") == "" {
		query := parsed.Query()
		query.Set("model", model)
		parsed.RawQuery = query.Encode()
	}

	connection, err := pion.NewPeerConnection(pion.Configuration{})
	if err != nil {
		return nil, fmt.Errorf("create WebRTC peer: %w", err)
	}
	fail := func(err error) (*webRTCSession, error) {
		_ = connection.Close()
		return nil, err
	}
	track, err := pion.NewTrackLocalStaticSample(pion.RTPCodecCapability{
		MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1,
	}, "audio", "meeting-benchmark")
	if err != nil {
		return fail(fmt.Errorf("create WebRTC audio track: %w", err))
	}
	sender, err := connection.AddTrack(track)
	if err != nil {
		return fail(fmt.Errorf("add WebRTC audio track: %w", err))
	}
	// Receiver reports must be drained or the sender eventually stops adapting.
	go func() {
		buffer := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buffer); err != nil {
				return
			}
		}
	}()

	sessionCtx, cancel := context.WithCancel(ctx)
	client := &webRTCSession{
		connection: connection, track: track, codec: rtcadapter.NewEventCodec(),
		events: make(chan realtimeclient.Event, 512), ctx: sessionCtx, cancel: cancel,
		done: make(chan struct{}),
	}
	connection.OnTrack(func(remote *pion.TrackRemote, _ *pion.RTPReceiver) {
		if remote.Kind() == pion.RTPCodecTypeAudio {
			go client.receiveAudio(remote)
		}
	})
	connection.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		switch state {
		case pion.PeerConnectionStateFailed, pion.PeerConnectionStateDisconnected:
			client.finish(fmt.Errorf("WebRTC connection entered %s", state))
		case pion.PeerConnectionStateClosed:
			client.finish(nil)
		}
	})
	channel, err := connection.CreateDataChannel(rtcadapter.EventChannel, nil)
	if err != nil {
		cancel()
		return fail(fmt.Errorf("create WebRTC event channel: %w", err))
	}
	client.channel = channel
	opened := make(chan struct{})
	var openedOnce sync.Once
	channel.OnOpen(func() { openedOnce.Do(func() { close(opened) }) })
	channel.OnMessage(func(message pion.DataChannelMessage) {
		raw, decodeErr := client.codec.Decode(message.Data, !message.IsString)
		if decodeErr != nil {
			client.finish(fmt.Errorf("decode WebRTC event: %w", decodeErr))
			return
		}
		if raw != nil {
			client.emit(raw)
		}
	})

	offer, err := connection.CreateOffer(nil)
	if err != nil {
		cancel()
		return fail(fmt.Errorf("create WebRTC offer: %w", err))
	}
	gathered := pion.GatheringCompletePromise(connection)
	if err := connection.SetLocalDescription(offer); err != nil {
		cancel()
		return fail(fmt.Errorf("set WebRTC offer: %w", err))
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		cancel()
		return fail(ctx.Err())
	case <-time.After(30 * time.Second):
		cancel()
		return fail(errors.New("WebRTC ICE gathering timed out"))
	}

	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, parsed.String(), strings.NewReader(connection.LocalDescription().SDP),
	)
	if err != nil {
		cancel()
		return fail(err)
	}
	request.Header.Set("Content-Type", "application/sdp")
	if strings.TrimSpace(token) != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		cancel()
		return fail(fmt.Errorf("exchange WebRTC offer: %w", err))
	}
	answer, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	_ = response.Body.Close()
	if readErr != nil {
		cancel()
		return fail(fmt.Errorf("read WebRTC answer: %w", readErr))
	}
	if response.StatusCode != http.StatusCreated {
		cancel()
		return fail(fmt.Errorf("WebRTC adapter refused the offer: HTTP %d: %s",
			response.StatusCode, strings.TrimSpace(string(answer))))
	}
	if err := connection.SetRemoteDescription(pion.SessionDescription{
		Type: pion.SDPTypeAnswer, SDP: string(answer),
	}); err != nil {
		cancel()
		return fail(fmt.Errorf("apply WebRTC answer: %w", err))
	}
	if err := waitForWebRTC(ctx, connection, opened); err != nil {
		cancel()
		return fail(err)
	}
	return client, nil
}

func waitForWebRTC(ctx context.Context, connection *pion.PeerConnection, opened <-chan struct{}) error {
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	connected := false
	channelOpen := false
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for !connected || !channelOpen {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("WebRTC setup timed out in state %s", connection.ConnectionState())
		case <-opened:
			channelOpen = true
			opened = nil
		case <-ticker.C:
			switch connection.ConnectionState() {
			case pion.PeerConnectionStateConnected:
				connected = true
			case pion.PeerConnectionStateFailed, pion.PeerConnectionStateClosed:
				return fmt.Errorf("WebRTC setup failed in state %s", connection.ConnectionState())
			}
		}
	}
	return nil
}

func (client *webRTCSession) Events() <-chan realtimeclient.Event { return client.events }

func (client *webRTCSession) Err() error {
	if pointer := client.readErr.Load(); pointer != nil {
		return *pointer
	}
	return nil
}

func (client *webRTCSession) Send(_ context.Context, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	if client.closed.Load() || client.channel == nil ||
		client.channel.ReadyState() != pion.DataChannelStateOpen {
		return errors.New("WebRTC event channel is closed")
	}
	for _, frame := range client.codec.Encode(raw, client.negotiatedMessageBytes()) {
		if frame.Binary {
			err = client.channel.Send(frame.Data)
		} else {
			err = client.channel.SendText(string(frame.Data))
		}
		if err != nil {
			return fmt.Errorf("send WebRTC event: %w", err)
		}
	}
	return nil
}

func (client *webRTCSession) SendPCM24k(ctx context.Context, samples []int16) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if client.closed.Load() {
		return errors.New("WebRTC audio track is closed")
	}
	muLaw := encodeMuLaw8k(downsamplePCM(samples, 24_000, 8_000))
	if len(muLaw) == 0 {
		return nil
	}
	duration := time.Duration(len(muLaw)) * time.Second / 8000
	if err := client.track.WriteSample(media.Sample{Data: muLaw, Duration: duration}); err != nil {
		return fmt.Errorf("send WebRTC audio: %w", err)
	}
	return nil
}

func (client *webRTCSession) Close() error {
	if client.closed.Swap(true) {
		return nil
	}
	client.cancel()
	err := client.connection.Close()
	client.finish(nil)
	return err
}

func (client *webRTCSession) negotiatedMessageBytes() uint32 {
	transport := client.connection.SCTP()
	if transport == nil {
		return 0
	}
	return transport.GetCapabilities().MaxMessageSize
}

func (client *webRTCSession) emit(raw []byte) {
	var envelope struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &envelope) != nil || strings.TrimSpace(envelope.Type) == "" {
		return
	}
	client.eventsMu.RLock()
	defer client.eventsMu.RUnlock()
	select {
	case client.events <- realtimeclient.Event{Type: envelope.Type, Raw: append([]byte(nil), raw...)}:
	case <-client.done:
	case <-client.ctx.Done():
	}
}

func (client *webRTCSession) receiveAudio(remote *pion.TrackRemote) {
	if !strings.EqualFold(remote.Codec().MimeType, pion.MimeTypePCMU) {
		client.finish(fmt.Errorf("unsupported WebRTC output codec %s", remote.Codec().MimeType))
		return
	}
	for {
		packet, _, err := remote.ReadRTP()
		if err != nil {
			if !client.closed.Load() {
				client.finish(fmt.Errorf("read WebRTC audio: %w", err))
			}
			return
		}
		pcm := expandMuLaw24k(packet.Payload)
		if len(pcm) == 0 {
			continue
		}
		raw, _ := json.Marshal(map[string]any{
			"type":  "response.output_audio.delta",
			"delta": base64.StdEncoding.EncodeToString(pcm),
		})
		client.emit(raw)
	}
}

func (client *webRTCSession) finish(failure error) {
	client.once.Do(func() {
		if failure != nil && !client.closed.Load() {
			client.readErr.Store(&failure)
		}
		close(client.done)
		client.eventsMu.Lock()
		close(client.events)
		client.eventsMu.Unlock()
	})
}

func downsamplePCM(samples []int16, sourceRate, targetRate int) []int16 {
	if len(samples) == 0 || sourceRate == targetRate {
		return append([]int16(nil), samples...)
	}
	count := len(samples) * targetRate / sourceRate
	result := make([]int16, count)
	for index := range result {
		start := index * sourceRate / targetRate
		end := min((index+1)*sourceRate/targetRate, len(samples))
		if end <= start {
			end = min(start+1, len(samples))
		}
		total := 0
		for position := start; position < end; position++ {
			total += int(samples[position])
		}
		result[index] = int16(total / (end - start))
	}
	return result
}

func encodeMuLaw8k(samples []int16) []byte {
	encoded := make([]byte, len(samples))
	for index, sample := range samples {
		value := int(sample)
		sign := byte(0)
		if value < 0 {
			sign = 0x80
			value = -value
			if value > 32767 {
				value = 32767
			}
		}
		value = min(value, 32635) + 0x84
		exponent := byte(7)
		for mask := 0x4000; exponent > 0 && value&mask == 0; mask >>= 1 {
			exponent--
		}
		mantissa := byte(value >> (exponent + 3) & 0x0f)
		encoded[index] = ^(sign | exponent<<4 | mantissa)
	}
	return encoded
}

func expandMuLaw24k(payload []byte) []byte {
	const ratio = 3
	pcm := make([]byte, 0, len(payload)*2*ratio)
	for _, encoded := range payload {
		value := ^encoded
		mantissa := int(value & 0x0f)
		exponent := uint((value >> 4) & 0x07)
		sample := ((mantissa << 3) + 0x84) << exponent
		sample -= 0x84
		if value&0x80 != 0 {
			sample = -sample
		}
		for repeat := 0; repeat < ratio; repeat++ {
			var pair [2]byte
			binary.LittleEndian.PutUint16(pair[:], uint16(int16(sample)))
			pcm = append(pcm, pair[:]...)
		}
	}
	return pcm
}
