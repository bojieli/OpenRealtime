package upstream_test

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

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/upstream"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
	"github.com/coder/websocket"
)

// fakeRemote is a minimal Realtime endpoint: it records what it was sent and
// emits what a test scripts.
type fakeRemote struct {
	server *httptest.Server

	mu       sync.Mutex
	received []map[string]any
	sendTo   chan map[string]any
	ready    chan struct{}
	once     sync.Once
}

func newFakeRemote(t *testing.T) *fakeRemote {
	remote := &fakeRemote{sendTo: make(chan map[string]any, 32), ready: make(chan struct{})}
	remote.server = httptest.NewServer(websocketHandler(t, remote))
	t.Cleanup(remote.server.Close)
	return remote
}

func websocketHandler(t *testing.T, remote *fakeRemote) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
			CompressionMode: websocket.CompressionDisabled,
		})
		if err != nil {
			return
		}
		defer connection.CloseNow()
		ctx := request.Context()
		remote.once.Do(func() { close(remote.ready) })
		go func() {
			for {
				_, input, err := connection.Read(ctx)
				if err != nil {
					return
				}
				var decoded map[string]any
				if json.Unmarshal(input, &decoded) == nil {
					remote.mu.Lock()
					remote.received = append(remote.received, decoded)
					remote.mu.Unlock()
				}
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case message := <-remote.sendTo:
				encoded, _ := json.Marshal(message)
				if err := connection.Write(ctx, websocket.MessageText, encoded); err != nil {
					return
				}
			}
		}
	}
}

func (remote *fakeRemote) url() string {
	return "ws" + strings.TrimPrefix(remote.server.URL, "http")
}

func (remote *fakeRemote) emit(message map[string]any) { remote.sendTo <- message }

func (remote *fakeRemote) sent() []map[string]any {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	return append([]map[string]any(nil), remote.received...)
}

type scriptedSlow struct {
	mu    sync.Mutex
	turns [][]continuation.Event
	calls int
}

func (provider *scriptedSlow) Descriptor() continuation.Descriptor {
	return continuation.Descriptor{
		Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow, Effort: continuation.EffortHigh,
		ToolAuthority: continuation.ToolAuthorityExecute, SpeechAuthority: continuation.SpeechAuthoritySilent,
	}
}

func (provider *scriptedSlow) Continue(
	_ context.Context, _ continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	provider.mu.Lock()
	index := provider.calls
	provider.calls++
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
	return continuation.Completion{StopReason: "stop"}, nil
}

type collectingSink struct {
	mu          sync.Mutex
	transcripts []binding.TranscriptEvent
	toolCalls   []binding.ToolCallEvent
	audioFrames int
	failures    []binding.ErrorEvent
}

func (sink *collectingSink) Activity(context.Context, binding.ActivityEvent) error { return nil }
func (sink *collectingSink) Transcript(_ context.Context, event binding.TranscriptEvent) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.transcripts = append(sink.transcripts, event)
	return nil
}
func (sink *collectingSink) Observation(context.Context, perception.Observation) error { return nil }
func (sink *collectingSink) SpeechBegin(context.Context, action.Utterance) error       { return nil }
func (sink *collectingSink) SpeechAudio(context.Context, action.Utterance, action.Frame) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.audioFrames++
	return nil
}
func (sink *collectingSink) SpeechEnd(context.Context, action.Utterance, action.Outcome) error {
	return nil
}
func (sink *collectingSink) ToolCalls(_ context.Context, event binding.ToolCallEvent) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.toolCalls = append(sink.toolCalls, event)
	return nil
}
func (sink *collectingSink) Failed(_ context.Context, event binding.ErrorEvent) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.failures = append(sink.failures, event)
}

func waitFor(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
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

func start(t *testing.T, remote *fakeRemote, slow continuation.Provider, tools []action.ToolSpec) (binding.Runtime, *collectingSink) {
	t.Helper()
	bind, err := upstream.New(upstream.Config{URL: remote.url(), Slow: slow, Model: "remote-model"})
	if err != nil {
		t.Fatalf("new upstream: %v", err)
	}
	sink := &collectingSink{}
	runtime, err := bind.Start(context.Background(), binding.Options{
		Sink: sink, SessionID: "test", Settings: binding.Settings{Tools: tools},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background(), nil) })
	return runtime, sink
}

