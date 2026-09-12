package gptlive_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/gptlive"
	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/realtimeclient"
	"github.com/coder/websocket"
)

// fakeLive is a Live endpoint that records what it was sent, reports the
// Authorization header it was dialled with, and replays a scripted stream.
type fakeLive struct {
	server *httptest.Server

	mu         sync.Mutex
	received   []map[string]json.RawMessage
	authHeader string
	// paths records the request path of every connection, so a test can see
	// that a fork went to the fork endpoint.
	paths []string
	// hangup is the current connection's drop signal, replaced per accept so
	// a second connection is not born already hung up.
	hangup chan struct{}

	// send is the channel of the most recent connection. A reopen or a fork
	// leaves an older handler alive for a moment, and one shared channel lets
	// the dying one swallow a message meant for its replacement.
	send  chan map[string]any
	ready chan struct{}
	once  sync.Once
}

func newFakeLive(t *testing.T) *fakeLive {
	t.Helper()
	fake := &fakeLive{
		send: make(chan map[string]any, 32), ready: make(chan struct{}),
	}
	fake.server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			hangup := make(chan struct{})
			outbound := make(chan map[string]any, 32)
			fake.mu.Lock()
			fake.authHeader = request.Header.Get("Authorization")
			fake.paths = append(fake.paths, request.URL.Path)
			fake.hangup = hangup
			// Emissions follow the newest connection.
			fake.send = outbound
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
				case <-hangup:
					// Drop the connection without a close frame, the way a
					// network failure does rather than a graceful shutdown.
					return
				case message := <-outbound:
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

func (fake *fakeLive) url() string { return "ws" + strings.TrimPrefix(fake.server.URL, "http") }

func (fake *fakeLive) emit(message map[string]any) {
	fake.mu.Lock()
	channel := fake.send
	fake.mu.Unlock()
	channel <- message
}

func (fake *fakeLive) sent() []map[string]json.RawMessage {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]map[string]json.RawMessage(nil), fake.received...)
}

// hangUp drops the current connection with no close handshake.
func (fake *fakeLive) hangUp() {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.hangup != nil {
		close(fake.hangup)
		fake.hangup = nil
	}
}

// connectionPaths reports the path each connection arrived on, in order.
func (fake *fakeLive) connectionPaths() []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]string(nil), fake.paths...)
}

func (fake *fakeLive) authorization() string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.authHeader
}

