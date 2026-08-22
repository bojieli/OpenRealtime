package geminilive_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/geminilive"
	"github.com/coder/websocket"
)

// fakeLive is a Live endpoint that records what it was sent and replays a
// scripted server stream.
type fakeLive struct {
	server *httptest.Server

	mu       sync.Mutex
	received []map[string]json.RawMessage
	key      string

	send  chan map[string]any
	ready chan struct{}
	once  sync.Once
}

func newFakeLive(t *testing.T) *fakeLive {
	fake := &fakeLive{send: make(chan map[string]any, 32), ready: make(chan struct{})}
	fake.server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			fake.mu.Lock()
			fake.key = request.URL.Query().Get("key")
			fake.mu.Unlock()
			connection, err := websocket.Accept(writer, request, nil)
			if err != nil {
				return
			}
			defer connection.CloseNow()
			ctx := request.Context()
			fake.once.Do(func() { close(fake.ready) })
			go func() {
				for {
					_, payload, err := connection.Read(ctx)
					if err != nil {
						return
					}
					var decoded map[string]json.RawMessage
					if json.Unmarshal(payload, &decoded) == nil {
						fake.mu.Lock()
						fake.received = append(fake.received, decoded)
						fake.mu.Unlock()
					}
				}
			}()
			for {
				select {
				case <-ctx.Done():
					return
				case message := <-fake.send:
					encoded, _ := json.Marshal(message)
					if connection.Write(ctx, websocket.MessageText, encoded) != nil {
						return
					}
				}
			}
		}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *fakeLive) url() string {
	return "ws" + strings.TrimPrefix(fake.server.URL, "http")
}

func (fake *fakeLive) emit(message map[string]any) { fake.send <- message }

func (fake *fakeLive) sent() []map[string]json.RawMessage {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]map[string]json.RawMessage(nil), fake.received...)
}

func (fake *fakeLive) apiKey() string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.key
}

func dial(t *testing.T, fake *fakeLive) *geminilive.Client {
	t.Helper()
	client, err := geminilive.Dial(context.Background(), geminilive.Config{
		URL: fake.url(), APIKey: "test-key", Model: "gemini-test",
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	<-fake.ready
	return client
}

func waitFor(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if condition() {
			return
		}
		select {
		case <-deadline:
			t.Fatal(message)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// find returns the first message carrying a top-level key.
func find(sent []map[string]json.RawMessage, key string) map[string]json.RawMessage {
	for _, message := range sent {
		if _, present := message[key]; present {
			return message
		}
	}
	return nil
}

// The handshake carries the instruction, asks for audio, and asks for both
// transcriptions - without which the reasoner would share a conversation it
// cannot read.
func TestSessionUpdateBecomesTheHandshake(t *testing.T) {
	fake := newFakeLive(t)
	client := dial(t, fake)

	if err := client.Send(context.Background(), map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime", "instructions": "You are the voice of this agent.",
		},
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return find(fake.sent(), "setup") != nil }, "no setup was sent")

	encoded, _ := json.Marshal(find(fake.sent(), "setup"))
	for _, want := range []string{
		`"model":"models/gemini-test"`, `"responseModalities":["AUDIO"]`,
		`"inputAudioTranscription":{}`, `"outputAudioTranscription":{}`,
		"You are the voice of this agent.",
	} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("setup is missing %s: %s", want, encoded)
		}
	}
	if fake.apiKey() != "test-key" {
		t.Errorf("the credential goes in the key query parameter, got %q", fake.apiKey())
	}

	// The instruction can only be set at the handshake, so a later update
	// must not send a second one.
	_ = client.Send(context.Background(), map[string]any{
		"type": "session.update", "session": map[string]any{"instructions": "different"},
	})
	time.Sleep(100 * time.Millisecond)
	count := 0
	for _, message := range fake.sent() {
		if _, present := message["setup"]; present {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the handshake must happen once, saw %d", count)
	}
}

// Audio arrives on the Realtime wire at 24 kHz and the Live API requires
// 16 kHz, so the translator is where that gets fixed.
func TestAudioIsResampledAndStreamedAsRealtimeInput(t *testing.T) {
	fake := newFakeLive(t)
	client := dial(t, fake)
	handshake(t, client)

	// 100 ms of 24 kHz PCM16 is 2400 samples; at 16 kHz that is 1600.
	frame := make([]byte, 2400*2)
	if err := client.Send(context.Background(), map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString(frame),
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return find(fake.sent(), "realtimeInput") != nil }, "no audio was sent")

	var decoded struct {
		RealtimeInput struct {
			Audio struct {
				MIMEType string `json:"mimeType"`
				Data     string `json:"data"`
			} `json:"audio"`
		} `json:"realtimeInput"`
	}
	encoded, _ := json.Marshal(find(fake.sent(), "realtimeInput"))
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.RealtimeInput.Audio.MIMEType != "audio/pcm;rate=16000" {
		t.Fatalf("mime type = %q", decoded.RealtimeInput.Audio.MIMEType)
	}
	payload, err := base64.StdEncoding.DecodeString(decoded.RealtimeInput.Audio.Data)
	if err != nil {
		t.Fatal(err)
	}
	// Two thirds of the samples, give or take the resampler's boundary hold.
	if samples := len(payload) / 2; samples < 1500 || samples > 1700 {
		t.Fatalf("resampled to %d samples, want about 1600", samples)
	}
}

