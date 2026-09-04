package cascade_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/computeruse"
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
	// completion is how this provider reports it stopped. The zero value is an
	// ordinary stop; a test that cares about truncation sets it.
	completion continuation.Completion
	mu         sync.Mutex
	turns      [][]continuation.Event
	calls      int
	completed  int
	requests   []continuation.Request
}

// stopping makes the provider report a particular completion, which is how a
// test says "this provider ran out of room" rather than "it had nothing to
// say".
func (provider *scriptedProvider) stopping(completion continuation.Completion) *scriptedProvider {
	provider.completion = completion
	return provider
}

func (provider *scriptedProvider) Descriptor() continuation.Descriptor { return provider.descriptor }

func (provider *scriptedProvider) Continue(
	_ context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	defer func() {
		provider.mu.Lock()
		provider.completed++
		provider.mu.Unlock()
	}()
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

func (provider *scriptedProvider) invocations() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.calls
}

func (provider *scriptedProvider) completions() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.completed
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

func newFastComputer(turns ...[]continuation.Event) *scriptedProvider {
	provider := newFast(turns...)
	provider.descriptor.ToolAuthority = continuation.ToolAuthorityExecute
	return provider
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

type toneSpeech struct {
	chunks int
	// silent produces no audio at all, which is how a test arranges for an
	// utterance to be decided and never heard.
	silent bool
}

func (toneSpeech) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "tone", Version: "1", Capabilities: v1.Capabilities{}}
}

func (toneSpeech) Synthesize(context.Context, v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	return nil, errors.New("unused")
}

func (speech toneSpeech) Stream(ctx context.Context, plan v1.SpeechPlan, emit func(v1.SpeechChunk) error) error {
	if speech.silent {
		return errors.New("no audio available")
	}
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
	outcomes     []binding.TurnOutcome
	transcripts  []binding.TranscriptEvent
	utterances   []action.Utterance
	ended        []endedSpeech
	frames       int
	spoken       []string
	toolCalls    []binding.ToolCallEvent
	observations []perception.Observation
	failures     []binding.ErrorEvent
	activities   []binding.ActivityEvent
}

func (sink *recordingSink) TurnBegin(context.Context) error { return nil }
func (sink *recordingSink) TurnEnd(_ context.Context, outcome binding.TurnOutcome) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.outcomes = append(sink.outcomes, outcome)
	return nil
}

// turnOutcomes is what the rollout reported about the turns it ran.
func (sink *recordingSink) turnOutcomes() []binding.TurnOutcome {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]binding.TurnOutcome(nil), sink.outcomes...)
}

func (sink *recordingSink) Activity(_ context.Context, event binding.ActivityEvent) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.activities = append(sink.activities, event)
	return nil
}

func (sink *recordingSink) activityEvents() []binding.ActivityEvent {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]binding.ActivityEvent(nil), sink.activities...)
}

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

func (sink *recordingSink) SpeechEnd(
	_ context.Context, utterance action.Utterance, outcome action.Outcome,
) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.ended = append(sink.ended, endedSpeech{utterance: utterance, outcome: outcome})
	return nil
}

// endedSpeech is an utterance and how it finished. Whether it finished is the
// whole question for anything the agent says over somebody else.
type endedSpeech struct {
	utterance action.Utterance
	outcome   action.Outcome
}

func (sink *recordingSink) speechOutcomes() []endedSpeech {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]endedSpeech(nil), sink.ended...)
}

func (sink *recordingSink) speechBegan() []action.Utterance {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]action.Utterance(nil), sink.utterances...)
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

// A silent reasoner that has nothing new can still leave behind a paraphrase
// of what the voice just said. The background voicing turn must not turn that
// into the same answer twice, while a later user revision remains free to ask
// for the same words again.
func TestBackgroundResultDoesNotRepeatTheSameResponseForOneRevision(t *testing.T) {
	const (
		response = "Could you confirm the order ID?"
		initial  = "I can help with that. " + response
	)
	fast := newFast(
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: initial}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: response}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: initial}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: response}},
	)
	slow := newSlow(
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "No additional result."}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Still no additional result."}},
	)
	runtime, sink := startSession(t, cascade.Config{Fast: fast, Slow: slow}, binding.Settings{})

	speak(t, runtime, 3)
	waitFor(t, func() bool { return fast.completions() >= 2 }, "the first background result was never processed")
	waitFor(t, func() bool { return len(sink.spokenTexts()) >= 1 }, "the first response was never voiced")
	if spoken := sink.spokenTexts(); len(spoken) != 1 || spoken[0] != initial {
		t.Fatalf("one observation repeated the same response: %#v", spoken)
	}

	// The text is identical, but this is a new user observation and therefore
	// a new answer rather than a duplicate of the first one.
	speak(t, runtime, 3)
	waitFor(t, func() bool { return fast.completions() >= 4 }, "the second background result was never processed")
	waitFor(t, func() bool { return len(sink.spokenTexts()) >= 2 }, "the new observation's response was suppressed")
	if spoken := sink.spokenTexts(); len(spoken) != 2 || spoken[0] != initial || spoken[1] != initial {
		t.Fatalf("revision-scoped suppression produced %#v", spoken)
	}
}