// awaitSent waits for the endpoint to have received a message of one type and
// returns it. Sends cross a socket, so polling is the honest way to wait.
func (fake *fakeLive) awaitSent(t *testing.T, eventType string) map[string]json.RawMessage {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, message := range fake.sent() {
			if decodeString(message["type"]) == eventType {
				return message
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the endpoint never received %s; it received %s", eventType, fake.sentTypes())
	return nil
}

// refuteSent fails if a message of this type ever arrives within a short grace
// period. It is the check that a dropped event really is dropped.
func (fake *fakeLive) refuteSent(t *testing.T, eventType string) {
	t.Helper()
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, message := range fake.sent() {
			if decodeString(message["type"]) == eventType {
				t.Fatalf("the endpoint received %s, which this dialect does not implement", eventType)
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (fake *fakeLive) sentTypes() []string {
	var types []string
	for _, message := range fake.sent() {
		types = append(types, decodeString(message["type"]))
	}
	return types
}

func decodeString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

// connect dials the fake with a manual scheduler, so every turn boundary in
// these tests is produced by advancing a clock rather than by sleeping.
func connect(t *testing.T, fake *fakeLive, adjust func(*gptlive.Config)) (*gptlive.Client, *clock.Manual) {
	t.Helper()
	scheduler := clock.NewManual(0)
	config := gptlive.Config{
		URL: fake.url(), APIKey: "test-key", Scheduler: scheduler,
		// Nothing here should need to wait on a real close handshake.
		CloseTimeout: -1,
	}
	if adjust != nil {
		adjust(&config)
	}
	client, err := gptlive.Dial(t.Context(), config)
	if err != nil {
		t.Fatalf("dial the fake Live endpoint: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, scheduler
}

// start performs the handshake the way the binding does: a session.update
// carrying the instruction, then the endpoint confirming the session.
func start(t *testing.T, fake *fakeLive, client *gptlive.Client, instruction string) {
	t.Helper()
	if err := client.Send(t.Context(), map[string]any{
		"type":    "session.update",
		"session": map[string]any{"type": "realtime", "instructions": instruction},
	}); err != nil {
		t.Fatalf("send the opening session.update: %v", err)
	}
	fake.awaitSent(t, "session.start")
	fake.emit(map[string]any{
		"type": "session.started", "event_id": "evt_started_001",
		"session": map[string]any{"id": "live_abc123", "model": "gpt-live-1", "expires_at": 1789210099},
	})
	created := expect(t, client, "session.created")
	if !strings.Contains(string(created.Raw), "live_abc123") {
		t.Fatalf("session.created did not carry the vendor's session id: %s", created.Raw)
	}
}

// fragment reads the caption one user transcript fragment produces.
func fragment(t *testing.T, client *gptlive.Client, want string) {
	t.Helper()
	event := expect(t, client, "conversation.item.input_audio_transcription.delta")
	if got := field(t, event.Raw, "delta"); got != want {
		t.Fatalf("caption fragment was %q, want %q", got, want)
	}
}

// delegated reads the escalation the binding is told about after a delegation
// has closed the user's turn.
func delegated(t *testing.T, client *gptlive.Client, want string) {
	t.Helper()
	event := expect(t, client, "openrealtime.upstream.delegation")
	if got := field(t, event.Raw, "delegation_id"); got != want {
		t.Fatalf("the binding was told about delegation %q, want %q", got, want)
	}
}

// nextEvent reads one translated event, failing rather than hanging.
func nextEvent(t *testing.T, client *gptlive.Client) realtimeclient.Event {
	t.Helper()
	select {
	case event, open := <-client.Events():
		if !open {
			t.Fatalf("the translated event stream closed: %v", client.Err())
		}
		return event
	case <-time.After(3 * time.Second):
		t.Fatal("no translated event arrived")
		return realtimeclient.Event{}
	}
}

// expect reads one event and asserts its type.
func expect(t *testing.T, client *gptlive.Client, eventType string) realtimeclient.Event {
	t.Helper()
	event := nextEvent(t, client)
	if event.Type != eventType {
		t.Fatalf("translated event was %s, want %s (%s)", event.Type, eventType, event.Raw)
	}
	return event
}

func field(t *testing.T, raw []byte, name string) string {
	t.Helper()
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode translated event: %v", err)
	}
	return decodeString(decoded[name])
}

// TestHandshakeCarriesTheSessionConfiguration checks the one message whose
// contents can never be corrected later: everything that matters about a Live
// session is fixed in session.start.
func TestHandshakeCarriesTheSessionConfiguration(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, nil)
	start(t, fake, client, "Be brief.")

	message := fake.awaitSent(t, "session.start")
	var session struct {
		Model        string `json:"model"`
		Instructions string `json:"instructions"`
		Audio        struct {
			Format struct {
				Type string `json:"type"`
				Rate int    `json:"rate"`
			} `json:"format"`
			Output struct {
				Voice string `json:"voice"`
			} `json:"output"`
		} `json:"audio"`
		Delegation struct {
			Type string `json:"type"`
		} `json:"delegation"`
	}
	if err := json.Unmarshal(message["session"], &session); err != nil {
		t.Fatalf("decode the session object: %v", err)
	}
	if session.Model != gptlive.DefaultModel {
		t.Errorf("session.start carried model %q, want %q", session.Model, gptlive.DefaultModel)
	}
	if session.Instructions != "Be brief." {
		t.Errorf("session.start carried instruction %q", session.Instructions)
	}
	if session.Audio.Format.Type != "audio/pcm" || session.Audio.Format.Rate != 24000 {
		t.Errorf("session.start asked for %s at %d Hz, want audio/pcm at 24000",
			session.Audio.Format.Type, session.Audio.Format.Rate)
	}
	if session.Audio.Output.Voice != gptlive.DefaultVoice {
		t.Errorf("session.start asked for voice %q", session.Audio.Output.Voice)
	}
	// This is the whole architectural claim of the adapter: the reasoner is the
	// backend, so the endpoint must delegate to this process and not to a
	// Responses model of its own.
	if session.Delegation.Type != "client" {
		t.Errorf("session.start selected %q delegation, want client", session.Delegation.Type)
	}
	if auth := fake.authorization(); auth != "Bearer test-key" {
		t.Errorf("the endpoint was dialled with Authorization %q", auth)
	}
}

// TestAudioIsHeldUntilTheSessionStarts checks the vendor's ordering rule. The
// caller streams from the moment it connects; the endpoint accepts nothing
// before it has confirmed the session.
func TestAudioIsHeldUntilTheSessionStarts(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, nil)

	frame := base64.StdEncoding.EncodeToString(make([]byte, 480))
	if err := client.Send(t.Context(), map[string]any{
		"type": "session.update", "session": map[string]any{"instructions": "Be brief."},
	}); err != nil {
		t.Fatalf("send session.update: %v", err)
	}
	fake.awaitSent(t, "session.start")
	if err := client.Send(t.Context(), map[string]any{
		"type": "input_audio_buffer.append", "audio": frame,
	}); err != nil {
		t.Fatalf("send audio: %v", err)
	}
	// The endpoint has not confirmed the session, so the frame must still be
	// held rather than on the wire.
	fake.refuteSent(t, "session.input_audio.append")

	fake.emit(map[string]any{
		"type": "session.started", "session": map[string]any{"id": "live_abc123"},
	})
	held := fake.awaitSent(t, "session.input_audio.append")
	if got := decodeString(held["audio"]); got != frame {
		t.Errorf("the released frame was %q, want the one that was held", got)
	}
}

// TestUserTurnClosesOnSilence checks the fallback boundary: a turn the endpoint
// answers itself still has to reach the trajectory, or the reasoner is later
// asked to continue a conversation with holes in it.
func TestUserTurnClosesOnSilence(t *testing.T) {
	fake := newFakeLive(t)
	client, scheduler := connect(t, fake, func(config *gptlive.Config) {
		config.InputTurnGap = 900 * time.Millisecond
	})
	start(t, fake, client, "Be brief.")

	fake.emit(map[string]any{
		"type": "session.input_transcript.delta", "delta": "What is",
		"start_ms": 1000, "end_ms": 1200,
	})
	started := expect(t, client, "input_audio_buffer.speech_started")
	if got := field(t, started.Raw, "audio_start_ms"); got != "" {
		t.Errorf("audio_start_ms decoded as a string %q", got)
	}
	fragment(t, client, "What is")
	fake.emit(map[string]any{
		"type": "session.input_transcript.delta", "delta": " the weather?",
		"start_ms": 1200, "end_ms": 1800,
	})
	fragment(t, client, " the weather?")

	// Short of the gap nothing closes: a pause for breath is not a turn end.
	scheduler.AdvanceNS(uint64(500 * time.Millisecond))
	select {
	case event := <-client.Events():
		t.Fatalf("a turn closed after 500ms of silence: %s", event.Type)
	case <-time.After(50 * time.Millisecond):
	}

	scheduler.AdvanceNS(uint64(900 * time.Millisecond))
	expect(t, client, "input_audio_buffer.speech_stopped")
	completed := expect(t, client, "conversation.item.input_audio_transcription.completed")
	if got := field(t, completed.Raw, "transcript"); got != "What is the weather?" {
		t.Errorf("the committed turn was %q, want the two fragments joined exactly", got)
	}
}

// TestDelegationClosesTheUserTurnImmediately checks the reliable boundary. The
// endpoint asking for backend help is its own judgement that the request is
// complete, and it arrives at the moment the reasoner should start - waiting
// out a silence gap after that would add latency for nothing.
func TestDelegationClosesTheUserTurnImmediately(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, nil)
	start(t, fake, client, "Delegate anything that needs a lookup.")

	fake.emit(map[string]any{
		"type": "session.input_transcript.delta", "delta": "Where is my order?",
		"start_ms": 1000, "end_ms": 2400,
	})
	expect(t, client, "input_audio_buffer.speech_started")
	fragment(t, client, "Where is my order?")
	fake.emit(map[string]any{
		"type": "session.delegation.created", "offset_ms": 2500,
		"delegation": map[string]any{
			"id": "item_9tA2bF3h7K9m2P5q8R1s4", "type": "delegation", "target": "client",
		},
	})
	// No clock advance: the boundary comes from the endpoint, not the timer.
	expect(t, client, "input_audio_buffer.speech_stopped")
	completed := expect(t, client, "conversation.item.input_audio_transcription.completed")
	if got := field(t, completed.Raw, "transcript"); got != "Where is my order?" {
		t.Errorf("the delegated turn committed %q", got)
	}
	// And the binding is told, after the turn it refers to has been committed.
	delegated(t, client, "item_9tA2bF3h7K9m2P5q8R1s4")
}

// TestATurnClosedByDelegationIsNotClosedAgain checks the idempotence that makes
// two boundary signals safe. Committing twice would hand the reasoner the same
// question again and put a duplicate utterance in the trajectory.
func TestATurnClosedByDelegationIsNotClosedAgain(t *testing.T) {
	fake := newFakeLive(t)
	client, scheduler := connect(t, fake, nil)
	start(t, fake, client, "Be brief.")

	fake.emit(map[string]any{
		"type": "session.input_transcript.delta", "delta": "Where is my order?",
		"start_ms": 1000, "end_ms": 2400,
	})
	expect(t, client, "input_audio_buffer.speech_started")
	fragment(t, client, "Where is my order?")
	fake.emit(map[string]any{
		"type":       "session.delegation.created",
		"delegation": map[string]any{"id": "item_1", "type": "delegation", "target": "client"},
	})
	expect(t, client, "input_audio_buffer.speech_stopped")
	expect(t, client, "conversation.item.input_audio_transcription.completed")
	delegated(t, client, "item_1")

	// A second delegation runs the same close path again with nothing left to
	// commit. This is the guard itself, not the timer: the endpoint can
	// delegate twice for one utterance, and a read failure closes both turns
	// on the way out whatever else has already closed them. The binding is
	// still told about the second request - that is a fact, not a turn.
	fake.emit(map[string]any{
		"type":       "session.delegation.created",
		"delegation": map[string]any{"id": "item_2", "type": "delegation", "target": "client"},
	})
	delegated(t, client, "item_2")
	// The gap that was running when the first delegation arrived must now be
	// inert too.
	scheduler.AdvanceNS(uint64(5 * time.Second))
	select {
	case event := <-client.Events():
		t.Fatalf("the same turn closed twice: %s (%s)", event.Type, event.Raw)
	case <-time.After(50 * time.Millisecond):
	}

	// The stream must still be live: silence above has to mean "nothing to
	// commit", not "the translator stopped".
	fake.emit(map[string]any{
		"type": "session.output_transcript.delta", "delta": "On its way.",
		"start_ms": 3000, "end_ms": 3400,
	})
	expect(t, client, "response.output_audio_transcript.delta")
}

// speechFrame builds an audible PCM16 frame; carrierFrame builds the digital
// silence the endpoint streams between utterances.
func speechFrame(samples int) string {
	payload := make([]byte, samples*2)
	for index := 0; index < samples; index++ {
		value := int16(6000)
		if index%2 == 0 {
			value = -6000
		}
		payload[index*2] = byte(uint16(value))
		payload[index*2+1] = byte(uint16(value) >> 8)
	}
	return base64.StdEncoding.EncodeToString(payload)
}

func carrierFrame(samples int) string {
	return base64.StdEncoding.EncodeToString(make([]byte, samples*2))
}

// TestAudibleAudioKeepsTheUtteranceOpen checks the boundary a transcript alone
// would get wrong: the samples that speak the end of a sentence are still
// arriving after the last word has been reported.
func TestAudibleAudioKeepsTheUtteranceOpen(t *testing.T) {
	fake := newFakeLive(t)
	client, scheduler := connect(t, fake, func(config *gptlive.Config) {
		config.OutputTurnGap = time.Second
		config.FrameInterval = -1
	})
	start(t, fake, client, "Be brief.")

	fake.emit(map[string]any{
		"type": "session.output_transcript.delta", "delta": "It is sunny.",
		"start_ms": 5400, "end_ms": 7200,
	})
	delta := expect(t, client, "response.output_audio_transcript.delta")
	if got := field(t, delta.Raw, "delta"); got != "It is sunny." {
		t.Errorf("forwarded transcript delta was %q", got)
	}

	// Three quarters through the gap, speech arrives with no text.
	scheduler.AdvanceNS(uint64(750 * time.Millisecond))
	frame := speechFrame(240)
	fake.emit(map[string]any{"type": "session.output_audio.delta", "delta": frame})
	audio := expect(t, client, "response.output_audio.delta")
	if got := field(t, audio.Raw, "delta"); got != frame {
		t.Errorf("forwarded audio was altered at a matching rate: %q", got)
	}

	// That speech must have restarted the gap.
	scheduler.AdvanceNS(uint64(500 * time.Millisecond))
	select {
	case event := <-client.Events():
		t.Fatalf("the utterance ended while speech was still arriving: %s", event.Type)
	case <-time.After(50 * time.Millisecond):
	}

	scheduler.AdvanceNS(uint64(time.Second))
	done := expect(t, client, "response.output_audio_transcript.done")
	if got := field(t, done.Raw, "transcript"); got != "It is sunny." {
		t.Errorf("the committed utterance was %q", got)
	}
	expect(t, client, "response.done")
}

// TestTheSilentCarrierNeverHoldsAnUtteranceOpen is the check the vendor's
// documentation does not imply and the real endpoint made unavoidable.
//
// Live's output is a continuous track: it sends a frame every 100 ms for the
// life of the session, and measured against the real endpoint 427 of 438 frames
// in 45 seconds were digital silence. A gap restarted by every frame is a gap
// that never expires, so no utterance would ever be committed and the mirror
// would never see the assistant speak at all.
func TestTheSilentCarrierNeverHoldsAnUtteranceOpen(t *testing.T) {
	fake := newFakeLive(t)
	client, scheduler := connect(t, fake, func(config *gptlive.Config) {
		config.OutputTurnGap = time.Second
		config.FrameInterval = -1
	})
	start(t, fake, client, "Be brief.")

	fake.emit(map[string]any{
		"type": "session.output_transcript.delta", "delta": "Hello.",
		"start_ms": 100, "end_ms": 500,
	})
	expect(t, client, "response.output_audio_transcript.delta")
	fake.emit(map[string]any{"type": "session.output_audio.delta", "delta": speechFrame(240)})
	expect(t, client, "response.output_audio.delta")

	// The carrier resumes at the endpoint's real cadence - a 100 ms frame every
	// 100 ms. Only the hangover's worth is passed on: a pause between words is
	// part of the speech around it, and anything past that is the agent having
	// stopped, which must reach the client as silence rather than as audio.
	fake.emit(map[string]any{"type": "session.output_audio.delta", "delta": carrierFrame(2400)})
	expect(t, client, "response.output_audio.delta")
	fake.emit(map[string]any{"type": "session.output_audio.delta", "delta": carrierFrame(2400)})
	expect(t, client, "response.output_audio.delta")
	for tick := 0; tick < 7; tick++ {
		fake.emit(map[string]any{"type": "session.output_audio.delta", "delta": carrierFrame(2400)})
		scheduler.AdvanceNS(uint64(100 * time.Millisecond))
	}
	// Past the hangover the stream is quiet, so a listener can tell the agent
	// stopped. The utterance itself has not closed yet - the gap is longer.
	select {
	case event := <-client.Events():
		t.Fatalf("carrier past the hangover was forwarded as speech: %s (%s)", event.Type, event.Raw)
	case <-time.After(50 * time.Millisecond):
	}
	scheduler.AdvanceNS(uint64(200 * time.Millisecond))

	// Crossing the gap on carrier alone must commit the utterance: silence had
	// its chance to hold it open and must not have taken it.
	scheduler.AdvanceNS(uint64(200 * time.Millisecond))
	done := expect(t, client, "response.output_audio_transcript.done")
	if got := field(t, done.Raw, "transcript"); got != "Hello." {
		t.Errorf("the committed utterance was %q", got)
	}
	expect(t, client, "response.done")

	// With no utterance open, the carrier must stop reaching the client
	// entirely: forwarding it would report an assistant that never stops
	// speaking.
	for tick := 0; tick < 5; tick++ {
		fake.emit(map[string]any{"type": "session.output_audio.delta", "delta": carrierFrame(2400)})
	}
	select {
	case event := <-client.Events():
		t.Fatalf("the silent carrier was forwarded outside an utterance: %s (%s)",
			event.Type, event.Raw)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestTheHandoffBecomesCommentaryOnTheOpenDelegation is the point of the
// adapter: a completed answer has to reach the voice, and in this protocol the
// channel for that is a commentary append correlated with what the endpoint
// asked for.
func TestTheHandoffBecomesCommentaryOnTheOpenDelegation(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, nil)
	start(t, fake, client, "Delegate lookups.")

	fake.emit(map[string]any{
		"type": "session.input_transcript.delta", "delta": "Where is my order?",
		"start_ms": 1000, "end_ms": 2400,
	})
	expect(t, client, "input_audio_buffer.speech_started")
	fragment(t, client, "Where is my order?")
	fake.emit(map[string]any{
		"type":       "session.delegation.created",
		"delegation": map[string]any{"id": "item_abc", "type": "delegation", "target": "client"},
	})
	expect(t, client, "input_audio_buffer.speech_stopped")
	expect(t, client, "conversation.item.input_audio_transcription.completed")
	delegated(t, client, "item_abc")

	// This is exactly the pair the binding's conversation-item hand-off sends.
	if err := client.Send(t.Context(), map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": "system",
			"content": []map[string]any{{"type": "input_text", "text": "It shipped today."}},
		},
	}); err != nil {
		t.Fatalf("send the hand-off item: %v", err)
	}
	// An item on its own says nothing yet.
	fake.refuteSent(t, "session.commentary.append")

	if err := client.Send(t.Context(), map[string]any{"type": "response.create"}); err != nil {
		t.Fatalf("send response.create: %v", err)
	}
	commentary := fake.awaitSent(t, "session.commentary.append")
	if got := decodeString(commentary["content"]); got != "It shipped today." {
		t.Errorf("commentary carried %q", got)
	}
	if got := decodeString(commentary["delegation_id"]); got != "item_abc" {
		t.Errorf("commentary was returned against %q, want the open delegation item_abc", got)
	}
	// The Realtime turn loop has no meaning here and must not reach the wire.
	fake.refuteSent(t, "conversation.item.create")
	fake.refuteSent(t, "response.create")
}

// TestAnUnrequestedAnswerIsStillSpoken checks the null delegation ID, which the
// vendor requires to be present rather than omitted. The binding reasons over
// every turn, including ones the endpoint chose to answer by itself.
func TestAnUnrequestedAnswerIsStillSpoken(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, nil)
	start(t, fake, client, "Be brief.")

	if err := client.Send(t.Context(), map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": "system",
			"content": []map[string]any{{"type": "input_text", "text": "Nothing was asked."}},
		},
	}); err != nil {
		t.Fatalf("send the hand-off item: %v", err)
	}
	if err := client.Send(t.Context(), map[string]any{"type": "response.create"}); err != nil {
		t.Fatalf("send response.create: %v", err)
	}
	commentary := fake.awaitSent(t, "session.commentary.append")
	raw, ok := commentary["delegation_id"]
	if !ok {
		t.Fatal("commentary omitted delegation_id, which the endpoint requires to be present")
	}
	if string(raw) != "null" {
		t.Errorf("delegation_id was %s, want null when the endpoint asked for nothing", raw)
	}
}

