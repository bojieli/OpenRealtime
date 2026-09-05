package graphs_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
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

// This regression mounts the exact locked production graph and wraps the
// cancellation coordinator at its existing ports. It holds activation_cancel
// while settlement cancellation and consequence processing remain live,
// making the otherwise timing-dependent cross-port race deterministic:
// settlement.cleanup must reach activation.effect_cleanup before
// activation.cancel is released. The same wrapper observes the settlement
// outcome at the coordinator and uses a same-lane processing barrier to prove
// that a valid cleanup inspection is not refused there. A newer intent and
// fresh visual admission then overtake the held cancel; releasing it must
// activate that retained work without another frame.
func TestRealtimeComputerUseSettlementCleanupOvertakesActivationCancellation(t *testing.T) {
	for _, testCase := range []struct {
		name                  string
		failed                bool
		cleanupCode           string
		settlementCleanupCode string
	}{
		{
			name: "failed canonical result", failed: true,
			cleanupCode: "canceled_effect_failed", settlementCleanupCode: "canceled_failed_effect_cleanup",
		},
		{
			name:        "successful canonical result",
			cleanupCode: "canceled_effect_succeeded", settlementCleanupCode: "canceled_successful_effect_cleanup",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			gate := newRealtimeCUActivationCancelGate()
			fixture := newFailedEffectCancellationFixtureWithGate(t, 0, gate)
			t.Cleanup(gate.releaseCancellation)

			sendRealtimeCUIntentWithFreshScreen(t, fixture.runtime, fixture.sink, 90, 100, 110)
			if invocation := receiveRealtimeCU(t, fixture.model.invocations, "raced cancellation model invocation"); invocation != 1 {
				t.Fatalf("raced cancellation invocation = %d, want 1", invocation)
			}
			call := receiveRealtimeCU(t, fixture.sink.calls, "effect before raced cancellation")
			if len(call.Calls) != 1 || call.InvocationID == "" {
				t.Fatalf("effect before raced cancellation = %+v", call)
			}
			var consequence realtimecu.VisualConsequence
			if testCase.failed {
				consequence = failRealtimeCUEffect(t, fixture.runtime, fixture.observer, call)
			} else {
				if err := fixture.runtime.ToolResult(context.Background(), trajectory.ToolResult{
					CallID: call.Calls[0].CallID, Name: call.Calls[0].Name,
					Output: json.RawMessage(`{"clicked":true}`),
				}); err != nil {
					t.Fatal(err)
				}
				consequence = receiveRealtimeCU(
					t, fixture.observer.consequences,
					"successful consequence request before raced cancellation",
				)
			}
			assertRealtimeCUCanonicalCancellationResult(
				t, fixture.runtime.Trajectory(), call, consequence, testCase.failed,
			)

			cancelCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			cancelDone := make(chan error, 1)
			go func() {
				cancelDone <- fixture.runtime.Cancel(cancelCtx, "hold activation cancellation behind cleanup")
			}()
			defer func() {
				gate.releaseCancellation()
				cancel()
			}()

			held := receiveRealtimeCU(t, gate.held, "held production activation cancellation")
			heldCancel, ok := held.Payload.(policyelements.GenerationCancel)
			if !ok || heldCancel.DurableIntent == nil || heldCancel.GenerationID != "" ||
				held.CancellationScope != heldCancel.DurableIntent.TrajectoryItemID {
				t.Fatalf("held activation cancellation = envelope=%+v payload=%+v", held, heldCancel)
			}
			select {
			case err := <-cancelDone:
				t.Fatalf("session cancellation returned before activation delivery was released: %v", err)
			default:
			}

			// The coordinator has already received the settlement gate and producer
			// acknowledgements before it can attempt activation_cancel. The next
			// result-linked frame therefore makes settlement emit cleanup while the
			// activation cancellation remains physically held at its output port.
			sendRealtimeCUConsequenceFrame(t, fixture.runtime, fixture.sink, 200)
			waiting := receiveRealtimeCUActivationOutcome(
				t, fixture.sink, "effect_cleanup_waiting_for_cancel",
			)
			if waiting.Attributes["outcome_kind"] != string(policyelements.GenerationIgnored) {
				t.Fatalf("cleanup-first activation outcome = %+v", waiting)
			}
			cleanupOutcome := receiveRealtimeCU(
				t, gate.settlementCleanup, "settlement cleanup outcome at cancellation coordinator",
			)
			assertRealtimeCUSettlementCleanupOutcome(
				t, cleanupOutcome, testCase.settlementCleanupCode,
			)
			sendRealtimeCUIntentWithFreshScreen(t, fixture.runtime, fixture.sink, 300, 310, 320)
			deferred := receiveRealtimeCUActivationOutcome(t, fixture.sink, "effect_pending")
			if deferred.Attributes["outcome_kind"] != string(policyelements.GenerationIgnored) {
				t.Fatalf("newer admission while old cancellation is held = %+v", deferred)
			}
			assertRealtimeCUCancellationSilence(t, fixture, 150*time.Millisecond)

			gate.releaseCancellation()
			assertRealtimeCUSettlementCleanupDidNotProduceCancellationRefusal(
				t, gate, cleanupOutcome.ItemID,
			)
			canceled := receiveRealtimeCUActivationOutcome(t, fixture.sink, "intent_revoked")
			if canceled.Attributes["outcome_kind"] != string(policyelements.GenerationCanceled) ||
				canceled.Attributes["generation_id"] != call.InvocationID {
				t.Fatalf("activation cancellation acknowledgement = %+v, want generation %q", canceled, call.InvocationID)
			}
			retired := receiveRealtimeCUActivationOutcome(t, fixture.sink, testCase.cleanupCode)
			if retired.Attributes["outcome_kind"] != string(policyelements.GenerationIgnored) ||
				retired.Attributes["generation_id"] != call.InvocationID {
				t.Fatalf("exact canceled-effect retirement = %+v, want generation %q", retired, call.InvocationID)
			}
			select {
			case err := <-cancelDone:
				if err != nil {
					t.Fatalf("complete raced session cancellation: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for raced session cancellation completion")
			}
			cancel()

			if invocation := receiveRealtimeCU(t, fixture.model.invocations, "deferred fresh intent after cleanup-first cancellation"); invocation != 2 {
				t.Fatalf("fresh-intent invocation = %d, want 2", invocation)
			}
			freshCall := receiveRealtimeCU(t, fixture.sink.calls, "deferred fresh effect after cleanup-first cancellation")
			completeRealtimeCUEffectSuccessfully(t, fixture, freshCall, 330)
			freshSettled := receiveRealtimeCUActivationOutcome(t, fixture.sink, "intent_succeeded")
			if freshSettled.Attributes["generation_id"] != freshCall.InvocationID {
				t.Fatalf("fresh intent settlement = %+v, want generation %q", freshSettled, freshCall.InvocationID)
			}

			for index := 0; index < 3; index++ {
				sendRealtimeCUConsequenceFrame(t, fixture.runtime, fixture.sink, uint64(340+index))
			}
			assertRealtimeCUCancellationSilence(t, fixture, 200*time.Millisecond)
		})
	}
}

