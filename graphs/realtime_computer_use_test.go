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
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	"github.com/bojieli/OpenRealtime/graph/binding/realtimecu"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/graphs"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
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
		RuntimeArtifact:  testRealtimeCUArtifact("runtime", "1"),
		SettlementPolicy: testRealtimeCUPolicyPlugin(),
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
			ResourceFactory: func(
				_ context.Context, _ legacy.Options, resources realtimecu.ObserverResources,
			) (realtimecu.Observer, error) {
				observerFactories.Add(1)
				observer.retainer = resources.Retainer
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
		!strings.Contains(string(config.Artifacts.Values.Data), `"max_tool_proposals":1`) ||
		!strings.Contains(string(config.Artifacts.Values.Data),
			"Preserve every ordinary spoken word's full spelling and every digit exactly") ||
		!strings.Contains(string(config.Artifacts.Values.Data),
			"Never type those punctuation names literally") ||
		!strings.Contains(string(config.Artifacts.Values.Data),
			"emit only the single tool call and no plan, narration, or assistant prose") {
		t.Fatal("values artifact did not retain the exact screen target, cognition bound, and dictated-identifier rule")
	}
	if !strings.Contains(string(config.Artifacts.Values.Data),
		`"expected_admission":{"mode":"after_intent","source_set":"observed_before_intent"}`) {
		t.Fatal("values artifact did not retain activation's independent temporal-admission contract")
	}
	launched, err := graphlaunch.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	identity := launched.Plan.Identity()
	if identity.SourceDigest != "sha256:c4e359429b829c4d33aa13771717ed04f239bf6afa2da4275d637936903e6085" ||
		identity.LockDigest != "sha256:6c213003767acaf3aeea6512d6880eb5a452ff25fe44ca388943c5c66c349926" ||
		identity.ValuesDigest != "sha256:ca50f15e6193b0684436f31d7c624e6321287ce4acc2dee38e77eb1e56248d01" ||
		identity.GraphFingerprint != "sha256:1e62c889887ac191b1368d7f067493c732ba4f1cb400d21067126cf3dfc53c41" ||
		identity.PlanFingerprint != "sha256:3d5efe455ddc85fbe3c30bd51340adfe24143a6fb2f1305b3a6901a5a55b5d75" {
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
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: 90,
		Image: []byte{0}, MIMEType: "image/png", Width: 1280, Height: 720,
	}); err != nil {
		t.Fatal(err)
	}
	receiveRealtimeCUObservation(t, sink.observations, realtimecu.SourceScreen, 90)
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
	select {
	case call := <-sink.calls:
		t.Fatalf("final intent activated before refreshed screen evidence: %+v", call)
	case <-time.After(100 * time.Millisecond):
	}
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: 102,
		Image: []byte{1}, MIMEType: "image/png", Width: 1280, Height: 720,
	}); err != nil {
		t.Fatal(err)
	}
	receiveRealtimeCUObservation(t, sink.observations, realtimecu.SourceScreen, 102)
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
		RuntimeArtifact:  testRealtimeCUArtifact("visual-reactivation-runtime", "1"),
		SettlementPolicy: testRealtimeCUPolicyPlugin(),
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
			ResourceFactory: func(
				_ context.Context, _ legacy.Options, resources realtimecu.ObserverResources,
			) (realtimecu.Observer, error) {
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
	for index, source := range []string{realtimecu.SourceScreen, realtimecu.SourceCamera} {
		if err := runtime.Video(context.Background(), perception.Frame{
			Kind: perception.FrameImage, Source: source, CapturedNS: uint64(80 + index),
			Image: []byte{byte(index + 1)}, MIMEType: "image/jpeg", Width: 320, Height: 240,
		}); err != nil {
			t.Fatal(err)
		}
		receiveRealtimeCUObservation(t, sink.observations, source, uint64(80+index))
	}
	if err := runtime.Audio(context.Background(), perception.Frame{
		Kind: perception.FrameAudio, Source: realtimecu.SourceMicrophone, CapturedNS: 100,
		PCM16LE: []byte{1, 0}, SampleRateHz: 24_000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: 110,
		Image: []byte{3}, MIMEType: "image/jpeg", Width: 320, Height: 240,
	}); err != nil {
		t.Fatal(err)
	}
	receiveRealtimeCUObservation(t, sink.observations, realtimecu.SourceScreen, 110)
	select {
	case invocation := <-model.invocations:
		t.Fatalf("fresh screen activated before frozen camera refreshed: %+v", invocation)
	case <-time.After(100 * time.Millisecond):
	}
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceCamera, CapturedNS: 120,
		Image: []byte{4}, MIMEType: "image/jpeg", Width: 320, Height: 240,
	}); err != nil {
		t.Fatal(err)
	}
	receiveRealtimeCUObservation(t, sink.observations, realtimecu.SourceCamera, 120)
	initial := receiveRealtimeCU(t, model.invocations, "initial user cognition")
	if initial.Number != 1 || initial.LastSource != realtimecu.SourceCamera || initial.ToolResults != 0 {
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
	// A fresh provider call ID does not make the same successful semantic
	// effect admissible under the same durable user intent. Suppression is a
	// typed pre-effect terminal: it releases activation without fabricating a
	// tool result, forcing a visual consequence, or reaching the client sink.
	select {
	case event := <-sink.calls:
		t.Fatalf("successful effect replay reached the client boundary: %+v", event)
	case <-time.After(200 * time.Millisecond):
	}

	// Suppression settles only that cognition run. A genuinely new camera
	// observation may reconsider the still-durable intent, but another identical
	// successful effect remains suppressed before the client boundary.
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceCamera, CapturedNS: 400,
		Image: []byte{5}, MIMEType: "image/jpeg", Width: 320, Height: 240,
	}); err != nil {
		t.Fatal(err)
	}
	retried := receiveRealtimeCU(t, model.invocations, "independent-evidence retry")
	if retried.Number != 4 || retried.LastSource != realtimecu.SourceCamera || retried.ToolResults != 1 {
		t.Fatalf("independent-evidence retry = %+v", retried)
	}
	select {
	case event := <-sink.calls:
		t.Fatalf("successful effect replay after independent evidence reached the client: %+v", event)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestRealtimeComputerUseFocusTypeSubmitRetriesIndeterminateAndSettlesUnderContinuousCadence(t *testing.T) {
	target := computeruse.Target{
		Name: "benchmark-browser", Sources: []string{realtimecu.SourceScreen}, Width: 320, Height: 240,
	}
	descriptor := testRealtimeCUDescriptor()
	model := &focusTypeSubmitRealtimeCUModel{
		descriptor: descriptor, invocations: make(chan int32, 8),
	}
	policyDescriptor := testRealtimeCUPolicyDescriptor()
	policy := &scriptedRealtimeCUDispositionDecider{
		descriptor: policyDescriptor,
		choices: []policyelements.IntentDispositionKind{
			policyelements.IntentDispositionIndeterminate,
			policyelements.IntentDispositionContinue,
			policyelements.IntentDispositionContinue,
			policyelements.IntentDispositionSucceeded,
		},
		decisions: make(chan policyelements.IntentDispositionKind, 4),
	}
	observer := newTestRealtimeCUObserver("focus-type-submit-observer")
	config, err := graphs.RealtimeComputerUseLaunchConfig(realtimecu.PluginConfig{
		RuntimeArtifact: testRealtimeCUArtifact("focus-type-submit-runtime", "1"),
		SettlementPolicy: realtimecu.PolicyPlugin{
			Reference:  realtimecu.SettlementPolicyReference,
			Artifact:   testRealtimeCUArtifact("focus-type-submit-policy", "4"),
			Descriptor: policyDescriptor,
			Factory: func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) {
				return policy, nil
			},
		},
		Model: realtimecu.ModelPlugin{
			Reference: "go://test/realtime-cu/focus-type-submit-model/v1",
			Artifact:  testRealtimeCUArtifact("focus-type-submit-model", "2"), Descriptor: descriptor,
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
				return model, nil
			},
		},
		Observer: realtimecu.ObserverPlugin{
			Reference: "go://test/realtime-cu/focus-type-submit-observer/v1", Name: observer.name,
			Artifact: testRealtimeCUArtifact("focus-type-submit-observer", "3"),
			Sources:  []string{realtimecu.SourceScreen, realtimecu.SourceCamera, realtimecu.SourceMicrophone},
			ResourceFactory: func(
				_ context.Context, _ legacy.Options, resources realtimecu.ObserverResources,
			) (realtimecu.Observer, error) {
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
	sink := newTestRealtimeCUSink()
	runtime, err := launched.Binding.Start(context.Background(), legacy.Options{
		Sink: sink, SessionID: "realtime-cu-focus-type-submit",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if closeErr := runtime.Close(ctx, errors.New("test complete")); closeErr != nil {
			t.Errorf("close focus-type-submit runtime: %v", closeErr)
		}
	})
	if err := runtime.Update(context.Background(), legacy.Settings{
		Instruction: "focus the field, type alpha-9, and submit it",
		Tools:       testRealtimeCUToolSpecs(t, target), Observers: []string{observer.name},
	}); err != nil {
		t.Fatal(err)
	}

	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: 90,
		Image: []byte{1}, MIMEType: "image/jpeg", Width: 320, Height: 240,
	}); err != nil {
		t.Fatal(err)
	}
	receiveRealtimeCUObservation(t, sink.observations, realtimecu.SourceScreen, 90)
	if err := runtime.Audio(context.Background(), perception.Frame{
		Kind: perception.FrameAudio, Source: realtimecu.SourceMicrophone, CapturedNS: 100,
		PCM16LE: []byte{1, 0}, SampleRateHz: 24_000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: 110,
		Image: []byte{2}, MIMEType: "image/jpeg", Width: 320, Height: 240,
	}); err != nil {
		t.Fatal(err)
	}
	receiveRealtimeCUObservation(t, sink.observations, realtimecu.SourceScreen, 110)

	steps := []struct {
		name        string
		arguments   string
		disposition policyelements.IntentDispositionKind
	}{
		{name: computeruse.Click, arguments: `{"source":"screen","x":10,"y":20}`,
			disposition: policyelements.IntentDispositionContinue},
		{name: computeruse.Type, arguments: `{"source":"screen","text":"alpha-9"}`,
			disposition: policyelements.IntentDispositionContinue},
		{name: computeruse.Key, arguments: `{"source":"screen","keys":["ENTER"]}`,
			disposition: policyelements.IntentDispositionSucceeded},
	}
	var previousResultVersion uint64
	for index, step := range steps {
		if invocation := receiveRealtimeCU(t, model.invocations, "focus-type-submit model invocation"); invocation != int32(index+1) {
			t.Fatalf("focus-type-submit invocation = %d, want %d", invocation, index+1)
		}
		event := receiveRealtimeCU(t, sink.calls, step.name+" client effect")
		if len(event.Calls) != 1 || event.Calls[0].Name != step.name ||
			string(event.Calls[0].Arguments) != step.arguments {
			t.Fatalf("focus-type-submit step %d = %+v", index+1, event)
		}
		call := event.Calls[0]
		if err := runtime.ToolResult(context.Background(), trajectory.ToolResult{
			CallID: call.CallID, Name: call.Name,
			Output: json.RawMessage(fmt.Sprintf(`{"ok":true,"step":%d}`, index+1)),
		}); err != nil {
			t.Fatal(err)
		}
		consequence := receiveRealtimeCU(t, observer.consequences, step.name+" visual consequence")
		if consequence.CallID != call.CallID || consequence.Name != step.name ||
			consequence.CanonicalResultItemID == "" || consequence.StoreVersion <= previousResultVersion {
			t.Fatalf("focus-type-submit consequence %d = %+v", index+1, consequence)
		}
		previousResultVersion = consequence.StoreVersion
		captured := uint64(200 + index)
		if err := runtime.Video(context.Background(), perception.Frame{
			Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: captured,
			Image: []byte{byte(index + 3)}, MIMEType: "image/jpeg", Width: 320, Height: 240,
		}); err != nil {
			t.Fatal(err)
		}
		receiveRealtimeCUObservation(t, sink.observations, realtimecu.SourceScreen, captured)
		if index == 0 {
			if decision := receiveRealtimeCU(t, policy.decisions, step.name+" indeterminate settlement decision"); decision != policyelements.IntentDispositionIndeterminate {
				t.Fatalf("focus-type-submit initial disposition = %q, want indeterminate", decision)
			}
		}
		if decision := receiveRealtimeCU(t, policy.decisions, step.name+" settlement decision"); decision != step.disposition {
			t.Fatalf("focus-type-submit disposition %d = %q, want %q",
				index+1, decision, step.disposition)
		}
	}

	// Terminal settlement must withstand ordinary changing screen cadence. It
	// suppresses the completed durable intent without imposing a one-action
	// limit on the three distinct effects that preceded it.
	for index := 0; index < 5; index++ {
		captured := uint64(300 + index)
		if err := runtime.Video(context.Background(), perception.Frame{
			Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: captured,
			Image: []byte{byte(10 + index)}, MIMEType: "image/jpeg", Width: 320, Height: 240,
		}); err != nil {
			t.Fatal(err)
		}
		receiveRealtimeCUObservation(t, sink.observations, realtimecu.SourceScreen, captured)
	}
	select {
	case invocation := <-model.invocations:
		t.Fatalf("terminal focus-type-submit intent reactivated under cadence: %d", invocation)
	case <-time.After(250 * time.Millisecond):
	}
	select {
	case extra := <-policy.decisions:
		t.Fatalf("terminal focus-type-submit intent was classified again as %q", extra)
	default:
	}

	// A new final user observation resets the terminal latch but still needs its
	// own post-intent visual evidence before the next cognition run.
	if err := runtime.Audio(context.Background(), perception.Frame{
		Kind: perception.FrameAudio, Source: realtimecu.SourceMicrophone, CapturedNS: 500,
		PCM16LE: []byte{2, 0}, SampleRateHz: 24_000,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case invocation := <-model.invocations:
		t.Fatalf("replacement intent activated without fresh screen evidence: %d", invocation)
	case <-time.After(100 * time.Millisecond):
	}
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: 600,
		Image: []byte{20}, MIMEType: "image/jpeg", Width: 320, Height: 240,
	}); err != nil {
		t.Fatal(err)
	}
	receiveRealtimeCUObservation(t, sink.observations, realtimecu.SourceScreen, 600)
	if invocation := receiveRealtimeCU(t, model.invocations, "replacement intent cognition"); invocation != 4 {
		t.Fatalf("replacement intent invocation = %d, want 4", invocation)
	}
}

