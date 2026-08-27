package gateway_test

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/gateway"
	protocol "github.com/bojieli/OpenRealtime/protocol/openai"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/bojieli/OpenRealtime/trajectory"
	"github.com/coder/websocket"
)

// --- doubles ----------------------------------------------------------------

type staticASR struct {
	text string
	// block, when set, holds the recogniser until it is closed. It stands in
	// for a recogniser under GPU contention - long enough for a client
	// keepalive to give up on the connection.
	block chan struct{}
}

type legacyOwnershipBinding struct{ binding.Binding }

func (bind legacyOwnershipBinding) Ownership() binding.Ownership {
	ownership := bind.Binding.Ownership()
	ownership.Interaction = ""
	return ownership
}

func (staticASR) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "static", Version: "1", Capabilities: v1.Capabilities{}}
}

func (asr staticASR) PushFrame(context.Context, v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	if asr.block != nil {
		<-asr.block
	}
	return nil, nil
}

func (asr staticASR) Finalize(context.Context, uint64) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{RevisionID: 1, StableText: asr.text, Final: true}, nil
}

type scripted struct {
	descriptor continuation.Descriptor
	// completion is how this provider reports it stopped. The zero value is
	// an ordinary stop; a test that cares about truncation sets it.
	completion continuation.Completion
	mu         sync.Mutex
	turns      [][]continuation.Event
	calls      int
	requests   []continuation.Request
}

// stopping makes the provider report a particular completion, which is how a
// test says "this provider ran out of room" rather than "this provider had
// nothing to say".
func (provider *scripted) stopping(completion continuation.Completion) *scripted {
	provider.completion = completion
	return provider
}

// lastRequest is what the provider was actually asked, so a test can check
// what reached it rather than what was meant to.
func (provider *scripted) lastRequest(t *testing.T) continuation.Request {
	t.Helper()
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) == 0 {
		t.Fatal("the provider was never invoked")
	}
	return provider.requests[len(provider.requests)-1]
}

func (provider *scripted) Descriptor() continuation.Descriptor { return provider.descriptor }

func (provider *scripted) Continue(
	_ context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	provider.mu.Lock()
	index := provider.calls
	provider.calls++
	provider.requests = append(provider.requests, request)
	var events []continuation.Event
	if index < len(provider.turns) {
		events = provider.turns[index]
	}
	provider.mu.Unlock()
	for _, event := range events {
		if err := emit(event); err != nil {
			return continuation.Completion{}, err
		}
	}
	if provider.completion.StopReason != "" {
		return provider.completion, nil
	}
	return continuation.Completion{StopReason: "stop"}, nil
}

func fast(turns ...[]continuation.Event) *scripted {
	return &scripted{descriptor: continuation.Descriptor{
		Provider: "test", Model: "fast", Phase: trajectory.PhaseFast, Effort: continuation.EffortMinimal,
		ToolAuthority: continuation.ToolAuthorityPropose, SpeechAuthority: continuation.SpeechAuthorityVoice,
	}, turns: turns}
}

func slow(turns ...[]continuation.Event) *scripted {
	return &scripted{descriptor: continuation.Descriptor{
		Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow, Effort: continuation.EffortHigh,
		ToolAuthority: continuation.ToolAuthorityExecute, SpeechAuthority: continuation.SpeechAuthoritySilent,
	}, turns: turns}
}

type toneSpeech struct{}

func (toneSpeech) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "tone", Version: "1", Capabilities: v1.Capabilities{}}
}

func (toneSpeech) Synthesize(context.Context, v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	return nil, errors.New("unused")
}

func (toneSpeech) Stream(ctx context.Context, plan v1.SpeechPlan, emit func(v1.SpeechChunk) error) error {
	return emit(v1.SpeechChunk{
		ChunkID: "c", CandidateID: plan.CandidateID, SampleRateHz: 24_000,
		PCM16LE: make([]byte, 4800), Final: true,
	})
}

// --- harness ----------------------------------------------------------------