// A background result may teach the voice enough to ask a better question,
// but the caller is already answering the one the voice asked first. Speaking
// both before any new user evidence turns one request into an interrogation
// pile-up: measured in a retail call, every identity answer received a second
// clarification, the caller began spelling over it, and the agent eventually
// treated the overlap it created as a communication failure.
func TestBackgroundResultDoesNotAskASecondQuestionBeforeTheFirstIsAnswered(t *testing.T) {
	const (
		first  = "Please provide your email address."
		second = "Could you provide your name and zip code?"
	)
	fast := newFast(
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: first}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: second}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: second}},
	)
	slow := newSlow([]continuation.Event{{
		Kind: continuation.EventAssistantDelta,
		Text: "Name and zip code are an alternative authentication path.",
	}})
	runtime, sink := startSession(t, cascade.Config{Fast: fast, Slow: slow}, binding.Settings{})

	speak(t, runtime, 3)
	waitFor(t, func() bool { return fast.invocations() >= 2 }, "the background result was never considered")
	if spoken := sink.spokenTexts(); len(spoken) != 1 || spoken[0] != first {
		t.Fatalf("one observation stacked questions before the caller could answer: %#v", spoken)
	}

	// The same question after another user observation is no longer stacked:
	// what they just said may have answered the first question or changed what
	// clarification is useful.
	speak(t, runtime, 3)
	waitFor(t, func() bool { return fast.invocations() >= 3 }, "the next observation was not answered")
	waitFor(t, func() bool { return len(sink.spokenTexts()) >= 2 }, "the new observation's question was suppressed")
	if spoken := sink.spokenTexts(); len(spoken) != 2 || spoken[1] != second {
		t.Fatalf("a later observation did not reopen clarification: %#v", spoken)
	}
}

func TestBackgroundResultStillAddsAFactAfterAQuestion(t *testing.T) {
	fast := newFast(
		[]continuation.Event{{
			Kind: continuation.EventAssistantDelta, Text: "Could you provide your email address?",
		}},
		[]continuation.Event{{
			Kind: continuation.EventAssistantDelta, Text: "Name and zip code are accepted instead.",
		}},
	)
	slow := newSlow([]continuation.Event{{
		Kind: continuation.EventAssistantDelta,
		Text: "The authentication policy also accepts a name and zip code.",
	}})
	runtime, sink := startSession(t, cascade.Config{Fast: fast, Slow: slow}, binding.Settings{})

	speak(t, runtime, 3)
	waitFor(t, func() bool { return fast.invocations() >= 2 }, "the factual background result was never voiced")
	waitFor(t, func() bool { return len(sink.spokenTexts()) >= 2 }, "the factual background result was suppressed")
	if spoken := sink.spokenTexts(); len(spoken) != 2 || spoken[1] != "Name and zip code are accepted instead." {
		t.Fatalf("a fact was mistaken for a second question: %#v", spoken)
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
			t.Fatal("fast is proposal-only in the default arrangement")
		}
	}
	if !sawResult {
		t.Fatal("the result must be committed to the trajectory")
	}
}

func TestFailedToolReportDoesNotAutomaticallyRetrySlow(t *testing.T) {
	fast := newFast(
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Checking."}},
		[]continuation.Event{
			{Kind: continuation.EventAssistantDelta, Text: "That lookup failed; please check the account."},
			{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
				CallID: "proposed_retry_1", Name: "get_balance", Arguments: json.RawMessage(`{"account":"guessed"}`),
			}},
		},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "I will try with that new information."}},
	)
	slow := newSlow(
		[]continuation.Event{{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "lookup_1", Name: "get_balance", Arguments: json.RawMessage(`{"account":"A1"}`),
		}}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "The new information is enough to continue."}},
	)
	var dispatched atomic.Int32
	runtime, _ := startSession(t, cascade.Config{
		Fast: fast, Slow: slow,
		Tools: []action.ToolSpec{{
			Name: "get_balance", Description: "read a balance",
			Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmNever,
			Dispatcher: action.DispatcherFunc(func(
				_ context.Context, call trajectory.ToolCall,
			) (trajectory.ToolResult, error) {
				dispatched.Add(1)
				return trajectory.ToolResult{}, errors.New("balance service unavailable")
			}),
		}},
	}, binding.Settings{})

	speak(t, runtime, 3)
	waitFor(t, func() bool {
		if fast.invocations() < 2 {
			return false
		}
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindToolProposal && item.ToolCall != nil &&
				item.ToolCall.CallID == "proposed_retry_1" {
				return true
			}
		}
		return false
	}, "the failed result was not returned to the voice")
	// The deliberately unfinished failure report and its proposal are both
	// escalation-shaped. Neither is new evidence, so neither may reopen slow.
	time.Sleep(150 * time.Millisecond)
	if got := slow.invocations(); got != 1 {
		t.Fatalf("a failure report automatically retried slow: got %d invocations", got)
	}
	if got := dispatched.Load(); got != 1 {
		t.Fatalf("the failed action crossed the dispatcher %d times", got)
	}

	// A later user observation is new evidence and starts an ordinary rollout;
	// the terminal failure handoff must not disable future reasoning.
	speak(t, runtime, 3)
	waitFor(t, func() bool { return slow.invocations() >= 2 }, "new user evidence did not reopen slow cognition")
}

func TestFailedToolDoesNotAskASecondQuestionBeforeTheFirstIsAnswered(t *testing.T) {
	const (
		first  = "Could you confirm the account ID?"
		second = "Please provide another account ID."
	)
	fast := newFast(
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: first}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: second}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: second}},
	)
	slow := newSlow([]continuation.Event{{
		Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "lookup_1", Name: "get_balance", Arguments: json.RawMessage(`{"account":"A1"}`),
		},
	}})
	runtime, sink := startSession(t, cascade.Config{
		Fast: fast, Slow: slow,
		Tools: []action.ToolSpec{{
			Name: "get_balance", Description: "read a balance",
			Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmNever,
			Dispatcher: action.DispatcherFunc(func(
				context.Context, trajectory.ToolCall,
			) (trajectory.ToolResult, error) {
				return trajectory.ToolResult{}, errors.New("balance service unavailable")
			}),
		}},
	}, binding.Settings{})

	speak(t, runtime, 3)
	waitFor(t, func() bool { return fast.invocations() >= 2 }, "the failed result was not returned to the voice")
	if spoken := sink.spokenTexts(); len(spoken) != 1 || spoken[0] != first {
		t.Fatalf("a failed tool stacked a second question without user evidence: %#v", spoken)
	}

	speak(t, runtime, 3)
	waitFor(t, func() bool { return len(sink.spokenTexts()) >= 2 }, "new user evidence did not reopen clarification")
	if spoken := sink.spokenTexts(); len(spoken) != 2 || spoken[1] != second {
		t.Fatalf("a later user observation did not reopen the failed lookup: %#v", spoken)
	}
}