// TestALongAnswerIsSplitRatherThanRejected checks the 500-token append cap. A
// hand-off that arrives in two pieces is a hand-off; one rejected for length is
// the binding's entire contribution silently lost.
func TestALongAnswerIsSplitRatherThanRejected(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, nil)
	start(t, fake, client, "Be brief.")

	// Every sentence is distinct. Repeated filler would make a reordered or
	// dropped chunk reassemble into exactly the original, which is a fixture
	// that cannot fail rather than a check that passes.
	var builder strings.Builder
	for index := range 60 {
		fmt.Fprintf(&builder, "Parcel %d left the depot at nine and is out for delivery. ", index)
	}
	answer := strings.TrimSpace(builder.String())
	if err := client.Send(t.Context(), map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": "system",
			"content": []map[string]any{{"type": "input_text", "text": answer}},
		},
	}); err != nil {
		t.Fatalf("send the hand-off item: %v", err)
	}
	if err := client.Send(t.Context(), map[string]any{"type": "response.create"}); err != nil {
		t.Fatalf("send response.create: %v", err)
	}
	fake.awaitSent(t, "session.commentary.append")
	time.Sleep(100 * time.Millisecond)

	var chunks []string
	for _, message := range fake.sent() {
		if decodeString(message["type"]) == "session.commentary.append" {
			chunks = append(chunks, decodeString(message["content"]))
		}
	}
	if len(chunks) < 2 {
		t.Fatalf("a %d-character answer went out as %d append(s); it exceeds the cap",
			len(answer), len(chunks))
	}
	for index, chunk := range chunks {
		if count := len([]rune(chunk)); count > 1400 {
			t.Errorf("append %d carried %d runes, over the chunk bound", index+1, count)
		}
	}
	// Splitting must not lose or reorder words. The answer carries facts and
	// identifiers the reasoner established and the hand-off directive says to
	// preserve exactly, so reassembly has to be the original text.
	if rejoined := strings.Join(chunks, " "); rejoined != answer {
		t.Errorf("the reassembled answer differs from the original:\n got %q\nwant %q",
			rejoined, answer)
	}
}

