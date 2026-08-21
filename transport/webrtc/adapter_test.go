package webrtc_test

import (
	"context"
	"encoding/base64"
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
}

func newBrowser(t *testing.T) *browser {
	t.Helper()
	connection, err := pion.NewPeerConnection(pion.Configuration{})
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
		var decoded map[string]any
		if json.Unmarshal(message.Data, &decoded) == nil {
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

	response, err := http.Post(endpoint.URL+"/v1/realtime", "application/sdp",
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

	// Server to client.
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
	response, err := http.Post(server.URL+"/v1/realtime", "application/sdp",
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
