package binding_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	legacy "github.com/bojieli/OpenRealtime/binding"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestCompatibilityBindingRunsCurrentSessionThroughGraph(t *testing.T) {
	underlying := &stubBinding{name: "stub"}
	wrapped, err := graphbinding.New(underlying)
	if err != nil {
		t.Fatal(err)
	}
	if wrapped.Graph().Fingerprint == "" || wrapped.Graph().Nodes[0].Implementation != "compat.binding/stub" {
		t.Fatalf("graph identity is incomplete: %+v", wrapped.Graph())
	}
	sink := &recordingSink{}
	live, err := wrapped.Start(context.Background(), legacy.Options{
		Sink: sink, SessionID: "session-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if underlying.sawDebug.Load() || underlying.sawReservations.Load() {
		t.Fatal("compatibility proxy advertised optional sink interfaces absent from downstream")
	}
	input := legacy.TextInput{ItemID: "user-1", Role: "user", Text: "hello"}
	if err := live.Text(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	sink.waitFor(t, 5)
	if got, want := sink.events(), []string{"turn.begin", "speech.begin", "speech.text:hello", "speech.end", "turn.end"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sink events = %v, want %v", got, want)
	}
	status := live.Status()
	if status.Graph.ID != wrapped.Graph().ID ||
		status.Graph.Fingerprint != wrapped.Graph().Fingerprint || status.Binding != "stub" {
		t.Fatalf("status = %+v", status)
	}
	inspected := live.(interface{ Live() inspect.Live }).Live()
	if inspected.Fingerprint != wrapped.Graph().Fingerprint || inspected.Edges["boundary:text"].Enqueued != 1 {
		t.Fatalf("live graph = %+v", inspected)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := live.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if underlying.live.closed.Load() != 1 {
		t.Fatalf("underlying Close count = %d, want 1", underlying.live.closed.Load())
	}
}

func TestCompatibilityBindingPreservesOptionalSinkCapabilities(t *testing.T) {
	underlying := &stubBinding{name: "optional"}
	wrapped, err := graphbinding.New(underlying)
	if err != nil {
		t.Fatal(err)
	}
	sink := &optionalSink{recordingSink: &recordingSink{}}
	live, err := wrapped.Start(context.Background(), legacy.Options{Sink: sink})
	if err != nil {
		t.Fatal(err)
	}
	if !underlying.sawDebug.Load() || !underlying.sawReservations.Load() {
		t.Fatal("compatibility proxy did not preserve optional sink interfaces")
	}
	if err := live.Text(context.Background(), legacy.TextInput{ItemID: "x", Role: "user", Text: "optional"}); err != nil {
		t.Fatal(err)
	}
	sink.waitFor(t, 7)
	if got := sink.events(); !slices.Contains(got, "reserved") || !slices.Contains(got, "debug") {
		t.Fatalf("optional events = %v", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := live.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCompatibilityBindingDrainsSynchronousStartEvents(t *testing.T) {
	underlying := &stubBinding{name: "start-event", emitOnStart: true}
	wrapped, err := graphbinding.New(underlying)
	if err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	live, err := wrapped.Start(ctx, legacy.Options{Sink: sink, SessionID: "start-session"})
	if err != nil {
		t.Fatal(err)
	}
	if got := sink.events(); !slices.Contains(got, "activity:started") {
		t.Fatalf("a synchronous Binding.Start event was not drained: %v", got)
	}
	if err := live.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCompatibilityBindingPropagatesSynchronousSinkErrors(t *testing.T) {
	underlying := &stubBinding{name: "sink-error"}
	wrapped, err := graphbinding.New(underlying)
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("downstream speech failed")
	sink := &failingSink{recordingSink: &recordingSink{}, failure: want}
	live, err := wrapped.Start(context.Background(), legacy.Options{Sink: sink})
	if err != nil {
		t.Fatal(err)
	}
	err = live.Text(context.Background(), legacy.TextInput{ItemID: "x", Role: "user", Text: "fail"})
	if !errors.Is(err, want) {
		t.Fatalf("Text error = %v, want %v", err, want)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = live.Close(ctx, nil)
}

func TestCompatibilityBindingDeliversCloseEventsBeforeEgressCloses(t *testing.T) {
	underlying := &stubBinding{name: "close-event", emitOnClose: true}
	wrapped, err := graphbinding.New(underlying)
	if err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}
	live, err := wrapped.Start(context.Background(), legacy.Options{Sink: sink})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := live.Close(ctx, errors.New("client disconnected")); err != nil {
		t.Fatal(err)
	}
	if got := sink.events(); !slices.Contains(got, "transcript:closing") {
		t.Fatalf("underlying Close event was lost: %v", got)
	}
}

func TestCompatibilityBindingOwnsNestedDebugEvidence(t *testing.T) {
	underlying := &stubBinding{name: "debug-clone", mutateDebug: true}
	wrapped, err := graphbinding.New(underlying)
	if err != nil {
		t.Fatal(err)
	}
	sink := &optionalSink{recordingSink: &recordingSink{}}
	live, err := wrapped.Start(context.Background(), legacy.Options{Sink: sink})
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Text(context.Background(), legacy.TextInput{ItemID: "x", Role: "user", Text: "clone"}); err != nil {
		t.Fatal(err)
	}
	debug := sink.lastDebug()
	nested, ok := debug.Payload["items"].([]any)
	if !ok || len(nested) != 1 {
		t.Fatalf("debug payload = %#v", debug.Payload)
	}
	entry, ok := nested[0].(map[string]any)
	if !ok || entry["value"] != "before" {
		t.Fatalf("debug payload mutated after admission: %#v", debug.Payload)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := live.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCompatibilityInputOwnsMutableMedia(t *testing.T) {
	underlying := &stubBinding{
		name: "media", blockAudio: make(chan struct{}), audioEntered: make(chan struct{}),
	}
	wrapped, err := graphbinding.New(underlying)
	if err != nil {
		t.Fatal(err)
	}
	live, err := wrapped.Start(context.Background(), legacy.Options{Sink: &recordingSink{}})
	if err != nil {
		t.Fatal(err)
	}
	frame := perception.Frame{
		Kind: perception.FrameAudio, Source: "microphone", PCM16LE: []byte{1, 2, 3, 4}, SampleRateHz: 16_000,
	}
	done := make(chan error, 1)
	go func() { done <- live.Audio(context.Background(), frame) }()
	select {
	case <-underlying.audioEntered:
	case <-time.After(time.Second):
		t.Fatal("underlying audio call did not start")
	}
	// The graph call has copied the frame before waiting for the underlying
	// runtime. Mutating the caller's buffer must not alter the admitted item.
	frame.PCM16LE[0] = 99
	close(underlying.blockAudio)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := underlying.live.lastAudio(); len(got) == 0 || got[0] != 1 {
		t.Fatalf("underlying audio = %v", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = live.Close(ctx, nil)
}

func TestCompatibilityGraphIdentityIncludesSelectedBinding(t *testing.T) {
	left, err := graphbinding.New(&stubBinding{name: "left"})
	if err != nil {
		t.Fatal(err)
	}
	right, err := graphbinding.New(&stubBinding{name: "right"})
	if err != nil {
		t.Fatal(err)
	}
	if left.Graph().Fingerprint == right.Graph().Fingerprint {
		t.Fatal("two selected binding implementations have one graph fingerprint")
	}
	if err := left.Graph().Validate(); err != nil {
		t.Fatal(err)
	}
	if err := right.Graph().Validate(); err != nil {
		t.Fatal(err)
	}
}

type stubBinding struct {
	name            string
	live            *stubRuntime
	sawDebug        atomic.Bool
	sawReservations atomic.Bool
	blockAudio      chan struct{}
	audioEntered    chan struct{}
	emitOnStart     bool
	emitOnClose     bool
	mutateDebug     bool
}

func (binding *stubBinding) Name() string { return binding.name }
func (*stubBinding) Ownership() legacy.Ownership {
	return legacy.Ownership{
		Perception: legacy.OwnerEngine, FastCognition: legacy.OwnerEngine,
		SlowCognition: legacy.OwnerEngine, Action: legacy.OwnerEngine,
		Interaction: legacy.OwnerEngine, Floor: legacy.OwnerEngine,
	}
}
func (*stubBinding) Capabilities() legacy.Capabilities {
	return legacy.Capabilities{FastSlow: true, Voice: legacy.VoiceControl{InForce: "test"}}
}
func (binding *stubBinding) Start(ctx context.Context, options legacy.Options) (legacy.Runtime, error) {
	if options.Sink == nil {
		return nil, errors.New("sink required")
	}
	binding.sawDebug.Store(isDebugSink(options.Sink))
	binding.sawReservations.Store(isReservationSink(options.Sink))
	if binding.emitOnStart {
		if err := options.Sink.Activity(ctx, legacy.ActivityEvent{Started: true, ItemID: "startup"}); err != nil {
			return nil, err
		}
	}
	binding.live = &stubRuntime{
		sink: options.Sink, blockAudio: binding.blockAudio, audioEntered: binding.audioEntered,
		emitOnClose: binding.emitOnClose, mutateDebug: binding.mutateDebug,
	}
	return binding.live, nil
}

type stubRuntime struct {
	sink         legacy.Sink
	closed       atomic.Int32
	blockAudio   <-chan struct{}
	audioEntered chan struct{}
	emitOnClose  bool
	mutateDebug  bool
	mu           sync.Mutex
	audio        []byte
}

func (*stubRuntime) Update(context.Context, legacy.Settings) error { return nil }
func (runtime *stubRuntime) Audio(_ context.Context, frame perception.Frame) error {
	if runtime.audioEntered != nil {
		close(runtime.audioEntered)
	}
	if runtime.blockAudio != nil {
		<-runtime.blockAudio
	}
	runtime.mu.Lock()
	runtime.audio = slices.Clone(frame.PCM16LE)
	runtime.mu.Unlock()
	return nil
}
func (*stubRuntime) Video(context.Context, perception.Frame) error { return nil }
func (runtime *stubRuntime) Text(ctx context.Context, input legacy.TextInput) error {
	utterance := action.Utterance{ID: "speech-1", Text: input.Text}
	if sink, ok := runtime.sink.(legacy.SpeechReservationSink); ok {
		if err := sink.SpeechReserved(ctx, utterance); err != nil {
			return err
		}
	}
	if sink, ok := runtime.sink.(legacy.DebugSink); ok {
		event := legacy.DebugEvent{Category: "test", Name: "debug"}
		var mutable map[string]any
		if runtime.mutateDebug {
			mutable = map[string]any{"value": "before"}
			event.Payload = map[string]any{"items": []any{mutable}}
		}
		if err := sink.Debug(ctx, event); err != nil {
			return err
		}
		if mutable != nil {
			mutable["value"] = "after"
		}
	}
	if err := runtime.sink.TurnBegin(ctx); err != nil {
		return err
	}
	if err := runtime.sink.SpeechBegin(ctx, utterance); err != nil {
		return err
	}
	if err := runtime.sink.SpeechText(ctx, utterance, input.Text); err != nil {
		return err
	}
	if err := runtime.sink.SpeechEnd(ctx, utterance, action.Outcome{Completed: true}); err != nil {
		return err
	}
	return runtime.sink.TurnEnd(ctx, legacy.TurnOutcome{})
}
func (*stubRuntime) ToolResult(context.Context, trajectory.ToolResult) error { return nil }
func (*stubRuntime) CommitAudio(context.Context) error                       { return nil }
func (*stubRuntime) CreateResponse(context.Context) error                    { return nil }
func (*stubRuntime) Cancel(context.Context, string) error                    { return nil }
func (*stubRuntime) Truncate(context.Context, legacy.Truncation) error       { return nil }
func (*stubRuntime) Trajectory() trajectory.Snapshot                         { return trajectory.Snapshot{} }
func (runtime *stubRuntime) Status() legacy.Status {
	return legacy.Status{Binding: "stub"}
}
func (runtime *stubRuntime) Close(ctx context.Context, _ error) error {
	runtime.closed.Add(1)
	if runtime.emitOnClose {
		return runtime.sink.Transcript(ctx, legacy.TranscriptEvent{ItemID: "closing", Text: "closing", Final: true})
	}
	return nil
}
func (runtime *stubRuntime) lastAudio() []byte {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return slices.Clone(runtime.audio)
}

type recordingSink struct {
	mu     sync.Mutex
	record []string
	signal chan struct{}
}

func (sink *recordingSink) add(event string) {
	sink.mu.Lock()
	sink.record = append(sink.record, event)
	if sink.signal != nil {
		close(sink.signal)
	}
	sink.signal = make(chan struct{})
	sink.mu.Unlock()
}
func (sink *recordingSink) events() []string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return slices.Clone(sink.record)
}
func (sink *recordingSink) waitFor(t *testing.T, count int) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		sink.mu.Lock()
		if len(sink.record) >= count {
			sink.mu.Unlock()
			return
		}
		if sink.signal == nil {
			sink.signal = make(chan struct{})
		}
		signal := sink.signal
		sink.mu.Unlock()
		select {
		case <-signal:
		case <-deadline:
			t.Fatalf("got only %d sink events: %v", len(sink.events()), sink.events())
		}
	}
}
func (sink *recordingSink) TurnBegin(context.Context) error { sink.add("turn.begin"); return nil }
func (sink *recordingSink) TurnEnd(context.Context, legacy.TurnOutcome) error {
	sink.add("turn.end")
	return nil
}
func (sink *recordingSink) Activity(ctx context.Context, event legacy.ActivityEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if event.Started {
		sink.add("activity:started")
	}
	return nil
}
func (sink *recordingSink) Transcript(ctx context.Context, event legacy.TranscriptEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sink.add("transcript:" + event.Text)
	return nil
}
func (*recordingSink) Observation(context.Context, perception.Observation) error { return nil }
func (sink *recordingSink) SpeechBegin(context.Context, action.Utterance) error {
	sink.add("speech.begin")
	return nil
}
func (sink *recordingSink) SpeechText(_ context.Context, _ action.Utterance, text string) error {
	sink.add("speech.text:" + text)
	return nil
}
func (sink *recordingSink) SpeechAudio(context.Context, action.Utterance, action.Frame) error {
	sink.add("speech.audio")
	return nil
}
func (sink *recordingSink) SpeechEnd(context.Context, action.Utterance, action.Outcome) error {
	sink.add("speech.end")
	return nil
}
func (*recordingSink) ToolCalls(context.Context, legacy.ToolCallEvent) error { return nil }
func (sink *recordingSink) Failed(_ context.Context, event legacy.ErrorEvent) {
	sink.add("failed:" + event.Code)
}

type optionalSink struct {
	*recordingSink
	debug legacy.DebugEvent
}

func (sink *optionalSink) SpeechReserved(context.Context, action.Utterance) error {
	sink.add("reserved")
	return nil
}
func (sink *optionalSink) SpeechReservationCancelled(context.Context, action.Utterance) {
	sink.add("reservation.cancelled")
}
func (sink *optionalSink) Debug(ctx context.Context, event legacy.DebugEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sink.add("debug")
	sink.mu.Lock()
	sink.debug = event
	sink.mu.Unlock()
	return nil
}

func (sink *optionalSink) lastDebug() legacy.DebugEvent {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.debug
}

type failingSink struct {
	*recordingSink
	failure error
}

func (sink *failingSink) SpeechText(context.Context, action.Utterance, string) error {
	return sink.failure
}

func isDebugSink(sink legacy.Sink) bool {
	_, ok := sink.(legacy.DebugSink)
	return ok
}
func isReservationSink(sink legacy.Sink) bool {
	_, ok := sink.(legacy.SpeechReservationSink)
	return ok
}

var (
	_ legacy.Binding                = (*stubBinding)(nil)
	_ legacy.Runtime                = (*stubRuntime)(nil)
	_ legacy.Sink                   = (*recordingSink)(nil)
	_ legacy.SpeechReservationSink  = (*optionalSink)(nil)
	_ legacy.DebugSink              = (*optionalSink)(nil)
	_ interface{ Graph() ir.Graph } = (*graphbinding.Binding)(nil)
)