// TestModerationCommitsWhatWasSaid checks the case the vendor warns about:
// moderation can cut off speech without ending the session. What was said up to
// that point really was said, and must not be lost behind the failure.
func TestModerationCommitsWhatWasSaid(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, nil)
	start(t, fake, client, "Be brief.")

	fake.emit(map[string]any{
		"type": "session.output_transcript.delta", "delta": "Here is what I found",
		"start_ms": 100, "end_ms": 900,
	})
	expect(t, client, "response.output_audio_transcript.delta")
	fake.emit(map[string]any{
		"type": "error", "event_id": "evt_error_001",
		"error": map[string]any{
			"type": "invalid_request_error", "code": "content_filter",
			"message": "Assistant audio was stopped.",
		},
	})
	done := expect(t, client, "response.output_audio_transcript.done")
	if got := field(t, done.Raw, "transcript"); got != "Here is what I found" {
		t.Errorf("the interrupted utterance committed %q", got)
	}
	expect(t, client, "response.done")
	failure := expect(t, client, "error")
	if !strings.Contains(string(failure.Raw), "content_filter") {
		t.Errorf("the translated error lost its code: %s", failure.Raw)
	}
}

// TestClosingFinalisesTheSession checks that the one event carrying final usage
// is asked for and waited on. Dropping the socket first prevents its delivery.
func TestClosingFinalisesTheSession(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, func(config *gptlive.Config) {
		config.CloseTimeout = 2 * time.Second
	})
	start(t, fake, client, "Be brief.")
	// The handshake has to have landed before a close can be meaningful.
	fake.awaitSent(t, "session.start")
	time.Sleep(50 * time.Millisecond)

	go func() {
		fake.awaitSent(t, "session.close")
		fake.emit(map[string]any{
			"type": "session.closed", "reason": "close_requested",
			"usage": map[string]any{"seconds": 12},
		})
	}()
	closed := make(chan error, 1)
	go func() { closed <- client.Close() }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return after the endpoint finalised the session")
	}
	fake.awaitSent(t, "session.close")
}