type client struct {
	connection *websocket.Conn
	t          *testing.T
	validator  *protocol.Validator
	received   []map[string]any
}

func startServer(t *testing.T, fastProvider, slowProvider *scripted, transcript string) *httptest.Server {
	t.Helper()
	return startServerWithASR(t, fastProvider, slowProvider, staticASR{text: transcript})
}

func startServerWithASR(
	t *testing.T, fastProvider, slowProvider *scripted, asr staticASR,
) *httptest.Server {
	t.Helper()
	return startServerWithSpeech(t, fastProvider, slowProvider, asr, toneSpeech{})
}

func startServerWithSpeech(
	t *testing.T, fastProvider, slowProvider *scripted, asr staticASR,
	speech v1.StreamingSpeechProvider,
) *httptest.Server {
	t.Helper()
	return startServerWith(t, fastProvider, slowProvider, asr, speech, nil)
}

// startServerWithLogger captures what the operator would have seen, which is
// the only place some failures are reported: a misconfiguration the client
// cannot act on belongs in the log, not on the wire.
func startServerWithLogger(
	t *testing.T, fastProvider, slowProvider *scripted, logs io.Writer,
) *httptest.Server {
	t.Helper()
	return startServerWith(t, fastProvider, slowProvider, staticASR{text: "hello"}, toneSpeech{}, logs)
}

func startServerWith(
	t *testing.T, fastProvider, slowProvider *scripted, asr staticASR,
	speech v1.StreamingSpeechProvider, logs io.Writer,
) *httptest.Server {
	t.Helper()
	bind, err := cascade.New(cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) { return asr, nil },
		Fast:       fastProvider, Slow: slowProvider, Speech: speech,
		Voice: "test-voice", FastMaxTokens: 512,
	})
	if err != nil {
		t.Fatalf("new cascade: %v", err)
	}
	config := gateway.Config{Binding: bind, Model: "openrealtime-test", ValidateWire: true}
	if logs != nil {
		config.Logger = slog.New(slog.NewTextHandler(logs, nil))
	}
	server, err := gateway.New(config)
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	http := httptest.NewServer(server.Handler())
	t.Cleanup(http.Close)
	return http
}

// syncBuffer collects log output from whichever goroutine wrote it.
type syncBuffer struct {
	mu      sync.Mutex
	written strings.Builder
}

func (buffer *syncBuffer) Write(input []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.written.Write(input)
}

func (buffer *syncBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.written.String()
}

func dial(t *testing.T, server *httptest.Server) *client {
	t.Helper()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime?model=openrealtime-test"
	connection, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close(websocket.StatusNormalClosure, "done") })
	return &client{connection: connection, t: t, validator: protocol.NewValidator()}
}

func (client *client) send(value map[string]any) {
	client.t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		client.t.Fatalf("marshal: %v", err)
	}
	if err := client.connection.Write(context.Background(), websocket.MessageText, encoded); err != nil {
		client.t.Fatalf("write: %v", err)
	}
}

// await reads until it sees eventType, validating every base-protocol event it
// passes on the way. That validation is the compatibility claim: a server event
// that does not match the pinned schema fails the test that saw it.
func (client *client) await(eventType string, timeout time.Duration) map[string]any {
	client.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		_, input, err := client.connection.Read(ctx)
		if err != nil {
			client.t.Fatalf("waiting for %q: %v (saw %s)", eventType, err, client.seen())
		}
		var decoded map[string]any
		if err := json.Unmarshal(input, &decoded); err != nil {
			client.t.Fatalf("decode: %v", err)
		}
		client.received = append(client.received, decoded)
		observed, _ := decoded["type"].(string)
		if !strings.HasPrefix(observed, "openrealtime.") {
			message, decodeErr := protocol.Decode(input)
			if decodeErr != nil {
				client.t.Fatalf("server sent an event the pinned registry rejects: %v", decodeErr)
			}
			if err := client.validator.Validate(protocol.ProfileRealtime, protocol.DirectionServer, message); err != nil {
				client.t.Fatalf("server event %q does not match the pinned schema: %v", observed, err)
			}
		}
		if observed == "error" {
			if eventType != "error" {
				client.t.Fatalf("server error while waiting for %q: %v", eventType, decoded["error"])
			}
			return decoded
		}
		if observed == eventType {
			return decoded
		}
	}
}

