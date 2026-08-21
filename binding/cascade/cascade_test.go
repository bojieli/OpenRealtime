package cascade_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// --- test doubles -----------------------------------------------------------

type scriptedASR struct {
	partials []string
	final    string
	pushes   int
}

func (asr *scriptedASR) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "scripted-asr", Version: "1", Capabilities: v1.Capabilities{}}
}

func (asr *scriptedASR) PushFrame(_ context.Context, _ v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	if asr.pushes >= len(asr.partials) {
		asr.pushes++
		return nil, nil
	}
	text := asr.partials[asr.pushes]
	asr.pushes++
	return []v1.PerceptionRevision{{
		RevisionID: uint64(asr.pushes), StableText: text, Final: false,
	}}, nil
}

func (asr *scriptedASR) Finalize(context.Context, uint64) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{RevisionID: 99, StableText: asr.final, Final: true}, nil
}

type scriptedProvider struct {
	descriptor continuation.Descriptor
	mu         sync.Mutex
	turns      [][]continuation.Event
	calls      int
	requests   []continuation.Request
}

func (provider *scriptedProvider) Descriptor() continuation.Descriptor { return provider.descriptor }

func (provider *scriptedProvider) Continue(
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
	return continuation.Completion{StopReason: "stop"}, nil
}

func (provider *scriptedProvider) invocations() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.calls
}

func newFast(turns ...[]continuation.Event) *scriptedProvider {
	return &scriptedProvider{
		descriptor: continuation.Descriptor{
			Provider: "test", Model: "fast", Phase: trajectory.PhaseFast,
			Effort: continuation.EffortMinimal, ToolAuthority: continuation.ToolAuthorityPropose,
			SpeechAuthority: continuation.SpeechAuthorityVoice,
		},
		turns: turns,
	}
}

func newSlow(turns ...[]continuation.Event) *scriptedProvider {
	return &scriptedProvider{
		descriptor: continuation.Descriptor{
			Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow,
			Effort: continuation.EffortHigh, ToolAuthority: continuation.ToolAuthorityExecute,
			SpeechAuthority: continuation.SpeechAuthoritySilent,
		},
		turns: turns,
	}
}

type toneSpeech struct{ chunks int }

func (toneSpeech) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "tone", Version: "1", Capabilities: v1.Capabilities{}}
}

func (toneSpeech) Synthesize(context.Context, v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	return nil, errors.New("unused")
}

func (speech toneSpeech) Stream(ctx context.Context, plan v1.SpeechPlan, emit func(v1.SpeechChunk) error) error {
	chunks := speech.chunks
	if chunks == 0 {
		chunks = 1
	}
	for index := 0; index < chunks; index++ {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		default:
		}
		if err := emit(v1.SpeechChunk{
			ChunkID: "c", CandidateID: plan.CandidateID, SampleRateHz: 24_000,
			PCM16LE: make([]byte, 4800), Final: index == chunks-1,
		}); err != nil {
			return err
		}
	}
	return nil
}

type recordingSink struct {
	mu           sync.Mutex
	transcripts  []binding.TranscriptEvent
	utterances   []action.Utterance
	frames       int
	spoken       []string
	toolCalls    []binding.ToolCallEvent
	observations []perception.Observation
	failures     []binding.ErrorEvent
}

func (sink *recordingSink) Activity(context.Context, binding.ActivityEvent) error { return nil }

func (sink *recordingSink) Transcript(_ context.Context, event binding.TranscriptEvent) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.transcripts = append(sink.transcripts, event)
	return nil
}

func (sink *recordingSink) Observation(_ context.Context, observation perception.Observation) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.observations = append(sink.observations, observation)
	return nil
}

func (sink *recordingSink) SpeechBegin(_ context.Context, utterance action.Utterance) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.utterances = append(sink.utterances, utterance)
	return nil
}

func (sink *recordingSink) SpeechText(_ context.Context, _ action.Utterance, delta string) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.spoken = append(sink.spoken, delta)
	return nil
}

func (sink *recordingSink) SpeechAudio(context.Context, action.Utterance, action.Frame) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.frames++
	return nil
}

func (sink *recordingSink) SpeechEnd(context.Context, action.Utterance, action.Outcome) error {
	return nil
}

func (sink *recordingSink) ToolCalls(_ context.Context, event binding.ToolCallEvent) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.toolCalls = append(sink.toolCalls, event)
	return nil
}

func (sink *recordingSink) Failed(_ context.Context, event binding.ErrorEvent) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.failures = append(sink.failures, event)
}

func (sink *recordingSink) spokenTexts() []string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]string(nil), sink.spoken...)
}

// --- helpers ----------------------------------------------------------------

func tone(samples int, amplitude float64) []byte {
	audio := make([]byte, samples*2)
	for index := 0; index < samples; index++ {
		value := int16(amplitude * math.Sin(2*math.Pi*220*float64(index)/24_000))
		binary.LittleEndian.PutUint16(audio[index*2:], uint16(value))
	}
	return audio
}