// TestSixteenKilohertzSessionsResampleBothWays checks the non-default rate. The
// caller speaks a protocol that says 24 kHz whatever the session was opened at.
func TestSixteenKilohertzSessionsResampleBothWays(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, func(config *gptlive.Config) {
		config.SessionSampleRateHz = 16000
	})
	start(t, fake, client, "Be brief.")

	message := fake.awaitSent(t, "session.start")
	if !strings.Contains(string(message["session"]), `"rate":16000`) {
		t.Errorf("session.start did not ask for 16 kHz: %s", message["session"])
	}

	// 2400 samples at 24 kHz is 100ms, which is 1600 samples at 16 kHz.
	if err := client.Send(t.Context(), map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString(make([]byte, 4800)),
	}); err != nil {
		t.Fatalf("send audio: %v", err)
	}
	appended := fake.awaitSent(t, "session.input_audio.append")
	converted, err := base64.StdEncoding.DecodeString(decodeString(appended["audio"]))
	if err != nil {
		t.Fatalf("decode the resampled frame: %v", err)
	}
	if samples := len(converted) / 2; samples < 1500 || samples > 1700 {
		t.Errorf("100ms of 24 kHz audio became %d samples at 16 kHz, want about 1600", samples)
	}
}

// TestAnUnexpectedResponsesDelegationIsReported checks the misconfiguration
// that would silently route the work somewhere this process cannot see.
func TestAnUnexpectedResponsesDelegationIsReported(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, nil)
	start(t, fake, client, "Be brief.")

	fake.emit(map[string]any{
		"type": "session.delegation.created",
		"delegation": map[string]any{
			"id": "item_x", "type": "delegation", "target": "responses",
			"response_id": "resp_1",
		},
	})
	failure := expect(t, client, "error")
	if !strings.Contains(string(failure.Raw), "unexpected_responses_delegation") {
		t.Errorf("a Responses delegation was not reported: %s", failure.Raw)
	}
}