func TestFastProposalsNeverBecomeSpeechOrExecutableCalls(t *testing.T) {
	fast := newFast([]continuation.Event{
		{Kind: continuation.EventAssistantDelta, Text: "The balance lookup succeeded."},
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
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindToolProposal {
				return true
			}
		}
		return false
	}, "expected the fast call to be recorded as a proposal")
	if spoken := sink.spokenTexts(); len(spoken) != 0 {
		t.Fatalf("prose accompanying an unresolved proposal crossed as speech: %#v", spoken)
	}
	for _, item := range runtime.Trajectory().Items {
		if item.Kind == trajectory.KindAssistant && item.Content == "The balance lookup succeeded." {
			t.Fatal("premature proposal prose became conversational history")
		}
	}

	sink.mu.Lock()
	toolCalls := len(sink.toolCalls)
	sink.mu.Unlock()
	if toolCalls != 0 {
		t.Fatal("a proposal must never reach the client as an executable call")
	}
}

// A user confirmation opens an action turn; it does not make the intended
// effect true. The fast voice may identify that intent through a proposal, but
// only a successful authoritative ToolResult lets a later voice turn report
// the result. This is the regression for an exchange being announced as
// processed before its write call had crossed the action boundary.
func TestActionSuccessSpeechWaitsForTheCommittedToolResult(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	fast := newFast(
		[]continuation.Event{
			{Kind: continuation.EventAssistantDelta, Text: "The change has been processed with value 999."},
			{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
				CallID: "proposal_1", Name: "apply_change", Arguments: json.RawMessage(`{"value":7}`),
			}},
		},
		[]continuation.Event{{
			Kind: continuation.EventAssistantDelta, Text: "The change was applied with value 7.",
		}},
	)
	slow := newSlow(
		[]continuation.Event{{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "apply_1", Name: "apply_change", Arguments: json.RawMessage(`{"value":7}`),
		}}},
		[]continuation.Event{{
			Kind: continuation.EventAssistantDelta,
			Text: `apply_change succeeded with {"value":7}`,
		}},
	)
	runtime, sink := startSession(t, cascade.Config{
		Fast: fast, Slow: slow,
		Tools: []action.ToolSpec{{
			Name: "apply_change", Description: "apply a requested change",
			Parameters: json.RawMessage(`{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"]}`),
			Confirm:    action.ConfirmNever,
			Dispatcher: action.DispatcherFunc(func(
				ctx context.Context, call trajectory.ToolCall,
			) (trajectory.ToolResult, error) {
				started <- struct{}{}
				select {
				case <-release:
					return trajectory.ToolResult{
						CallID: call.CallID, Name: call.Name, Output: json.RawMessage(`{"value":7}`),
					}, nil
				case <-ctx.Done():
					return trajectory.ToolResult{}, context.Cause(ctx)
				}
			}),
		}},
	}, binding.Settings{})

	speak(t, runtime, 3)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the authoritative action never reached its dispatcher")
	}
	if spoken := sink.spokenTexts(); len(spoken) != 0 {
		t.Fatalf("success crossed before the tool returned: %#v", spoken)
	}
	for _, item := range runtime.Trajectory().Items {
		if item.Kind == trajectory.KindAssistant && strings.Contains(item.Content, "value 999") {
			t.Fatal("premature success became conversational history")
		}
	}

	close(release)
	waitFor(t, func() bool { return len(sink.spokenTexts()) == 1 }, "the committed result was never voiced")
	if spoken := sink.spokenTexts(); len(spoken) != 1 || spoken[0] != "The change was applied with value 7." {
		t.Fatalf("result-grounded speech = %#v", spoken)
	}
	var resultAt, groundedAt = -1, -1
	for index, item := range runtime.Trajectory().Items {
		if item.Kind == trajectory.KindToolResult && item.ToolResult != nil && item.ToolResult.CallID == "apply_1" {
			resultAt = index
		}
		if item.Kind == trajectory.KindAssistant && item.Content == "The change was applied with value 7." {
			groundedAt = index
		}
	}
	if resultAt < 0 || groundedAt <= resultAt {
		t.Fatalf("grounded narration did not follow its result: result=%d speech=%d", resultAt, groundedAt)
	}
}

func TestEmptySlowContinuationAfterToolResultStillWakesTheVoice(t *testing.T) {
	fast := newFast(
		nil,
		[]continuation.Event{{
			Kind: continuation.EventAssistantDelta,
			Text: "The latest conversion rate is 18.4 percent.",
		}},
	)
	slow := newSlow([]continuation.Event{{
		Kind: continuation.EventToolCall,
		ToolCall: &trajectory.ToolCall{
			CallID: "read_launch_review_1", Name: "read_launch_review",
			Arguments: json.RawMessage(`{}`),
		},
	}})
	runtime, sink := startSession(t, cascade.Config{
		Fast: fast, Slow: slow,
		Tools: []action.ToolSpec{{
			Name: "read_launch_review", Description: "read authoritative launch facts",
			Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmNever,
			Dispatcher: action.DispatcherFunc(func(
				_ context.Context, call trajectory.ToolCall,
			) (trajectory.ToolResult, error) {
				return trajectory.ToolResult{
					CallID: call.CallID, Name: call.Name,
					Output: json.RawMessage(`{"conversion_rate":18.4}`),
				}, nil
			}),
		}},
	}, binding.Settings{})

	speak(t, runtime, 3)
	waitFor(t, func() bool { return len(sink.spokenTexts()) == 1 },
		"an empty post-tool slow continuation never handed the authoritative result to the voice")
	if spoken := sink.spokenTexts(); len(spoken) != 1 ||
		spoken[0] != "The latest conversion rate is 18.4 percent." {
		t.Fatalf("result-grounded speech = %#v", spoken)
	}
	if got := slow.invocations(); got != 2 {
		t.Fatalf("slow invocations = %d, want tool selection plus post-result continuation", got)
	}
	if got := fast.invocations(); got != 2 {
		t.Fatalf("fast invocations = %d, want initial turn plus background-result rendering", got)
	}
}

