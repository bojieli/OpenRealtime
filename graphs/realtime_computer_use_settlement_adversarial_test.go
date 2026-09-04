package graphs_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	"github.com/bojieli/OpenRealtime/graph/binding/realtimecu"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/graphs"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// This regression uses the exact locked production topology. A failed client
// effect must remain canonical and visible, but it is not successful
// settlement evidence: the disposition policy must not run until a later
// successful result-linked consequence exists. The durable intent may then
// recover through a different effect and settle normally.
func TestRealtimeComputerUseFailedEffectRemainsReactiveUntilSuccessfulSettlement(t *testing.T) {
	target := computeruse.Target{
		Name: "benchmark-browser", Sources: []string{realtimecu.SourceScreen}, Width: 320, Height: 240,
	}
	modelDescriptor := testRealtimeCUDescriptor()
	model := &failedThenRecoveredRealtimeCUModel{
		descriptor: modelDescriptor, invocations: make(chan int32, 4),
	}
	policyDescriptor := testRealtimeCUPolicyDescriptor()
	policy := &scriptedRealtimeCUDispositionDecider{
		descriptor: policyDescriptor,
		choices:    []policyelements.IntentDispositionKind{policyelements.IntentDispositionSucceeded},
		decisions:  make(chan policyelements.IntentDispositionKind, 1),
	}
	observer := newTestRealtimeCUObserver("failed-effect-observer")
	config, err := graphs.RealtimeComputerUseLaunchConfig(realtimecu.PluginConfig{
		RuntimeArtifact: testRealtimeCUArtifact("failed-effect-runtime", "1"),
		SettlementPolicy: realtimecu.PolicyPlugin{
			Reference: realtimecu.SettlementPolicyReference,
			Artifact:  testRealtimeCUArtifact("failed-effect-policy", "4"), Descriptor: policyDescriptor,
			Factory: func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) {
				return policy, nil
			},
		},
		Model: realtimecu.ModelPlugin{
			Reference: "go://test/realtime-cu/failed-effect-model/v1",
			Artifact:  testRealtimeCUArtifact("failed-effect-model", "2"), Descriptor: modelDescriptor,
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
				return model, nil
			},
		},
		Observer: realtimecu.ObserverPlugin{
			Reference: "go://test/realtime-cu/failed-effect-observer/v1", Name: observer.name,
			Artifact: testRealtimeCUArtifact("failed-effect-observer", "3"),
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
		Instruction: "click the visible control and recover if the first click fails",
		Tools:       testRealtimeCUToolSpecs(t, target), Observers: []string{observer.name},
	}); err != nil {
		t.Fatal(err)
	}

	sendRealtimeCUIntentWithFreshScreen(t, runtime, sink, 90, 100, 110)
	if invocation := receiveRealtimeCU(t, model.invocations, "failed-effect first model invocation"); invocation != 1 {
		t.Fatalf("first failed-effect invocation = %d, want 1", invocation)
	}
	failedCall := receiveRealtimeCU(t, sink.calls, "effect that will fail")
	if len(failedCall.Calls) != 1 || failedCall.Calls[0].Name != computeruse.Click {
		t.Fatalf("failed-effect first call = %+v", failedCall)
	}
	if err := runtime.ToolResult(context.Background(), trajectory.ToolResult{
		CallID: failedCall.Calls[0].CallID, Name: failedCall.Calls[0].Name,
		Error: "target moved before the click crossed",
	}); err != nil {
		t.Fatal(err)
	}
	failedConsequence := receiveRealtimeCU(t, observer.consequences, "failed visual consequence request")
	if failedConsequence.CallID != failedCall.Calls[0].CallID || failedConsequence.CanonicalResultItemID == "" {
		t.Fatalf("failed visual consequence = %+v", failedConsequence)
	}
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: 200,
		Image: []byte{4}, MIMEType: "image/jpeg", Width: 320, Height: 240,
	}); err != nil {
		t.Fatal(err)
	}
	receiveRealtimeCUObservation(t, sink.observations, realtimecu.SourceScreen, 200)
	select {
	case invocation := <-model.invocations:
		if invocation != 2 {
			t.Fatalf("recovery invocation = %d, want 2", invocation)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for failed-effect recovery; trajectory=%+v", runtime.Trajectory())
	}
	select {
	case decision := <-policy.decisions:
		t.Fatalf("failed result was offered to settlement policy as %q", decision)
	default:
	}

	recovery := receiveRealtimeCU(t, sink.calls, "recovery effect")
	if len(recovery.Calls) != 1 || recovery.Calls[0].Name != computeruse.Click ||
		string(recovery.Calls[0].Arguments) != `{"source":"screen","x":30,"y":40}` {
		t.Fatalf("recovery call = %+v", recovery)
	}
	if err := runtime.ToolResult(context.Background(), trajectory.ToolResult{
		CallID: recovery.Calls[0].CallID, Name: recovery.Calls[0].Name,
		Output: json.RawMessage(`{"clicked":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	successConsequence := receiveRealtimeCU(t, observer.consequences, "successful visual consequence request")
	if successConsequence.CallID != recovery.Calls[0].CallID ||
		successConsequence.CanonicalResultItemID == "" ||
		successConsequence.StoreVersion <= failedConsequence.StoreVersion {
		t.Fatalf("successful visual consequence = %+v after %+v", successConsequence, failedConsequence)
	}
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: 210,
		Image: []byte{5}, MIMEType: "image/jpeg", Width: 320, Height: 240,
	}); err != nil {
		t.Fatal(err)
	}
	receiveRealtimeCUObservation(t, sink.observations, realtimecu.SourceScreen, 210)
	if decision := receiveRealtimeCU(t, policy.decisions, "successful terminal disposition"); decision != policyelements.IntentDispositionSucceeded {
		t.Fatalf("successful disposition = %q, want succeeded", decision)
	}

	for index := 0; index < 3; index++ {
		captured := uint64(220 + index)
		if err := runtime.Video(context.Background(), perception.Frame{
			Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: captured,
			Image: []byte{byte(6 + index)}, MIMEType: "image/jpeg", Width: 320, Height: 240,
		}); err != nil {
			t.Fatal(err)
		}
		receiveRealtimeCUObservation(t, sink.observations, realtimecu.SourceScreen, captured)
	}
	select {
	case invocation := <-model.invocations:
		t.Fatalf("settled recovery intent reactivated after success: %d", invocation)
	case <-time.After(200 * time.Millisecond):
	}
	select {
	case call := <-sink.calls:
		t.Fatalf("settled recovery emitted another client effect: %+v", call)
	default:
	}

	assertRealtimeCUFailedAndSuccessfulResultLineage(
		t, runtime.Trajectory(), failedCall.Calls[0].CallID, recovery.Calls[0].CallID,
		failedConsequence.CanonicalResultItemID, successConsequence.CanonicalResultItemID,
	)
}

func sendRealtimeCUIntentWithFreshScreen(
	t *testing.T, runtime legacy.Runtime, sink *testRealtimeCUSink,
	before, intent, after uint64,
) {
	t.Helper()
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: before,
		Image: []byte{1}, MIMEType: "image/jpeg", Width: 320, Height: 240,
	}); err != nil {
		t.Fatal(err)
	}
	receiveRealtimeCUObservation(t, sink.observations, realtimecu.SourceScreen, before)
	if err := runtime.Audio(context.Background(), perception.Frame{
		Kind: perception.FrameAudio, Source: realtimecu.SourceMicrophone, CapturedNS: intent,
		PCM16LE: []byte{1, 0}, SampleRateHz: 24_000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: after,
		Image: []byte{2}, MIMEType: "image/jpeg", Width: 320, Height: 240,
	}); err != nil {
		t.Fatal(err)
	}
	receiveRealtimeCUObservation(t, sink.observations, realtimecu.SourceScreen, after)
}

func assertRealtimeCUFailedAndSuccessfulResultLineage(
	t *testing.T, snapshot trajectory.Snapshot, failedCallID, successfulCallID,
	failedResultID, successfulResultID string,
) {
	t.Helper()
	var failed, successful *trajectory.Item
	var failedObservation, successfulObservation bool
	for index := range snapshot.Items {
		item := &snapshot.Items[index]
		if item.Kind == trajectory.KindToolResult && item.ToolResult != nil {
			switch item.ToolResult.CallID {
			case failedCallID:
				failed = item
			case successfulCallID:
				successful = item
			}
		}
		if item.Kind == trajectory.KindObservation {
			failedObservation = failedObservation || slices.Contains(item.CausalParentIDs, failedResultID)
			successfulObservation = successfulObservation || slices.Contains(item.CausalParentIDs, successfulResultID)
		}
	}
	if failed == nil || failed.ID != failedResultID ||
		failed.ToolResult.Error != "target moved before the click crossed" || len(failed.ToolResult.Output) != 0 {
		t.Fatalf("canonical failed result = %+v, wanted item %q", failed, failedResultID)
	}
	if successful == nil || successful.ID != successfulResultID ||
		successful.ToolResult.Error != "" || string(successful.ToolResult.Output) != `{"clicked":true}` {
		t.Fatalf("canonical successful result = %+v, wanted item %q", successful, successfulResultID)
	}
	if !failedObservation || !successfulObservation {
		t.Fatalf("result-linked observations failed=%t successful=%t snapshot=%+v",
			failedObservation, successfulObservation, snapshot)
	}
}

type failedThenRecoveredRealtimeCUModel struct {
	descriptor  continuation.Descriptor
	invocations chan int32
	count       atomic.Int32
}

func (model *failedThenRecoveredRealtimeCUModel) Descriptor() continuation.Descriptor {
	return model.descriptor
}

func (model *failedThenRecoveredRealtimeCUModel) Continue(
	_ context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	number := model.count.Add(1)
	model.invocations <- number
	if number > 2 {
		return continuation.Completion{StopReason: "stop"}, nil
	}
	results := 0
	for _, item := range request.Trajectory.Items {
		if item.Kind == trajectory.KindToolResult {
			results++
		}
	}
	if int(number)-1 != results {
		return continuation.Completion{}, fmt.Errorf(
			"failed-effect invocation %d sees %d canonical results", number, results,
		)
	}
	arguments := json.RawMessage(`{"source":"screen","x":10,"y":20}`)
	if number == 2 {
		arguments = json.RawMessage(`{"source":"screen","x":30,"y":40}`)
	}
	call := trajectory.ToolCall{
		CallID: request.InvocationID + fmt.Sprintf(":attempt-%d", number),
		Name:   computeruse.Click, Arguments: arguments,
	}
	if err := emit(continuation.Event{Kind: continuation.EventToolCall, ToolCall: &call}); err != nil {
		return continuation.Completion{}, err
	}
	return continuation.Completion{StopReason: "tool_call"}, nil
}
