package graphs_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	"github.com/bojieli/OpenRealtime/graph/binding/realtimecu"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// A completed classification can still be in transit when cancellation wins.
// Deliver it only after a replacement intent has produced its own effect, then
// require that effect to settle from its own result and classification.
func TestRealtimeComputerUseCanceledDispositionCannotSettleReplacement(t *testing.T) {
	for _, kind := range []policyelements.IntentDispositionKind{
		policyelements.IntentDispositionContinue,
		policyelements.IntentDispositionSucceeded,
		policyelements.IntentDispositionFailed,
	} {
		t.Run(string(kind), func(t *testing.T) {
			gate := newRealtimeCUTransactionReplayGate()
			dispositions := gate.add("policy.IntentDispositionProducer", "disposition", false, false)
			delivery := gate.add("policy.IntentSettlement", "disposition", true, false)
			fixture := newFailedEffectCancellationFixtureWithGate(t, 0, gate)
			fixture.policy.mu.Lock()
			fixture.policy.choices = []policyelements.IntentDispositionKind{kind, policyelements.IntentDispositionSucceeded}
			fixture.policy.mu.Unlock()
			sendRealtimeCUIntentWithFreshScreen(t, fixture.runtime, fixture.sink, 90, 100, 110)
			receiveRealtimeCU(t, fixture.model.invocations, "first model")
			first := receiveRealtimeCU(t, fixture.sink.calls, "first effect")
			if err := fixture.runtime.ToolResult(t.Context(), trajectory.ToolResult{
				CallID: first.Calls[0].CallID, Name: first.Calls[0].Name,
				Output: json.RawMessage(`{"clicked":true}`),
			}); err != nil {
				t.Fatal(err)
			}
			receiveRealtimeCU(t, fixture.observer.consequences, "first consequence")
			sendRealtimeCUConsequenceFrame(t, fixture.runtime, fixture.sink, 200)
			if got := receiveRealtimeCU(t, fixture.policy.decisions, "first disposition"); got != kind {
				t.Fatalf("disposition = %s, want %s", got, kind)
			}
			old := receiveRealtimeCU(t, dispositions.captured, "original disposition")
			held := receiveRealtimeCU(t, delivery.captured, "delayed disposition")
			t.Cleanup(held.release)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := fixture.runtime.Cancel(ctx, "cancel before classification delivery"); err != nil {
				t.Fatal(err)
			}
			sendRealtimeCUIntentWithFreshScreen(t, fixture.runtime, fixture.sink, 290, 300, 310)
			if invocation := receiveRealtimeCU(t, fixture.model.invocations, "replacement model"); invocation != 2 {
				t.Fatalf("replacement invocation = %d", invocation)
			}
			next := receiveRealtimeCU(t, fixture.sink.calls, "replacement effect")
			held.release()
			probe := old.envelope.Payload.(policyelements.IntentDisposition).Probe
			refused := gate.recording.await(t, "settlement.outcome", func(e element.Envelope) bool {
				o, ok := e.Payload.(policyelements.IntentSettlementOutcome)
				return ok && o.Operation == "disposition" && o.ProbeID == probe.ProbeID
			}).Payload.(policyelements.IntentSettlementOutcome)
			if refused.Kind != policyelements.IntentSettlementRefused || refused.Code != "unknown_probe" {
				t.Fatalf("canceled disposition was applied: %+v", refused)
			}
			// Replay the exact producer value, without fabricating its identity.
			if _, err := old.output.Broadcast(ctx, old.envelope); err != nil {
				t.Fatal(err)
			}
			replay := receiveRealtimeCU(t, delivery.captured, "duplicate canceled disposition")
			t.Cleanup(replay.release)
			replay.release()
			replayed := gate.recording.await(t, "settlement.outcome", func(e element.Envelope) bool {
				o, ok := e.Payload.(policyelements.IntentSettlementOutcome)
				return ok && o.Operation == "disposition" && o.ProbeID == probe.ProbeID &&
					o.StateRevisionBefore >= refused.StateRevisionAfter
			}).Payload.(policyelements.IntentSettlementOutcome)
			if replayed.Kind != policyelements.IntentSettlementRefused || replayed.Code != "unknown_probe" {
				t.Fatalf("duplicate canceled disposition was applied: %+v", replayed)
			}
			completeRealtimeCUEffectSuccessfully(t, fixture, next, 320)
			current := receiveRealtimeCU(t, delivery.captured, "replacement disposition")
			t.Cleanup(current.release)
			current.release()
			gate.recording.await(t, "settlement.outcome", func(e element.Envelope) bool {
				o, ok := e.Payload.(policyelements.IntentSettlementOutcome)
				return ok && o.Code == "terminal_acknowledged" && o.DurableIntentItemID != probe.DurableIntent.TrajectoryItemID
			})
			if fixture.model.count.Load() != 2 {
				t.Fatal("delayed disposition restarted canceled work or consumed replacement work")
			}
		})
	}
}