func TestOptInFastComputerActionDispatchesInProcess(t *testing.T) {
	dispatched := make(chan trajectory.ToolCall, 1)
	fast := newFastComputer([]continuation.Event{{
		Kind: continuation.EventToolCall,
		ToolCall: &trajectory.ToolCall{
			CallID: "fast_click_1", Name: computeruse.Click,
			Arguments: json.RawMessage(`{"source":"screen","x":10,"y":10}`),
		},
	}})
	policies := defaultPolicies()
	rollout, err := parseRollout("fast-only")
	if err != nil {
		t.Fatal(err)
	}
	policies.Rollout = rollout
	runtime, sink := startSession(t, cascade.Config{
		Fast: fast, Slow: newSlow(), Policies: policies, FastComputerUse: true,
		Tools: []action.ToolSpec{{
			Name: computeruse.Click, Description: "click the screen",
			Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmNever,
			Target: "browser", Dispatcher: action.DispatcherFunc(func(
				_ context.Context, call trajectory.ToolCall,
			) (trajectory.ToolResult, error) {
				dispatched <- call
				return trajectory.ToolResult{
					CallID: call.CallID, Name: call.Name, Output: json.RawMessage(`{"ok":true}`),
				}, nil
			}),
		}},
	}, binding.Settings{})

	speak(t, runtime, 3)
	select {
	case call := <-dispatched:
		if call.Name != computeruse.Click {
			t.Fatalf("unexpected fast dispatch: %+v", call)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the bounded fast action never reached its in-process dispatcher")
	}
	waitFor(t, func() bool {
		var call, result bool
		for _, item := range runtime.Trajectory().Items {
			call = call || (item.Kind == trajectory.KindToolCall && item.Producer.Phase == trajectory.PhaseFast)
			result = result || item.Kind == trajectory.KindToolResult
		}
		return call && result
	}, "the fast call and result were not committed")
	fast.mu.Lock()
	request := fast.requests[0]
	fast.mu.Unlock()
	if len(request.Invocation.Tools) != 1 || request.Invocation.Tools[0].Name != computeruse.Click {
		t.Fatalf("fast received more than its exact server-owned allowlist: %+v", request.Invocation.Tools)
	}
	sink.mu.Lock()
	remoteCalls := len(sink.toolCalls)
	sink.mu.Unlock()
	if remoteCalls != 0 {
		t.Fatal("a fast server-owned action was handed to the client")
	}
}

func TestSlowToolCallIsNotDispatchedAfterUserResumes(t *testing.T) {
	t.Parallel()
	const toolName = "lookup"
	slow := &slowProvider{
		scriptedProvider: *newSlow([]continuation.Event{{
			Kind: continuation.EventToolCall,
			ToolCall: &trajectory.ToolCall{
				CallID: "slow_lookup_1", Name: toolName, Arguments: json.RawMessage(`{"key":"old"}`),
			},
		}}),
		delay: 150 * time.Millisecond, entered: make(chan struct{}),
	}
	var dispatched atomic.Int32
	runtime, _ := startSession(t, cascade.Config{
		// Keep the voice silent so barge-in does not cancel the whole processor;
		// this isolates the later action-boundary check in runSlow.
		Fast: newFast(),
		Slow: slow,
		Tools: []action.ToolSpec{{
			Name: toolName, Description: "look up a value",
			Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmNever,
			Dispatcher: action.DispatcherFunc(func(
				_ context.Context, call trajectory.ToolCall,
			) (trajectory.ToolResult, error) {
				dispatched.Add(1)
				return trajectory.ToolResult{CallID: call.CallID, Name: call.Name}, nil
			}),
		}},
	}, binding.Settings{})

	speak(t, runtime, 3)
	select {
	case <-slow.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the slow continuation never started")
	}
	// Start a new stretch but do not endpoint it. There are deliberately no
	// words yet: acoustic onset alone must fence the older action.
	pushAudio(t, runtime, tone(2400, 8000), 3)
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindToolPlaceholder && item.ToolPlaceholder != nil &&
				item.ToolPlaceholder.CallID == "slow_lookup_1" {
				return true
			}
		}
		return false
	}, "the overtaken tool call was not closed with a placeholder")
	if dispatched.Load() != 0 {
		t.Fatal("a slow tool call crossed the action boundary after the user resumed")
	}
}