func handshake(t *testing.T, client *geminilive.Client) {
	t.Helper()
	if err := client.Send(context.Background(), map[string]any{
		"type":    "session.update",
		"session": map[string]any{"instructions": "be brief"},
	}); err != nil {
		t.Fatal(err)
	}
}

// A Live turn becomes the Realtime events the mirror reads. This is the whole
// point of the package, and every name here is one the mirror switches on.
func TestALiveTurnBecomesRealtimeEvents(t *testing.T) {
	fake := newFakeLive(t)
	client := dial(t, fake)
	handshake(t, client)

	fake.emit(map[string]any{"setupComplete": map[string]any{}})
	fake.emit(map[string]any{"serverContent": map[string]any{
		"inputTranscription": map[string]any{"text": "what is my "},
	}})
	fake.emit(map[string]any{"serverContent": map[string]any{
		"inputTranscription": map[string]any{"text": "balance"},
	}})
	fake.emit(map[string]any{"serverContent": map[string]any{
		"outputTranscription": map[string]any{"text": "It is forty dollars."},
	}})
	fake.emit(map[string]any{"serverContent": map[string]any{
		"modelTurn": map[string]any{"parts": []map[string]any{
			// A thinking part carries text and no audio; it must not be
			// mistaken for speech.
			{"text": "the user wants a balance", "thought": true},
			{"inlineData": map[string]any{
				"mimeType": "audio/pcm;rate=24000",
				"data":     base64.StdEncoding.EncodeToString(make([]byte, 480)),
			}},
		}},
	}})
	fake.emit(map[string]any{"serverContent": map[string]any{"turnComplete": true}})

	events := collect(t, client, "response.done")
	if got := text(events, "conversation.item.input_audio_transcription.completed", "transcript"); got != "what is my balance" {
		t.Errorf("user transcript = %q", got)
	}
	if got := text(events, "response.output_audio_transcript.delta", "delta"); got != "It is forty dollars." {
		t.Errorf("assistant transcript delta = %q", got)
	}
	if got := text(events, "response.output_audio_transcript.done", "transcript"); got != "It is forty dollars." {
		t.Errorf("assistant transcript done = %q", got)
	}
	if count(events, "response.output_audio.delta") != 1 {
		t.Errorf("expected exactly one audio delta, got %d", count(events, "response.output_audio.delta"))
	}
	if count(events, "response.done") != 1 {
		t.Errorf("expected one response.done, got %d", count(events, "response.done"))
	}
}

// Interruption and turn completion are different events, and conflating them
// would commit half a sentence as the user's turn.
func TestAnInterruptionDoesNotCommitAPartialUserTurn(t *testing.T) {
	fake := newFakeLive(t)
	client := dial(t, fake)
	handshake(t, client)

	fake.emit(map[string]any{"serverContent": map[string]any{
		"inputTranscription": map[string]any{"text": "what is my "},
	}})
	fake.emit(map[string]any{"serverContent": map[string]any{
		"outputTranscription": map[string]any{"text": "Let me"},
	}})
	fake.emit(map[string]any{"serverContent": map[string]any{"interrupted": true}})

	events := collect(t, client, "response.done")
	if count(events, "conversation.item.input_audio_transcription.completed") != 0 {
		t.Fatal("an interruption is the model stopping, not the user finishing")
	}
	if got := text(events, "response.output_audio_transcript.done", "transcript"); got != "Let me" {
		t.Errorf("what the model managed to say is still real: %q", got)
	}

	// The user keeps talking, and the whole sentence lands when the turn ends.
	fake.emit(map[string]any{"serverContent": map[string]any{
		"inputTranscription": map[string]any{"text": "balance today"},
	}})
	fake.emit(map[string]any{"serverContent": map[string]any{"turnComplete": true}})
	events = collect(t, client, "response.done")
	if got := text(events, "conversation.item.input_audio_transcription.completed", "transcript"); got != "what is my balance today" {
		t.Fatalf("the accumulated user turn = %q", got)
	}
}