// awaitOptional reads until it sees eventType or the timeout expires, and
// reports which happened. It is how a test asserts that something does not
// arrive: await would fail on the read deadline instead.
func (client *client) awaitOptional(eventType string, timeout time.Duration) (map[string]any, bool) {
	client.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		_, input, err := client.connection.Read(ctx)
		if err != nil {
			return nil, false
		}
		var decoded map[string]any
		if err := json.Unmarshal(input, &decoded); err != nil {
			client.t.Fatalf("decode: %v", err)
		}
		client.received = append(client.received, decoded)
		if observed, _ := decoded["type"].(string); observed == eventType {
			return decoded, true
		}
	}
}

// messages returns every event of one type the client received, in order.
func (client *client) messages(eventType string) []map[string]any {
	var found []map[string]any
	for _, message := range client.received {
		if observed, _ := message["type"].(string); observed == eventType {
			found = append(found, message)
		}
	}
	return found
}

func (client *client) seen() string {
	types := make([]string, 0, len(client.received))
	for _, message := range client.received {
		observed, _ := message["type"].(string)
		types = append(types, observed)
	}
	return strings.Join(types, ", ")
}

func pcm16Tone(samples int) string {
	audio := make([]byte, samples*2)
	for index := 0; index < samples; index++ {
		value := int16(8000 * math.Sin(2*math.Pi*220*float64(index)/24_000))
		binary.LittleEndian.PutUint16(audio[index*2:], uint16(value))
	}
	return base64.StdEncoding.EncodeToString(audio)
}

func pcm16Silence(samples int) string {
	return base64.StdEncoding.EncodeToString(make([]byte, samples*2))
}

func (client *client) speak() {
	client.t.Helper()
	for index := 0; index < 3; index++ {
		client.send(map[string]any{"type": "input_audio_buffer.append", "audio": pcm16Tone(2400)})
	}
	for index := 0; index < 8; index++ {
		client.send(map[string]any{"type": "input_audio_buffer.append", "audio": pcm16Silence(2400)})
	}
}

// configureManualTurns switches server voice-activity detection off: the
// client will say where its turns end.
func (client *client) configureManualTurns() {
	client.t.Helper()
	client.send(map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime",
			"audio": map[string]any{
				"input": map[string]any{
					"format":         map[string]any{"type": "audio/pcm", "rate": 24000},
					"turn_detection": nil,
				},
				"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
			},
		},
	})
}

// configureText asks for a text session: the same conversation with a
// different output boundary.
func (client *client) configureText() {
	client.t.Helper()
	client.send(map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime", "output_modalities": []string{"text"},
			"audio": map[string]any{
				"input": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
			},
		},
	})
}

// refusingSpeech fails if anything asks it to synthesise. It is how a test
// says "no audio was produced" rather than "no audio was observed".
type refusingSpeech struct{}

func (refusingSpeech) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "refusing", Version: "1", Capabilities: v1.Capabilities{}}
}

func (refusingSpeech) Synthesize(context.Context, v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	return nil, errors.New("a text session must not synthesise")
}