// TestHousekeepingIsNamed checks that the endpoint's acknowledgements, usage
// snapshots, and notices reach the binding under names no Realtime endpoint
// would send. None of them changes the conversation; each is a fact an
// operator or a test needs, and an acknowledgement is the only proof an append
// was ever injected.
func TestHousekeepingIsNamed(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, nil)
	start(t, fake, client, "Be brief.")

	fake.emit(map[string]any{
		"type": "session.commentary.appended", "client_event_id": "openrealtime_handoff_1",
		"start_ms": 1400, "end_ms": 1600,
	})
	ack := expect(t, client, "openrealtime.upstream.ack")
	if got := field(t, ack.Raw, "client_event_id"); got != "openrealtime_handoff_1" {
		t.Errorf("the acknowledgement lost its correlation: %s", ack.Raw)
	}
	if got := field(t, ack.Raw, "of"); got != "session.commentary.appended" {
		t.Errorf("the acknowledgement lost what it acknowledges: %s", ack.Raw)
	}
	fake.emit(map[string]any{"type": "session.input_audio.muted", "client_event_id": "openrealtime_session.input_audio.mute"})
	expect(t, client, "openrealtime.upstream.ack")
	fake.emit(map[string]any{
		"type": "session.usage.updated", "usage": map[string]any{"seconds": 12.5},
		"context_window": map[string]any{"usage_ratio": 0.42},
	})
	usage := expect(t, client, "openrealtime.upstream.usage")
	if !strings.Contains(string(usage.Raw), "12.5") || !strings.Contains(string(usage.Raw), "0.42") {
		t.Errorf("usage lost its numbers: %s", usage.Raw)
	}
	fake.emit(map[string]any{"type": "info", "code": "data_channel_permissions", "message": "restricted"})
	info := expect(t, client, "openrealtime.upstream.info")
	if !strings.Contains(string(info.Raw), "data_channel_permissions") {
		t.Errorf("info lost its code: %s", info.Raw)
	}
}

