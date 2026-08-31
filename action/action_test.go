package action_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestLedgerHoldsOneLineForEveryOutputKind(t *testing.T) {
	for _, kind := range []action.Kind{
		action.KindSpeech, action.KindText, action.KindToolCall, action.KindComputerAction,
	} {
		ledger := action.NewLedger()
		id := string(kind)
		if err := ledger.Prepare(action.Commitment{ID: id, Kind: kind}); err != nil {
			t.Fatalf("prepare %s: %v", kind, err)
		}
		if err := ledger.Queue(id); err != nil {
			t.Fatalf("queue %s: %v", kind, err)
		}
		// Before the crossing, everything is cancellable, identically.
		crossed, err := ledger.Cancel(id, "superseded")
		if err != nil || crossed {
			t.Fatalf("%s must be cancellable before it crosses: crossed=%v err=%v", kind, crossed, err)
		}
	}
}

func TestCancellingAfterTheCrossingReportsWhatWasHeard(t *testing.T) {
	ledger := action.NewLedger()
	if err := ledger.Prepare(action.Commitment{ID: "s1", Kind: action.KindSpeech}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := ledger.Emit("s1"); err != nil {
		t.Fatalf("emit: %v", err)
	}
	crossed, err := ledger.Cancel("s1", "barge-in")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !crossed {
		t.Fatal("cancelling emitted output must report that it crossed")
	}
	commitment, _ := ledger.Lookup("s1")
	if commitment.State != action.StatePlayed {
		t.Fatalf("what was heard stays heard, got %s", commitment.State)
	}
}

func TestInvalidationCreatesAnObligationOnlyForWhatCrossed(t *testing.T) {
	ledger := action.NewLedger()
	_ = ledger.Prepare(action.Commitment{ID: "heard", Kind: action.KindSpeech, AssistantItemIDs: []string{"a1"}})
	_ = ledger.Emit("heard")
	_ = ledger.Complete("heard", 420)
	_ = ledger.Prepare(action.Commitment{ID: "unheard", Kind: action.KindSpeech})
	_ = ledger.Queue("unheard")

	if _, created := ledger.Invalidate("unheard", 7); created {
		t.Fatal("content nobody heard owes no repair")
	}
	obligation, created := ledger.Invalidate("heard", 7)
	if !created || obligation.PlayedMS != 420 || obligation.InvalidatedByRevision != 7 {
		t.Fatalf("unexpected obligation %+v created=%v", obligation, created)
	}
	if _, again := ledger.Invalidate("heard", 8); again {
		t.Fatal("one outstanding obligation per commitment")
	}
	if len(ledger.Obligations()) != 1 {
		t.Fatal("the obligation must be outstanding")
	}
	if !ledger.ResolveObligation("heard") || len(ledger.Obligations()) != 0 {
		t.Fatal("resolution must discharge it")
	}
}

func TestDuplicateCallIdentityIsRejected(t *testing.T) {
	ledger := action.NewLedger()
	if err := ledger.Prepare(action.Commitment{ID: "a", Kind: action.KindToolCall, CallID: "c1"}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	err := ledger.Prepare(action.Commitment{ID: "b", Kind: action.KindToolCall, CallID: "c1"})
	if !errors.Is(err, action.ErrDuplicateCall) {
		t.Fatalf("expected duplicate call rejection, got %v", err)
	}
}

type recordingSink struct {
	mu     sync.Mutex
	began  []string
	frames []action.Frame
	ended  []action.Outcome
	failOn int
	seen   int
}

type terminalContextSink struct {
	recordingSink
	endedWith chan error
}

func (sink *terminalContextSink) End(
	ctx context.Context, utterance action.Utterance, outcome action.Outcome,
) error {
	cause := context.Cause(ctx)
	sink.endedWith <- cause
	if cause != nil {
		return cause
	}
	return sink.recordingSink.End(ctx, utterance, outcome)
}

func (sink *recordingSink) Begin(_ context.Context, utterance action.Utterance) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.began = append(sink.began, utterance.ID)
	return nil
}

func (sink *recordingSink) Audio(_ context.Context, _ action.Utterance, frame action.Frame) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.seen++
	if sink.failOn > 0 && sink.seen >= sink.failOn {
		return errors.New("sink failure")
	}
	sink.frames = append(sink.frames, frame)
	return nil
}