func TestRealtimeComputerUseCanceledEffectConsequenceOrdering(t *testing.T) {
	t.Run("cancel before failed consequence", func(t *testing.T) {
		fixture := newFailedEffectCancellationFixture(t, 0)
		sendRealtimeCUIntentWithFreshScreen(t, fixture.runtime, fixture.sink, 90, 100, 110)
		if invocation := receiveRealtimeCU(t, fixture.model.invocations, "first cancellation model invocation"); invocation != 1 {
			t.Fatalf("first cancellation invocation = %d, want 1", invocation)
		}
		failedCall := receiveRealtimeCU(t, fixture.sink.calls, "failed call before cancellation")
		failedConsequence := failRealtimeCUEffect(t, fixture.runtime, fixture.observer, failedCall)

		cancelCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := fixture.runtime.Cancel(cancelCtx, "cancel before failed visual consequence"); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		sendRealtimeCUConsequenceFrame(t, fixture.runtime, fixture.sink, 200)
		debug := receiveRealtimeCUActivationOutcome(t, fixture.sink, "canceled_effect_failed")
		if debug.Attributes["generation_id"] != failedCall.InvocationID ||
			debug.Attributes["outcome_kind"] != string(policyelements.GenerationIgnored) {
			t.Fatalf("failed-effect cleanup debug = %+v", debug)
		}
		select {
		case invocation := <-fixture.model.invocations:
			t.Fatalf("canceled failed consequence reactivated old intent: %d", invocation)
		default:
		}
		select {
		case decision := <-fixture.policy.decisions:
			t.Fatalf("failed result reached disposition policy as %q", decision)
		default:
		}

		sendRealtimeCUIntentWithFreshScreen(t, fixture.runtime, fixture.sink, 300, 310, 320)
		if invocation := receiveRealtimeCU(t, fixture.model.invocations, "fresh intent after canceled failure"); invocation != 2 {
			t.Fatalf("fresh-intent invocation = %d, want 2", invocation)
		}
		freshCall := receiveRealtimeCU(t, fixture.sink.calls, "fresh intent call")
		completeRealtimeCUEffectSuccessfully(t, fixture, freshCall, 330)
		if failedConsequence.CanonicalResultItemID == "" {
			t.Fatal("failed effect did not remain canonical across cancellation")
		}
	})

	t.Run("cancel after successful result before consequence", func(t *testing.T) {
		fixture := newFailedEffectCancellationFixture(t, 0)
		sendRealtimeCUIntentWithFreshScreen(t, fixture.runtime, fixture.sink, 90, 100, 110)
		if invocation := receiveRealtimeCU(t, fixture.model.invocations, "successful cancellation model invocation"); invocation != 1 {
			t.Fatalf("successful cancellation invocation = %d, want 1", invocation)
		}
		successfulCall := receiveRealtimeCU(t, fixture.sink.calls, "successful call before cancellation")
		if len(successfulCall.Calls) != 1 {
			t.Fatalf("successful call before cancellation = %+v", successfulCall)
		}
		if err := fixture.runtime.ToolResult(context.Background(), trajectory.ToolResult{
			CallID: successfulCall.Calls[0].CallID, Name: successfulCall.Calls[0].Name,
			Output: json.RawMessage(`{"clicked":true}`),
		}); err != nil {
			t.Fatal(err)
		}
		successfulConsequence := receiveRealtimeCU(
			t, fixture.observer.consequences, "successful visual consequence request before cancellation",
		)

		cancelCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := fixture.runtime.Cancel(cancelCtx, "cancel before successful visual consequence"); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		sendRealtimeCUConsequenceFrame(t, fixture.runtime, fixture.sink, 200)
		debug := receiveRealtimeCUActivationOutcome(t, fixture.sink, "canceled_effect_succeeded")
		if debug.Attributes["generation_id"] != successfulCall.InvocationID ||
			debug.Attributes["outcome_kind"] != string(policyelements.GenerationIgnored) {
			t.Fatalf("successful-effect cleanup debug = %+v", debug)
		}
		select {
		case invocation := <-fixture.model.invocations:
			t.Fatalf("canceled successful consequence reactivated old intent: %d", invocation)
		default:
		}
		select {
		case decision := <-fixture.policy.decisions:
			t.Fatalf("canceled successful result reached disposition policy as %q", decision)
		default:
		}

		sendRealtimeCUIntentWithFreshScreen(t, fixture.runtime, fixture.sink, 300, 310, 320)
		if invocation := receiveRealtimeCU(t, fixture.model.invocations, "fresh intent after canceled success"); invocation != 2 {
			t.Fatalf("fresh-intent invocation = %d, want 2", invocation)
		}
		freshCall := receiveRealtimeCU(t, fixture.sink.calls, "fresh intent call after canceled success")
		completeRealtimeCUEffectSuccessfully(t, fixture, freshCall, 330)
		if successfulConsequence.CanonicalResultItemID == "" {
			t.Fatal("successful effect did not remain canonical across cancellation")
		}
	})

	t.Run("cancel recovery generation", func(t *testing.T) {
		fixture := newFailedEffectCancellationFixture(t, 2)
		sendRealtimeCUIntentWithFreshScreen(t, fixture.runtime, fixture.sink, 90, 100, 110)
		if invocation := receiveRealtimeCU(t, fixture.model.invocations, "first recovery-cancel invocation"); invocation != 1 {
			t.Fatalf("first recovery-cancel invocation = %d, want 1", invocation)
		}
		failedCall := receiveRealtimeCU(t, fixture.sink.calls, "failed call before recovery cancellation")
		_ = failRealtimeCUEffect(t, fixture.runtime, fixture.observer, failedCall)
		sendRealtimeCUConsequenceFrame(t, fixture.runtime, fixture.sink, 200)
		if invocation := receiveRealtimeCU(t, fixture.model.invocations, "blocked recovery generation"); invocation != 2 {
			t.Fatalf("recovery invocation = %d, want 2", invocation)
		}
		if blocked := receiveRealtimeCU(t, fixture.model.blocked, "recovery provider block"); blocked != 2 {
			t.Fatalf("blocked recovery invocation = %d, want 2", blocked)
		}

		cancelCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := fixture.runtime.Cancel(cancelCtx, "cancel exact recovery generation"); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		for index := 0; index < 3; index++ {
			sendRealtimeCUConsequenceFrame(t, fixture.runtime, fixture.sink, uint64(210+index))
		}
		select {
		case invocation := <-fixture.model.invocations:
			t.Fatalf("cadence revived canceled recovery intent: %d", invocation)
		case <-time.After(100 * time.Millisecond):
		}
		select {
		case decision := <-fixture.policy.decisions:
			t.Fatalf("failed result reached disposition policy as %q", decision)
		default:
		}

		sendRealtimeCUIntentWithFreshScreen(t, fixture.runtime, fixture.sink, 300, 310, 320)
		if invocation := receiveRealtimeCU(t, fixture.model.invocations, "fresh intent after recovery cancellation"); invocation != 3 {
			t.Fatalf("post-recovery-cancel invocation = %d, want 3", invocation)
		}
		freshCall := receiveRealtimeCU(t, fixture.sink.calls, "fresh call after recovery cancellation")
		completeRealtimeCUEffectSuccessfully(t, fixture, freshCall, 330)
	})
}

