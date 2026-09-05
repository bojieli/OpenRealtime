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
)

// Delay the exact activation acknowledgement until cancellation is recorded,
// then replay that earlier pending outcome after the terminal has arrived at
// the coordinator. The independent producer acknowledgement is held so the
// transaction remains live while the replay is consumed.
func TestRealtimeComputerUseLatePendingSettlementCannotStrandCancellation(t *testing.T) {
	gate := &realtimeCUCancellationReplayGate{
		recording: newScenarioAddressingGraphRecorder(),
		ack: &realtimeCUReplayGate{
			factoryName: "policy.IntentSettlement", portName: "ack", input: true,
			recording: newScenarioAddressingGraphRecorder(), captured: make(chan realtimeCUReplayEnvelope, 4),
		},
		producer: &realtimeCUReplayGate{
			factoryName: realtimecu.CancellationCoordinatorReference, portName: "settlement_producer_outcome", input: true,
			recording: newScenarioAddressingGraphRecorder(), captured: make(chan realtimeCUReplayEnvelope, 4),
			match: func(envelope element.Envelope) bool {
				outcome, ok := envelope.Payload.(policyelements.IntentDispositionProducerOutcome)
				return ok && outcome.Kind == policyelements.IntentDispositionProducerCanceled
			},
		},
	}
	fixture := newFailedEffectCancellationFixtureWithGate(t, 0, gate)
	fixture.policy.mu.Lock()
	fixture.policy.choices = []policyelements.IntentDispositionKind{
		policyelements.IntentDispositionSucceeded, policyelements.IntentDispositionSucceeded,
	}
	fixture.policy.mu.Unlock()
	sendRealtimeCUIntentWithFreshScreen(t, fixture.runtime, fixture.sink, 90, 100, 110)
	receiveRealtimeCU(t, fixture.model.invocations, "initial model")
	call := receiveRealtimeCU(t, fixture.sink.calls, "initial effect")
	completeRealtimeCUEffectSuccessfully(t, fixture, call, 200)
	ack := receiveRealtimeCU(t, gate.ack.captured, "held terminal acknowledgement")
	t.Cleanup(ack.release)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	canceled := make(chan error, 1)
	go func() { canceled <- fixture.runtime.Cancel(ctx, "cancel while terminal acknowledgement is delayed") }()
	producer := receiveRealtimeCU(t, gate.producer.captured, "producer cancellation acknowledgement")
	t.Cleanup(producer.release)
	gate.recording.await(t, "cancellation_coordinator.outcome", func(envelope element.Envelope) bool {
		outcome, ok := envelope.Payload.(realtimecu.SessionCancellationOutcome)
		return ok && outcome.Operation == "settlement_ack" && outcome.PendingSettlement
	})
	ack.release()
	// This sentinel shares the coordinator input lane with the terminal and
	// replay; its refusal proves both earlier values were processed.
	gate.recording.await(t, "cancellation_coordinator.outcome", func(envelope element.Envelope) bool {
		return slices.Contains(envelope.CausalParents, realtimeCUCancellationReplayBarrier)
	})
	var last realtimecu.SessionCancellationOutcome
	for _, envelope := range gate.recording.records("cancellation_coordinator.outcome") {
		outcome := envelope.Payload.(realtimecu.SessionCancellationOutcome)
		if outcome.Operation == "settlement_ack" {
			last = outcome
		}
	}
	if last.PendingSettlement || !last.PendingProducer {
		t.Fatalf("replayed pending outcome revoked terminal settlement: %+v", last)
	}
	producer.release()
	if err := receiveRealtimeCU(t, canceled, "complete cancellation after replay"); err != nil {
		t.Fatal(err)
	}
	for _, captured := range []uint64{210, 220, 230} {
		sendRealtimeCUConsequenceFrame(t, fixture.runtime, fixture.sink, captured)
	}
	if fixture.model.count.Load() != 1 {
		t.Fatal("settlement replay reactivated the canceled intent")
	}
	sendRealtimeCUIntentWithFreshScreen(t, fixture.runtime, fixture.sink, 290, 300, 310)
	if invocation := receiveRealtimeCU(t, fixture.model.invocations, "replacement intent"); invocation != 2 {
		t.Fatalf("replacement invocation=%d", invocation)
	}
	next := receiveRealtimeCU(t, fixture.sink.calls, "replacement effect")
	completeRealtimeCUEffectSuccessfully(t, fixture, next, 320)
	nextAck := receiveRealtimeCU(t, gate.ack.captured, "replacement settlement acknowledgement")
	nextAck.release()
	gate.ack.recording.await(t, "settlement.outcome", func(envelope element.Envelope) bool {
		outcome, ok := envelope.Payload.(policyelements.IntentSettlementOutcome)
		return ok && outcome.Code == "terminal_acknowledged"
	})
}