func TestRealtimeComputerUseToolAdmissionSuppressesPlaceholderAndReleasesNextVisual(t *testing.T) {
	target := computeruse.Target{
		Name: "benchmark-browser", Sources: []string{realtimecu.SourceScreen}, Width: 320, Height: 240,
	}
	descriptor := testRealtimeCUDescriptor()
	model := &placeholderThenClickRealtimeCUModel{
		descriptor: descriptor, invocations: make(chan visualReactivationInvocation, 2),
	}
	observer := newTestRealtimeCUObserver("tool-admission-observer")
	config, err := graphs.RealtimeComputerUseLaunchConfig(realtimecu.PluginConfig{
		RuntimeArtifact:  testRealtimeCUArtifact("tool-admission-runtime", "1"),
		SettlementPolicy: testRealtimeCUPolicyPlugin(),
		Model: realtimecu.ModelPlugin{
			Reference: "go://test/realtime-cu/tool-admission-model/v1",
			Artifact:  testRealtimeCUArtifact("tool-admission-model", "2"), Descriptor: descriptor,
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
				return model, nil
			},
		},
		Observer: realtimecu.ObserverPlugin{
			Reference: "go://test/realtime-cu/tool-admission-observer/v1", Name: observer.name,
			Artifact: testRealtimeCUArtifact("tool-admission-observer", "3"),
			Sources:  []string{realtimecu.SourceScreen, realtimecu.SourceCamera, realtimecu.SourceMicrophone},
			ResourceFactory: func(
				_ context.Context, _ legacy.Options, resources realtimecu.ObserverResources,
			) (realtimecu.Observer, error) {
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
	sink := newTestRealtimeCUSink()
	runtime, err := launched.Binding.Start(context.Background(), legacy.Options{
		Sink: sink, SessionID: "realtime-cu-tool-admission",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if closeErr := runtime.Close(ctx, errors.New("test complete")); closeErr != nil {
			t.Errorf("close tool-admission runtime: %v", closeErr)
		}
	})
	if err := runtime.Update(context.Background(), legacy.Settings{
		Instruction: "when the visual threshold appears, click it without placeholder actions",
		Tools:       testRealtimeCUToolSpecs(t, target), Observers: []string{observer.name},
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: 90,
		Image: []byte{1}, MIMEType: "image/jpeg", Width: 320, Height: 240,
	}); err != nil {
		t.Fatal(err)
	}
	receiveRealtimeCUObservation(t, sink.observations, realtimecu.SourceScreen, 90)
	if err := runtime.Audio(context.Background(), perception.Frame{
		Kind: perception.FrameAudio, Source: realtimecu.SourceMicrophone, CapturedNS: 100,
		PCM16LE: []byte{1, 0}, SampleRateHz: 24_000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: 110,
		Image: []byte{2}, MIMEType: "image/jpeg", Width: 320, Height: 240,
	}); err != nil {
		t.Fatal(err)
	}
	receiveRealtimeCUObservation(t, sink.observations, realtimecu.SourceScreen, 110)
	first := receiveRealtimeCU(t, model.invocations, "placeholder proposal cognition")
	if first.Number != 1 || first.LastSource != realtimecu.SourceScreen {
		t.Fatalf("first cognition = %+v", first)
	}
	for {
		debug := receiveRealtimeCU(t, sink.debug, "tool-admission outcome")
		if debug.Name == "tool_admission_outcome" {
			break
		}
	}
	select {
	case event := <-sink.calls:
		t.Fatalf("denied placeholder crossed the client boundary: %+v", event)
	default:
	}
	select {
	case consequence := <-observer.consequences:
		t.Fatalf("denied placeholder requested a visual consequence: %+v", consequence)
	default:
	}

	// The suppression is not merely an in-memory activation shortcut. Wait for
	// the activation append to traverse the graph's trajectory Mux and for the
	// exact commit acknowledgement to return through its Tee before presenting
	// the next observation.
	dispositionSnapshot, deniedProposal, disposition :=
		waitForRealtimeCUToolPolicyDisposition(t, runtime, computeruse.Wait)
	if disposition.ToolProposalDisposition.ProposalItemID != deniedProposal.ID ||
		disposition.ToolProposalDisposition.CallID != deniedProposal.ToolCall.CallID ||
		disposition.ToolProposalDisposition.Name != deniedProposal.ToolCall.Name ||
		disposition.ToolProposalDisposition.Kind != trajectory.ToolProposalToolPolicySuppressed ||
		disposition.InvocationID != deniedProposal.InvocationID ||
		disposition.SourceRevision != deniedProposal.SourceRevision ||
		disposition.Producer.Phase != trajectory.PhaseRuntime ||
		!slices.Contains(disposition.CausalParentIDs, deniedProposal.ID) ||
		disposition.ToolCall != nil || disposition.Content != "" {
		t.Fatalf("canonical denied-proposal disposition mismatch: proposal=%+v disposition=%+v",
			deniedProposal, disposition)
	}
	wireDisposition, err := json.Marshal(disposition)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wireDisposition), `"arguments"`) ||
		strings.Contains(string(wireDisposition), "duration_ms") {
		t.Fatalf("canonical disposition copied denied proposal arguments: %s", wireDisposition)
	}
	terminalProposals, terminalEvidence := trajectory.TerminalToolProposalIDs(dispositionSnapshot)
	if _, ok := terminalProposals[deniedProposal.ID]; !ok {
		t.Fatalf("canonical disposition did not terminate proposal %q: %v",
			deniedProposal.ID, terminalProposals)
	}
	if _, ok := terminalEvidence[disposition.ID]; !ok {
		t.Fatalf("canonical disposition evidence %q was not validated: %v",
			disposition.ID, terminalEvidence)
	}
	for _, item := range dispositionSnapshot.Items {
		if item.ToolCall != nil && item.ToolCall.CallID == deniedProposal.ToolCall.CallID &&
			item.Kind != trajectory.KindToolProposal {
			t.Fatalf("denied proposal acquired executable authority: %+v", item)
		}
		if item.ToolResult != nil && item.ToolResult.CallID == deniedProposal.ToolCall.CallID {
			t.Fatalf("denied proposal fabricated a canonical tool result: %+v", item)
		}
	}

	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: 200,
		Image: []byte{84}, MIMEType: "image/jpeg", Width: 320, Height: 240,
	}); err != nil {
		t.Fatal(err)
	}
	second := receiveRealtimeCU(t, model.invocations, "visual generation after placeholder suppression")
	if second.Number != 2 || second.LastSource != realtimecu.SourceScreen || second.ToolResults != 0 {
		t.Fatalf("post-suppression cognition = %+v", second)
	}
	callEvent := receiveRealtimeCU(t, sink.calls, "post-suppression click")
	if len(callEvent.Calls) != 1 || callEvent.Calls[0].Name != computeruse.Click {
		t.Fatalf("post-suppression client call = %+v", callEvent)
	}
	call := callEvent.Calls[0]
	if err := runtime.ToolResult(context.Background(), trajectory.ToolResult{
		CallID: call.CallID, Name: call.Name, Output: json.RawMessage(`{"clicked":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	consequence := receiveRealtimeCU(t, observer.consequences, "post-suppression click consequence")
	if consequence.CallID != call.CallID || consequence.Name != computeruse.Click {
		t.Fatalf("post-suppression consequence = %+v", consequence)
	}
}

func TestRealtimeComputerUseGraphNormalizesOnlyTheDeclaredCoordinatePairShape(t *testing.T) {
	target := computeruse.Target{
		Name: "benchmark-browser", Sources: []string{realtimecu.SourceScreen}, Width: 1280, Height: 720,
	}
	descriptor := testRealtimeCUDescriptor()
	observer := newTestRealtimeCUObserver("coordinate-pair-observer")
	config, err := graphs.RealtimeComputerUseLaunchConfig(realtimecu.PluginConfig{
		RuntimeArtifact:  testRealtimeCUArtifact("coordinate-pair-runtime", "1"),
		SettlementPolicy: testRealtimeCUPolicyPlugin(),
		Model: realtimecu.ModelPlugin{
			Reference: "go://test/realtime-cu/coordinate-pair-model/v1",
			Artifact:  testRealtimeCUArtifact("coordinate-pair-model", "2"), Descriptor: descriptor,
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
				return &coordinatePairRealtimeCUModel{descriptor: descriptor}, nil
			},
		},
		Observer: realtimecu.ObserverPlugin{
			Reference: "go://test/realtime-cu/coordinate-pair-observer/v1", Name: observer.name,
			Artifact: testRealtimeCUArtifact("coordinate-pair-observer", "3"),
			Sources:  []string{realtimecu.SourceScreen, realtimecu.SourceCamera, realtimecu.SourceMicrophone},
			ResourceFactory: func(
				_ context.Context, _ legacy.Options, resources realtimecu.ObserverResources,
			) (realtimecu.Observer, error) {
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
	sink := newTestRealtimeCUSink()
	runtime, err := launched.Binding.Start(context.Background(), legacy.Options{
		Sink: sink, SessionID: "realtime-cu-coordinate-pair",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if closeErr := runtime.Close(ctx, errors.New("test complete")); closeErr != nil {
			t.Errorf("close coordinate-pair runtime: %v", closeErr)
		}
	})
	var click legacyaction.ToolSpec
	for _, spec := range testRealtimeCUToolSpecs(t, target) {
		if spec.Name == computeruse.ClickNormalized {
			click = spec
			break
		}
	}
	if click.Name == "" || len(click.ArgumentNormalizers) != 0 {
		t.Fatalf("provider-visible normalized-click declaration = %+v", click)
	}
	if err := runtime.Update(context.Background(), legacy.Settings{
		Instruction: "click the incident field", Tools: []legacyaction.ToolSpec{click},
		Observers: []string{observer.name},
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: 90,
		Image: []byte{1}, MIMEType: "image/jpeg", Width: 1280, Height: 720,
	}); err != nil {
		t.Fatal(err)
	}
	receiveRealtimeCUObservation(t, sink.observations, realtimecu.SourceScreen, 90)
	if err := runtime.Audio(context.Background(), perception.Frame{
		Kind: perception.FrameAudio, Source: realtimecu.SourceMicrophone, CapturedNS: 100,
		PCM16LE: []byte{1, 0}, SampleRateHz: 24_000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: 110,
		Image: []byte{2}, MIMEType: "image/jpeg", Width: 1280, Height: 720,
	}); err != nil {
		t.Fatal(err)
	}
	receiveRealtimeCUObservation(t, sink.observations, realtimecu.SourceScreen, 110)
	event := receiveRealtimeCU(t, sink.calls, "normalized coordinate call")
	if len(event.Calls) != 1 {
		t.Fatalf("normalized coordinate event = %+v", event)
	}
	call := event.Calls[0]
	if call.Name != computeruse.ClickNormalized ||
		string(call.Arguments) != `{"source":"screen","x":255,"y":566}` {
		t.Fatalf("effective coordinate call = %+v", call)
	}

	var proposal, committed *trajectory.Item
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := runtime.Trajectory()
		for index := range snapshot.Items {
			item := &snapshot.Items[index]
			if item.ToolCall == nil || item.ToolCall.CallID != call.CallID {
				continue
			}
			switch item.Kind {
			case trajectory.KindToolProposal:
				proposal = item
			case trajectory.KindToolCall:
				committed = item
			}
		}
		if proposal != nil && committed != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if proposal == nil || string(proposal.ToolCall.Arguments) != `{"source":"screen","x":[255,566]}` {
		t.Fatalf("canonical original proposal = %+v", proposal)
	}
	if committed == nil || committed.ToolCall == nil ||
		string(committed.ToolCall.Arguments) != `{"source":"screen","x":255,"y":566}` ||
		committed.ToolCallDerivation == nil ||
		len(committed.ToolCallDerivation.Rewrites) != 1 ||
		committed.ToolCallDerivation.Rewrites[0] != (trajectory.ToolCallArgumentRewrite{
			Argument: "x", Normalizer: action.ToolParameterCoordinatePairXYV1,
		}) {
		t.Fatalf("committed coordinate derivation = %+v", committed)
	}
	if committed.ToolCallDerivation.SourceArgumentsDigest !=
		trajectory.ToolCallArgumentsDigest(proposal.ToolCall.Arguments) ||
		committed.ToolCallDerivation.EffectiveArgumentsDigest !=
			trajectory.ToolCallArgumentsDigest(committed.ToolCall.Arguments) {
		t.Fatalf("committed coordinate derivation digests = %+v", committed.ToolCallDerivation)
	}
	if err := runtime.ToolResult(context.Background(), trajectory.ToolResult{
		CallID: call.CallID, Name: call.Name, Output: json.RawMessage(`{"clicked":true}`),
	}); err != nil {
		t.Fatal(err)
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
		RuntimeArtifact:  testRealtimeCUArtifact("resource-runtime", "1"),
		SettlementPolicy: testRealtimeCUPolicyPlugin(),
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
		Image: []byte("pre-intent-keyframe"), MIMEType: "image/jpeg", Width: 64, Height: 48,
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
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: 102,
		Image: exactFrame, MIMEType: "image/jpeg", Width: 64, Height: 48,
	}); err != nil {
		t.Fatal(err)
	}
	receiveRealtimeCUObservation(t, sink.observations, realtimecu.SourceScreen, 102)
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

func testRealtimeCUPolicyPlugin() realtimecu.PolicyPlugin {
	descriptor := testRealtimeCUPolicyDescriptor()
	return realtimecu.PolicyPlugin{
		Reference:  realtimecu.SettlementPolicyReference,
		Artifact:   testRealtimeCUArtifact("settlement-policy", "4"),
		Descriptor: descriptor,
		Factory: func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) {
			return testRealtimeCUDispositionDecider{descriptor: descriptor}, nil
		},
	}
}

func testRealtimeCUPolicyDescriptor() policyelements.SemanticDeciderDescriptor {
	return policyelements.SemanticDeciderDescriptor{
		Provider: "test", Model: "realtime-cu-settlement", Protocol: "enum-v1",
		Revision: "v1", ConfigurationDigest: "sha256:" + strings.Repeat("9", 64),
		Vision: true, DecisionTimeoutMS: 1_000,
	}
}

type testRealtimeCUDispositionDecider struct {
	descriptor policyelements.SemanticDeciderDescriptor
}

func (testRealtimeCUDispositionDecider) Name() string { return "test-realtime-cu-settlement" }

func (decider testRealtimeCUDispositionDecider) Descriptor() policyelements.SemanticDeciderDescriptor {
	return decider.descriptor
}

func (testRealtimeCUDispositionDecider) Decide(
	ctx context.Context, request coreinteraction.Decision,
) (coreinteraction.Outcome, error) {
	if err := context.Cause(ctx); err != nil {
		return coreinteraction.Outcome{}, err
	}
	wanted := string(policyelements.IntentDispositionContinue)
	index := slices.Index(request.Options, wanted)
	if index < 0 {
		return coreinteraction.Outcome{}, fmt.Errorf("settlement decision does not offer %q", wanted)
	}
	return coreinteraction.Outcome{Index: index, Option: wanted}, nil
}

type scriptedRealtimeCUDispositionDecider struct {
	descriptor policyelements.SemanticDeciderDescriptor
	mu         sync.Mutex
	choices    []policyelements.IntentDispositionKind
	decisions  chan policyelements.IntentDispositionKind
}

func (*scriptedRealtimeCUDispositionDecider) Name() string {
	return "scripted-realtime-cu-settlement"
}

func (decider *scriptedRealtimeCUDispositionDecider) Descriptor() policyelements.SemanticDeciderDescriptor {
	return decider.descriptor
}

func (decider *scriptedRealtimeCUDispositionDecider) Decide(
	ctx context.Context, request coreinteraction.Decision,
) (coreinteraction.Outcome, error) {
	if err := context.Cause(ctx); err != nil {
		return coreinteraction.Outcome{}, err
	}
	decider.mu.Lock()
	if len(decider.choices) == 0 {
		decider.mu.Unlock()
		return coreinteraction.Outcome{}, errors.New("scripted settlement has no remaining disposition")
	}
	choice := decider.choices[0]
	decider.choices = decider.choices[1:]
	decider.mu.Unlock()
	index := slices.Index(request.Options, string(choice))
	if index < 0 {
		return coreinteraction.Outcome{}, fmt.Errorf("settlement decision does not offer %q", choice)
	}
	select {
	case decider.decisions <- choice:
	case <-ctx.Done():
		return coreinteraction.Outcome{}, context.Cause(ctx)
	}
	return coreinteraction.Outcome{Index: index, Option: string(choice)}, nil
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
		!slices.ContainsFunc(request.Trajectory.Items, func(item trajectory.Item) bool {
			return item.Kind == trajectory.KindObservation &&
				trajectory.AuthorityOf(item) == trajectory.AuthorityUser
		}) {
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

type focusTypeSubmitRealtimeCUModel struct {
	descriptor  continuation.Descriptor
	invocations chan int32
	count       atomic.Int32
}

func (model *focusTypeSubmitRealtimeCUModel) Descriptor() continuation.Descriptor {
	return model.descriptor
}

func (model *focusTypeSubmitRealtimeCUModel) Continue(
	_ context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	invocation := model.count.Add(1)
	model.invocations <- invocation
	results := 0
	for _, item := range request.Trajectory.Items {
		if item.Kind == trajectory.KindToolResult {
			results++
		}
	}
	var name string
	var arguments json.RawMessage
	switch results {
	case 0:
		name = computeruse.Click
		arguments = json.RawMessage(`{"source":"screen","x":10,"y":20}`)
	case 1:
		name = computeruse.Type
		arguments = json.RawMessage(`{"source":"screen","text":"alpha-9"}`)
	case 2:
		name = computeruse.Key
		arguments = json.RawMessage(`{"source":"screen","keys":["ENTER"]}`)
	default:
		return continuation.Completion{StopReason: "stop"}, nil
	}
	call := trajectory.ToolCall{
		CallID: request.InvocationID + fmt.Sprintf(":step-%d", results+1),
		Name:   name, Arguments: arguments,
	}
	if err := emit(continuation.Event{Kind: continuation.EventToolCall, ToolCall: &call}); err != nil {
		return continuation.Completion{}, err
	}
	return continuation.Completion{StopReason: "tool_call"}, nil
}

type coordinatePairRealtimeCUModel struct{ descriptor continuation.Descriptor }

func (model *coordinatePairRealtimeCUModel) Descriptor() continuation.Descriptor {
	return model.descriptor
}

func (*coordinatePairRealtimeCUModel) Continue(
	_ context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	call := trajectory.ToolCall{
		CallID: request.InvocationID + ":click", Name: computeruse.ClickNormalized,
		Arguments: json.RawMessage(`{"source":"screen","x":[255,566]}`),
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

type placeholderThenClickRealtimeCUModel struct {
	descriptor  continuation.Descriptor
	count       atomic.Int32
	invocations chan visualReactivationInvocation
}

func (model *placeholderThenClickRealtimeCUModel) Descriptor() continuation.Descriptor {
	return model.descriptor
}

func (model *placeholderThenClickRealtimeCUModel) Continue(
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
	call := trajectory.ToolCall{CallID: request.InvocationID + ":proposal"}
	switch number {
	case 1:
		if !slices.ContainsFunc(request.Invocation.Tools, func(tool continuation.ToolDefinition) bool {
			return tool.Name == computeruse.Wait
		}) {
			return continuation.Completion{}, errors.New("test provider did not receive declared wait tool")
		}
		call.Name = computeruse.Wait
		call.Arguments = json.RawMessage(`{"duration_ms":1000}`)
	case 2:
		call.Name = computeruse.Click
		call.Arguments = json.RawMessage(`{"source":"screen","x":10,"y":20}`)
	default:
		return continuation.Completion{StopReason: "stop"}, nil
	}
	if err := emit(continuation.Event{Kind: continuation.EventToolCall, ToolCall: &call}); err != nil {
		return continuation.Completion{}, err
	}
	return continuation.Completion{StopReason: "tool_call"}, nil
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
	if number != 2 && number != 3 && number != 4 {
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
	debug        chan legacy.DebugEvent
}

func newTestRealtimeCUSink() *testRealtimeCUSink {
	return &testRealtimeCUSink{
		calls: make(chan legacy.ToolCallEvent, 8), observations: make(chan perception.Observation, 16),
		// Graph debug outputs are lossless. Retain enough events for multi-turn
		// tests that inspect only selected outcomes after the action lifecycle.
		transcripts: make(chan legacy.TranscriptEvent, 8), debug: make(chan legacy.DebugEvent, 1024),
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
func (sink *testRealtimeCUSink) Debug(ctx context.Context, event legacy.DebugEvent) error {
	select {
	case sink.debug <- event:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

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

func receiveRealtimeCUObservation(
	t *testing.T, source <-chan perception.Observation, wantedSource string, occurredNS uint64,
) perception.Observation {
	t.Helper()
	for {
		observation := receiveRealtimeCU(t, source, wantedSource+" observation")
		if observation.Source != wantedSource || observation.OccurredNS != occurredNS {
			continue
		}
		return observation
	}
}

func waitForRealtimeCUToolPolicyDisposition(
	t *testing.T, runtime legacy.Runtime, toolName string,
) (trajectory.Snapshot, trajectory.Item, trajectory.Item) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := runtime.Trajectory()
		var proposal, disposition trajectory.Item
		for _, item := range snapshot.Items {
			if item.Kind == trajectory.KindToolProposal && item.ToolCall != nil &&
				item.ToolCall.Name == toolName {
				proposal = item
			}
			if item.Kind == trajectory.KindToolProposalDisposition &&
				item.ToolProposalDisposition != nil &&
				item.ToolProposalDisposition.Name == toolName {
				disposition = item
			}
		}
		if proposal.ID != "" && disposition.ID != "" {
			return snapshot, proposal, disposition
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for canonical %s proposal disposition; trajectory=%+v",
		toolName, runtime.Trajectory())
	return trajectory.Snapshot{}, trajectory.Item{}, trajectory.Item{}
}