func TestDeclaredBackgroundSlowToolStartsAfterUserResumes(t *testing.T) {
	t.Parallel()
	const toolName = "analyze"
	slow := &slowProvider{
		scriptedProvider: *newSlow([]continuation.Event{{
			Kind: continuation.EventToolCall,
			ToolCall: &trajectory.ToolCall{
				CallID: "background_analyze_1", Name: toolName, Arguments: json.RawMessage(`{}`),
			},
		}}),
		delay: 150 * time.Millisecond, entered: make(chan struct{}),
	}
	dispatched := make(chan trajectory.ToolCall, 1)
	runtime, _ := startSession(t, cascade.Config{
		Fast: newFast(), Slow: slow,
		Tools: []action.ToolSpec{{
			Name: toolName, Description: "analyze in the background",
			Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmNever,
			Background: true,
			Dispatcher: action.DispatcherFunc(func(
				_ context.Context, call trajectory.ToolCall,
			) (trajectory.ToolResult, error) {
				dispatched <- call
				return trajectory.ToolResult{CallID: call.CallID, Name: call.Name}, nil
			}),
		}},
	}, binding.Settings{})

	speak(t, runtime, 3)
	select {
	case <-slow.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the background continuation never started")
	}
	// New acoustic evidence overtakes the provider prefix before its tool call
	// reaches the action boundary. The declaration says that this analysis is
	// still valid and exists specifically to continue while listening.
	pushAudio(t, runtime, tone(2400, 8000), 3)
	select {
	case call := <-dispatched:
		if call.Name != toolName {
			t.Fatalf("unexpected background dispatch: %+v", call)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a declared background call did not cross the action boundary")
	}
	for _, item := range runtime.Trajectory().Items {
		if item.Kind == trajectory.KindToolPlaceholder && item.ToolPlaceholder != nil &&
			item.ToolPlaceholder.CallID == "background_analyze_1" {
			t.Fatal("the executed background call was also closed as interrupted")
		}
	}
}

func TestFastComputerLaneKeepsObservationControlSlowOnly(t *testing.T) {
	fast := newFastComputer(
		[]continuation.Event{{
			Kind: continuation.EventToolCall,
			ToolCall: &trajectory.ToolCall{
				CallID: "fast_wait_1", Name: computeruse.Wait,
				Arguments: json.RawMessage(`{"duration_ms":2000}`),
			},
		}},
		[]continuation.Event{{
			Kind: continuation.EventToolCall,
			ToolCall: &trajectory.ToolCall{
				CallID: "fast_shot_1", Name: computeruse.Screenshot,
				Arguments: json.RawMessage(`{"source":"screen"}`),
			},
		}},
	)
	policies := defaultPolicies()
	rollout, _ := parseRollout("fast-only")
	policies.Rollout = rollout
	var dispatched atomic.Int32
	runtime, _ := startSession(t, cascade.Config{
		Fast: fast, Slow: newSlow(), Policies: policies, FastComputerUse: true,
		Tools: []action.ToolSpec{
			{
				Name: computeruse.Wait, Description: "wait",
				Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmNever,
				Target: "browser", Dispatcher: action.DispatcherFunc(func(
					_ context.Context, call trajectory.ToolCall,
				) (trajectory.ToolResult, error) {
					dispatched.Add(1)
					return trajectory.ToolResult{CallID: call.CallID, Name: call.Name}, nil
				}),
			},
			{
				Name: computeruse.Screenshot, Description: "capture",
				Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmNever,
				Target: "browser", Dispatcher: action.DispatcherFunc(func(
					_ context.Context, call trajectory.ToolCall,
				) (trajectory.ToolResult, error) {
					dispatched.Add(1)
					return trajectory.ToolResult{CallID: call.CallID, Name: call.Name}, nil
				}),
			},
		},
	}, binding.Settings{})

	speak(t, runtime, 3)
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindToolProposal && item.ToolCall != nil && item.ToolCall.Name == computeruse.Wait {
				return true
			}
		}
		return false
	}, "fast wait did not remain a proposal")
	if dispatched.Load() != 0 {
		t.Fatal("observation control crossed the fast execution boundary")
	}
	fast.mu.Lock()
	request := fast.requests[0]
	fast.mu.Unlock()
	if len(request.Invocation.Tools) != 0 {
		t.Fatalf("fast received observation-control schemas: %+v", request.Invocation.Tools)
	}
}

func TestFastBackgroundLaneCanStartOnlyExplicitlyDeclaredBackgroundWork(t *testing.T) {
	fast := newFastComputer([]continuation.Event{{
		Kind: continuation.EventToolCall,
		ToolCall: &trajectory.ToolCall{
			CallID: "fast_analysis_1", Name: "analyze", Arguments: json.RawMessage(`{}`),
		},
	}})
	policies := defaultPolicies()
	rollout, _ := parseRollout("fast-only")
	policies.Rollout = rollout
	runtime, sink := startSession(t, cascade.Config{
		Fast: fast, Slow: newSlow(), Policies: policies, FastBackgroundTools: true,
	}, binding.Settings{})
	if err := runtime.Update(context.Background(), binding.Settings{
		Gate: perception.DefaultGateConfig(),
		Tools: []action.ToolSpec{
			{Name: "analyze", Description: "analyze while listening", Parameters: json.RawMessage(`{"type":"object"}`), Background: true},
			{Name: "delete", Description: "delete a record", Parameters: json.RawMessage(`{"type":"object"}`)},
		},
	}); err != nil {
		t.Fatalf("declare client tools: %v", err)
	}

	speak(t, runtime, 3)
	waitFor(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return len(sink.toolCalls) == 1
	}, "the fast background call never crossed the protocol boundary")
	fast.mu.Lock()
	request := fast.requests[0]
	fast.mu.Unlock()
	if len(request.Invocation.Tools) != 1 || request.Invocation.Tools[0].Name != "analyze" ||
		!request.Invocation.Tools[0].Background {
		t.Fatalf("fast background lane received more than its typed allowlist: %+v", request.Invocation.Tools)
	}
	sink.mu.Lock()
	event := sink.toolCalls[0]
	sink.mu.Unlock()
	if len(event.Calls) != 1 || event.Calls[0].Name != "analyze" {
		t.Fatalf("unexpected fast background call: %+v", event)
	}
}