type failedEffectCancellationFixture struct {
	runtime  legacy.Runtime
	sink     *testRealtimeCUSink
	observer *testRealtimeCUObserver
	model    *failedEffectCancellationModel
	policy   *scriptedRealtimeCUDispositionDecider
}

func newFailedEffectCancellationFixture(t *testing.T, blockAt int32) *failedEffectCancellationFixture {
	return newFailedEffectCancellationFixtureWithGate(t, blockAt, nil)
}

func newFailedEffectCancellationFixtureWithGate(
	t *testing.T, blockAt int32, gate interface {
		install(*testing.T, *graphlaunch.Config)
	},
) *failedEffectCancellationFixture {
	t.Helper()
	target := computeruse.Target{
		Name: "benchmark-browser", Sources: []string{realtimecu.SourceScreen}, Width: 320, Height: 240,
	}
	modelDescriptor := testRealtimeCUDescriptor()
	model := &failedEffectCancellationModel{
		descriptor: modelDescriptor, invocations: make(chan int32, 8),
		blockAt: blockAt, blocked: make(chan int32, 1),
	}
	policyDescriptor := testRealtimeCUPolicyDescriptor()
	policy := &scriptedRealtimeCUDispositionDecider{
		descriptor: policyDescriptor,
		choices:    []policyelements.IntentDispositionKind{policyelements.IntentDispositionSucceeded},
		decisions:  make(chan policyelements.IntentDispositionKind, 2),
	}
	observer := newTestRealtimeCUObserver("failed-effect-cancellation-observer")
	config, err := graphs.RealtimeComputerUseLaunchConfig(realtimecu.PluginConfig{
		RuntimeArtifact: testRealtimeCUArtifact("failed-effect-cancellation-runtime", "1"),
		SettlementPolicy: realtimecu.PolicyPlugin{
			Reference: realtimecu.SettlementPolicyReference,
			Artifact:  testRealtimeCUArtifact("failed-effect-cancellation-policy", "4"), Descriptor: policyDescriptor,
			Factory: func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) {
				return policy, nil
			},
		},
		Model: realtimecu.ModelPlugin{
			Reference: "go://test/realtime-cu/failed-effect-cancellation-model/v1",
			Artifact:  testRealtimeCUArtifact("failed-effect-cancellation-model", "2"), Descriptor: modelDescriptor,
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
				return model, nil
			},
		},
		Observer: realtimecu.ObserverPlugin{
			Reference: "go://test/realtime-cu/failed-effect-cancellation-observer/v1", Name: observer.name,
			Artifact: testRealtimeCUArtifact("failed-effect-cancellation-observer", "3"),
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
	if gate != nil {
		gate.install(t, &config)
	}
	launched, err := graphlaunch.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	sink := newTestRealtimeCUSink()
	runtime, err := launched.Binding.Start(context.Background(), legacy.Options{
		Sink: sink, SessionID: fmt.Sprintf("realtime-cu-failed-effect-cancel-%d", blockAt),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if closeErr := runtime.Close(ctx, errors.New("test complete")); closeErr != nil {
			t.Errorf("close failed-effect cancellation runtime: %v", closeErr)
		}
	})
	if err := runtime.Update(context.Background(), legacy.Settings{
		Instruction: "click the visible control and honor exact cancellation",
		Tools:       testRealtimeCUToolSpecs(t, target), Observers: []string{observer.name},
	}); err != nil {
		t.Fatal(err)
	}
	return &failedEffectCancellationFixture{
		runtime: runtime, sink: sink, observer: observer, model: model, policy: policy,
	}
}

type realtimeCUActivationCancelGate struct {
	held                chan element.Envelope
	release             chan struct{}
	releaseOnce         sync.Once
	settlementCleanup   chan element.Envelope
	coordinatorOutcomes chan element.Envelope
}

const realtimeCUSettlementCleanupBarrierItemID = "test-settlement-cleanup-processing-barrier"

func newRealtimeCUActivationCancelGate() *realtimeCUActivationCancelGate {
	return &realtimeCUActivationCancelGate{
		held: make(chan element.Envelope, 1), release: make(chan struct{}),
		settlementCleanup:   make(chan element.Envelope, 1),
		coordinatorOutcomes: make(chan element.Envelope, 128),
	}
}

func (gate *realtimeCUActivationCancelGate) releaseCancellation() {
	gate.releaseOnce.Do(func() { close(gate.release) })
}

func (gate *realtimeCUActivationCancelGate) install(t *testing.T, config *graphlaunch.Config) {
	t.Helper()
	for index := range config.Catalog.Assembly.Implementations {
		registration := &config.Catalog.Assembly.Implementations[index]
		if registration.Factory.Descriptor().Name != realtimecu.CancellationCoordinatorReference {
			continue
		}
		registration.Factory = realtimeCUActivationCancelGateFactory{
			base: registration.Factory, gate: gate,
		}
		return
	}
	t.Fatalf("production assembly omitted %s", realtimecu.CancellationCoordinatorReference)
}

type realtimeCUActivationCancelGateFactory struct {
	base element.Factory
	gate *realtimeCUActivationCancelGate
}

func (factory realtimeCUActivationCancelGateFactory) Descriptor() element.Descriptor {
	return factory.base.Descriptor()
}

func (factory realtimeCUActivationCancelGateFactory) ValidateConfig(source json.RawMessage) error {
	validator, ok := factory.base.(element.ConfigValidator)
	if !ok {
		return nil
	}
	return validator.ValidateConfig(source)
}

func (factory realtimeCUActivationCancelGateFactory) Mount(
	ctx context.Context, mount element.MountContext,
) (element.Runnable, error) {
	mount.Ports = realtimeCUActivationCancelGatePorts{Ports: mount.Ports, gate: factory.gate}
	return factory.base.Mount(ctx, mount)
}

type realtimeCUActivationCancelGatePorts struct {
	element.Ports
	gate *realtimeCUActivationCancelGate
}

func (ports realtimeCUActivationCancelGatePorts) Input(name string) (element.InputPort, error) {
	input, err := ports.Ports.Input(name)
	if err != nil || name != "settlement_outcome" {
		return input, err
	}
	return &realtimeCUSettlementCleanupBarrierInput{InputPort: input, gate: ports.gate}, nil
}

func (ports realtimeCUActivationCancelGatePorts) Output(name string) (element.OutputPort, error) {
	output, err := ports.Ports.Output(name)
	if err != nil {
		return output, err
	}
	switch name {
	case "activation_cancel":
		return realtimeCUActivationCancelGateOutput{OutputPort: output, gate: ports.gate}, nil
	case "outcome":
		return realtimeCUCancellationOutcomeTap{OutputPort: output, gate: ports.gate}, nil
	default:
		return output, nil
	}
}

// realtimeCUSettlementCleanupBarrierInput inserts one deliberately invalid,
// uniquely identified outcome immediately behind the valid cleanup outcome on
// the coordinator's single settlement-outcome lane. The coordinator consumes
// inputs serially, so observing the sentinel's refusal proves that it finished
// handling the cleanup first without relying on a timeout to prove absence.
type realtimeCUSettlementCleanupBarrierInput struct {
	element.InputPort
	gate     *realtimeCUActivationCancelGate
	barrier  *element.Envelope
	injected bool
}

func (input *realtimeCUSettlementCleanupBarrierInput) Receive(
	ctx context.Context,
) (element.Envelope, error) {
	if input.barrier != nil {
		barrier := input.barrier.Clone()
		input.barrier = nil
		return barrier, nil
	}
	envelope, err := input.InputPort.Receive(ctx)
	if err != nil {
		return element.Envelope{}, err
	}
	outcome, ok := envelope.Payload.(policyelements.IntentSettlementOutcome)
	if !ok || input.injected || outcome.Kind != policyelements.IntentSettlementCleanupForwarded ||
		outcome.Operation != "evidence" {
		return envelope, nil
	}
	input.injected = true
	select {
	case input.gate.settlementCleanup <- envelope.Clone():
	case <-ctx.Done():
		return element.Envelope{}, context.Cause(ctx)
	}
	barrier := envelope.Clone()
	barrier.ItemID = realtimeCUSettlementCleanupBarrierItemID
	barrier.Sequence++
	barrier.CausalParents = []string{envelope.ItemID}
	barrier.Payload = policyelements.IntentSettlementOutcome{
		Kind:      policyelements.IntentSettlementOutcomeKind("test-processing-barrier"),
		Operation: "evidence", SessionID: envelope.SessionID,
		Code: "test_processing_barrier",
	}
	input.barrier = &barrier
	return envelope, nil
}

type realtimeCUActivationCancelGateOutput struct {
	element.OutputPort
	gate *realtimeCUActivationCancelGate
}

func (output realtimeCUActivationCancelGateOutput) Broadcast(
	ctx context.Context, envelope element.Envelope,
) (element.SendResult, error) {
	select {
	case output.gate.held <- envelope.Clone():
	case <-ctx.Done():
		return element.SendResult{}, context.Cause(ctx)
	}
	select {
	case <-output.gate.release:
	case <-ctx.Done():
		return element.SendResult{}, context.Cause(ctx)
	}
	return output.OutputPort.Broadcast(ctx, envelope)
}

type realtimeCUCancellationOutcomeTap struct {
	element.OutputPort
	gate *realtimeCUActivationCancelGate
}

func (output realtimeCUCancellationOutcomeTap) Broadcast(
	ctx context.Context, envelope element.Envelope,
) (element.SendResult, error) {
	result, err := output.OutputPort.Broadcast(ctx, envelope)
	if err != nil {
		return result, err
	}
	select {
	case output.gate.coordinatorOutcomes <- envelope.Clone():
		return result, nil
	case <-ctx.Done():
		return element.SendResult{}, context.Cause(ctx)
	}
}

func assertRealtimeCUSettlementCleanupOutcome(
	t *testing.T, envelope element.Envelope, wantCode string,
) {
	t.Helper()
	outcome, ok := envelope.Payload.(policyelements.IntentSettlementOutcome)
	if !ok || !envelope.Type.Equal(policyelements.IntentSettlementOutcomeType()) ||
		outcome.Kind != policyelements.IntentSettlementCleanupForwarded ||
		outcome.Operation != "evidence" || outcome.Code != wantCode ||
		outcome.SessionID == "" || outcome.DurableIntentItemID == "" ||
		outcome.TriggerObservationItemID == "" ||
		outcome.StateRevisionAfter != outcome.StateRevisionBefore+1 || outcome.FinishedNS == 0 {
		t.Fatalf("settlement cleanup outcome = envelope=%+v payload=%+v, want valid cleanup/evidence/%s",
			envelope, outcome, wantCode)
	}
}

func assertRealtimeCUSettlementCleanupDidNotProduceCancellationRefusal(
	t *testing.T, gate *realtimeCUActivationCancelGate, cleanupItemID string,
) {
	t.Helper()
	for {
		envelope := receiveRealtimeCU(
			t, gate.coordinatorOutcomes, "cancellation coordinator cleanup processing barrier",
		)
		outcome, ok := envelope.Payload.(realtimecu.SessionCancellationOutcome)
		if !ok {
			t.Fatalf("cancellation coordinator outcome payload = %T", envelope.Payload)
		}
		if outcome.RequestItemID == cleanupItemID {
			t.Fatalf("valid settlement cleanup emitted a cancellation outcome: %+v", outcome)
		}
		if outcome.RequestItemID != realtimeCUSettlementCleanupBarrierItemID {
			continue
		}
		if outcome.Kind != realtimecu.SessionCancellationRefused ||
			outcome.Operation != "settlement_outcome" ||
			outcome.Code != "invalid_settlement_outcome" {
			t.Fatalf("settlement cleanup processing barrier = %+v", outcome)
		}
		return
	}
}

func assertRealtimeCUCanonicalCancellationResult(
	t *testing.T, snapshot trajectory.Snapshot, call legacy.ToolCallEvent,
	consequence realtimecu.VisualConsequence, failed bool,
) {
	t.Helper()
	if len(call.Calls) != 1 || consequence.CallID != call.Calls[0].CallID ||
		consequence.CanonicalResultItemID == "" {
		t.Fatalf("canonical cancellation consequence = %+v for call %+v", consequence, call)
	}
	for _, item := range snapshot.Items {
		if item.ID != consequence.CanonicalResultItemID {
			continue
		}
		if item.Kind != trajectory.KindToolResult || item.ToolResult == nil ||
			item.InvocationID != call.InvocationID || item.ToolResult.CallID != call.Calls[0].CallID {
			t.Fatalf("canonical cancellation result = %+v", item)
		}
		if failed {
			if item.ToolResult.Error == "" || len(item.ToolResult.Output) != 0 {
				t.Fatalf("canonical failed cancellation result = %+v", item.ToolResult)
			}
		} else if item.ToolResult.Error != "" || string(item.ToolResult.Output) != `{"clicked":true}` {
			t.Fatalf("canonical successful cancellation result = %+v", item.ToolResult)
		}
		return
	}
	t.Fatalf("trajectory omitted canonical cancellation result %q", consequence.CanonicalResultItemID)
}

func assertRealtimeCUCancellationSilence(
	t *testing.T, fixture *failedEffectCancellationFixture, duration time.Duration,
) {
	t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case invocation := <-fixture.model.invocations:
		t.Fatalf("canceled effect invoked cognition generation %d", invocation)
	case decision := <-fixture.policy.decisions:
		t.Fatalf("canceled effect invoked disposition policy as %q", decision)
	case call := <-fixture.sink.calls:
		t.Fatalf("canceled effect emitted client work: %+v", call)
	case <-timer.C:
	}
}