// A conversation item followed by a request to respond is one Live turn. This
// is how the reasoner's answer reaches Gemini's voice.
func TestAConversationItemAndResponseCreateBecomeOneClientTurn(t *testing.T) {
	fake := newFakeLive(t)
	client := dial(t, fake)
	handshake(t, client)

	if err := client.Send(context.Background(), map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": "system",
			"content": []map[string]any{{"type": "input_text", "text": "Say: the balance is $40."}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Nothing goes out yet: a Live turn is delivered complete.
	time.Sleep(100 * time.Millisecond)
	if find(fake.sent(), "clientContent") != nil {
		t.Fatal("a turn must not be sent before the caller asks for a response")
	}
	if err := client.Send(context.Background(), map[string]any{"type": "response.create"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return find(fake.sent(), "clientContent") != nil }, "no turn was sent")

	encoded, _ := json.Marshal(find(fake.sent(), "clientContent"))
	for _, want := range []string{`"turnComplete":true`, `"role":"user"`, "Say: the balance is $40."} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("client turn is missing %s: %s", want, encoded)
		}
	}
}

// A remote tool call is a misconfiguration in this arrangement, and silence
// would hide it.
func TestARemoteToolCallIsReportedRatherThanIgnored(t *testing.T) {
	fake := newFakeLive(t)
	client := dial(t, fake)
	handshake(t, client)

	fake.emit(map[string]any{"toolCall": map[string]any{
		"functionCalls": []map[string]any{{"id": "c1", "name": "get_balance", "args": map[string]any{}}},
	}})
	events := collect(t, client, "error")
	if !strings.Contains(text(events, "error", "message"), "get_balance") {
		t.Fatalf("the attempted call should be named: %+v", events)
	}
}

// Events with no Realtime equivalent must not fail the session.
func TestEventsWithNoEquivalentAreAccepted(t *testing.T) {
	fake := newFakeLive(t)
	client := dial(t, fake)
	handshake(t, client)
	for _, event := range []map[string]any{
		{"type": "input_audio_buffer.commit"},
		{"type": "response.cancel"},
		{"type": "conversation.item.truncate"},
		{"type": "something.this.package.has.never.heard.of"},
	} {
		if err := client.Send(context.Background(), event); err != nil {
			t.Errorf("%v must be accepted: %v", event["type"], err)
		}
	}
}

func TestDialRefusesAnUnusableConfiguration(t *testing.T) {
	t.Parallel()
	if _, err := geminilive.Dial(context.Background(), geminilive.Config{
		URL: "wss://example.invalid/live",
	}); err == nil {
		t.Error("an API key is required")
	}
	if _, err := geminilive.Dial(context.Background(), geminilive.Config{
		URL: "https://example.invalid", APIKey: "k",
	}); err == nil {
		t.Error("a non-WebSocket URL must be refused")
	}
}

// collect drains events until one of the given type arrives.
func collect(t *testing.T, client *geminilive.Client, until string) []realtimeEvent {
	t.Helper()
	var seen []realtimeEvent
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event, open := <-client.Events():
			if !open {
				t.Fatalf("stream closed before %q; err = %v", until, client.Err())
			}
			var body map[string]json.RawMessage
			_ = json.Unmarshal(event.Raw, &body)
			seen = append(seen, realtimeEvent{kind: event.Type, body: body})
			if event.Type == until {
				return seen
			}
		case <-deadline:
			t.Fatalf("never saw %q; saw %+v", until, seen)
		}
	}
}

type realtimeEvent struct {
	kind string
	body map[string]json.RawMessage
}

func text(events []realtimeEvent, kind, field string) string {
	for _, event := range events {
		if event.kind != kind {
			continue
		}
		if raw, present := event.body[field]; present {
			var value string
			if json.Unmarshal(raw, &value) == nil {
				return value
			}
		}
		// The error event nests its fields.
		if raw, present := event.body["error"]; present {
			var nested map[string]string
			if json.Unmarshal(raw, &nested) == nil {
				return nested[field]
			}
		}
	}
	return ""
}

func count(events []realtimeEvent, kind string) int {
	total := 0
	for _, event := range events {
		if event.kind == kind {
			total++
		}
	}
	return total
}