func TestOptInFastComputerActionCanUseABoundedClientEnvironment(t *testing.T) {
	fast := newFastComputer([]continuation.Event{{
		Kind: continuation.EventToolCall,
		ToolCall: &trajectory.ToolCall{
			CallID: "fast_mark_1", Name: computeruse.ClickElement,
			Arguments: json.RawMessage(`{"source":"screen","element_id":"1"}`),
		},
	}})
	policies := defaultPolicies()
	rollout, _ := parseRollout("fast-only")
	policies.Rollout = rollout
	runtime, sink := startSession(t, cascade.Config{
		Fast: fast, Slow: newSlow(), Policies: policies, FastComputerUse: true,
	}, binding.Settings{})
	// Realtime clients declare tools in session.update, after the binding
	// runtime and cognition engine already exist. The fast filter must read the
	// live registry or the repository-owned evaluator would silently exercise
	// the slow path despite asking for fast computer use.
	settings := binding.Settings{Gate: perception.DefaultGateConfig(), Tools: []action.ToolSpec{{
		Name: computeruse.ClickElement, Description: "click a visible mark",
		Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmNever,
		Target: "benchmark-browser",
	}}}
	if err := runtime.Update(context.Background(), settings); err != nil {
		t.Fatalf("declare client tool: %v", err)
	}

	speak(t, runtime, 3)
	waitFor(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return len(sink.toolCalls) == 1
	}, "the bounded client action never crossed the protocol boundary")
	sink.mu.Lock()
	event := sink.toolCalls[0]
	sink.mu.Unlock()
	if len(event.Calls) != 1 || event.Calls[0].Name != computeruse.ClickElement {
		t.Fatalf("unexpected client call: %+v", event)
	}
	if err := runtime.ToolResult(context.Background(), trajectory.ToolResult{
		CallID: "fast_mark_1", Name: computeruse.ClickElement, Output: json.RawMessage(`{"ok":true}`),
	}); err != nil {
		t.Fatalf("client result: %v", err)
	}
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindToolResult && item.ToolResult != nil && item.ToolResult.CallID == "fast_mark_1" {
				return true
			}
		}
		return false
	}, "the client action result did not rejoin the trajectory")
	fast.mu.Lock()
	request := fast.requests[0]
	fast.mu.Unlock()
	if len(request.Invocation.Tools) != 1 || request.Invocation.Tools[0].Name != computeruse.ClickElement {
		t.Fatalf("the post-construction client declaration was absent from fast cognition: %+v", request.Invocation.Tools)
	}
}

func TestFastComputerModeDoesNotAdmitArbitraryOrClientNamedTools(t *testing.T) {
	fast := newFastComputer([]continuation.Event{
		{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "fake_1", Name: "computer.exfiltrate", Arguments: json.RawMessage(`{}`),
		}},
		{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "money_1", Name: "transfer_funds", Arguments: json.RawMessage(`{}`),
		}},
	})
	policies := defaultPolicies()
	rollout, err := parseRollout("fast-only")
	if err != nil {
		t.Fatal(err)
	}
	policies.Rollout = rollout
	runtime, sink := startSession(t, cascade.Config{
		Fast: fast, Slow: newSlow(), Policies: policies, FastComputerUse: true,
		Tools: []action.ToolSpec{{
			Name: computeruse.Screenshot, Description: "capture",
			Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmNever,
			Dispatcher: action.DispatcherFunc(func(_ context.Context, call trajectory.ToolCall) (trajectory.ToolResult, error) {
				t.Fatalf("unrequested allowed tool dispatched: %+v", call)
				return trajectory.ToolResult{}, nil
			}),
		}},
	}, binding.Settings{Tools: []action.ToolSpec{
		{Name: "computer.exfiltrate", Description: "client impostor", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "transfer_funds", Description: "move money", Parameters: json.RawMessage(`{"type":"object"}`)},
	}})

	speak(t, runtime, 3)
	waitFor(t, func() bool {
		count := 0
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindToolProposal {
				count++
			}
			if item.Kind == trajectory.KindToolCall && item.Producer.Phase == trajectory.PhaseFast {
				return false
			}
		}
		return count == 2
	}, "non-allowed fast calls did not remain proposals")
	sink.mu.Lock()
	remoteCalls := len(sink.toolCalls)
	sink.mu.Unlock()
	if remoteCalls != 0 {
		t.Fatal("a client tool escaped through the fast execution lane")
	}
}

func TestFastComputerActionStillRequiresDeclaredConfirmation(t *testing.T) {
	var dispatched atomic.Int32
	audited := make(chan action.Record, 1)
	fast := newFastComputer([]continuation.Event{{
		Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "fast_click_1", Name: computeruse.Click,
			Arguments: json.RawMessage(`{"source":"screen","x":10,"y":10}`),
		},
	}})
	policies := defaultPolicies()
	rollout, _ := parseRollout("fast-only")
	policies.Rollout = rollout
	runtime, _ := startSession(t, cascade.Config{
		Fast: fast, Slow: newSlow(), Policies: policies, FastComputerUse: true,
		ActionAudit: func(record action.Record) { audited <- record },
		Tools: []action.ToolSpec{{
			Name: computeruse.Click, Description: "click",
			Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmAlways,
			Target: "browser", Dispatcher: action.DispatcherFunc(func(
				_ context.Context, call trajectory.ToolCall,
			) (trajectory.ToolResult, error) {
				dispatched.Add(1)
				return trajectory.ToolResult{CallID: call.CallID, Name: call.Name}, nil
			}),
		}},
	}, binding.Settings{})
	speak(t, runtime, 3)
	select {
	case record := <-audited:
		if record.Executed || !strings.Contains(record.Error, "confirmed") {
			t.Fatalf("unexpected confirmation audit: %+v", record)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the refused fast action was not audited")
	}
	if dispatched.Load() != 0 {
		t.Fatal("a fast action bypassed its declared confirmation requirement")
	}
}

func TestFastComputerActionStillObeysTheSourceFence(t *testing.T) {
	surface := &fastTestSurface{}
	target := computeruse.Target{Name: "browser", Sources: []string{"screen"}, Width: 100, Height: 100}
	dispatcher, err := computeruse.NewDispatcher(computeruse.DispatcherConfig{Target: target, Surface: surface})
	if err != nil {
		t.Fatal(err)
	}
	specs, err := computeruse.Specs(target, dispatcher, map[string]action.Confirm{
		computeruse.Click: action.ConfirmNever,
	})
	if err != nil {
		t.Fatal(err)
	}
	fast := newFastComputer([]continuation.Event{{
		Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "fast_click_1", Name: computeruse.Click,
			Arguments: json.RawMessage(`{"source":"physical-camera","x":10,"y":10}`),
		},
	}})
	policies := defaultPolicies()
	rollout, _ := parseRollout("fast-only")
	policies.Rollout = rollout
	runtime, _ := startSession(t, cascade.Config{
		Fast: fast, Slow: newSlow(), Policies: policies, FastComputerUse: true, Tools: specs,
	}, binding.Settings{})
	speak(t, runtime, 3)
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindToolResult && item.ToolResult != nil {
				return strings.Contains(item.ToolResult.Error, "does not own source")
			}
		}
		return false
	}, "the source-fence refusal did not reach the trajectory")
	if surface.clicks.Load() != 0 {
		t.Fatal("a fast action crossed from camera evidence into an undeclared target")
	}
}