func TestOwnershipPutsSlowCognitionInTheEngine(t *testing.T) {
	remote := newFakeRemote(t)
	bind, err := upstream.New(upstream.Config{URL: remote.url(), Slow: &scriptedSlow{}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ownership := bind.Ownership()
	if err := ownership.Validate(); err != nil {
		t.Fatalf("ownership: %v", err)
	}
	if ownership.FastCognition != binding.OwnerRemote || ownership.SlowCognition != binding.OwnerEngine {
		t.Fatalf("the remote owns the voice and the engine owns the reasoner: %+v", ownership)
	}
	if _, err := upstream.New(upstream.Config{URL: remote.url()}); err == nil {
		t.Fatal("a background reasoner is what this binding adds and must be required")
	}
}

// The whole binding in one test: the remote hears the user, the engine's
// reasoner works over the same conversation, and the remote is handed the
// answer to say.
func TestSlowAnswerIsHandedBackToTheRemoteToSay(t *testing.T) {
	remote := newFakeRemote(t)
	slow := &scriptedSlow{turns: [][]continuation.Event{{
		{Kind: continuation.EventAssistantDelta, Text: "The balance is $40.00."},
	}}}
	_, sink := start(t, remote, slow, nil)
	<-remote.ready

	remote.emit(map[string]any{
		"type":    "conversation.item.input_audio_transcription.completed",
		"item_id": "item_1", "transcript": "what is my balance",
	})
	waitFor(t, func() bool {
		for _, message := range remote.sent() {
			if message["type"] == "response.create" {
				return true
			}
		}
		return false
	}, "expected the completed answer to be handed back for the remote to say")

	var handoff string
	for _, message := range remote.sent() {
		if message["type"] != "conversation.item.create" {
			continue
		}
		encoded, _ := json.Marshal(message)
		handoff = string(encoded)
	}
	if !strings.Contains(handoff, "The balance is $40.00.") {
		t.Fatalf("the hand-off must carry the reasoner's answer, got %q", handoff)
	}
	sink.mu.Lock()
	transcripts := len(sink.transcripts)
	sink.mu.Unlock()
	if transcripts == 0 {
		t.Fatal("the client must see the transcript the remote produced")
	}
}

// The remote's own speech is evidence, not a request. Mirroring it must not
// make the reasoner answer the voice model.
func TestRemoteSpeechDoesNotOpenATurn(t *testing.T) {
	remote := newFakeRemote(t)
	slow := &scriptedSlow{}
	runtime, _ := start(t, remote, slow, nil)
	<-remote.ready

	remote.emit(map[string]any{
		"type": "response.output_audio_transcript.done", "transcript": "Let me check that.",
	})
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindObservation &&
				trajectory.AuthorityOf(item) == trajectory.AuthorityObserver {
				return true
			}
		}
		return false
	}, "expected the remote's speech to be mirrored as observer-authority evidence")

	time.Sleep(100 * time.Millisecond)
	slow.mu.Lock()
	calls := slow.calls
	slow.mu.Unlock()
	if calls != 0 {
		t.Fatalf("the reasoner must not answer the voice model, ran %d times", calls)
	}
}

func TestReasonerToolCallsGoToTheClientNotTheRemote(t *testing.T) {
	remote := newFakeRemote(t)
	slow := &scriptedSlow{turns: [][]continuation.Event{
		{{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "call_1", Name: "get_balance", Arguments: json.RawMessage(`{}`),
		}}},
		{{Kind: continuation.EventAssistantDelta, Text: "It is $40.00."}},
	}}
	runtime, sink := start(t, remote, slow, []action.ToolSpec{{
		Name: "get_balance", Description: "read a balance", Parameters: json.RawMessage(`{"type":"object"}`),
	}})
	<-remote.ready

	remote.emit(map[string]any{
		"type":    "conversation.item.input_audio_transcription.completed",
		"item_id": "item_1", "transcript": "what is my balance",
	})
	waitFor(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return len(sink.toolCalls) == 1
	}, "expected the reasoner's call to reach the client")

	for _, message := range remote.sent() {
		if message["type"] == "response.function_call_arguments.done" {
			t.Fatal("the remote must never be asked to execute the reasoner's calls")
		}
	}
	if err := runtime.ToolResult(context.Background(), trajectory.ToolResult{
		CallID: "call_1", Name: "get_balance", Output: json.RawMessage(`{"balance":40}`),
	}); err != nil {
		t.Fatalf("tool result: %v", err)
	}
	waitFor(t, func() bool {
		for _, message := range remote.sent() {
			if message["type"] == "response.create" {
				return true
			}
		}
		return false
	}, "expected the turn to complete with a hand-off after the result")
}

func TestRemoteAudioIsForwardedAndTracksPlayback(t *testing.T) {
	remote := newFakeRemote(t)
	runtime, sink := start(t, remote, &scriptedSlow{}, nil)
	<-remote.ready

	remote.emit(map[string]any{
		"type": "response.output_audio.delta", "item_id": "item_1",
		"delta": base64Encode(make([]byte, 4800)),
	})
	waitFor(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return sink.audioFrames == 1
	}, "expected the remote's audio to reach the client")
	_ = runtime
}

func base64Encode(payload []byte) string { return base64.StdEncoding.EncodeToString(payload) }