func failRealtimeCUEffect(
	t *testing.T, runtime legacy.Runtime, observer *testRealtimeCUObserver, call legacy.ToolCallEvent,
) realtimecu.VisualConsequence {
	t.Helper()
	if len(call.Calls) != 1 {
		t.Fatalf("failed-effect call = %+v", call)
	}
	if err := runtime.ToolResult(context.Background(), trajectory.ToolResult{
		CallID: call.Calls[0].CallID, Name: call.Calls[0].Name, Error: "target moved before effect",
	}); err != nil {
		t.Fatal(err)
	}
	return receiveRealtimeCU(t, observer.consequences, "failed visual consequence request")
}

func sendRealtimeCUConsequenceFrame(
	t *testing.T, runtime legacy.Runtime, sink *testRealtimeCUSink, captured uint64,
) {
	t.Helper()
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: realtimecu.SourceScreen, CapturedNS: captured,
		Image: []byte{byte(captured)}, MIMEType: "image/jpeg", Width: 320, Height: 240,
	}); err != nil {
		t.Fatal(err)
	}
	receiveRealtimeCUObservation(t, sink.observations, realtimecu.SourceScreen, captured)
}

func receiveRealtimeCUActivationOutcome(
	t *testing.T, sink *testRealtimeCUSink, code string,
) legacy.DebugEvent {
	t.Helper()
	for {
		debug := receiveRealtimeCU(t, sink.debug, "activation outcome "+code)
		if debug.Name == "activation_outcome" && debug.Attributes["outcome_code"] == code {
			return debug
		}
	}
}