// TestNoPhantomResponseWhenTheAssistantNeverSpoke checks the other half of the
// close guard. Closing a session flushes both turns, and a response.done for an
// utterance that never began tells the mirror to finish a response that does
// not exist - which on the session-instruction hand-off restores an instruction
// nobody borrowed, and on any endpoint commits an empty assistant turn.
func TestNoPhantomResponseWhenTheAssistantNeverSpoke(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, nil)
	start(t, fake, client, "Be brief.")

	fake.emit(map[string]any{
		"type": "session.input_transcript.delta", "delta": "Hello?",
		"start_ms": 100, "end_ms": 600,
	})
	expect(t, client, "input_audio_buffer.speech_started")
	fragment(t, client, "Hello?")
	fake.emit(map[string]any{
		"type":       "session.delegation.created",
		"delegation": map[string]any{"id": "item_1", "type": "delegation", "target": "client"},
	})
	expect(t, client, "input_audio_buffer.speech_stopped")
	expect(t, client, "conversation.item.input_audio_transcription.completed")
	delegated(t, client, "item_1")

	// An error closes both turns. The assistant has said nothing, so there is
	// no utterance to end.
	fake.emit(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type": "invalid_request_error", "code": "rate_limit_exceeded", "message": "slow down",
		},
	})
	event := expect(t, client, "error")
	if !strings.Contains(string(event.Raw), "rate_limit_exceeded") {
		t.Errorf("the translated error lost its code: %s", event.Raw)
	}
	select {
	case extra := <-client.Events():
		t.Fatalf("a response was ended that never started: %s (%s)", extra.Type, extra.Raw)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestTheDelegationClosesWhenItsAnswerHasBeenSpoken checks that a finished
// request stops collecting results. Returning a later answer against a
// delegation the endpoint has already closed attaches it to work that is over,
// and the vendor accepts only a known open ID.
func TestTheDelegationClosesWhenItsAnswerHasBeenSpoken(t *testing.T) {
	fake := newFakeLive(t)
	client, scheduler := connect(t, fake, func(config *gptlive.Config) {
		config.OutputTurnGap = time.Second
	})
	start(t, fake, client, "Delegate lookups.")

	// The user speaks first, so the delegation that follows has an observable
	// effect. In the binding the same ordering is causal rather than incidental:
	// the reasoner is started by the transcript this delegation commits, so a
	// hand-off can never overtake the delegation it answers.
	fake.emit(map[string]any{
		"type": "session.input_transcript.delta", "delta": "Where is my order?",
		"start_ms": 1000, "end_ms": 2400,
	})
	expect(t, client, "input_audio_buffer.speech_started")
	fragment(t, client, "Where is my order?")
	fake.emit(map[string]any{
		"type":       "session.delegation.created",
		"delegation": map[string]any{"id": "item_first", "type": "delegation", "target": "client"},
	})
	expect(t, client, "input_audio_buffer.speech_stopped")
	expect(t, client, "conversation.item.input_audio_transcription.completed")
	delegated(t, client, "item_first")
	handOff(t, client, "It shipped today.")
	first := fake.awaitSent(t, "session.commentary.append")
	if got := decodeString(first["delegation_id"]); got != "item_first" {
		t.Fatalf("the first answer was returned against %q, want item_first", got)
	}

	// The endpoint says it, and then stops speaking.
	fake.emit(map[string]any{
		"type": "session.output_transcript.delta", "delta": "It shipped today.",
		"start_ms": 100, "end_ms": 900,
	})
	expect(t, client, "response.output_audio_transcript.delta")
	scheduler.AdvanceNS(uint64(time.Second))
	expect(t, client, "response.output_audio_transcript.done")
	expect(t, client, "response.done")

	// A later answer the endpoint did not ask for is session context, not a
	// continuation of the request it has finished.
	handOff(t, client, "One more thing.")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var appends []map[string]json.RawMessage
		for _, message := range fake.sent() {
			if decodeString(message["type"]) == "session.commentary.append" {
				appends = append(appends, message)
			}
		}
		if len(appends) >= 2 {
			second := appends[len(appends)-1]
			if got := string(second["delegation_id"]); got != "null" {
				t.Fatalf("the second answer was returned against %s, want null: "+
					"item_first was closed when its answer was spoken", got)
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the second answer never reached the endpoint")
}

// handOff sends the item-then-response pair the binding uses to give the remote
// a completed answer.
func handOff(t *testing.T, client *gptlive.Client, answer string) {
	t.Helper()
	if err := client.Send(t.Context(), map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": "system",
			"content": []map[string]any{{"type": "input_text", "text": answer}},
		},
	}); err != nil {
		t.Fatalf("send the hand-off item: %v", err)
	}
	if err := client.Send(t.Context(), map[string]any{"type": "response.create"}); err != nil {
		t.Fatalf("send response.create: %v", err)
	}
}

// TestADroppedConnectionStillCommitsWhatWasHeard checks the last turn of a
// failed session. The words were heard and the endpoint reported them; losing
// them because the socket died means the reasoner never sees the last thing
// anyone said, on every disconnection.
func TestADroppedConnectionStillCommitsWhatWasHeard(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, nil)
	start(t, fake, client, "Be brief.")

	fake.emit(map[string]any{
		"type": "session.input_transcript.delta", "delta": "Can you check my",
		"start_ms": 1000, "end_ms": 1900,
	})
	expect(t, client, "input_audio_buffer.speech_started")
	fragment(t, client, "Can you check my")

	// The gap has not elapsed, so nothing has closed this turn yet.
	fake.hangUp()

	expect(t, client, "input_audio_buffer.speech_stopped")
	completed := expect(t, client, "conversation.item.input_audio_transcription.completed")
	if got := field(t, completed.Raw, "transcript"); got != "Can you check my" {
		t.Errorf("the interrupted turn committed %q", got)
	}
	// The stream then ends. Whether a drop with no close frame is reported as
	// a failure is a convention this adapter shares with the other clients
	// here rather than one it sets: they all treat EOF as a clean ending, so
	// what is asserted is the part this package is responsible for.
	select {
	case _, open := <-client.Events():
		if open {
			t.Error("the translated stream produced another event after the connection dropped")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the translated stream never closed after the connection dropped")
	}
}

// countSent reports how many messages of one type the endpoint has received.
func (fake *fakeLive) countSent(eventType string) int {
	count := 0
	for _, message := range fake.sent() {
		if decodeString(message["type"]) == eventType {
			count++
		}
	}
	return count
}

// TestSilenceKeepsTheEndpointsClockRunning is the check that a reading of the
// event contract does not produce, because the failure it prevents is silent.
//
// A Live session with no input frames arriving accepts a commentary append and
// then does nothing with it: no injection, no acknowledgement, no speech, and
// no error. A caller speaking the Realtime protocol stops sending audio
// whenever the user is quiet, so without this the first pause would stall the
// conversation and look like a model that had decided not to answer.
func TestSilenceKeepsTheEndpointsClockRunning(t *testing.T) {
	fake := newFakeLive(t)
	client, scheduler := connect(t, fake, func(config *gptlive.Config) {
		config.FrameInterval = 20 * time.Millisecond
	})

	// Before the session is confirmed, nothing may go out at all.
	scheduler.AdvanceNS(uint64(200 * time.Millisecond))
	fake.refuteSent(t, "session.input_audio.append")

	start(t, fake, client, "Be brief.")
	fake.awaitSent(t, "session.start")
	// The clock starts when the endpoint confirms the session, which the read
	// goroutine handles, so wait for that before advancing.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		scheduler.AdvanceNS(uint64(20 * time.Millisecond))
		if fake.countSent("session.input_audio.append") > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	frames := fake.countSent("session.input_audio.append")
	if frames == 0 {
		t.Fatal("the caller went quiet and no silence reached the endpoint; " +
			"its clock would never advance and it would never speak")
	}

	// Each frame must be a whole number of 16-bit samples at the session rate:
	// 20ms of 24 kHz mono PCM16 is 960 bytes.
	for _, message := range fake.sent() {
		if decodeString(message["type"]) != "session.input_audio.append" {
			continue
		}
		payload, err := base64.StdEncoding.DecodeString(decodeString(message["audio"]))
		if err != nil {
			t.Fatalf("a keepalive frame was not valid base64: %v", err)
		}
		if len(payload) != 960 {
			t.Errorf("a keepalive frame was %d bytes, want 960 (20ms of 24 kHz PCM16)", len(payload))
		}
		if len(payload)%2 != 0 {
			t.Errorf("a keepalive frame of %d bytes is not whole 16-bit samples", len(payload))
		}
	}
}

// TestTheCallersAudioIsTheClockWhenItIsStreaming checks that the gap filler
// fills gaps. Padding a live stream would displace real speech on the
// endpoint's timeline, which is worse than the stall it is there to prevent.
func TestTheCallersAudioIsTheClockWhenItIsStreaming(t *testing.T) {
	fake := newFakeLive(t)
	client, scheduler := connect(t, fake, func(config *gptlive.Config) {
		config.FrameInterval = 20 * time.Millisecond
	})
	start(t, fake, client, "Be brief.")
	fake.awaitSent(t, "session.start")
	awaitFrameClock(t, fake, scheduler)
	quiesce(t, fake)

	speech := make([]byte, 960)
	for index := range speech {
		speech[index] = byte(index)
	}
	encoded := base64.StdEncoding.EncodeToString(speech)
	if err := client.Send(t.Context(), map[string]any{
		"type": "input_audio_buffer.append", "audio": encoded,
	}); err != nil {
		t.Fatalf("send caller audio: %v", err)
	}
	fake.awaitSent(t, "session.input_audio.append")
	// Frames already on the wire must land before the baseline is taken, or a
	// filler sent earlier arrives during the next step and is counted against
	// it. This test failed exactly that way before the quiesce.
	before := quiesce(t, fake)

	// A whole interval passes with the caller's frame already delivered. The
	// tick must see it and add nothing.
	scheduler.AdvanceNS(uint64(20 * time.Millisecond))
	time.Sleep(50 * time.Millisecond)
	if after := fake.countSent("session.input_audio.append"); after != before {
		t.Errorf("the filler added %d frame(s) for a tick the caller had already "+
			"filled; its audio is the clock", after-before)
	}

	// Once the caller has been quiet for longer than the idle gap, silence
	// resumes - otherwise one frame from the caller would stop the clock for
	// good and the endpoint would stall.
	for tick := 0; tick < 6; tick++ {
		scheduler.AdvanceNS(uint64(20 * time.Millisecond))
	}
	resumed := time.Now().Add(3 * time.Second)
	for time.Now().Before(resumed) {
		if fake.countSent("session.input_audio.append") > before {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if fake.countSent("session.input_audio.append") <= before {
		t.Error("the filler never resumed after the caller stopped sending")
	}

	// The caller's own bytes must have gone out unaltered.
	var sawSpeech bool
	for _, message := range fake.sent() {
		if decodeString(message["type"]) == "session.input_audio.append" &&
			decodeString(message["audio"]) == encoded {
			sawSpeech = true
		}
	}
	if !sawSpeech {
		t.Error("the caller's frame did not reach the endpoint unaltered")
	}
}

// awaitFrameClock advances the manual scheduler until the gap filler is armed
// and has produced its first frame, so a test can start from a running clock.
func awaitFrameClock(t *testing.T, fake *fakeLive, scheduler *clock.Manual) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		scheduler.AdvanceNS(uint64(20 * time.Millisecond))
		if fake.countSent("session.input_audio.append") > 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the frame clock never started")
}

// quiesce waits until the endpoint has stopped receiving new messages and
// returns the settled input-frame count. Advancing a manual clock queues sends
// that cross a real socket, so a count taken immediately afterwards is a count
// of whatever happened to have arrived.
func quiesce(t *testing.T, fake *fakeLive) int {
	t.Helper()
	settled := fake.countSent("session.input_audio.append")
	for attempt := 0; attempt < 50; attempt++ {
		time.Sleep(10 * time.Millisecond)
		current := fake.countSent("session.input_audio.append")
		if current == settled {
			return settled
		}
		settled = current
	}
	t.Fatal("input frames never stopped arriving")
	return settled
}

// TestCloseAfterTheVendorHangsUpIsClean reproduces the real endpoint's
// behaviour: it sends session.closed and then drops the socket. A close
// handshake with a peer that has gone cannot complete, and that is not a
// failed close - the session was finalised, which is the only thing that
// matters.
func TestCloseAfterTheVendorHangsUpIsClean(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, func(config *gptlive.Config) {
		config.CloseTimeout = 2 * time.Second
	})
	start(t, fake, client, "Be brief.")
	fake.awaitSent(t, "session.start")
	time.Sleep(50 * time.Millisecond)

	go func() {
		fake.awaitSent(t, "session.close")
		fake.emit(map[string]any{
			"type": "session.closed", "reason": "close_requested", "usage": map[string]any{"seconds": 4},
		})
		time.Sleep(20 * time.Millisecond)
		fake.hangUp()
	}()
	closed := make(chan error, 1)
	go func() { closed <- client.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("a finalised session's close reported an error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return")
	}
}

// TestAStopIsAudibleAsAStop is the defect Full-Duplex-Bench found on the real
// endpoint, reduced to a unit test.
//
// Live emits a 100 ms frame every 100 ms whether or not anyone is talking. This
// adapter used to pass the whole carrier on for as long as an utterance was
// open, so when the agent genuinely stopped, everything downstream still saw
// audio arriving - and when a later answer began, there had been no silence
// between the two. A barge-in policy, the duplex state, and the bench all read
// that gap; inventing audio across it put the agent's apparent stop 16.4
// seconds after the user took the floor, on a suite that allows one.
func TestAStopIsAudibleAsAStop(t *testing.T) {
	fake := newFakeLive(t)
	client, scheduler := connect(t, fake, func(config *gptlive.Config) {
		config.SilenceHangover = 200 * time.Millisecond
		config.OutputTurnGap = 5 * time.Second
		config.FrameInterval = -1
	})
	start(t, fake, client, "Be brief.")

	// The agent speaks: 300ms of audible audio in three frames.
	for tick := 0; tick < 3; tick++ {
		fake.emit(map[string]any{"type": "session.output_audio.delta", "delta": speechFrame(2400)})
		expect(t, client, "response.output_audio.delta")
	}
	// It stops. Two carrier frames are the hangover and reach the client.
	forwarded := 0
	for tick := 0; tick < 20; tick++ {
		fake.emit(map[string]any{"type": "session.output_audio.delta", "delta": carrierFrame(2400)})
		select {
		case event := <-client.Events():
			if event.Type == "response.output_audio.delta" {
				forwarded++
			}
		case <-time.After(40 * time.Millisecond):
		}
		scheduler.AdvanceNS(uint64(100 * time.Millisecond))
	}
	// Two seconds of carrier must not become two seconds of agent audio.
	if forwarded > 3 {
		t.Fatalf("%d carrier frames were forwarded as speech; the agent stopped after the "+
			"third audible frame and the silence must reach the client as silence", forwarded)
	}
	if forwarded == 0 {
		t.Error("no hangover at all: a genuine pause between words would be clipped")
	}

	// And when it speaks again, that audio still flows - the stream was quiet,
	// not closed.
	fake.emit(map[string]any{"type": "session.output_audio.delta", "delta": speechFrame(2400)})
	expect(t, client, "response.output_audio.delta")
}
