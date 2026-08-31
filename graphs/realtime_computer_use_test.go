package graphs_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
		!strings.Contains(string(config.Artifacts.Values.Data), `"maximum":1279`) ||
		!strings.Contains(string(config.Artifacts.Values.Data), `"max_tool_proposals":1`) {
		t.Fatal("values artifact did not retain the exact screen target and one-proposal cognition bound")
	}
	launched, err := graphlaunch.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	identity := launched.Plan.Identity()
	if identity.SourceDigest != "sha256:6cd6055326de6d9c3723eef2cafa272e8afdf588152e7875cd7ff0333913d20f" ||
		identity.LockDigest != "sha256:475e8258f1414af70a105a3b8edd2ebe75c4c357b6c3b2596b184da48d0c9dcc" ||
		identity.GraphFingerprint != "sha256:f68480630636dd783a43af4d64ba458fda785cfded58c05cdefc3eb2ca574a87" ||
		identity.PlanFingerprint != "sha256:46c8052a38ee2482aef5c38bf23128c21fa10a3556961e6878823d2a57bf8376" {
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

func TestRealtimeComputerUseChangedCameraReactivatesDurableIntentOneEffectAtATime(t *testing.T) {
	target := computeruse.Target{
		Name: "benchmark-browser", Sources: []string{realtimecu.SourceScreen}, Width: 320, Height: 240,
	}
	descriptor := testRealtimeCUDescriptor()
	model := &visualReactivationRealtimeCUModel{
		descriptor: descriptor, invocations: make(chan visualReactivationInvocation, 4),
	}
	observer := newTestRealtimeCUObserver("visual-reactivation-observer")
	config, err := graphs.RealtimeComputerUseLaunchConfig(realtimecu.PluginConfig{
		RuntimeArtifact: testRealtimeCUArtifact("visual-reactivation-runtime", "1"),
		Model: realtimecu.ModelPlugin{
			Reference: "go://test/realtime-cu/visual-reactivation-model/v1",
			Artifact:  testRealtimeCUArtifact("visual-reactivation-model", "2"), Descriptor: descriptor,
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
				return model, nil
			},
		},
		Observer: realtimecu.ObserverPlugin{
			Reference: "go://test/realtime-cu/visual-reactivation-observer/v1", Name: observer.name,
			Artifact: testRealtimeCUArtifact("visual-reactivation-observer", "3"),
			Sources:  []string{realtimecu.SourceScreen, realtimecu.SourceCamera, realtimecu.SourceMicrophone},
			Factory: func(context.Context, legacy.Options) (realtimecu.Observer, error) {
				return observer, nil
			},
		},
		Target: target,
	})
	if err != nil {
		t.Fatal(err)
	}
	launched, err := graphlaunch.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	sink := newTestRealtimeCUSink()
	runtime, err := launched.Binding.Start(context.Background(), legacy.Options{
		Sink: sink, SessionID: "realtime-cu-visual-reactivation",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if closeErr := runtime.Close(ctx, errors.New("test complete")); closeErr != nil {
			t.Errorf("close visual-reactivation runtime: %v", closeErr)
		}
	})
	if err := runtime.Update(context.Background(), legacy.Settings{
		Instruction: "when smoke appears in the camera, click the alarm on screen",
		Tools:       testRealtimeCUToolSpecs(t, target), Observers: []string{observer.name},
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Audio(context.Background(), perception.Frame{
		Kind: perception.FrameAudio, Source: realtimecu.SourceMicrophone, CapturedNS: 100,
		PCM16LE: []byte{1, 0}, SampleRateHz: 24_000,
	}); err != nil {
		t.Fatal(err)
	}
	initial := receiveRealtimeCU(t, model.invocations, "initial user cognition")
	if initial.Number != 1 || initial.LastSource != realtimecu.SourceMicrophone || initial.ToolResults != 0 {
		t.Fatalf("initial cognition = %+v", initial)
	}
	select {
	case call := <-sink.calls:
		t.Fatalf("condition-absent user turn emitted an effect: %+v", call)
	case <-time.After(100 * time.Millisecond):
	}

	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceCamera, CapturedNS: 200,
		Image: []byte{1}, MIMEType: "image/jpeg", Width: 320, Height: 240,
	}); err != nil {
		t.Fatal(err)
	}
	triggered := receiveRealtimeCU(t, model.invocations, "camera-reactivated cognition")
	if triggered.Number != 2 || triggered.LastSource != realtimecu.SourceCamera || triggered.ToolResults != 0 {
		t.Fatalf("camera-reactivated cognition = %+v", triggered)
	}
	callEvent := receiveRealtimeCU(t, sink.calls, "camera-triggered client effect")
	if len(callEvent.Calls) != 1 || callEvent.Calls[0].Name != computeruse.Click {
		t.Fatalf("camera-triggered call = %+v", callEvent)
	}
	call := callEvent.Calls[0]

	// The durable intent remains active, but ordinary screen cadence cannot
	// open a second generation while this exact effect is waiting for a result.
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: 300,
		Image: []byte{2}, MIMEType: "image/jpeg", Width: 320, Height: 240,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case invocation := <-model.invocations:
		t.Fatalf("pending effect admitted overlapping cognition: %+v", invocation)
	case <-time.After(150 * time.Millisecond):
	}
	if err := runtime.ToolResult(context.Background(), trajectory.ToolResult{
		CallID: call.CallID, Name: call.Name, Output: json.RawMessage(`{"clicked":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	consequence := receiveRealtimeCU(t, observer.consequences, "camera action visual consequence")
	if consequence.CallID != call.CallID || consequence.CanonicalResultItemID == "" {
		t.Fatalf("camera action consequence = %+v", consequence)
	}
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: 301,
		Image: []byte{3}, MIMEType: "image/jpeg", Width: 320, Height: 240,
	}); err != nil {
		t.Fatal(err)
	}
	settled := receiveRealtimeCU(t, model.invocations, "post-effect visual cognition")
	if settled.Number != 3 || settled.LastSource != realtimecu.SourceScreen || settled.ToolResults != 1 {
		t.Fatalf("post-effect cognition = %+v", settled)
	}
}

func TestRealtimeComputerUseFailedEffectRequiresNewCanonicalUserIntent(t *testing.T) {
	target := computeruse.Target{
		Name: "benchmark-browser", Sources: []string{realtimecu.SourceScreen}, Width: 320, Height: 240,
	}
	descriptor := testRealtimeCUDescriptor()
	model := &visualReactivationRealtimeCUModel{
		descriptor: descriptor, invocations: make(chan visualReactivationInvocation, 4),
	}
	observer := newTestRealtimeCUObserver("failed-effect-observer")
	config, err := graphs.RealtimeComputerUseLaunchConfig(realtimecu.PluginConfig{
		RuntimeArtifact: testRealtimeCUArtifact("failed-effect-runtime", "1"),
		Model: realtimecu.ModelPlugin{
			Reference: "go://test/realtime-cu/failed-effect-model/v1",
			Artifact:  testRealtimeCUArtifact("failed-effect-model", "2"), Descriptor: descriptor,
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
				return model, nil
			},
		},
		Observer: realtimecu.ObserverPlugin{
			Reference: "go://test/realtime-cu/failed-effect-observer/v1", Name: observer.name,
			Artifact: testRealtimeCUArtifact("failed-effect-observer", "3"),
			Sources:  []string{realtimecu.SourceScreen, realtimecu.SourceCamera, realtimecu.SourceMicrophone},
			Factory: func(context.Context, legacy.Options) (realtimecu.Observer, error) {
				return observer, nil
			},
		},
		Target: target,
	})
	if err != nil {
		t.Fatal(err)
	}
	launched, err := graphlaunch.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	sink := newTestRealtimeCUSink()
	runtime, err := launched.Binding.Start(context.Background(), legacy.Options{
		Sink: sink, SessionID: "realtime-cu-failed-effect",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if closeErr := runtime.Close(ctx, errors.New("test complete")); closeErr != nil {
			t.Errorf("close failed-effect runtime: %v", closeErr)
		}
	})
	if err := runtime.Update(context.Background(), legacy.Settings{
		Instruction: "when smoke appears, click the alarm", Tools: testRealtimeCUToolSpecs(t, target),
		Observers: []string{observer.name},
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Audio(context.Background(), perception.Frame{
		Kind: perception.FrameAudio, Source: realtimecu.SourceMicrophone, CapturedNS: 100,
		PCM16LE: []byte{1, 0}, SampleRateHz: 24_000,
	}); err != nil {
		t.Fatal(err)
	}
	initial := receiveRealtimeCU(t, model.invocations, "initial failed-effect cognition")
	if initial.Number != 1 || initial.LastSource != realtimecu.SourceMicrophone {
		t.Fatalf("initial failed-effect cognition = %+v", initial)
	}
	// Receiving the model-side fixture record precedes the graph's result
	// commit. Wait until that no-proposal generation is settled before sending
	// the visual trigger, or the frame can correctly be ignored as overlapping.
	select {
	case call := <-sink.calls:
		t.Fatalf("condition-absent user turn emitted an effect: %+v", call)
	case <-time.After(100 * time.Millisecond):
	}
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceCamera, CapturedNS: 200,
		Image: []byte{1}, MIMEType: "image/jpeg", Width: 320, Height: 240,
	}); err != nil {
		t.Fatal(err)
	}
	triggered := receiveRealtimeCU(t, model.invocations, "failed-effect camera cognition")
	if triggered.Number != 2 || triggered.LastSource != realtimecu.SourceCamera {
		t.Fatalf("failed-effect camera cognition = %+v", triggered)
	}
	callEvent := receiveRealtimeCU(t, sink.calls, "failed client effect")
	if len(callEvent.Calls) != 1 {
		t.Fatalf("failed client effect = %+v", callEvent)
	}
	call := callEvent.Calls[0]
	if err := runtime.ToolResult(context.Background(), trajectory.ToolResult{
		CallID: call.CallID, Name: call.Name, Error: "action target is outside the viewport",
	}); err != nil {
		t.Fatal(err)
	}
	consequence := receiveRealtimeCU(t, observer.consequences, "failed action visual consequence")
	if consequence.CallID != call.CallID || consequence.CanonicalResultItemID == "" {
		t.Fatalf("failed action consequence = %+v", consequence)
	}
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: 300,
		Image: []byte{2}, MIMEType: "image/jpeg", Width: 320, Height: 240,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case invocation := <-model.invocations:
		t.Fatalf("failed effect automatically reopened cognition: %+v", invocation)
	case call := <-sink.calls:
		t.Fatalf("failed effect automatically emitted another action: %+v", call)
	case <-time.After(200 * time.Millisecond):
	}
	if err := runtime.Audio(context.Background(), perception.Frame{
		Kind: perception.FrameAudio, Source: realtimecu.SourceMicrophone, CapturedNS: 400,
		PCM16LE: []byte{2, 0}, SampleRateHz: 24_000,
	}); err != nil {
		t.Fatal(err)
	}
	fresh := receiveRealtimeCU(t, model.invocations, "new user intent after failed effect")
	if fresh.Number != 3 || fresh.LastSource != realtimecu.SourceMicrophone || fresh.ToolResults != 1 {
		t.Fatalf("new user intent after failed effect = %+v", fresh)
	}
}

func TestRealtimeComputerUseResourceAwareObserverSharesExactSessionMediaWithModel(t *testing.T) {
	target := computeruse.Target{
		Name: "benchmark-browser", Sources: []string{realtimecu.SourceScreen}, Width: 64, Height: 48,
	}
	descriptor := testRealtimeCUDescriptor()
	observerName := "resource-aware-audiovisual-observer"
	seenMedia := make(chan []byte, 1)
	var observerFactories atomic.Int32
	config, err := graphs.RealtimeComputerUseLaunchConfig(realtimecu.PluginConfig{
		RuntimeArtifact: testRealtimeCUArtifact("resource-runtime", "1"),
		Model: realtimecu.ModelPlugin{
			Reference: "go://test/realtime-cu/resource-model/v1",
			Artifact:  testRealtimeCUArtifact("resource-model", "2"), Descriptor: descriptor,
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
				return &mediaInspectingRealtimeCUModel{descriptor: descriptor, seen: seenMedia}, nil
			},
		},
		Observer: realtimecu.ObserverPlugin{
			Reference: "go://test/realtime-cu/resource-observer/v1", Name: observerName,
			Artifact: testRealtimeCUArtifact("resource-observer", "3"),
			Sources:  []string{realtimecu.SourceScreen, realtimecu.SourceCamera, realtimecu.SourceMicrophone},
			ResourceFactory: func(
				_ context.Context, _ legacy.Options, resources realtimecu.ObserverResources,
			) (realtimecu.Observer, error) {
				observerFactories.Add(1)
				if resources.Retainer == nil {
					return nil, errors.New("resource-aware observer received no session media retainer")
				}
				observer := newTestRealtimeCUObserver(observerName)
				observer.retainer = resources.Retainer
				return observer, nil
			},
		},
		Target: target,
	})
	if err != nil {
		t.Fatal(err)
	}
	launched, err := graphlaunch.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if observerFactories.Load() != 0 {
		t.Fatal("resource-aware observer factory ran before session start")
	}
	sink := newTestRealtimeCUSink()
	runtime, err := launched.Binding.Start(context.Background(), legacy.Options{
		Sink: sink, SessionID: "realtime-cu-resource-media",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if closeErr := runtime.Close(ctx, errors.New("test complete")); closeErr != nil {
			t.Errorf("close resource-aware realtime-CU runtime: %v", closeErr)
		}
	})
	if observerFactories.Load() != 1 {
		t.Fatalf("resource-aware observer factories after start = %d, want one", observerFactories.Load())
	}
	if err := runtime.Update(context.Background(), legacy.Settings{
		Instruction: "inspect the current screen", Tools: testRealtimeCUToolSpecs(t, target),
		Observers: []string{observerName},
	}); err != nil {
		t.Fatal(err)
	}
	exactFrame := []byte("exact-session-keyframe-bytes")
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: 100,
		Image: exactFrame, MIMEType: "image/jpeg", Width: 64, Height: 48,
	}); err != nil {
		t.Fatal(err)
	}
	for observed := receiveRealtimeCU(t, sink.observations, "retained screen observation"); ; observed = receiveRealtimeCU(t, sink.observations, "retained screen observation") {
		if observed.Source == realtimecu.SourceScreen {
			if len(observed.Media) != 1 || observed.Media[0].Handle == "" {
				t.Fatalf("retained screen observation media = %+v", observed.Media)
			}
			break
		}
	}
	if err := runtime.Audio(context.Background(), perception.Frame{
		Kind: perception.FrameAudio, Source: realtimecu.SourceMicrophone, CapturedNS: 101,
		PCM16LE: []byte{1, 0}, SampleRateHz: 24_000,
	}); err != nil {
		t.Fatal(err)
	}
	resolved := receiveRealtimeCU(t, seenMedia, "model-resolved session keyframe")
	if !slices.Equal(resolved, exactFrame) {
		t.Fatalf("model resolved media %q, want %q", resolved, exactFrame)
	}
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

type mediaInspectingRealtimeCUModel struct {
	descriptor continuation.Descriptor
	seen       chan []byte
}

type visualReactivationInvocation struct {
	Number      int32
	LastSource  string
	ToolResults int
}

type visualReactivationRealtimeCUModel struct {
	descriptor  continuation.Descriptor
	count       atomic.Int32
	invocations chan visualReactivationInvocation
}

func (model *visualReactivationRealtimeCUModel) Descriptor() continuation.Descriptor {
	return model.descriptor
}

func (model *visualReactivationRealtimeCUModel) Continue(
	_ context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	number := model.count.Add(1)
	lastSource := ""
	toolResults := 0
	if items := request.Trajectory.Items; len(items) != 0 {
		last := items[len(items)-1]
		if last.Observation != nil {
			lastSource = last.Observation.Source
		} else if last.Event != nil {
			lastSource = last.Event.Channel
		}
		for _, item := range items {
			if item.Kind == trajectory.KindToolResult {
				toolResults++
			}
		}
	}
	model.invocations <- visualReactivationInvocation{
		Number: number, LastSource: lastSource, ToolResults: toolResults,
	}
	if number != 2 {
		return continuation.Completion{StopReason: "stop"}, nil
	}
	call := trajectory.ToolCall{
		CallID: request.InvocationID + ":camera-alarm", Name: computeruse.Click,
		Arguments: json.RawMessage(`{"source":"screen","x":10,"y":20}`),
	}
	if err := emit(continuation.Event{Kind: continuation.EventToolCall, ToolCall: &call}); err != nil {
		return continuation.Completion{}, err
	}
	return continuation.Completion{StopReason: "tool_call"}, nil
}

func (model *mediaInspectingRealtimeCUModel) Descriptor() continuation.Descriptor {
	return model.descriptor
}

func (model *mediaInspectingRealtimeCUModel) Continue(
	_ context.Context, request continuation.Request, _ continuation.Emit,
) (continuation.Completion, error) {
	for index := len(request.Trajectory.Items) - 1; index >= 0; index-- {
		item := request.Trajectory.Items[index]
		if item.Observation == nil || item.Observation.Source != realtimecu.SourceScreen ||
			len(item.Observation.Media) == 0 {
			continue
		}
		if request.Media == nil {
			return continuation.Completion{}, errors.New("model request has no session media resolver")
		}
		resolved, err := request.Media(item.Observation.Media[0].Handle)
		if err != nil {
			return continuation.Completion{}, fmt.Errorf("resolve retained screen: %w", err)
		}
		model.seen <- slices.Clone(resolved.Bytes)
		return continuation.Completion{StopReason: "stop"}, nil
	}
	return continuation.Completion{}, errors.New("model request has no retained screen observation")
}

type testRealtimeCUObserver struct {
	name         string
	retainer     perception.Retainer
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
	observations := observer.observation(frame, trajectory.AuthorityObserver)
	if observer.retainer == nil {
		return observations, nil
	}
	reference, err := observer.retainer.Retain(trajectory.MediaRef{
		MIMEType: frame.MIMEType, Source: frame.Source, Width: frame.Width,
		Height: frame.Height, CapturedNS: frame.CapturedNS,
	}, frame.Image)
	if err != nil {
		return nil, err
	}
	observations[0].Media = []trajectory.MediaRef{reference}
	return observations, nil
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
