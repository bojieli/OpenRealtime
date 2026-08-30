package graphs_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	legacyaction "github.com/bojieli/OpenRealtime/action"
	benchrealtimecu "github.com/bojieli/OpenRealtime/bench/realtimecu"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/graph/binding/realtimecu"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/graphs"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestRealtimeComputerUseGraphLaunchesResourceFreeAndCommitsClientEffectFeedback(t *testing.T) {
	target := computeruse.Target{
		Name: "benchmark-browser", Sources: []string{realtimecu.SourceScreen}, Width: 1280, Height: 720,
	}
	descriptor := testRealtimeCUDescriptor()
	observer := newTestRealtimeCUObserver("test-audiovisual-observer")
	observer.audioFinal.Store(false)
	var modelFactories, observerFactories atomic.Int32
	config, err := graphs.RealtimeComputerUseLaunchConfig(realtimecu.PluginConfig{
		RuntimeArtifact: testRealtimeCUArtifact("runtime", "1"),
		Model: realtimecu.ModelPlugin{
			Reference: "go://test/realtime-cu/model/v1",
			Artifact:  testRealtimeCUArtifact("model", "2"), Descriptor: descriptor,
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
				modelFactories.Add(1)
				return &testRealtimeCUModel{descriptor: descriptor}, nil
			},
		},
		Observer: realtimecu.ObserverPlugin{
			Reference: "go://test/realtime-cu/observer/v1", Name: observer.name,
			Artifact: testRealtimeCUArtifact("observer", "3"),
			Sources:  []string{realtimecu.SourceScreen, realtimecu.SourceCamera, realtimecu.SourceMicrophone},
			Factory: func(context.Context, legacy.Options) (realtimecu.Observer, error) {
				observerFactories.Add(1)
				return observer, nil
			},
		},
		Target: target,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config.Artifacts.Values.Data), `"enum":["screen"]`) ||
		!strings.Contains(string(config.Artifacts.Values.Data), `"maximum":1279`) {
		t.Fatal("target-bound values artifact did not retain the exact screen source and viewport")
	}
	launched, err := graphlaunch.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	identity := launched.Plan.Identity()
	if identity.SourceDigest != "sha256:8d9e1cb92213dd4cd3ed59d6f7b10f6566c45bd7649468e434e5ac5514afa303" ||
		identity.LockDigest != "sha256:abb61a946cda1470827778d6193f2ba4ac1e54141f20f8f74026dd216b09f7fd" ||
		identity.GraphFingerprint != "sha256:418950e1e32240812f30714677c43c2eabb72e159f2c5d39a7da435d84cb2757" ||
		identity.PlanFingerprint != "sha256:7780d7e4315c6837dd84944a40e09b275c8cec6a7e6d79fec7c503719dbfef4c" {
		t.Fatalf("Realtime-CU graph artifacts drifted: %+v", identity)
	}
	if modelFactories.Load() != 0 || observerFactories.Load() != 0 {
		t.Fatalf("resource factories ran during graph launch: model=%d observer=%d",
			modelFactories.Load(), observerFactories.Load())
	}
	if launched.Plan.Graph().ID != "realtime_computer_use" ||
		launched.Binding.Name() != realtimecu.ProfileName {
		t.Fatalf("launched graph/provider = %s/%s", launched.Plan.Graph().ID, launched.Binding.Name())
	}

	sink := newTestRealtimeCUSink()
	runtime, err := launched.Binding.Start(context.Background(), legacy.Options{
		Sink: sink, SessionID: "realtime-cu-integration",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if closeErr := runtime.Close(ctx, errors.New("test complete")); closeErr != nil {
			t.Errorf("close realtime-CU runtime: %v", closeErr)
		}
	}()
	if modelFactories.Load() > 1 || observerFactories.Load() != 1 {
		t.Fatalf("session factories immediately after start = model %d observer %d",
			modelFactories.Load(), observerFactories.Load())
	}
	specs := testRealtimeCUToolSpecs(t, target)
	if err := runtime.Update(context.Background(), legacy.Settings{
		Instruction: "click the visible control", Tools: []legacyaction.ToolSpec{specs[0]},
		Observers: []string{observer.name},
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Audio(context.Background(), perception.Frame{
		Kind: perception.FrameAudio, Source: realtimecu.SourceMicrophone, CapturedNS: 100,
		PCM16LE: []byte{1, 0, 2, 0}, SampleRateHz: 24_000,
	}); err != nil {
		t.Fatal(err)
	}
	partial := receiveRealtimeCU(t, sink.observations, "provisional user observation")
	if partial.Final || !partial.Provisional {
		t.Fatalf("first audio observation was not provisional: %+v", partial)
	}
	select {
	case call := <-sink.calls:
		t.Fatalf("provisional user observation activated a client effect: %+v", call)
	case <-time.After(100 * time.Millisecond):
	}
	observer.audioFinal.Store(true)
	if err := runtime.Audio(context.Background(), perception.Frame{
		Kind: perception.FrameAudio, Source: realtimecu.SourceMicrophone, CapturedNS: 101,
		PCM16LE: []byte{3, 0, 4, 0}, SampleRateHz: 24_000,
	}); err != nil {
		t.Fatal(err)
	}
	callEvent := receiveRealtimeCU(t, sink.calls, "client call")
	if modelFactories.Load() != 1 {
		t.Fatalf("model session factories after first cognition = %d, want one", modelFactories.Load())
	}
	if len(callEvent.Calls) != 1 || callEvent.Calls[0].Name != computeruse.Click ||
		callEvent.Calls[0].CallID == "" || callEvent.InvocationID == "" {
		t.Fatalf("client call event = %+v", callEvent)
	}
	call := callEvent.Calls[0]
	if err := runtime.ToolResult(context.Background(), trajectory.ToolResult{
		CallID: call.CallID, Name: call.Name, Output: json.RawMessage(`{"clicked":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	feedback := receiveRealtimeCU(t, observer.consequences, "visual consequence")
	if feedback.CallID != call.CallID || feedback.Name != call.Name ||
		feedback.TargetSource != realtimecu.SourceScreen || feedback.CanonicalResultItemID == "" ||
		feedback.StoreVersion == 0 {
		t.Fatalf("visual consequence = %+v", feedback)
	}
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: 200,
		Image: []byte{1, 2, 3}, MIMEType: "image/png", Width: 1280, Height: 720,
	}); err != nil {
		t.Fatal(err)
	}
	for {
		observed := receiveRealtimeCU(t, sink.observations, "screen observation")
		if observed.Source == realtimecu.SourceScreen {
			if observed.Authority != trajectory.AuthorityObserver {
				t.Fatalf("screen authority = %q", observed.Authority)
			}
			break
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := runtime.Trajectory()
		var hasCall, hasResult, hasFeedback bool
		for _, item := range snapshot.Items {
			hasCall = hasCall || item.Kind == trajectory.KindToolCall
			hasResult = hasResult || item.Kind == trajectory.KindToolResult
			hasFeedback = hasFeedback || (item.Kind == trajectory.KindObservation &&
				item.Observation != nil && item.Observation.Source == realtimecu.SourceScreen)
		}
		if hasCall && hasResult && hasFeedback {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("canonical trajectory did not retain action/result/visual feedback: %+v", runtime.Trajectory())
}

func TestRealtimeComputerUseHarnessHasSixteenCasesBehindOneStableEndpointContract(t *testing.T) {
	cases, err := benchrealtimecu.Select(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 16 {
		t.Fatalf("Realtime-CU harness cases = %d, want 16", len(cases))
	}
	seen := make(map[string]struct{}, len(cases))
	for _, item := range cases {
		if _, duplicate := seen[item.ID()]; duplicate {
			t.Fatalf("duplicate Realtime-CU case %q", item.ID())
		}
		seen[item.ID()] = struct{}{}
	}
	// Endpoint selection is suite-wide in Options, rather than attached to a
	// task or grounding. Therefore every selected case exercises the unchanged
	// OpenAI Realtime extension route through the same graph-native provider.
	options := benchrealtimecu.Options{Endpoint: "ws://127.0.0.1:8765/v1/realtime"}
	if options.Endpoint != "ws://127.0.0.1:8765/v1/realtime" {
		t.Fatalf("stable Realtime endpoint = %q", options.Endpoint)
	}
}

func testRealtimeCUDescriptor() continuation.Descriptor {
	return continuation.Descriptor{
		Provider: "test", Model: "realtime-cu", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, Streaming: true,
		ToolAuthority:   continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthoritySilent,
	}
}

func testRealtimeCUArtifact(name, digit string) inspect.ArtifactIdentity {
	return inspect.ArtifactIdentity{
		ID: "artifact://test/realtime-cu/" + name, Revision: "v1",
		Digest: "sha256:" + strings.Repeat(digit, 64),
	}
}

type testRealtimeCUModel struct{ descriptor continuation.Descriptor }

func (model *testRealtimeCUModel) Descriptor() continuation.Descriptor { return model.descriptor }
func (*testRealtimeCUModel) Continue(
	_ context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	if len(request.Trajectory.Items) == 0 ||
		request.Trajectory.Items[len(request.Trajectory.Items)-1].Producer.Phase != trajectory.PhaseUser {
		return continuation.Completion{StopReason: "stop"}, nil
	}
	call := trajectory.ToolCall{
		CallID: request.InvocationID + ":click", Name: computeruse.Click,
		Arguments: json.RawMessage(`{"source":"screen","x":10,"y":20}`),
	}
	if err := emit(continuation.Event{Kind: continuation.EventToolCall, ToolCall: &call}); err != nil {
		return continuation.Completion{}, err
	}
	return continuation.Completion{StopReason: "tool_call"}, nil
}

type testRealtimeCUObserver struct {
	name         string
	mu           sync.Mutex
	revisions    map[string]uint64
	consequences chan realtimecu.VisualConsequence
	closed       atomic.Bool
	audioFinal   atomic.Bool
}

func newTestRealtimeCUObserver(name string) *testRealtimeCUObserver {
	observer := &testRealtimeCUObserver{
		name: name, revisions: make(map[string]uint64),
		consequences: make(chan realtimecu.VisualConsequence, 4),
	}
	observer.audioFinal.Store(true)
	return observer
}

func (observer *testRealtimeCUObserver) observation(
	frame perception.Frame, authority trajectory.Authority,
) []perception.Observation {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	observer.revisions[frame.Source]++
	text := "click the visible control"
	if frame.Source != realtimecu.SourceMicrophone {
		text = "screen after client action"
	}
	final := frame.Source != realtimecu.SourceMicrophone || observer.audioFinal.Load()
	return []perception.Observation{{
		Text: text, Observer: observer.name, Source: frame.Source, Authority: authority,
		Revision: observer.revisions[frame.Source], StableText: text,
		Provisional: !final, Final: final,
	}}
}

func (observer *testRealtimeCUObserver) Audio(
	_ context.Context, frame perception.Frame,
) ([]perception.Observation, error) {
	return observer.observation(frame, trajectory.AuthorityUser), nil
}

func (observer *testRealtimeCUObserver) Video(
	_ context.Context, frame perception.Frame,
) ([]perception.Observation, error) {
	return observer.observation(frame, trajectory.AuthorityObserver), nil
}

func (observer *testRealtimeCUObserver) Consequence(
	_ context.Context, feedback realtimecu.VisualConsequence,
) error {
	observer.consequences <- feedback
	return nil
}

func (observer *testRealtimeCUObserver) Close() error {
	observer.closed.Store(true)
	return nil
}

type testRealtimeCUSink struct {
	calls        chan legacy.ToolCallEvent
	observations chan perception.Observation
	transcripts  chan legacy.TranscriptEvent
}

func newTestRealtimeCUSink() *testRealtimeCUSink {
	return &testRealtimeCUSink{
		calls: make(chan legacy.ToolCallEvent, 8), observations: make(chan perception.Observation, 16),
		transcripts: make(chan legacy.TranscriptEvent, 8),
	}
}

func (*testRealtimeCUSink) TurnBegin(context.Context) error                      { return nil }
func (*testRealtimeCUSink) TurnEnd(context.Context, legacy.TurnOutcome) error    { return nil }
func (*testRealtimeCUSink) Activity(context.Context, legacy.ActivityEvent) error { return nil }
func (sink *testRealtimeCUSink) Transcript(_ context.Context, event legacy.TranscriptEvent) error {
	sink.transcripts <- event
	return nil
}
func (sink *testRealtimeCUSink) Observation(_ context.Context, observation perception.Observation) error {
	sink.observations <- observation
	return nil
}
func (*testRealtimeCUSink) SpeechBegin(context.Context, action.Utterance) error        { return nil }
func (*testRealtimeCUSink) SpeechText(context.Context, action.Utterance, string) error { return nil }
func (*testRealtimeCUSink) SpeechAudio(context.Context, action.Utterance, action.Frame) error {
	return nil
}
func (*testRealtimeCUSink) SpeechEnd(context.Context, action.Utterance, action.Outcome) error {
	return nil
}
func (sink *testRealtimeCUSink) ToolCalls(_ context.Context, event legacy.ToolCallEvent) error {
	sink.calls <- event
	return nil
}
func (*testRealtimeCUSink) Failed(context.Context, legacy.ErrorEvent) {}

type testRealtimeCUDispatcher struct{}

func (testRealtimeCUDispatcher) Name() string { return "test-client-dispatcher" }
func (testRealtimeCUDispatcher) Dispatch(
	context.Context, trajectory.ToolCall,
) (trajectory.ToolResult, error) {
	return trajectory.ToolResult{}, errors.New("test declaration dispatcher must not run")
}

func testRealtimeCUToolSpecs(t *testing.T, target computeruse.Target) []legacyaction.ToolSpec {
	t.Helper()
	overrides := make(map[string]legacyaction.Confirm, len(computeruse.Names()))
	for _, name := range computeruse.Names() {
		overrides[name] = legacyaction.ConfirmNever
	}
	specs, err := computeruse.Specs(target, testRealtimeCUDispatcher{}, overrides)
	if err != nil {
		t.Fatal(err)
	}
	return specs
}

func receiveRealtimeCU[T any](t *testing.T, source <-chan T, name string) T {
	t.Helper()
	select {
	case value := <-source:
		return value
	case <-time.After(5 * time.Second):
		var zero T
		t.Fatalf("timed out waiting for %s", name)
		return zero
	}
}