func silence(samples int) []byte { return make([]byte, samples*2) }

func speak(t *testing.T, runtime binding.Runtime, blocks int) {
	t.Helper()
	ctx := context.Background()
	for index := 0; index < blocks; index++ {
		if err := runtime.Audio(ctx, perception.Frame{
			Kind: perception.FrameAudio, Source: "microphone", SampleRateHz: 24_000,
			PCM16LE: tone(2400, 8000),
		}); err != nil {
			t.Fatalf("speak block %d: %v", index, err)
		}
	}
	// Enough silence to cross the endpoint threshold.
	for index := 0; index < 8; index++ {
		if err := runtime.Audio(ctx, perception.Frame{
			Kind: perception.FrameAudio, Source: "microphone", SampleRateHz: 24_000,
			PCM16LE: silence(2400),
		}); err != nil {
			t.Fatalf("silence block %d: %v", index, err)
		}
	}
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

func startSession(t *testing.T, config cascade.Config, settings binding.Settings) (binding.Runtime, *recordingSink) {
	t.Helper()
	if config.Perception == nil {
		config.Perception = func() (v1.PerceptionProvider, error) {
			return &scriptedASR{final: "what is my balance"}, nil
		}
	}
	if config.Speech == nil {
		config.Speech = toneSpeech{chunks: 1}
	}
	bind, err := cascade.New(config)
	if err != nil {
		t.Fatalf("new cascade: %v", err)
	}
	sink := &recordingSink{}
	runtime, err := bind.Start(context.Background(), binding.Options{
		Sink: sink, Settings: settings, SessionID: "test",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background(), nil) })
	return runtime, sink
}

// --- tests ------------------------------------------------------------------

func TestCascadeDeclaresEngineOwnershipOfEverything(t *testing.T) {
	bind, err := cascade.New(cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) { return &scriptedASR{}, nil },
		Fast:       newFast(), Slow: newSlow(), Speech: toneSpeech{},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ownership := bind.Ownership()
	if err := ownership.Validate(); err != nil {
		t.Fatalf("ownership: %v", err)
	}
	if ownership.Perception != binding.OwnerEngine || ownership.Floor != binding.OwnerEngine {
		t.Fatalf("a cascade owns nothing itself: %+v", ownership)
	}
	if !bind.Capabilities().FastSlow {
		t.Fatal("the background reasoner is the differentiator and must be reported")
	}
}

// The whole turn: speech in, fast answers, slow reasons silently, a fast step
// voices what slow produced.
func TestTurnAnswersFastThenVoicesSlow(t *testing.T) {
	fast := newFast(
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Let me check that."}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "You have forty dollars."}},
	)
	slow := newSlow([]continuation.Event{{
		Kind: continuation.EventAssistantDelta,
		Text: "The account balance is $40.00 as of the latest statement.",
	}})
	runtime, sink := startSession(t, cascade.Config{Fast: fast, Slow: slow}, binding.Settings{})

	speak(t, runtime, 3)
	waitFor(t, func() bool { return len(sink.spokenTexts()) >= 2 }, "expected a fast answer and a voiced slow answer")

	spoken := sink.spokenTexts()
	if spoken[0] != "Let me check that." {
		t.Fatalf("the fast provider answers first, got %q", spoken[0])
	}
	if spoken[1] != "You have forty dollars." {
		t.Fatalf("a fast step voices the slow answer, got %q", spoken[1])
	}
	// Slow's own text must never be handed to speech.
	for _, text := range spoken {
		if text == "The account balance is $40.00 as of the latest statement." {
			t.Fatal("slow output must not be voiced directly")
		}
	}
	if slow.invocations() != 1 {
		t.Fatalf("expected exactly one slow continuation, got %d", slow.invocations())
	}
}

