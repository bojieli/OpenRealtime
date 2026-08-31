package webrtc_test

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	adapter "github.com/bojieli/OpenRealtime/transport/webrtc"
	"github.com/coder/websocket"
	pion "github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// protocolServer is a stand-in endpoint that records what the adapter sent and
// emits what a test scripts. The adapter must be indistinguishable from any
// other client to it.
type protocolServer struct {
	server *httptest.Server

	mu       sync.Mutex
	received []map[string]any
	send     chan map[string]any
	ready    chan struct{}
	once     sync.Once
}

func newProtocolServer(t *testing.T) *protocolServer {
	endpoint := &protocolServer{send: make(chan map[string]any, 64), ready: make(chan struct{})}
	endpoint.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{})
		if err != nil {
			return
		}
		// A real gateway sizes this from the video limits it advertises. The
		// default would refuse a screen frame long before the protocol saw it.
		connection.SetReadLimit(16 << 20)
		defer connection.CloseNow()
		ctx := request.Context()
		endpoint.once.Do(func() { close(endpoint.ready) })
		go func() {
			for {
				_, input, err := connection.Read(ctx)
				if err != nil {
					return
				}
				var decoded map[string]any
				if json.Unmarshal(input, &decoded) == nil {
					endpoint.mu.Lock()
					endpoint.received = append(endpoint.received, decoded)
					endpoint.mu.Unlock()
				}
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case message := <-endpoint.send:
				encoded, _ := json.Marshal(message)
				if err := connection.Write(ctx, websocket.MessageText, encoded); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(endpoint.server.Close)
	return endpoint
}

func (endpoint *protocolServer) url() string {
	return "ws" + strings.TrimPrefix(endpoint.server.URL, "http")
}

func (endpoint *protocolServer) sent() []map[string]any {
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	return append([]map[string]any(nil), endpoint.received...)
}

func (endpoint *protocolServer) waitFor(t *testing.T, match func(map[string]any) bool, message string) map[string]any {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		for _, event := range endpoint.sent() {
			if match(event) {
				return event
			}
		}
		select {
		case <-deadline:
			t.Fatal(message)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// browser is a minimal WebRTC client: one audio track, one data channel.
type browser struct {
	connection *pion.PeerConnection
	track      *pion.TrackLocalStaticSample
	channel    *pion.DataChannel

	mu       sync.Mutex
	events   []map[string]any
	audio    int
	audioLen int
	partial  [][]byte
	chunks   int
}

func newBrowser(t *testing.T) *browser {
	return newBrowserWithMaxMessageSize(t, 0)
}

// newBrowserWithMaxMessageSize builds a client that advertises a specific SCTP
// maximum, which is how the peers that make chunking necessary behave: two
// pion peers negotiate a gigabyte and would never chunk anything, so a test
// that used the default would be testing the wrong peer.
func newBrowserWithMaxMessageSize(t *testing.T, maxMessageSize uint32) *browser {
	t.Helper()
	settings := pion.SettingEngine{}
	if maxMessageSize > 0 {
		settings.SetSCTPMaxMessageSize(maxMessageSize)
	}
	api := pion.NewAPI(pion.WithSettingEngine(settings))
	connection, err := api.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatalf("new peer connection: %v", err)
	}
	track, err := pion.NewTrackLocalStaticSample(pion.RTPCodecCapability{
		MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1,
	}, "audio", "browser")
	if err != nil {
		t.Fatalf("new track: %v", err)
	}
	if _, err := connection.AddTrack(track); err != nil {
		t.Fatalf("add track: %v", err)
	}
	client := &browser{connection: connection, track: track}
	connection.OnTrack(func(remote *pion.TrackRemote, _ *pion.RTPReceiver) {
		for {
			packet, _, err := remote.ReadRTP()
			if err != nil {
				return
			}
			client.mu.Lock()
			client.audio++
			client.audioLen += len(packet.Payload)
			client.mu.Unlock()
		}
	})
	channel, err := connection.CreateDataChannel(adapter.EventChannel, nil)
	if err != nil {
		t.Fatalf("create data channel: %v", err)
	}
	channel.OnMessage(func(message pion.DataChannelMessage) {
		payload := message.Data
		if !message.IsString {
			// Binary is a chunk of an event rather than an event. A client
			// that ignored these would simply not see anything too large for
			// one SCTP message, which is what happens today.
			client.mu.Lock()
			client.chunks++
			client.mu.Unlock()
			complete := client.reassemble(message.Data)
			if complete == nil {
				return
			}
			payload = complete
		}
		var decoded map[string]any
		if json.Unmarshal(payload, &decoded) == nil {
			client.mu.Lock()
			client.events = append(client.events, decoded)
			client.mu.Unlock()
		}
	})
	client.channel = channel
	t.Cleanup(func() { _ = connection.Close() })
	return client
}

func (client *browser) connect(t *testing.T, endpoint *httptest.Server) {
	t.Helper()
	offer, err := client.connection.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	gathered := pion.GatheringCompletePromise(client.connection)
	if err := client.connection.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local: %v", err)
	}
	<-gathered

	response, err := http.Post(endpoint.URL+"/v1/realtime/calls", "application/sdp",
		strings.NewReader(client.connection.LocalDescription().SDP))
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("offer rejected: %d %s", response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Type"); got != "application/sdp" {
		t.Fatalf("expected an SDP answer, got %q", got)
	}
	if err := client.connection.SetRemoteDescription(pion.SessionDescription{
		Type: pion.SDPTypeAnswer, SDP: string(body),
	}); err != nil {
		t.Fatalf("set remote: %v", err)
	}
}

func (client *browser) waitConnected(t *testing.T) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		if client.connection.ConnectionState() == pion.PeerConnectionStateConnected {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("peer connection never connected, state %s", client.connection.ConnectionState())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (client *browser) audioPackets() (int, int) {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.audio, client.audioLen
}

func (client *browser) received() []map[string]any {
	client.mu.Lock()
	defer client.mu.Unlock()
	return append([]map[string]any(nil), client.events...)
}

func startAdapterWithOrigins(t *testing.T, endpoint *protocolServer, origins ...string) *httptest.Server {
	t.Helper()
	bridge, err := adapter.New(adapter.Config{
		Endpoint: endpoint.url(), Model: "test", AllowedOrigins: origins,
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	server := httptest.NewServer(bridge.Handler())
	t.Cleanup(server.Close)
	return server
}

func startAdapter(t *testing.T, endpoint *protocolServer) *httptest.Server {
	t.Helper()
	bridge, err := adapter.New(adapter.Config{Endpoint: endpoint.url(), Model: "test"})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	server := httptest.NewServer(bridge.Handler())
	t.Cleanup(server.Close)
	return server
}

func TestAdapterConnectsMediaAndEventsThroughTheProtocol(t *testing.T) {
	endpoint := newProtocolServer(t)
	server := startAdapter(t, endpoint)
	client := newBrowser(t)
	client.connect(t, server)
	client.waitConnected(t)
	<-endpoint.ready

	// The adapter owns the media format because it terminates media, and the
	// two directions differ because the codecs do: full-bandwidth PCM into
	// speech recognition, mu-law back out.
	configured := endpoint.waitFor(t, func(event map[string]any) bool {
		return event["type"] == "session.update"
	}, "the adapter must configure the protocol session")
	audio := configured["session"].(map[string]any)["audio"].(map[string]any)
	input := audio["input"].(map[string]any)["format"].(map[string]any)
	if input["type"] != "audio/pcm" || input["rate"].(float64) != 24000 {
		t.Fatalf("inbound audio must reach the session at full rate, got %v", input)
	}
	output := audio["output"].(map[string]any)["format"].(map[string]any)
	if output["type"] != "audio/pcmu" {
		t.Fatalf("outbound audio is mu-law, got %v", output)
	}

	// Inbound mu-law is expanded to the session's rate rather than being
	// pushed onto it as a second format.
	payload := make([]byte, 160)
	for index := range payload {
		payload[index] = 0x7F
	}
	for index := 0; index < 5; index++ {
		if err := client.track.WriteSample(media.Sample{Data: payload, Duration: 20 * time.Millisecond}); err != nil {
			t.Fatalf("write sample: %v", err)
		}
	}
	appended := endpoint.waitFor(t, func(event map[string]any) bool {
		return event["type"] == "input_audio_buffer.append"
	}, "inbound audio must reach the protocol")
	decoded, err := base64.StdEncoding.DecodeString(appended["audio"].(string))
	if err != nil || len(decoded) == 0 {
		t.Fatalf("inbound audio must decode: %v", err)
	}
	if len(decoded)%2 != 0 {
		t.Fatal("PCM16 must arrive as whole samples")
	}
	// 160 mu-law samples at 8 kHz become 480 samples at 24 kHz.
	if len(decoded) != 160*3*2 {
		t.Fatalf("expected 480 samples at the session rate, got %d bytes", len(decoded))
	}
}

func TestServerAudioIsRepacketisedOntoTheTrack(t *testing.T) {
	endpoint := newProtocolServer(t)
	server := startAdapter(t, endpoint)
	client := newBrowser(t)
	client.connect(t, server)
	client.waitConnected(t)
	<-endpoint.ready
	endpoint.waitFor(t, func(event map[string]any) bool {
		return event["type"] == "session.update"
	}, "adapter did not connect")

	// One 100 ms protocol delta must become five 20 ms RTP packets: handing
	// the whole delta to the pacer as one sample would emit a burst.
	delta := make([]byte, 800)
	for index := range delta {
		delta[index] = 0x55
	}
	endpoint.send <- map[string]any{
		"type": "response.output_audio.delta", "event_id": "e1",
		"delta": base64.StdEncoding.EncodeToString(delta),
	}
	deadline := time.After(15 * time.Second)
	for {
		packets, bytes := client.audioPackets()
		if packets >= 5 && bytes >= 800 {
			return
		}
		select {
		case <-deadline:
			packets, bytes := client.audioPackets()
			t.Fatalf("expected five packets carrying 800 bytes, got %d packets and %d bytes", packets, bytes)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestNonAudioEventsAreForwardedVerbatimInBothDirections(t *testing.T) {
	endpoint := newProtocolServer(t)
	server := startAdapter(t, endpoint)
	client := newBrowser(t)
	client.connect(t, server)
	client.waitConnected(t)
	<-endpoint.ready
	endpoint.waitFor(t, func(event map[string]any) bool {
		return event["type"] == "session.update"
	}, "adapter did not connect")

	// Server to client. This is sent as soon as the endpoint is up, which is
	// deliberately inside the window where the peer connection is established
	// but SCTP has not finished opening the data channel. An endpoint that
	// greets a session emits exactly there, so an adapter that dropped what it
	// could not send yet would strand a client waiting for session.created.
	endpoint.send <- map[string]any{
		"type":     "conversation.item.input_audio_transcription.completed",
		"event_id": "e1", "item_id": "item_1", "transcript": "hello there",
	}
	deadline := time.After(15 * time.Second)
	for {
		found := false
		for _, event := range client.received() {
			if event["type"] == "conversation.item.input_audio_transcription.completed" &&
				event["transcript"] == "hello there" {
				found = true
			}
		}
		if found {
			break
		}
		select {
		case <-deadline:
			t.Fatal("a protocol event must reach the browser unchanged")
		case <-time.After(20 * time.Millisecond):
		}
	}

	// Client to server. A tool result is an ordinary event and must cross
	// untouched: an adapter that could not express it would be a defect.
	waitOpen(t, client.channel)
	if err := client.channel.SendText(`{"type":"conversation.item.create","item":{"type":"function_call_output","call_id":"c1","output":"{\"ok\":true}"}}`); err != nil {
		t.Fatalf("send: %v", err)
	}
	forwarded := endpoint.waitFor(t, func(event map[string]any) bool {
		return event["type"] == "conversation.item.create"
	}, "a client event must reach the endpoint")
	item := forwarded["item"].(map[string]any)
	if item["call_id"] != "c1" || item["output"] != `{"ok":true}` {
		t.Fatalf("the event must cross unchanged, got %v", item)
	}
}

// The adapter must not let a client break the media path it terminates, and
// must not gain anything from doing so.
func TestClientAudioFormatChangesAreDroppedButTheRestSurvives(t *testing.T) {
	endpoint := newProtocolServer(t)
	server := startAdapter(t, endpoint)
	client := newBrowser(t)
	client.connect(t, server)
	client.waitConnected(t)
	<-endpoint.ready
	endpoint.waitFor(t, func(event map[string]any) bool {
		return event["type"] == "session.update"
	}, "adapter did not connect")
	waitOpen(t, client.channel)

	if err := client.channel.SendText(`{"type":"session.update","session":{"type":"realtime","instructions":"be brief","audio":{"input":{"format":{"type":"audio/pcm","rate":24000},"turn_detection":{"type":"server_vad"}}}}}`); err != nil {
		t.Fatalf("send: %v", err)
	}
	updated := endpoint.waitFor(t, func(event map[string]any) bool {
		session, ok := event["session"].(map[string]any)
		return ok && session["instructions"] == "be brief"
	}, "the client's own configuration must reach the endpoint")
	session := updated["session"].(map[string]any)
	audio := session["audio"].(map[string]any)
	input := audio["input"].(map[string]any)
	if _, present := input["format"]; present {
		t.Fatal("a client's audio format must not reach a session whose media it does not carry")
	}
	if _, present := input["turn_detection"]; !present {
		t.Fatal("everything else in the same event must survive")
	}
}

func TestOfferIsRejectedWhenTheEndpointIsUnreachable(t *testing.T) {
	bridge, err := adapter.New(adapter.Config{
		Endpoint: "ws://127.0.0.1:1/v1/realtime", ConnectTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	server := httptest.NewServer(bridge.Handler())
	defer server.Close()

	client := newBrowser(t)
	offer, _ := client.connection.CreateOffer(nil)
	gathered := pion.GatheringCompletePromise(client.connection)
	_ = client.connection.SetLocalDescription(offer)
	<-gathered
	response, err := http.Post(server.URL+"/v1/realtime/calls", "application/sdp",
		strings.NewReader(client.connection.LocalDescription().SDP))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("an unreachable endpoint must fail the offer, got %d", response.StatusCode)
	}
	if bridge.Metrics().Failures != 1 {
		t.Fatal("the failure must be counted")
	}
}

func TestAdapterRequiresAnEndpoint(t *testing.T) {
	if _, err := adapter.New(adapter.Config{}); err == nil {
		t.Fatal("an adapter with no protocol endpoint has no session to bridge to")
	}
}

func TestAdapterExposesOnlyTheGAWebRTCCallPath(t *testing.T) {
	bridge, err := adapter.New(adapter.Config{
		Endpoint: "ws://127.0.0.1:1/v1/realtime", ConnectTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(bridge.Handler())
	defer server.Close()

	legacy, err := http.Post(server.URL+"/v1/realtime", "application/sdp", strings.NewReader("v=0"))
	if err != nil {
		t.Fatal(err)
	}
	legacy.Body.Close()
	if legacy.StatusCode != http.StatusNotFound {
		t.Fatalf("legacy WebRTC offer path status = %d, want 404", legacy.StatusCode)
	}

	current, err := http.Post(server.URL+"/v1/realtime/calls", "application/sdp", strings.NewReader("v=0"))
	if err != nil {
		t.Fatal(err)
	}
	current.Body.Close()
	if current.StatusCode == http.StatusNotFound {
		t.Fatal("GA WebRTC call path is not mounted")
	}
}

func waitOpen(t *testing.T, channel *pion.DataChannel) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		if channel.ReadyState() == pion.DataChannelStateOpen {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("data channel never opened, state %s", channel.ReadyState())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

var _ = context.Background

// The acceptance test the design asks for, stated adversarially: anything this
// adapter can express, a plain WebSocket client must be able to express too.
//
// The adapter forwards client events unchanged and originates exactly two
// event types of its own, both of which any WebSocket client can send. If it
// ever originates a third, or alters an event in a way a client could not
// reproduce, that is a defect and this test is what catches it.
func TestAdapterExpressesNothingAPlainClientCannot(t *testing.T) {
	endpoint := newProtocolServer(t)
	server := startAdapter(t, endpoint)
	client := newBrowser(t)
	client.connect(t, server)
	client.waitConnected(t)
	<-endpoint.ready
	endpoint.waitFor(t, func(event map[string]any) bool {
		return event["type"] == "session.update"
	}, "adapter did not connect")
	waitOpen(t, client.channel)

	// Drive a representative session: configuration, a tool result, a
	// response request, a cancellation, and media.
	for _, message := range []string{
		`{"type":"session.update","session":{"type":"realtime","instructions":"be brief"}}`,
		`{"type":"conversation.item.create","item":{"type":"function_call_output","call_id":"c1","output":"{}"}}`,
		`{"type":"response.create"}`,
		`{"type":"response.cancel"}`,
		`{"type":"conversation.item.truncate","item_id":"item_1","content_index":0,"audio_end_ms":120}`,
	} {
		if err := client.channel.SendText(message); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	payload := make([]byte, 160)
	for index := 0; index < 3; index++ {
		if err := client.track.WriteSample(media.Sample{Data: payload, Duration: 20 * time.Millisecond}); err != nil {
			t.Fatalf("write sample: %v", err)
		}
	}
	endpoint.waitFor(t, func(event map[string]any) bool {
		return event["type"] == "conversation.item.truncate"
	}, "the last client event must reach the endpoint")

	// Every event the endpoint saw must be one a WebSocket client could have
	// sent. These are exactly the base protocol's client events.
	expressible := map[string]bool{
		"session.update": true, "input_audio_buffer.append": true,
		"conversation.item.create": true, "response.create": true,
		"response.cancel": true, "conversation.item.truncate": true,
		"input_audio_buffer.clear": true,
	}
	for _, event := range endpoint.sent() {
		observed, _ := event["type"].(string)
		if !expressible[observed] {
			t.Fatalf("the adapter expressed %q, which a plain WebSocket client cannot send", observed)
		}
		if strings.HasPrefix(observed, "openrealtime.") {
			// The extension's client events are equally expressible over
			// WebSocket, but the adapter must not be the one inventing them.
			t.Fatalf("the adapter originated an extension event %q", observed)
		}
	}
}

// The chunk framing, written out by hand rather than shared with the adapter.
//
// A client implements this from the transport documentation, so a test that
// called the adapter's own helpers would only prove they are self-consistent.
const (
	testChunkHeader  = 14
	testChunkPayload = 16 << 10
)

func (client *browser) sendChunked(t *testing.T, identifier uint32, event map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	count := (len(encoded) + testChunkPayload - 1) / testChunkPayload
	for index := 0; index < count; index++ {
		start := index * testChunkPayload
		end := min(start+testChunkPayload, len(encoded))
		frame := make([]byte, testChunkHeader+(end-start))
		copy(frame[0:4], []byte("ORTC"))
		frame[4] = 1
		if index == count-1 {
			frame[5] = 1
		}
		binary.BigEndian.PutUint32(frame[6:10], identifier)
		binary.BigEndian.PutUint16(frame[10:12], uint16(index))
		binary.BigEndian.PutUint16(frame[12:14], uint16(count))
		copy(frame[testChunkHeader:], encoded[start:end])
		if err := client.channel.Send(frame); err != nil {
			t.Fatalf("send chunk %d: %v", index, err)
		}
	}
}

func (client *browser) reassemble(frame []byte) []byte {
	if len(frame) < testChunkHeader || string(frame[0:4]) != "ORTC" {
		return nil
	}
	index := int(binary.BigEndian.Uint16(frame[10:12]))
	count := int(binary.BigEndian.Uint16(frame[12:14]))
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.partial == nil {
		client.partial = make([][]byte, count)
	}
	if index < len(client.partial) {
		client.partial[index] = append([]byte(nil), frame[testChunkHeader:]...)
	}
	for _, chunk := range client.partial {
		if chunk == nil {
			return nil
		}
	}
	var complete []byte
	for _, chunk := range client.partial {
		complete = append(complete, chunk...)
	}
	client.partial = nil
	return complete
}

// A screen frame is larger than one SCTP message on every peer in the field,
// so the transport has to carry it in pieces and hand the protocol back the
// one event it would have received over a WebSocket.
func TestALargeVideoFrameCrossesTheDataChannelWhole(t *testing.T) {
	endpoint := newProtocolServer(t)
	server := startAdapter(t, endpoint)
	client := newBrowser(t)
	client.connect(t, server)
	client.waitConnected(t)
	waitOpen(t, client.channel)

	// Three hundred kilobytes of base64: a modest screen at a modest quality,
	// and roughly five times what the most conservative peer takes whole.
	image := strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVoK", 8500)
	client.sendChunked(t, 1, map[string]any{
		"type": "openrealtime.input_video_frame.append", "source": "screen",
		"frame": image, "timestamp_ms": 1000,
	})

	event := endpoint.waitFor(t, func(event map[string]any) bool {
		return event["type"] == "openrealtime.input_video_frame.append"
	}, "the frame never reached the protocol endpoint")
	if event["frame"] != image {
		t.Fatalf("the frame arrived with %d of %d characters",
			len(event["frame"].(string)), len(image))
	}
	if event["source"] != "screen" {
		t.Fatalf("the event lost its source: %v", event)
	}
}

// The same framing in the other direction, against a peer that advertises the
// smallest maximum in the field, plus the guarantee that goes with it: a
// message the peer can take whole is still sent whole, so a client that never
// implements reassembly sees exactly what it sees today.
func TestOutboundEventsAreChunkedOnlyWhenTheyMustBe(t *testing.T) {
	endpoint := newProtocolServer(t)
	server := startAdapter(t, endpoint)
	client := newBrowserWithMaxMessageSize(t, 65536)
	client.connect(t, server)
	client.waitConnected(t)
	waitOpen(t, client.channel)
	<-endpoint.ready

	long := strings.Repeat("an observation of a very busy screen. ", 20000)
	endpoint.send <- map[string]any{
		"type": "openrealtime.observation.added", "observation_id": "obs_1",
		"observer": "video", "source": "screen", "text": long,
	}
	endpoint.send <- map[string]any{"type": "response.done", "event_id": "event_1"}

	deadline := time.After(15 * time.Second)
	for {
		var observation, done map[string]any
		for _, event := range client.received() {
			switch event["type"] {
			case "openrealtime.observation.added":
				observation = event
			case "response.done":
				done = event
			}
		}
		if observation != nil && done != nil {
			if observation["text"] != long {
				t.Fatalf("the observation arrived with %d of %d characters",
					len(observation["text"].(string)), len(long))
			}
			client.mu.Lock()
			chunks := client.chunks
			client.mu.Unlock()
			if chunks == 0 {
				t.Fatal("the observation was larger than the peer accepts, so it had to be chunked")
			}
			// The small event crossed as one text message rather than as a
			// chunk of its own, which is what keeps existing clients working.
			if chunks > (len(long)/testChunkPayload)+2 {
				t.Fatalf("%d chunks for one large event and one small one", chunks)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("the chunked observation and the small event did not both arrive")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// The adapter answers a browser only for an origin the operator named. It is
// the difference between a browser client being able to reach it at all and
// not, and between that and any page on the internet being able to open a
// session against it.
func TestTheAdapterAnswersOnlyTheOriginsItWasGiven(t *testing.T) {
	t.Parallel()
	allowed := "https://app.example.com"
	endpoint := newProtocolServer(t)
	server := startAdapterWithOrigins(t, endpoint, allowed)

	preflight := func(origin string) *http.Response {
		t.Helper()
		request, err := http.NewRequest(http.MethodOptions, server.URL+"/v1/realtime/calls", nil)
		if err != nil {
			t.Fatalf("build the preflight: %v", err)
		}
		request.Header.Set("Origin", origin)
		request.Header.Set("Access-Control-Request-Method", "POST")
		request.Header.Set("Access-Control-Request-Headers", "content-type,authorization,x-openai-agents-sdk")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("preflight: %v", err)
		}
		t.Cleanup(func() { response.Body.Close() })
		return response
	}

	answered := preflight(allowed)
	if answered.StatusCode != http.StatusNoContent {
		t.Fatalf("a named origin must be answered, got %d", answered.StatusCode)
	}
	if got := answered.Header.Get("Access-Control-Allow-Origin"); got != allowed {
		t.Fatalf("expected the origin echoed, got %q", got)
	}
	// A client's own headers cannot be enumerated in advance, so what it asks
	// for is what it is allowed - including the SDK's telemetry header, which
	// is what made this endpoint unreachable from a browser.
	if got := answered.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "x-openai-agents-sdk") {
		t.Fatalf("the requested headers must be answered, got %q", got)
	}
	if got := answered.Header.Get("Vary"); !strings.Contains(got, "Origin") {
		t.Fatalf("an origin-dependent answer must vary on Origin, got %q", got)
	}

	refused := preflight("https://not-your-app.example.com")
	if refused.StatusCode == http.StatusNoContent {
		t.Fatal("an origin nobody named must not be answered")
	}
	if got := refused.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("a refused origin must not be granted anything, got %q", got)
	}
}

func TestAnAdapterWithNoAllowedOriginsAnswersNoBrowser(t *testing.T) {
	t.Parallel()
	endpoint := newProtocolServer(t)
	server := startAdapter(t, endpoint)
	request, err := http.NewRequest(http.MethodOptions, server.URL+"/v1/realtime/calls", nil)
	if err != nil {
		t.Fatalf("build the preflight: %v", err)
	}
	request.Header.Set("Origin", "https://app.example.com")
	request.Header.Set("Access-Control-Request-Method", "POST")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer response.Body.Close()
	// The default is a server-to-server deployment, where no browser should be
	// able to open a session and nothing needs to.
	if response.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("the default must grant no origin anything")
	}
}

// The SDP endpoint opens a session using the adapter's own upstream
// credential. Anyone who can reach it unauthenticated is therefore spending
// the deployment's models, so a configured credential has to be required
// before the offer is even read.
func TestOfferRequiresTheConfiguredClientCredential(t *testing.T) {
	endpoint := newProtocolServer(t)
	bridge, err := adapter.New(adapter.Config{
		Endpoint: endpoint.url(), Model: "test", ClientCredential: "expected-credential",
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	server := httptest.NewServer(bridge.Handler())
	t.Cleanup(server.Close)

	for _, row := range []struct {
		name          string
		authorization string
		want          int
	}{
		{name: "absent", authorization: "", want: http.StatusUnauthorized},
		{name: "wrong", authorization: "Bearer other-credential", want: http.StatusUnauthorized},
		{name: "prefix only", authorization: "Bearer ", want: http.StatusUnauthorized},
		{name: "wrong scheme", authorization: "Basic expected-credential", want: http.StatusUnauthorized},
		// The offer body is deliberately not a valid SDP: reaching any status
		// other than 401 is what proves the credential was accepted.
		{name: "exact", authorization: "Bearer expected-credential", want: http.StatusBadGateway},
	} {
		t.Run(row.name, func(t *testing.T) {
			request, err := http.NewRequest(
				http.MethodPost, server.URL+"/v1/realtime/calls", strings.NewReader("v=0"),
			)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			request.Header.Set("Content-Type", "application/sdp")
			if row.authorization != "" {
				request.Header.Set("Authorization", row.authorization)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatalf("post offer: %v", err)
			}
			defer response.Body.Close()
			if response.StatusCode != row.want {
				t.Fatalf("status = %d, want %d", response.StatusCode, row.want)
			}
			if row.want == http.StatusUnauthorized &&
				response.Header.Get("WWW-Authenticate") == "" {
				t.Error("a refusal must state the scheme it expects")
			}
		})
	}
}

// An adapter with no configured credential keeps working, which is what the
// loopback development path relies on.
func TestOfferWithoutAConfiguredCredentialStaysOpen(t *testing.T) {
	endpoint := newProtocolServer(t)
	server := startAdapter(t, endpoint)
	response, err := http.Post(
		server.URL+"/v1/realtime/calls", "application/sdp", strings.NewReader("v=0"),
	)
	if err != nil {
		t.Fatalf("post offer: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		t.Fatal("an adapter with no credential must not demand one")
	}
}

// CORS only stops a browser from reading an answer. Serving the POST anyway
// would already have started the session, so a disallowed origin has to be
// refused outright rather than merely denied the header.
func TestOfferRefusesADisallowedOriginInsteadOfStartingTheSession(t *testing.T) {
	endpoint := newProtocolServer(t)
	server := startAdapterWithOrigins(t, endpoint, "https://allowed.example")
	request, err := http.NewRequest(
		http.MethodPost, server.URL+"/v1/realtime/calls", strings.NewReader("v=0"),
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/sdp")
	request.Header.Set("Origin", "https://denied.example")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post offer: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
}

// PCMU is the default because it is the one codec every build can produce.
// A build without the Opus encoder has to refuse the setting rather than
// silently downgrade it: an operator who asked for 24 kHz audio and got 8 kHz
// without being told has no way to notice except by listening.
func TestAudioCodecDefaultsToPCMUAndRefusesWhatItCannotEncode(t *testing.T) {
	for _, value := range []string{"", "pcmu", "PCMU", " pcmu "} {
		codec, err := adapter.ParseAudioCodec(value)
		if err != nil {
			t.Fatalf("ParseAudioCodec(%q) = %v", value, err)
		}
		if codec != adapter.AudioCodecPCMU {
			t.Fatalf("ParseAudioCodec(%q) = %q, want %q", value, codec, adapter.AudioCodecPCMU)
		}
	}
	for _, value := range []string{"g722", "opus-ish", "pcm"} {
		if _, err := adapter.ParseAudioCodec(value); err == nil {
			t.Errorf("ParseAudioCodec(%q) was accepted", value)
		}
	}
	codec, err := adapter.ParseAudioCodec("opus")
	if adapter.OpusEncoderAvailable {
		if err != nil || codec != adapter.AudioCodecOpus {
			t.Fatalf("a tagged build refused Opus: codec=%q err=%v", codec, err)
		}
		return
	}
	if err == nil {
		t.Fatal("a build with no Opus encoder accepted the Opus codec")
	}
	if !strings.Contains(err.Error(), "-tags opus") {
		t.Errorf("refusal does not say how to get an Opus build: %v", err)
	}
}