type fastTestSurface struct{ clicks atomic.Int32 }

func (*fastTestSurface) Name() string { return "fast-test" }
func (surface *fastTestSurface) Click(context.Context, int, int, string) error {
	surface.clicks.Add(1)
	return nil
}
func (*fastTestSurface) DoubleClick(context.Context, int, int) error      { return nil }
func (*fastTestSurface) Move(context.Context, int, int) error             { return nil }
func (*fastTestSurface) Drag(context.Context, int, int, int, int) error   { return nil }
func (*fastTestSurface) Type(context.Context, string) error               { return nil }
func (*fastTestSurface) Key(context.Context, []string) error              { return nil }
func (*fastTestSurface) Scroll(context.Context, int, int, int, int) error { return nil }
func (*fastTestSurface) Screenshot(context.Context) error                 { return nil }

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

// closingASR reports whether the runtime ever released it.
type closingASR struct {
	scriptedASR
	closed atomic.Bool
}

func (asr *closingASR) Close() error {
	asr.closed.Store(true)
	return nil
}

// One recogniser exists per utterance and the endpoint is what normally
// retires it. A session that ends while the user is still speaking never
// reaches an endpoint, and hanging up mid-sentence is ordinary behaviour, so
// the socket and the goroutine reading it would outlive the session.
func TestClosingASessionReleasesARecogniserMidUtterance(t *testing.T) {
	asr := &closingASR{scriptedASR: scriptedASR{final: "what is my balance"}}
	fast := newFast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "One moment."}})
	slow := newSlow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Forty dollars."}})
	runtime, _ := startSession(t, cascade.Config{
		Fast: fast, Slow: slow,
		Perception: func() (v1.PerceptionProvider, error) { return asr, nil },
	}, binding.Settings{})

	// Speech with no trailing silence: the utterance is still open. More than
	// one block, because the gate wants sustained voicing before it believes a
	// turn has started, and one block is a transient.
	for range 3 {
		if err := runtime.Audio(context.Background(), perception.Frame{
			Kind: perception.FrameAudio, Source: "microphone", SampleRateHz: 24_000,
			PCM16LE: tone(2400, 8000),
		}); err != nil {
			t.Fatalf("speak: %v", err)
		}
	}
	waitFor(t, func() bool { return asr.pushes > 0 }, "the recogniser never saw audio")
	if asr.closed.Load() {
		t.Fatal("the recogniser was released while the utterance was still open")
	}

	if err := runtime.Close(context.Background(), nil); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !asr.closed.Load() {
		t.Fatal("closing the session left the recogniser open")
	}
}

// slowProvider takes a declared amount of time, the way a reasoner working
// through a hard question does.
type slowProvider struct {
	scriptedProvider
	delay   time.Duration
	entered chan struct{}
	once    sync.Once
}

func (provider *slowProvider) Continue(
	ctx context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	provider.once.Do(func() { close(provider.entered) })
	select {
	case <-time.After(provider.delay):
	case <-ctx.Done():
		return continuation.Completion{}, ctx.Err()
	}
	return provider.scriptedProvider.Continue(ctx, request, emit)
}

// turnClock records when the client was told a turn began and ended.
type turnClock struct {
	recordingSink
	clockMu sync.Mutex
	began   time.Time
	ended   time.Time
}

func (sink *turnClock) TurnBegin(ctx context.Context) error {
	sink.clockMu.Lock()
	if sink.began.IsZero() {
		sink.began = time.Now()
	}
	sink.clockMu.Unlock()
	return sink.recordingSink.TurnBegin(ctx)
}

func (sink *turnClock) TurnEnd(ctx context.Context, outcome binding.TurnOutcome) error {
	sink.clockMu.Lock()
	sink.ended = time.Now()
	sink.clockMu.Unlock()
	return sink.recordingSink.TurnEnd(ctx, outcome)
}