func (refusingSpeech) Stream(context.Context, v1.SpeechPlan, func(v1.SpeechChunk) error) error {
	return errors.New("a text session must not synthesise")
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

func (client *client) configurePCM16(extension map[string]any) {
	session := map[string]any{
		"type": "realtime",
		"audio": map[string]any{
			"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
			"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
		},
	}
	if extension != nil {
		session["openrealtime"] = extension
	}
	client.send(map[string]any{"type": "session.update", "session": session})
}

// --- tests ------------------------------------------------------------------

func TestUnmodifiedRealtimeClientCompletesAVoiceTurn(t *testing.T) {
	server := startServer(t,
		fast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Checking."}},
			[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Forty dollars."}}),
		slow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "The balance is $40.00."}}),
		"what is my balance")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configurePCM16(nil)
	client.await("session.updated", 5*time.Second)

	client.speak()
	client.await("input_audio_buffer.speech_started", 5*time.Second)
	client.await("input_audio_buffer.speech_stopped", 5*time.Second)
	transcription := client.await("conversation.item.input_audio_transcription.completed", 5*time.Second)
	if transcription["transcript"] != "what is my balance" {
		t.Fatalf("unexpected transcript %v", transcription["transcript"])
	}
	client.await("response.created", 5*time.Second)
	delta := client.await("response.output_audio.delta", 5*time.Second)
	audio, err := base64.StdEncoding.DecodeString(delta["delta"].(string))
	if err != nil || len(audio) == 0 {
		t.Fatalf("expected audio on the wire: %v", err)
	}
	client.await("response.done", 5*time.Second)
}

func TestFunctionCallingRoundTripsUnmodified(t *testing.T) {
	server := startServer(t,
		fast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Checking."}},
			[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Forty dollars."}}),
		slow([]continuation.Event{{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "call_1", Name: "get_balance", Arguments: json.RawMessage(`{"account":"A1"}`),
		}}},
			[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "The balance is $40.00."}}),
		"what is my balance")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.send(map[string]any{"type": "session.update", "session": map[string]any{
		"type": "realtime",
		"audio": map[string]any{
			"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
			"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
		},
		"tools": []map[string]any{{
			"type": "function", "name": "get_balance", "description": "read a balance",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		}},
	}})
	client.await("session.updated", 5*time.Second)
	client.speak()

	done := client.await("response.function_call_arguments.done", 10*time.Second)
	if done["name"] != "get_balance" || done["call_id"] != "call_1" {
		t.Fatalf("unexpected call %v", done)
	}
	client.send(map[string]any{"type": "conversation.item.create", "item": map[string]any{
		"type": "function_call_output", "call_id": "call_1", "output": `{"balance":40}`,
	}})
	client.await("conversation.item.created", 5*time.Second)
	// The turn resumes and the voice reports the answer.
	transcript := client.await("response.output_audio_transcript.delta", 10*time.Second)
	if transcript["delta"] == "The balance is $40.00." {
		t.Fatal("slow output must be voiced by the fast provider, not spoken directly")
	}
}