func TestSlowToolCallsReachTheClientAndResultsResumeTheTurn(t *testing.T) {
	fast := newFast(
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Checking."}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "It is forty dollars."}},
	)
	slow := newSlow(
		[]continuation.Event{{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "call_1", Name: "get_balance", Arguments: json.RawMessage(`{"account":"A1"}`),
		}}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "The balance is $40.00."}},
	)
	runtime, sink := startSession(t, cascade.Config{Fast: fast, Slow: slow}, binding.Settings{
		Tools: []action.ToolSpec{{
			Name: "get_balance", Description: "read a balance",
			Parameters: json.RawMessage(`{"type":"object"}`),
		}},
	})

	speak(t, runtime, 3)
	waitFor(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return len(sink.toolCalls) == 1
	}, "expected an authoritative call to reach the client")

	sink.mu.Lock()
	event := sink.toolCalls[0]
	sink.mu.Unlock()
	if len(event.Calls) != 1 || event.Calls[0].Name != "get_balance" {
		t.Fatalf("unexpected call batch %+v", event)
	}
	if err := runtime.ToolResult(context.Background(), trajectory.ToolResult{
		CallID: "call_1", Name: "get_balance", Output: json.RawMessage(`{"balance":40}`),
	}); err != nil {
		t.Fatalf("tool result: %v", err)
	}
	waitFor(t, func() bool { return len(sink.spokenTexts()) >= 2 }, "expected the turn to resume after the result")

	snapshot := runtime.Trajectory()
	var sawResult bool
	for _, item := range snapshot.Items {
		if item.Kind == trajectory.KindToolResult {
			sawResult = true
		}
		if item.Kind == trajectory.KindToolCall && item.Producer.Phase != trajectory.PhaseSlow {
			t.Fatal("only the slow provider may append an executable call")
		}
	}
	if !sawResult {
		t.Fatal("the result must be committed to the trajectory")
	}
}

func TestFastProposalsNeverBecomeExecutableCalls(t *testing.T) {
	fast := newFast([]continuation.Event{
		{Kind: continuation.EventAssistantDelta, Text: "Checking."},
		{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "proposed_1", Name: "get_balance", Arguments: json.RawMessage(`{}`),
		}},
	})
	slow := newSlow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Balance is $40."}})
	runtime, sink := startSession(t, cascade.Config{Fast: fast, Slow: slow}, binding.Settings{
		Tools: []action.ToolSpec{{
			Name: "get_balance", Description: "read a balance",
			Parameters: json.RawMessage(`{"type":"object"}`),
		}},
	})
	speak(t, runtime, 3)
	waitFor(t, func() bool { return len(sink.spokenTexts()) >= 1 }, "expected a fast answer")
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindToolProposal {
				return true
			}
		}
		return false
	}, "expected the fast call to be recorded as a proposal")

	sink.mu.Lock()
	toolCalls := len(sink.toolCalls)
	sink.mu.Unlock()
	if toolCalls != 0 {
		t.Fatal("a proposal must never reach the client as an executable call")
	}
}

func TestServerSideToolsDispatchInProcess(t *testing.T) {
	var dispatched int
	fast := newFast(
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "One moment."}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Done."}},
	)
	slow := newSlow(
		[]continuation.Event{{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "call_1", Name: "local", Arguments: json.RawMessage(`{}`),
		}}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Finished."}},
	)
	runtime, sink := startSession(t, cascade.Config{Fast: fast, Slow: slow}, binding.Settings{
		Tools: []action.ToolSpec{{
			Name: "local", Description: "runs here", Parameters: json.RawMessage(`{"type":"object"}`),
			Dispatcher: action.DispatcherFunc(func(_ context.Context, call trajectory.ToolCall) (trajectory.ToolResult, error) {
				dispatched++
				return trajectory.ToolResult{CallID: call.CallID, Name: call.Name, Output: json.RawMessage(`{"ok":true}`)}, nil
			}),
		}},
	})
	speak(t, runtime, 3)
	waitFor(t, func() bool { return len(sink.spokenTexts()) >= 2 }, "expected the turn to complete locally")
	if dispatched != 1 {
		t.Fatalf("expected one in-process dispatch, got %d", dispatched)
	}
	sink.mu.Lock()
	toolCalls := len(sink.toolCalls)
	sink.mu.Unlock()
	if toolCalls != 0 {
		t.Fatal("a server-side tool must not be handed to the client")
	}
}

func TestFastOnlyRolloutNeverStartsSlowWork(t *testing.T) {
	fast := newFast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Forty dollars."}})
	slow := newSlow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "unused"}})
	policies := defaultPolicies()
	rollout, err := parseRollout("fast-only")
	if err != nil {
		t.Fatalf("parse rollout: %v", err)
	}
	policies.Rollout = rollout
	runtime, sink := startSession(t, cascade.Config{Fast: fast, Slow: slow, Policies: policies}, binding.Settings{})
	speak(t, runtime, 3)
	waitFor(t, func() bool { return len(sink.spokenTexts()) >= 1 }, "expected the fast answer")
	time.Sleep(150 * time.Millisecond)
	if slow.invocations() != 0 {
		t.Fatalf("fast-only must not run slow, got %d invocations", slow.invocations())
	}
}

func TestObservationsCommitBeforeTheTurnIsActedOn(t *testing.T) {
	fast := newFast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Sure."}})
	slow := newSlow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Done."}})
	runtime, sink := startSession(t, cascade.Config{Fast: fast, Slow: slow}, binding.Settings{})
	speak(t, runtime, 2)
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindObservation && item.Content == "what is my balance" {
				return true
			}
		}
		return false
	}, "expected the endpoint observation in the log")
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.transcripts) == 0 {
		t.Fatal("the client must see the transcript")
	}
}