// The reasoner is silent by construction, so a turn that needs it produces a
// gap with nothing in it. What keeps that gap from being indistinguishable
// from a finished conversation is that the turn is still open: a client - and
// a benchmark driver - can tell work is owed because nobody said it was done.
//
// This holds only while deliberation happens inside the turn. Moving it back
// outside would close the response first and leave the silence unexplained,
// which is what it used to do.
func TestTheTurnStaysOpenWhileTheReasonerWorks(t *testing.T) {
	const deliberation = 900 * time.Millisecond
	fast := newFast([]continuation.Event{
		{Kind: continuation.EventAssistantDelta, Text: "Let me look that up."},
	})
	slow := &slowProvider{
		scriptedProvider: *newSlow([]continuation.Event{
			{Kind: continuation.EventAssistantDelta, Text: "The balance is $40.00."},
		}),
		delay: deliberation, entered: make(chan struct{}),
	}
	sink := &turnClock{}
	bind, err := cascade.New(cascade.Config{
		Fast: fast, Slow: slow, Speech: toneSpeech{chunks: 1},
		Perception: func() (v1.PerceptionProvider, error) {
			return &scriptedASR{final: "what is my balance"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := bind.Start(context.Background(), binding.Options{
		Sink: sink, SessionID: "open-turn",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background(), nil) })

	speak(t, runtime, 2)
	select {
	case <-slow.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the reasoner never ran, so there was nothing to stay open for")
	}
	waitFor(t, func() bool {
		sink.clockMu.Lock()
		defer sink.clockMu.Unlock()
		return !sink.ended.IsZero()
	}, "the turn never ended")

	sink.clockMu.Lock()
	span := sink.ended.Sub(sink.began)
	sink.clockMu.Unlock()
	if span < deliberation {
		t.Fatalf("the turn closed after %v, before the reasoner had finished thinking for %v: "+
			"a caller would hear silence with nothing saying work was owed", span, deliberation)
	}
}

// A caller waiting on the reasoner hears nothing, because the reasoner never
// speaks. How long that lasts is a property of the question, and a hard
// question and a broken agent sound identical.
//
// The deadline is what makes this a test of the holding turn rather than of
// the reasoner finishing: slow takes six seconds, so a second spoken turn
// inside four of them cannot be the answer - the answer does not exist yet.
func TestALongDeliberationDoesNotLeaveTheUserInSilence(t *testing.T) {
	// More scripted turns than the gap should need, so a holding line that
	// repeated would be visible rather than silently exhausting the script.
	fast := newFast(
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Let me look that up."}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Still checking on that."}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Still going."}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Nearly there."}},
	)
	slow := &slowProvider{
		scriptedProvider: *newSlow([]continuation.Event{
			{Kind: continuation.EventAssistantDelta, Text: "The balance is $40.00."},
		}),
		delay: 6 * time.Second, entered: make(chan struct{}),
	}
	runtime, sink := startSession(t, cascade.Config{
		Fast: fast, Slow: slow, HoldingAfter: 150 * time.Millisecond,
	}, binding.Settings{})
	speak(t, runtime, 2)
	select {
	case <-slow.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the reasoner never started")
	}

	deadline := time.After(4 * time.Second)
	for {
		sink.mu.Lock()
		spoken := append([]string(nil), sink.spoken...)
		sink.mu.Unlock()
		if len(spoken) >= 2 {
			if spoken[1] == spoken[0] {
				t.Fatalf("the gap was filled by repeating the last line: %q", spoken[1])
			}
			// Once, and once only. The reasoner is still working - it has five
			// seconds left - so anything further would be the voice filling
			// the same silence again, which is the repetition it is told to
			// avoid and which sounds like an agent that has lost track.
			time.Sleep(time.Second)
			sink.mu.Lock()
			again := len(sink.spoken)
			sink.mu.Unlock()
			if again > 2 {
				t.Fatalf("the silence was broken %d times over: %v", again, sink.spoken)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("the agent left the user in silence while it deliberated: %v", spoken)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Unconfigured, the behaviour is what it was: the caller waits.
func TestSilenceIsLeftAloneWhenNoHoldingIntervalIsConfigured(t *testing.T) {
	fast := newFast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Let me look that up."}})
	slow := &slowProvider{
		scriptedProvider: *newSlow([]continuation.Event{
			{Kind: continuation.EventAssistantDelta, Text: "The balance is $40.00."},
		}),
		delay: 900 * time.Millisecond, entered: make(chan struct{}),
	}
	runtime, sink := startSession(t, cascade.Config{Fast: fast, Slow: slow}, binding.Settings{})
	speak(t, runtime, 2)
	select {
	case <-slow.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the reasoner never started")
	}
	time.Sleep(450 * time.Millisecond)
	sink.mu.Lock()
	spoken := len(sink.spoken)
	sink.mu.Unlock()
	if spoken > 1 {
		t.Fatalf("nothing should fill the gap unconfigured, got %d turns", spoken)
	}
}

// The reasoner finishing while the holding turn is still being produced is the
// outcome the mechanism is hoping for: the silence was filled, and then it
// stopped being a silence. Cancelling that continuation reports an error, and
// reporting it to the client turns a well-handled gap into a failed session.
func TestAHoldingTurnOvertakenByTheAnswerIsNotAFailure(t *testing.T) {
	// The race made deterministic: the holding turn's continuation blocks
	// until its context is cancelled, and the reasoner returns while it is
	// still blocked. That is exactly the ordering the timing-based version
	// only reached sometimes.
	fast := &blockingSecondTurn{
		scriptedProvider: *newFast(
			[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Let me look that up."}},
			[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Still checking."}},
		),
		blocked: make(chan struct{}),
	}
	slow := &slowProvider{
		scriptedProvider: *newSlow([]continuation.Event{
			{Kind: continuation.EventAssistantDelta, Text: "The balance is $40.00."},
		}),
		delay: 900 * time.Millisecond, entered: make(chan struct{}),
	}
	runtime, sink := startSession(t, cascade.Config{
		Fast: fast, Slow: slow, HoldingAfter: 100 * time.Millisecond,
	}, binding.Settings{})
	speak(t, runtime, 2)
	select {
	case <-slow.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the reasoner never started")
	}
	select {
	case <-fast.blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("the holding turn never started, so the race never happened")
	}
	time.Sleep(2 * time.Second)

	sink.mu.Lock()
	failures := append([]binding.ErrorEvent(nil), sink.failures...)
	sink.mu.Unlock()
	for _, failure := range failures {
		if failure.Code == "holding_error" {
			t.Fatalf("filling a silence that then ended was reported as a fault: %+v", failure)
		}
	}
}

// blockingSecondTurn answers the first turn normally and then blocks, so a
// holding turn is guaranteed to still be in flight when the reasoner returns
// and the turn's context is cancelled.
type blockingSecondTurn struct {
	scriptedProvider
	blocked chan struct{}
	once    sync.Once
	turns   atomic.Int32
}

func (provider *blockingSecondTurn) Continue(
	ctx context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	if provider.turns.Add(1) == 1 {
		return provider.scriptedProvider.Continue(ctx, request, emit)
	}
	provider.once.Do(func() { close(provider.blocked) })
	<-ctx.Done()
	return continuation.Completion{}, ctx.Err()
}