func completeRealtimeCUEffectSuccessfully(
	t *testing.T, fixture *failedEffectCancellationFixture, call legacy.ToolCallEvent, captured uint64,
) {
	t.Helper()
	if len(call.Calls) != 1 {
		t.Fatalf("successful effect call = %+v", call)
	}
	if err := fixture.runtime.ToolResult(context.Background(), trajectory.ToolResult{
		CallID: call.Calls[0].CallID, Name: call.Calls[0].Name,
		Output: json.RawMessage(`{"clicked":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	_ = receiveRealtimeCU(t, fixture.observer.consequences, "successful consequence request")
	sendRealtimeCUConsequenceFrame(t, fixture.runtime, fixture.sink, captured)
	if decision := receiveRealtimeCU(t, fixture.policy.decisions, "successful disposition"); decision != policyelements.IntentDispositionSucceeded {
		t.Fatalf("successful disposition = %q", decision)
	}
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

type failedEffectCancellationModel struct {
	descriptor  continuation.Descriptor
	invocations chan int32
	blocked     chan int32
	blockAt     int32
	count       atomic.Int32
}

func (model *failedEffectCancellationModel) Descriptor() continuation.Descriptor {
	return model.descriptor
}

func (model *failedEffectCancellationModel) Continue(
	ctx context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	number := model.count.Add(1)
	model.invocations <- number
	if number == model.blockAt {
		select {
		case model.blocked <- number:
		case <-ctx.Done():
			return continuation.Completion{}, context.Cause(ctx)
		}
		<-ctx.Done()
		return continuation.Completion{}, context.Cause(ctx)
	}
	call := trajectory.ToolCall{
		CallID: request.InvocationID + fmt.Sprintf(":attempt-%d", number),
		Name:   computeruse.Click,
		Arguments: json.RawMessage(fmt.Sprintf(
			`{"source":"screen","x":%d,"y":%d}`, 10*number, 10*number+10,
		)),
	}
	if err := emit(continuation.Event{Kind: continuation.EventToolCall, ToolCall: &call}); err != nil {
		return continuation.Completion{}, err
	}
	return continuation.Completion{StopReason: "tool_call"}, nil
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