const realtimeCUCancellationReplayBarrier = "cancellation-pending-replay-barrier"

type realtimeCUCancellationReplayGate struct {
	ack, producer *realtimeCUReplayGate
	recording     *scenarioAddressingGraphRecorder
}

func (gate *realtimeCUCancellationReplayGate) install(t *testing.T, config *graphlaunch.Config) {
	gate.ack.install(t, config)
	gate.producer.install(t, config)
	instrumentScenarioAddressingFactory(t, config, realtimecu.CancellationCoordinatorReference, gate.recording, "outcome")
	for index := range config.Catalog.Assembly.Implementations {
		registration := &config.Catalog.Assembly.Implementations[index]
		if registration.Factory.Descriptor().Name == realtimecu.CancellationCoordinatorReference {
			registration.Factory = realtimeCUCancellationReplayFactory{Factory: registration.Factory}
			return
		}
	}
	t.Fatal("production cancellation coordinator missing")
}

type realtimeCUCancellationReplayFactory struct{ element.Factory }

func (factory realtimeCUCancellationReplayFactory) ValidateConfig(raw json.RawMessage) error {
	return factory.Factory.(element.ConfigValidator).ValidateConfig(raw)
}

func (factory realtimeCUCancellationReplayFactory) Mount(ctx context.Context, mount element.MountContext) (element.Runnable, error) {
	mount.Ports = realtimeCUCancellationReplayPorts{Ports: mount.Ports}
	return factory.Factory.Mount(ctx, mount)
}

type realtimeCUCancellationReplayPorts struct{ element.Ports }

func (ports realtimeCUCancellationReplayPorts) Input(name string) (element.InputPort, error) {
	input, err := ports.Ports.Input(name)
	if err != nil || name != "settlement_outcome" {
		return input, err
	}
	return &realtimeCUCancellationReplayInput{InputPort: input}, nil
}

type realtimeCUCancellationReplayInput struct {
	element.InputPort
	pending  *element.Envelope
	queued   []element.Envelope
	replayed bool
}

func (input *realtimeCUCancellationReplayInput) Receive(ctx context.Context) (element.Envelope, error) {
	if len(input.queued) != 0 {
		envelope := input.queued[0]
		input.queued = input.queued[1:]
		return envelope, nil
	}
	envelope, err := input.InputPort.Receive(ctx)
	if err != nil {
		return envelope, err
	}
	outcome, ok := envelope.Payload.(policyelements.IntentSettlementOutcome)
	if !ok || input.replayed {
		return envelope, nil
	}
	if outcome.Operation == "cancel" && outcome.Code == "terminal_ack_pending" {
		copy := envelope.Clone()
		input.pending = &copy
	}
	if outcome.Operation == "ack" && outcome.Kind == policyelements.IntentSettlementCanceled && input.pending != nil {
		barrier := envelope.Clone()
		barrier.ItemID = realtimeCUCancellationReplayBarrier
		barrier.Payload = policyelements.IntentSettlementOutcome{
			Kind: "test-processing-barrier", Operation: "ack", SessionID: envelope.SessionID,
		}
		input.queued = []element.Envelope{input.pending.Clone(), barrier}
		input.replayed = true
	}
	return envelope, nil
}