func (sink *recordingSink) End(_ context.Context, _ action.Utterance, outcome action.Outcome) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.ended = append(sink.ended, outcome)
	return nil
}

type fakeSpeechProvider struct {
	rate   uint32
	chunks int
	bytes  int
}

func (provider fakeSpeechProvider) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "fake", Version: "1", Capabilities: v1.Capabilities{}}
}

func (provider fakeSpeechProvider) Synthesize(context.Context, v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	return nil, errors.New("unused")
}

func (provider fakeSpeechProvider) Stream(ctx context.Context, plan v1.SpeechPlan, emit func(v1.SpeechChunk) error) error {
	for index := 0; index < provider.chunks; index++ {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		default:
		}
		if err := emit(v1.SpeechChunk{
			ChunkID: "c", CandidateID: plan.CandidateID, SampleRateHz: provider.rate,
			PCM16LE: make([]byte, provider.bytes), Final: index == provider.chunks-1,
		}); err != nil {
			return err
		}
	}
	return nil
}

type playbackRecorder struct {
	mu      sync.Mutex
	handed  time.Duration
	stopped int
}

func (recorder *playbackRecorder) AgentAudioHandedOff(_ string, duration time.Duration) error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.handed += duration
	return nil
}

func (recorder *playbackRecorder) AgentAudioStopped(uint64) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.stopped++
}

func TestSpeechRefusesSilentProducers(t *testing.T) {
	ledger := action.NewLedger()
	speech, err := action.NewSpeech(action.SpeechConfig{
		Provider: fakeSpeechProvider{rate: 24000, chunks: 1, bytes: 4800},
		Sink:     &recordingSink{}, Ledger: ledger,
	})
	if err != nil {
		t.Fatalf("new speech: %v", err)
	}
	err = speech.Enqueue(action.Utterance{ID: "u1", Text: "the balance is forty dollars"}, "silent")
	if !errors.Is(err, action.ErrSilentProducer) {
		t.Fatalf("expected the second cognition boundary to be enforced here, got %v", err)
	}
	if _, exists := ledger.Lookup("u1"); exists {
		t.Fatal("a refused utterance must not enter the ledger")
	}
}