func TestExplicitClientToolFailureDoesNotAutomaticallyRetry(t *testing.T) {
	server := startServer(t,
		fast(
			[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Checking."}},
			[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "That lookup failed; please check the name."}},
			[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "I will try again with that new information."}},
		),
		slow(
			[]continuation.Event{{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
				CallID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"name":"heard"}`),
			}}},
			[]continuation.Event{{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
				CallID: "call_2", Name: "lookup", Arguments: json.RawMessage(`{"name":"corrected"}`),
			}}},
		),
		"find my account")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.send(map[string]any{"type": "session.update", "session": map[string]any{
		"type": "realtime",
		"audio": map[string]any{
			"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
			"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
		},
		"tools": []map[string]any{{
			"type": "function", "name": "lookup", "description": "find an account",
			"parameters": map[string]any{"type": "object"},
		}},
	}})
	client.await("session.updated", 5*time.Second)
	client.speak()

	call := client.await("response.function_call_arguments.done", 10*time.Second)
	if call["call_id"] != "call_1" {
		t.Fatalf("unexpected first call: %v", call)
	}
	// Realtime has no separate error field on function_call_output. This is the
	// explicit failure representation used by OpenAI-compatible tool clients,
	// including tau2-bench's audio-native agent.
	client.send(map[string]any{"type": "conversation.item.create", "item": map[string]any{
		"type": "function_call_output", "call_id": "call_1", "output": "Error: User not found",
	}})
	client.await("conversation.item.created", 5*time.Second)
	if retried, ok := client.awaitOptional("response.function_call_arguments.done", 2*time.Second); ok {
		t.Fatalf("a failed function output automatically reopened slow cognition: %v", retried)
	}
}

// A client that never mentions the extension gets an ordinary session, and the
// server never volunteers the key.
func TestBaseProtocolClientSeesNoExtension(t *testing.T) {
	server := startServer(t, fast(), slow(), "hello")
	client := dial(t, server)
	created := client.await("session.created", 5*time.Second)
	sessionObject := created["session"].(map[string]any)
	if _, present := sessionObject["openrealtime"]; present {
		t.Fatal("the extension must not appear without negotiation")
	}
	client.configurePCM16(nil)
	updated := client.await("session.updated", 5*time.Second)
	if _, present := updated["session"].(map[string]any)["openrealtime"]; present {
		t.Fatal("the extension must not appear without negotiation")
	}
}

func TestExtensionNegotiationEnablesOnlyWhatTheBindingProvides(t *testing.T) {
	server := startServer(t, fast(), slow(), "hello")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configurePCM16(map[string]any{
		"version": openrealtime.Version, "supports": []string{"video.input", "observations", "computer_use"},
	})
	updated := client.await("session.updated", 5*time.Second)
	extension := updated["session"].(map[string]any)["openrealtime"].(map[string]any)
	if int(extension["version"].(float64)) != openrealtime.Version {
		t.Fatalf("unexpected version %v", extension["version"])
	}
	enabled := extension["enabled"].([]any)
	names := make([]string, 0, len(enabled))
	for _, value := range enabled {
		names = append(names, value.(string))
	}
	// This cascade has no video observer configured, so video is not enabled -
	// the client asked, the server answered honestly, and the session works.
	for _, name := range names {
		if name == "video.input" {
			t.Fatal("video must not be enabled without a video observer")
		}
	}
	if len(names) != 2 {
		t.Fatalf("expected observations and computer use, got %v", names)
	}
}

func TestVideoFramesAreRefusedWithoutNegotiation(t *testing.T) {
	server := startServer(t, fast(), slow(), "hello")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.send(map[string]any{
		"type": openrealtime.EventVideoSourceUpdate, "source": "screen",
		"state": "active", "width": 1280, "height": 720,
	})
	failure := client.await("error", 5*time.Second)
	message := failure["error"].(map[string]any)["message"].(string)
	if !strings.Contains(message, "not negotiated") {
		t.Fatalf("expected a negotiation error, got %q", message)
	}
}

func TestUnknownEventIsRejectedWithoutClosingTheSession(t *testing.T) {
	server := startServer(t, fast(), slow(), "hello")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.send(map[string]any{"type": "openrealtime.nonsense"})
	client.await("error", 5*time.Second)
	// The session survives: a bad event is a client mistake, not a connection
	// failure.
	client.configurePCM16(nil)
	client.await("session.updated", 5*time.Second)
}

func TestHealthReportsTheBindingAndProtocol(t *testing.T) {
	server := startServer(t, fast(), slow(), "hello")
	response, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	defer response.Body.Close()
	var health map[string]any
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if health["binding"] != "cascade" {
		t.Fatalf("unexpected binding %v", health["binding"])
	}
	ownership := health["ownership"].(map[string]any)
	if ownership["slow_cognition"] != "engine" {
		t.Fatalf("slow cognition is always the engine's, got %v", ownership["slow_cognition"])
	}
}

func TestHealthExpandsLegacyInteractionOwnership(t *testing.T) {
	bind, err := cascade.New(cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) { return staticASR{text: "hi"}, nil },
		Fast:       fast(), Slow: slow(), Speech: toneSpeech{},
	})
	if err != nil {
		t.Fatalf("new cascade: %v", err)
	}
	server, err := gateway.New(gateway.Config{Binding: legacyOwnershipBinding{bind}})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	listening := httptest.NewServer(server.Handler())
	defer listening.Close()
	health := getHealth(t, listening.URL)
	ownership := health["ownership"].(map[string]any)
	if ownership["interaction"] != ownership["floor"] || ownership["interaction"] != "engine" {
		t.Fatalf("legacy ownership was not expanded at the public seam: %v", ownership)
	}
}

func TestBearerTokenIsRequiredWhenConfigured(t *testing.T) {
	bind, err := cascade.New(cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) { return staticASR{text: "hi"}, nil },
		Fast:       fast(), Slow: slow(), Speech: toneSpeech{},
	})
	if err != nil {
		t.Fatalf("new cascade: %v", err)
	}
	server, err := gateway.New(gateway.Config{Binding: bind, Token: "secret"})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	http := httptest.NewServer(server.Handler())
	defer http.Close()
	url := "ws" + strings.TrimPrefix(http.URL, "http") + "/v1/realtime"
	if _, _, err := websocket.Dial(context.Background(), url, nil); err == nil {
		t.Fatal("expected an unauthenticated dial to be refused")
	}
	if _, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{
		HTTPHeader: map[string][]string{"Authorization": {"Bearer secret"}},
	}); err != nil {
		t.Fatalf("authenticated dial: %v", err)
	}
}

// Two concurrent sessions must be distinguishable. Deriving the identity from
// a session's own item counter names every session in the process
// sess_000000000001, which makes a log useless and anything keyed on the
// identifier wrong.
func TestConcurrentSessionsGetDistinctIdentities(t *testing.T) {
	server := startServer(t, fast(), slow(), "hello")

	seen := map[string]bool{}
	for index := 0; index < 4; index++ {
		client := dial(t, server)
		created := client.await("session.created", 5*time.Second)
		id, _ := created["session"].(map[string]any)["id"].(string)
		if id == "" {
			t.Fatal("session.created must carry an identifier")
		}
		if seen[id] {
			t.Fatalf("session identifier %q was reused", id)
		}
		seen[id] = true
	}
}

// A handler that waits on a model must not keep the socket from answering
// pings. The WebSocket library answers them from inside Read, so a session
// that handled events on the read goroutine would be dropped by any client
// with a keepalive whenever a provider was slow - a fault the system under
// test then gets blamed for.
func TestASlowProviderDoesNotStallTheConnection(t *testing.T) {
	release := make(chan struct{})
	server := startServerWithASR(t, fast(), slow(), staticASR{text: "hello", block: release})
	t.Cleanup(func() { close(release) })

	client := dial(t, server)
	client.await("session.created", 5*time.Second)

	// Audio reaches the recogniser, which is now blocked. Before the handler
	// ran on its own goroutine this held the read loop, and the library
	// answers pings from inside Read.
	client.send(map[string]any{
		"type": "input_audio_buffer.append", "audio": pcm16Tone(2400),
	})

	// A pong is only processed by a concurrent reader, so the client runs one
	// while it waits - the same shape a real client has.
	reading := make(chan struct{})
	go func() {
		defer close(reading)
		for {
			if _, _, err := client.connection.Read(context.Background()); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.connection.Ping(ctx); err != nil {
		t.Fatalf("the connection must answer a ping while the recogniser is busy: %v", err)
	}
}

// The two things an unmodified official client sends that this server used to
// refuse. Both were found by running OpenAI's own SDK against it; both had
// gone unnoticed because nothing ever had.
func TestTheSessionAnOfficialClientSendsIsAccepted(t *testing.T) {
	t.Parallel()
	server := startServer(t,
		fast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Fine."}}),
		slow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Fine."}}),
		"hello")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)

	// This is what @openai/agents-realtime puts on the wire on every
	// connection: a null noise_reduction, which OpenAI's own specification
	// documents as the way to turn it off, and semantic_vad, which is a
	// detector this server does not have.
	client.send(map[string]any{
		"type":     "session.update",
		"event_id": "event_from_the_client",
		"session": map[string]any{
			"type":         "realtime",
			"instructions": "Be brief.",
			"audio": map[string]any{
				"input": map[string]any{
					"format":          map[string]any{"type": "audio/pcm", "rate": 24000},
					"noise_reduction": nil,
					"transcription":   map[string]any{"model": "gpt-4o-mini-transcribe"},
					"turn_detection":  map[string]any{"type": "semantic_vad"},
				},
				"output": map[string]any{
					"format": map[string]any{"type": "audio/pcm", "rate": 24000}, "speed": 1,
				},
			},
		},
	})

	// The confirmation comes first, and everything the deployment can honour
	// is in it. A null noise_reduction is not an error at all.
	updated := client.await("session.updated", 5*time.Second)
	session := updated["session"].(map[string]any)
	if session["instructions"] != "Be brief." {
		t.Fatalf("the rest of the session must be applied, not discarded: %v", session["instructions"])
	}
	detection := session["audio"].(map[string]any)["input"].(map[string]any)["turn_detection"].(map[string]any)
	if detection["type"] != "server_vad" {
		t.Fatalf("expected the detector actually in force, got %v", detection["type"])
	}
	if silence, _ := detection["silence_duration_ms"].(float64); silence <= 0 {
		t.Fatalf("the session must be left with a usable gate, got %v", detection)
	}

	// And then the parts that could not be honoured, each by name. Quietly
	// substituting a different detector would leave the client believing it
	// got the endpointing behaviour it asked for; the same is true of the
	// recogniser, and this client asks for both. A session.update can carry
	// more than one field this deployment does not apply, so each gets its own
	// error rather than the first one standing for the rest.
	refusals := map[string]map[string]any{}
	for range 2 {
		event, ok := client.awaitOptional("error", 5*time.Second)
		if !ok {
			break
		}
		failure, _ := event["error"].(map[string]any)
		param, _ := failure["param"].(string)
		refusals[param] = failure
	}
	failure, named := refusals["session.audio.input.turn_detection.type"]
	if !named {
		t.Fatalf("the detector this deployment does not have must be refused by name, saw %v",
			slices.Collect(maps.Keys(refusals)))
	}
	if _, named := refusals["session.audio.input.transcription.model"]; !named {
		t.Fatalf("the recogniser this deployment does not have must be refused by name, saw %v",
			slices.Collect(maps.Keys(refusals)))
	}
	if failure["code"] != "unsupported_value" {
		t.Fatalf("expected an unsupported-value error, got %v", failure)
	}
	if failure["event_id"] != "event_from_the_client" {
		t.Fatalf("the error must identify the request that caused it, got %v", failure["event_id"])
	}
	if message, _ := failure["message"].(string); !strings.Contains(message, "semantic_vad") {
		t.Fatalf("the error must say what was asked for, got %q", message)
	}

	// The session is still usable, which is what makes this an error about a
	// field rather than a failure of the event.
	client.speak()
	client.await("input_audio_buffer.speech_started", 5*time.Second)
	client.await("response.created", 10*time.Second)
}

// A supported detector produces no complaint at all. The loud error has to be
// about the field being unsupported, not about turn detection being mentioned.
func TestASupportedDetectorIsAcceptedSilently(t *testing.T) {
	t.Parallel()
	server := startServer(t,
		fast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Fine."}}),
		slow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Fine."}}),
		"hello")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.send(map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime",
			"audio": map[string]any{
				"input": map[string]any{"turn_detection": map[string]any{"type": "server_vad"}},
			},
		},
	})
	client.await("session.updated", 5*time.Second)

	// Nothing follows the confirmation, so the next thing the session produces
	// is the turn rather than a complaint about it.
	client.speak()
	if event := client.await("input_audio_buffer.speech_started", 5*time.Second); event == nil {
		t.Fatal("expected the session to proceed")
	}
	for _, seen := range client.received {
		if seen["type"] == "error" {
			t.Fatalf("a supported detector must produce no error: %v", seen)
		}
	}
}

// A client that names a detector without naming its parameters is asking for
// the deployment's, not for zero.
func TestTurnDetectionParametersAreOptional(t *testing.T) {
	t.Parallel()
	server := startServer(t,
		fast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Fine."}}),
		slow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Fine."}}),
		"hello")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.send(map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime",
			"audio": map[string]any{
				"input": map[string]any{"turn_detection": map[string]any{"type": "server_vad"}},
			},
		},
	})
	updated := client.await("session.updated", 5*time.Second)
	detection := updated["session"].(map[string]any)["audio"].(map[string]any)["input"].(map[string]any)["turn_detection"].(map[string]any)
	if silence, _ := detection["silence_duration_ms"].(float64); silence <= 0 {
		t.Fatalf("unspecified parameters must resolve to the deployment's: %v", detection)
	}

	// One the client did specify is honoured, so the defaults fill gaps rather
	// than overriding intent.
	client.send(map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime",
			"audio": map[string]any{
				"input": map[string]any{"turn_detection": map[string]any{
					"type": "server_vad", "silence_duration_ms": 900,
				}},
			},
		},
	})
	updated = client.await("session.updated", 5*time.Second)
	detection = updated["session"].(map[string]any)["audio"].(map[string]any)["input"].(map[string]any)["turn_detection"].(map[string]any)
	if silence, _ := detection["silence_duration_ms"].(float64); silence != 900 {
		t.Fatalf("a specified parameter must be honoured, got %v", detection)
	}
}

// A recogniser that is getting slower is the run-up to one that stops, and
// until this reached /healthz the only observable signal was the failure.
func TestHealthReportsTheRecogniserWhenTheBindingOwnsOne(t *testing.T) {
	bind, err := cascade.New(cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) { return staticASR{text: "hi"}, nil },
		Fast:       fast(), Slow: slow(), Speech: toneSpeech{},
	})
	if err != nil {
		t.Fatal(err)
	}
	reported := gateway.RecogniserSnapshot{
		Utterances: 7, InFlight: 1,
		AdvanceInvocations: 40, AdvanceElapsedNS: 400, AdvanceMeanElapsedNS: 10, AdvanceMaxElapsedNS: 90,
	}
	server, err := gateway.New(gateway.Config{
		Binding: bind, Recogniser: func() gateway.RecogniserSnapshot { return reported },
	})
	if err != nil {
		t.Fatal(err)
	}
	listening := httptest.NewServer(server.Handler())
	defer listening.Close()

	health := getHealth(t, listening.URL)
	recogniser, present := health["recogniser"].(map[string]any)
	if !present {
		t.Fatalf("a cascade deployment should report its recogniser, got %v", health["recogniser"])
	}
	if recogniser["utterances"] != float64(7) || recogniser["in_flight"] != float64(1) {
		t.Fatalf("unexpected utterance counts: %v", recogniser)
	}
	// The mean is the number an operator alerts on: a maximum jumps once on a
	// single bad call and stays there for the life of the process.
	if recogniser["advance_mean_elapsed_ns"] != float64(10) {
		t.Fatalf("mean advance = %v, want 10", recogniser["advance_mean_elapsed_ns"])
	}
	if recogniser["advance_max_elapsed_ns"] != float64(90) {
		t.Fatalf("peak advance = %v, want 90", recogniser["advance_max_elapsed_ns"])
	}
}

// On omni, duplex, and upstream the model hears the user directly. Reporting
// zeroes there would read as a recogniser answering instantly.
func TestHealthOmitsTheRecogniserWhenThereIsNoneToTime(t *testing.T) {
	server := startServer(t, fast(), slow(), "hello")
	if health := getHealth(t, server.URL); health["recogniser"] != nil {
		t.Fatalf("an unconfigured recogniser should be absent, got %v", health["recogniser"])
	}
}

func getHealth(t *testing.T, base string) map[string]any {
	t.Helper()
	response, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	defer response.Body.Close()
	var health map[string]any
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	return health
}