// An acknowledgement and the resulting cancellation completion are distinct
// graph messages. Replay both while a second cancellation is awaiting its own
// acknowledgement, and observe the real coordinator after a same-lane barrier.
func TestRealtimeComputerUseOldAcknowledgementsCannotFinishNewCancellation(t *testing.T) {
	gate := newRealtimeCUTransactionReplayGate()
	acks := gate.add(realtimecu.ActivationReference, "settlement_ack", false, true)
	outcomes := gate.add("policy.IntentSettlement", "outcome", false, false)
	fixture := newFailedEffectCancellationFixtureWithGate(t, 0, gate)
	fixture.policy.mu.Lock()
	fixture.policy.choices = []policyelements.IntentDispositionKind{
		policyelements.IntentDispositionSucceeded, policyelements.IntentDispositionSucceeded,
		policyelements.IntentDispositionSucceeded,
	}
	fixture.policy.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := func(before uint64) realtimeCUReplayEnvelope {
		t.Helper()
		sendRealtimeCUIntentWithFreshScreen(t, fixture.runtime, fixture.sink, before, before+10, before+20)
		receiveRealtimeCU(t, fixture.model.invocations, "model invocation")
		call := receiveRealtimeCU(t, fixture.sink.calls, "effect")
		completeRealtimeCUEffectSuccessfully(t, fixture, call, before+110)
		ack := receiveRealtimeCU(t, acks.captured, "held acknowledgement")
		t.Cleanup(ack.release)
		return ack
	}
	cancelPending := func(ack realtimeCUReplayEnvelope) chan error {
		t.Helper()
		intent := ack.envelope.Payload.(policyelements.IntentSettlementAcknowledgement).Decision.Probe.DurableIntent.TrajectoryItemID
		done := make(chan error, 1)
		go func() { done <- fixture.runtime.Cancel(ctx, "cancel while acknowledgement is delayed") }()
		gate.recording.await(t, "cancellation_coordinator.outcome", func(e element.Envelope) bool {
			o, ok := e.Payload.(realtimecu.SessionCancellationOutcome)
			return ok && o.DurableIntentItemID == intent && o.PendingSettlement && !o.PendingProducer
		})
		return done
	}
	old := start(90)
	firstCanceled := cancelPending(old)
	old.release()
	if err := receiveRealtimeCU(t, firstCanceled, "first cancellation"); err != nil {
		t.Fatal(err)
	}
	var oldCompletion realtimeCUReplayEnvelope
	for {
		oldCompletion = receiveRealtimeCU(t, outcomes.captured, "first cancellation settlement outcome")
		o := oldCompletion.envelope.Payload.(policyelements.IntentSettlementOutcome)
		if o.Operation == "ack" && o.Kind == policyelements.IntentSettlementCanceled {
			break
		}
	}
	current := start(290)
	secondCanceled := cancelPending(current)
	for range 3 {
		if _, err := old.output.Broadcast(ctx, old.envelope); err != nil {
			t.Fatal(err)
		}
		if _, err := oldCompletion.output.Broadcast(ctx, oldCompletion.envelope); err != nil {
			t.Fatal(err)
		}
	}
	gate.recording.await(t, "settlement.outcome", func(e element.Envelope) bool {
		o, ok := e.Payload.(policyelements.IntentSettlementOutcome)
		if !ok || o.Code != "duplicate_acknowledgement" {
			return false
		}
		count := 0
		for _, recorded := range gate.recording.records("settlement.outcome") {
			if recorded.Payload.(policyelements.IntentSettlementOutcome).Code == "duplicate_acknowledgement" {
				count++
			}
		}
		return count == 3
	})
	barrier := oldCompletion.envelope.Clone()
	barrier.ItemID = "cross-cancellation-ack-replay-barrier"
	barrier.Payload = policyelements.IntentSettlementOutcome{
		Kind: "test-processing-barrier", Operation: "ack", SessionID: barrier.SessionID,
	}
	if _, err := oldCompletion.output.Broadcast(ctx, barrier); err != nil {
		t.Fatal(err)
	}
	gate.recording.await(t, "cancellation_coordinator.outcome", func(e element.Envelope) bool {
		return slices.Contains(e.CausalParents, barrier.ItemID)
	})
	currentIntent := current.envelope.Payload.(policyelements.IntentSettlementAcknowledgement).Decision.Probe.DurableIntent.TrajectoryItemID
	var last realtimecu.SessionCancellationOutcome
	for _, recorded := range gate.recording.records("cancellation_coordinator.outcome") {
		outcome := recorded.Payload.(realtimecu.SessionCancellationOutcome)
		if outcome.DurableIntentItemID == currentIntent {
			last = outcome
		}
	}
	if last.Kind != realtimecu.SessionCancellationProgress || !last.PendingSettlement || last.PendingProducer {
		t.Fatalf("old acknowledgement changed the new cancellation: %+v", last)
	}
	select {
	case err := <-secondCanceled:
		t.Fatalf("old acknowledgement finished a different cancellation: %v", err)
	default:
	}
	current.release()
	if err := receiveRealtimeCU(t, secondCanceled, "second exact cancellation"); err != nil {
		t.Fatal(err)
	}
	// Complete a third intent through the same mounted graph. This detects a
	// stale message poisoning future work even when cancellation returned nil.
	fresh := start(490)
	fresh.release()
	intent := fresh.envelope.Payload.(policyelements.IntentSettlementAcknowledgement).Decision.Probe.DurableIntent.TrajectoryItemID
	gate.recording.await(t, "settlement.outcome", func(e element.Envelope) bool {
		o, ok := e.Payload.(policyelements.IntentSettlementOutcome)
		return ok && o.Code == "terminal_acknowledged" && o.DurableIntentItemID == intent
	})
	if fixture.model.count.Load() != 3 {
		t.Fatal("replayed acknowledgements changed the number of real effects")
	}
}