func TestSpeechPacesFramesAndReportsPlayback(t *testing.T) {
	ledger := action.NewLedger()
	sink := &recordingSink{}
	playback := &playbackRecorder{}
	// 24 kHz, 4800 bytes = 2400 samples = 100 ms per chunk, three chunks.
	speech, err := action.NewSpeech(action.SpeechConfig{
		Provider: fakeSpeechProvider{rate: 24000, chunks: 3, bytes: 4800},
		Sink:     sink, Ledger: ledger, Playback: playback,
		FrameDuration: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new speech: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go speech.Run(ctx)
	if err := speech.Enqueue(action.Utterance{ID: "u1", Text: "hello", AssistantItemIDs: []string{"a1"}}, "voice"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		commitment, exists := ledger.Lookup("u1")
		if exists && commitment.State == action.StatePlayed {
			if commitment.PlayedMS != 300 {
				t.Fatalf("expected 300 ms played, got %d", commitment.PlayedMS)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("utterance never completed")
		case <-time.After(5 * time.Millisecond):
		}
	}
	sink.mu.Lock()
	frames := len(sink.frames)
	ended := len(sink.ended)
	sink.mu.Unlock()
	if frames != 3 || ended != 1 {
		t.Fatalf("expected three paced frames and one end, got %d %d", frames, ended)
	}
	playback.mu.Lock()
	handed := playback.handed
	playback.mu.Unlock()
	if handed != 300*time.Millisecond {
		t.Fatalf("playback must be reported as audio is handed off, got %s", handed)
	}
}

func TestCancelDropsQueuedSpeechAndReportsWhatWasHeard(t *testing.T) {
	ledger := action.NewLedger()
	speech, err := action.NewSpeech(action.SpeechConfig{
		Provider: fakeSpeechProvider{rate: 24000, chunks: 1, bytes: 4800},
		Sink:     &recordingSink{}, Ledger: ledger,
	})
	if err != nil {
		t.Fatalf("new speech: %v", err)
	}
	// Nothing is running, so both utterances are queued and fully cancellable.
	if err := speech.Enqueue(action.Utterance{ID: "u1", Text: "one"}, "voice"); err != nil {
		t.Fatalf("enqueue u1: %v", err)
	}
	if err := speech.Enqueue(action.Utterance{ID: "u2", Text: "two"}, "voice"); err != nil {
		t.Fatalf("enqueue u2: %v", err)
	}
	cancelled, heard := speech.Cancel("barge-in")
	if heard {
		t.Fatal("nothing was emitted, so nothing was heard")
	}
	if len(cancelled) != 2 {
		t.Fatalf("expected both queued utterances cancelled, got %d", len(cancelled))
	}
}

func TestCancelMatchingLeavesUnrelatedSpeechAlone(t *testing.T) {
	ledger := action.NewLedger()
	speech, err := action.NewSpeech(action.SpeechConfig{
		Provider: fakeSpeechProvider{rate: 24000, chunks: 1, bytes: 4800},
		Sink:     &recordingSink{}, Ledger: ledger,
	})
	if err != nil {
		t.Fatalf("new speech: %v", err)
	}
	_ = speech.Enqueue(action.Utterance{ID: "fast", Text: "one", Phase: trajectory.PhaseFast}, "voice")
	_ = speech.Enqueue(action.Utterance{ID: "other", Text: "two", Phase: trajectory.PhaseSlow}, "voice")
	cancelled, heard := speech.CancelMatching("superseded", func(utterance action.Utterance) bool {
		return utterance.Phase == trajectory.PhaseFast
	})
	if len(cancelled) != 1 || cancelled[0].ID != "fast" {
		t.Fatalf("expected only the fast utterance cancelled, got %+v", cancelled)
	}
	if len(heard) != 0 {
		t.Fatalf("nothing was emitted, so nothing can be owed a repair, got %+v", heard)
	}
	commitment, _ := ledger.Lookup("other")
	if commitment.State == action.StateCancelled {
		t.Fatal("unrelated queued speech must survive")
	}
}

// Supersession has two outcomes and they are not the same outcome. Speech that
// nobody heard is cancelled and can be forgotten; speech that reached the user
// cannot be, and comes back as something a repair is owed for.
func TestCancelMatchingReportsWhatWasAlreadyHeard(t *testing.T) {
	ledger := action.NewLedger()
	sink := &recordingSink{}
	speech, err := action.NewSpeech(action.SpeechConfig{
		Provider: fakeSpeechProvider{rate: 24000, chunks: 10, bytes: 4800},
		Sink:     sink, Ledger: ledger, FrameDuration: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new speech: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go speech.Run(ctx)

	if err := speech.Enqueue(action.Utterance{
		ID: "heard", Text: "the order is for twelve", Phase: trajectory.PhaseFast,
		SourceRevision: 1, AssistantItemIDs: []string{"assistant-1"},
	}, "voice"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	waitFor(t, func() bool {
		commitment, exists := ledger.Lookup("heard")
		return exists && commitment.State.Crossed()
	}, "the utterance never reached the world")

	cancelled, heard := speech.CancelMatching("superseded", func(utterance action.Utterance) bool {
		return utterance.SourceRevision < 2
	})
	if len(cancelled) != 0 {
		t.Fatalf("audio that was already playing cannot be reported as cancelled, got %+v", cancelled)
	}
	if len(heard) != 1 || heard[0].ID != "heard" {
		t.Fatalf("expected the playing utterance reported as heard, got %+v", heard)
	}
	obligation, created := ledger.Invalidate(heard[0].ID, 2)
	if !created || len(obligation.AssistantItemIDs) != 1 {
		t.Fatalf("invalidating heard content must create an obligation, got %+v", obligation)
	}
}

func TestCancellingActiveSpeechKeepsItsTerminalSinkContextLive(t *testing.T) {
	ledger := action.NewLedger()
	sink := &terminalContextSink{endedWith: make(chan error, 1)}
	speech, err := action.NewSpeech(action.SpeechConfig{
		Provider: fakeSpeechProvider{rate: 24_000, chunks: 20, bytes: 4_800},
		Sink:     sink, Ledger: ledger, FrameDuration: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new speech: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go speech.Run(ctx)
	if err := speech.Enqueue(action.Utterance{
		ID: "cancelled-active", Text: "the first answer is being corrected",
		AssistantItemIDs: []string{"assistant-cancelled-active"},
	}, "voice"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	waitFor(t, func() bool {
		commitment, exists := ledger.Lookup("cancelled-active")
		return exists && commitment.State.Crossed()
	}, "the utterance never reached the world")

	_, heard := speech.Cancel("superseded by a correction")
	if !heard {
		t.Fatal("active speech was not reported as heard")
	}
	select {
	case cause := <-sink.endedWith:
		if cause != nil {
			t.Fatalf("terminal sink context inherited utterance cancellation: %v", cause)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled active speech never reached its terminal sink callback")
	}
}

func waitFor(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(message)
}

func seedCall(t *testing.T, store *trajectory.Store, callID, name string) {
	t.Helper()
	if err := store.AppendBatch([]trajectory.Item{
		{
			ID: "obs-" + callID, Kind: trajectory.KindObservation, MonotonicNS: 1, SourceRevision: 1,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "do it",
		},
		{
			ID: "call-" + callID, Kind: trajectory.KindToolCall, MonotonicNS: 2,
			CausalParentIDs: []string{"obs-" + callID}, SourceRevision: 1, InvocationID: "inv-1",
			Producer: trajectory.Producer{Phase: trajectory.PhaseSlow},
			ToolCall: &trajectory.ToolCall{CallID: callID, Name: name, Arguments: json.RawMessage(`{"a":1}`)},
		},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func newTools(t *testing.T, store *trajectory.Store, spec action.ToolSpec, confirmer action.Confirmer) *action.Tools {
	t.Helper()
	registry := action.NewRegistry()
	if err := registry.Declare(spec); err != nil {
		t.Fatalf("declare: %v", err)
	}
	tools, err := action.NewTools(action.ToolsConfig{
		Registry: registry, Ledger: action.NewLedger(), Store: store, Confirmer: confirmer,
	})
	if err != nil {
		t.Fatalf("new tools: %v", err)
	}
	return tools
}

func echoDispatcher(calls *int) action.Dispatcher {
	return action.DispatcherFunc(func(_ context.Context, call trajectory.ToolCall) (trajectory.ToolResult, error) {
		*calls++
		return trajectory.ToolResult{CallID: call.CallID, Name: call.Name, Output: json.RawMessage(`{"ok":true}`)}, nil
	})
}

// A proposal is structurally non-executable. The check is here, at the point
// of effect, rather than only where providers are configured.
func TestDispatchRefusesCallsWithoutTrajectoryAuthority(t *testing.T) {
	store := trajectory.NewStore()
	if err := store.AppendBatch([]trajectory.Item{
		{
			ID: "obs-1", Kind: trajectory.KindObservation, MonotonicNS: 1, SourceRevision: 1,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "pay",
		},
		{
			ID: "proposal-1", Kind: trajectory.KindToolProposal, MonotonicNS: 2,
			CausalParentIDs: []string{"obs-1"}, SourceRevision: 1,
			Producer: trajectory.Producer{Phase: trajectory.PhaseFast},
			ToolCall: &trajectory.ToolCall{CallID: "c1", Name: "pay", Arguments: json.RawMessage(`{"a":1}`)},
		},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	calls := 0
	tools := newTools(t, store, action.ToolSpec{
		Name: "pay", Description: "pay", Parameters: json.RawMessage(`{"type":"object"}`),
		Dispatcher: echoDispatcher(&calls),
	}, nil)
	_, err := tools.Dispatch(context.Background(), trajectory.ToolCall{
		CallID: "c1", Name: "pay", Arguments: json.RawMessage(`{"a":1}`),
	})
	if !errors.Is(err, action.ErrNoAuthority) {
		t.Fatalf("expected the proposal to be non-executable, got %v", err)
	}
	if calls != 0 {
		t.Fatal("a proposal must never reach a dispatcher")
	}
}

func TestDispatchIsIdempotentByCallID(t *testing.T) {
	store := trajectory.NewStore()
	seedCall(t, store, "c1", "lookup")
	calls := 0
	tools := newTools(t, store, action.ToolSpec{
		Name: "lookup", Description: "lookup", Parameters: json.RawMessage(`{"type":"object"}`),
		Dispatcher: echoDispatcher(&calls),
	}, nil)
	call := trajectory.ToolCall{CallID: "c1", Name: "lookup", Arguments: json.RawMessage(`{"a":1}`)}
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := tools.Dispatch(context.Background(), call); err != nil {
			t.Fatalf("dispatch %d: %v", attempt, err)
		}
	}
	if calls != 1 {
		t.Fatalf("a retried call must not duplicate its external effect, ran %d times", calls)
	}
}

func TestActionAuditCarriesCanonicalProducerPhase(t *testing.T) {
	store := trajectory.NewStore()
	seedCall(t, store, "c1", "lookup")
	registry := action.NewRegistry()
	calls := 0
	if err := registry.Declare(action.ToolSpec{
		Name: "lookup", Description: "lookup", Parameters: json.RawMessage(`{"type":"object"}`),
		Dispatcher: echoDispatcher(&calls),
	}); err != nil {
		t.Fatal(err)
	}
	var audited action.Record
	tools, err := action.NewTools(action.ToolsConfig{
		Registry: registry, Ledger: action.NewLedger(), Store: store,
		Audit: func(record action.Record) { audited = record },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tools.Dispatch(context.Background(), trajectory.ToolCall{
		CallID: "c1", Name: "lookup", Arguments: json.RawMessage(`{"a":1}`),
	}); err != nil {
		t.Fatal(err)
	}
	if audited.ProducerPhase != trajectory.PhaseSlow {
		t.Fatalf("audit phase = %q, want canonical producer %q", audited.ProducerPhase, trajectory.PhaseSlow)
	}
}

func TestDeclaredConfirmationGatesDispatch(t *testing.T) {
	store := trajectory.NewStore()
	seedCall(t, store, "c1", "computer.click")
	calls := 0
	spec := action.ToolSpec{
		Name: "computer.click", Description: "click", Parameters: json.RawMessage(`{"type":"object"}`),
		Confirm: action.ConfirmAlways, Target: "browser-1", Dispatcher: echoDispatcher(&calls),
	}
	call := trajectory.ToolCall{CallID: "c1", Name: "computer.click", Arguments: json.RawMessage(`{"a":1}`)}

	denied := newTools(t, store, spec, nil)
	if _, err := denied.Dispatch(context.Background(), call); !errors.Is(err, action.ErrNotConfirmed) {
		t.Fatalf("an unattended deployment must refuse, got %v", err)
	}
	if calls != 0 {
		t.Fatal("an unconfirmed action must not run")
	}

	var sawTarget string
	allowed := newTools(t, store, spec, action.ConfirmerFunc(
		func(_ context.Context, request action.ConfirmationRequest) (bool, error) {
			sawTarget = request.Target
			return true, nil
		}))
	if _, err := allowed.Dispatch(context.Background(), call); err != nil {
		t.Fatalf("confirmed dispatch: %v", err)
	}
	if calls != 1 || sawTarget != "browser-1" {
		t.Fatalf("confirmation must see the declared target, got %q after %d calls", sawTarget, calls)
	}
}

func TestUnknownToolIsRejectedBeforeAuthority(t *testing.T) {
	store := trajectory.NewStore()
	seedCall(t, store, "c1", "ghost")
	tools := newTools(t, store, action.ToolSpec{
		Name: "real", Description: "real", Parameters: json.RawMessage(`{"type":"object"}`),
		Dispatcher: action.DispatcherFunc(func(context.Context, trajectory.ToolCall) (trajectory.ToolResult, error) {
			return trajectory.ToolResult{}, nil
		}),
	}, nil)
	_, err := tools.Dispatch(context.Background(), trajectory.ToolCall{
		CallID: "c1", Name: "ghost", Arguments: json.RawMessage(`{}`),
	})
	if !errors.Is(err, action.ErrUnknownTool) {
		t.Fatalf("expected unknown-tool rejection, got %v", err)
	}
}

func TestDispatchAllReturnsAnOutcomeForEveryCall(t *testing.T) {
	store := trajectory.NewStore()
	seedCall(t, store, "c1", "lookup")
	calls := 0
	tools := newTools(t, store, action.ToolSpec{
		Name: "lookup", Description: "lookup", Parameters: json.RawMessage(`{"type":"object"}`),
		Dispatcher: echoDispatcher(&calls),
	}, nil)
	results, err := tools.DispatchAll(context.Background(), []trajectory.ToolCall{
		{CallID: "c1", Name: "lookup", Arguments: json.RawMessage(`{"a":1}`)},
		{CallID: "c2", Name: "lookup", Arguments: json.RawMessage(`{"a":2}`)},
	})
	if err != nil {
		t.Fatalf("dispatch all: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("every call needs an outcome, got %d", len(results))
	}
	if results[0].Error != "" {
		t.Fatalf("the authoritative call should have succeeded: %v", results[0].Error)
	}
	if !strings.Contains(results[1].Error, "authority") {
		t.Fatalf("the unauthorised call must report why, got %q", results[1].Error)
	}
}

func TestRegistryReplaceDropsWithdrawnTools(t *testing.T) {
	registry := action.NewRegistry()
	parameters := json.RawMessage(`{"type":"object"}`)
	if err := registry.Declare(action.ToolSpec{Name: "a", Description: "a", Parameters: parameters}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if err := registry.Replace([]action.ToolSpec{{Name: "b", Description: "b", Parameters: parameters}}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if _, exists := registry.Lookup("a"); exists {
		t.Fatal("a withdrawn tool must be gone")
	}
	if specs := registry.Specs(); len(specs) != 1 || specs[0].Name != "b" {
		t.Fatalf("unexpected registry contents %+v", specs)
	}
}

func TestConfirmParsingRejectsUnknownRequirements(t *testing.T) {
	if _, err := action.ParseConfirm("maybe"); err == nil {
		t.Fatal("expected an unknown confirmation requirement to be rejected")
	}
	confirm, err := action.ParseConfirm("")
	if err != nil || confirm != action.ConfirmNever {
		t.Fatalf("an unset requirement means never, got %q %v", confirm, err)
	}
}

// Cancelling twice is a normal race - the speech planner finishing and a
// supersession noticing it are different goroutines - and the ledger says so by
// not erroring. But a caller that recorded a cancellation for the second one
// would submit a visibility transition that already happened, and the log
// refuses cancelled after cancelled. So only the call that actually moved the
// commitment reports it.
func TestCancellingTwiceReportsTheCommitmentOnce(t *testing.T) {
	ledger := action.NewLedger()
	speech, err := action.NewSpeech(action.SpeechConfig{
		Provider: fakeSpeechProvider{rate: 24000, chunks: 1, bytes: 4800},
		Sink:     &recordingSink{}, Ledger: ledger,
	})
	if err != nil {
		t.Fatalf("new speech: %v", err)
	}
	if err := speech.Enqueue(action.Utterance{
		ID: "one", Text: "hello", Phase: trajectory.PhaseFast,
		AssistantItemIDs: []string{"assistant-1"},
	}, "voice"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	first, _ := speech.Cancel("superseded")
	if len(first) != 1 || first[0].ID != "one" {
		t.Fatalf("the first cancellation reports the commitment: %+v", first)
	}
	second, _ := speech.Cancel("superseded again")
	if len(second) != 0 {
		t.Fatalf("a second cancellation has nothing to report: %+v", second)
	}
	third, _ := speech.CancelMatching("and again", func(action.Utterance) bool { return true })
	if len(third) != 0 {
		t.Fatalf("a matching cancellation has nothing to report either: %+v", third)
	}
}