func TestRealtimeComputerUseDuplicateDispositionPreservesPendingCancellation(t *testing.T) {
	gate := newRealtimeCUTransactionReplayGate()
	dispositions := gate.add("policy.IntentDispositionProducer", "disposition", false, false)
	acks := gate.add(realtimecu.ActivationReference, "settlement_ack", false, true)
	fixture := newFailedEffectCancellationFixtureWithGate(t, 0, gate)
	sendRealtimeCUIntentWithFreshScreen(t, fixture.runtime, fixture.sink, 90, 100, 110)
	receiveRealtimeCU(t, fixture.model.invocations, "model")
	call := receiveRealtimeCU(t, fixture.sink.calls, "effect")
	completeRealtimeCUEffectSuccessfully(t, fixture, call, 200)
	ack := receiveRealtimeCU(t, acks.captured, "pending terminal acknowledgement")
	t.Cleanup(ack.release)
	disposition := receiveRealtimeCU(t, dispositions.captured, "original disposition")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	canceled := make(chan error, 1)
	go func() { canceled <- fixture.runtime.Cancel(ctx, "cancel before duplicate classification arrives") }()
	pending := gate.recording.await(t, "settlement.outcome", func(e element.Envelope) bool {
		o, ok := e.Payload.(policyelements.IntentSettlementOutcome)
		return ok && o.Operation == "cancel" && o.Code == "terminal_ack_pending"
	}).Payload.(policyelements.IntentSettlementOutcome)
	for range 3 {
		if _, err := disposition.output.Broadcast(ctx, disposition.envelope); err != nil {
			t.Fatal(err)
		}
		next := gate.recording.await(t, "settlement.outcome", func(e element.Envelope) bool {
			o, ok := e.Payload.(policyelements.IntentSettlementOutcome)
			return ok && o.Operation == "disposition" && o.StateRevisionBefore >= pending.StateRevisionAfter
		}).Payload.(policyelements.IntentSettlementOutcome)
		if next.Kind != policyelements.IntentSettlementIgnored || next.Code != "duplicate_terminal_disposition" {
			t.Fatalf("duplicate replaced the pending terminal: %+v", next)
		}
		pending = next
	}
	ack.release()
	if err := receiveRealtimeCU(t, canceled, "cancellation after duplicate classifications"); err != nil {
		t.Fatal(err)
	}
	gate.recording.await(t, "settlement.outcome", func(e element.Envelope) bool {
		o, ok := e.Payload.(policyelements.IntentSettlementOutcome)
		return ok && o.Code == "canceled_after_terminal_ack"
	})
	if fixture.model.count.Load() != 1 {
		t.Fatal("duplicate disposition revived the canceled intent")
	}
}

type realtimeCUTransactionReplayGate struct {
	recording *scenarioAddressingGraphRecorder
	ports     []*realtimeCUReplayGate
}

func newRealtimeCUTransactionReplayGate() *realtimeCUTransactionReplayGate {
	return &realtimeCUTransactionReplayGate{recording: newScenarioAddressingGraphRecorder()}
}

func (gate *realtimeCUTransactionReplayGate) add(factory, port string, input, hold bool) *realtimeCUReplayGate {
	p := &realtimeCUReplayGate{
		factoryName: factory, portName: port, input: input, hold: hold,
		recording: newScenarioAddressingGraphRecorder(), captured: make(chan realtimeCUReplayEnvelope, 128),
	}
	gate.ports = append(gate.ports, p)
	return p
}

func (gate *realtimeCUTransactionReplayGate) install(t *testing.T, config *graphlaunch.Config) {
	for _, p := range gate.ports {
		p.install(t, config)
	}
	instrumentScenarioAddressingFactory(t, config, "policy.IntentSettlement", gate.recording, "outcome")
	instrumentScenarioAddressingFactory(t, config, realtimecu.CancellationCoordinatorReference, gate.recording, "outcome")
}
