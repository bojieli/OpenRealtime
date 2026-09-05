package graphs_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	"github.com/bojieli/OpenRealtime/graph/binding/realtimecu"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// Replaying an old consequence after continuation must not classify that
// already-consumed result again or strand settlement of the next real effect.
// The port wrapper preserves the production graph and real canonical history.
func TestRealtimeComputerUseReplayedConsequenceCannotStrandNextEffect(t *testing.T) {
	gate := &realtimeCUReplayGate{recording: newScenarioAddressingGraphRecorder(), captured: make(chan realtimeCUReplayEnvelope, 16)}
	fixture := newFailedEffectCancellationFixtureWithGate(t, 0, gate)
	fixture.policy.mu.Lock()
	fixture.policy.choices = []policyelements.IntentDispositionKind{policyelements.IntentDispositionContinue, policyelements.IntentDispositionSucceeded}
	fixture.policy.mu.Unlock()
	sendRealtimeCUIntentWithFreshScreen(t, fixture.runtime, fixture.sink, 90, 100, 110)
	receiveRealtimeCU(t, fixture.model.invocations, "first invocation")
	first := receiveRealtimeCU(t, fixture.sink.calls, "first effect")
	resultFrame := func(callID, name string, captured uint64) {
		t.Helper()
		if err := fixture.runtime.ToolResult(t.Context(), trajectory.ToolResult{CallID: callID, Name: name, Output: json.RawMessage(`{"clicked":true}`)}); err != nil {
			t.Fatal(err)
		}
		receiveRealtimeCU(t, fixture.observer.consequences, "result consequence request")
		sendRealtimeCUConsequenceFrame(t, fixture.runtime, fixture.sink, captured)
	}
	resultFrame(first.Calls[0].CallID, first.Calls[0].Name, 200)
	receiveRealtimeCU(t, fixture.policy.decisions, "first continuation")
	receiveRealtimeCU(t, fixture.model.invocations, "second invocation")
	second := receiveRealtimeCU(t, fixture.sink.calls, "second effect")
	continued := gate.recording.await(t, "settlement.outcome", func(e element.Envelope) bool {
		o, ok := e.Payload.(policyelements.IntentSettlementOutcome)
		return ok && o.Code == "continue"
	}).Payload.(policyelements.IntentSettlementOutcome)
	var replay realtimeCUReplayEnvelope
	for {
		replay = receiveRealtimeCU(t, gate.captured, "post-effect evidence")
		evidence := replay.envelope.Payload.(policyelements.AdmittedTemporalEvidence)
		if evidence.TriggerObservation.OccurredNS == 200 {
			break
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := replay.output.Broadcast(ctx, replay.envelope); err != nil {
		t.Fatal(err)
	}
	// The next actor outcome establishes that replay was processed before we
	// deliver the second result. No delay is used to guess whether it arrived.
	outcome := gate.recording.await(t, "settlement.outcome", func(e element.Envelope) bool {
		o, ok := e.Payload.(policyelements.IntentSettlementOutcome)
		return ok && o.Operation == "evidence" && o.TriggerObservationItemID == replay.envelope.Payload.(policyelements.AdmittedTemporalEvidence).TriggerObservation.TrajectoryItemID && o.StateRevisionBefore >= continued.StateRevisionAfter
	}).Payload.(policyelements.IntentSettlementOutcome)
	if outcome.Kind != policyelements.IntentSettlementIgnored || outcome.Code != "continued_result_replayed" {
		t.Fatalf("replayed consequence = %+v", outcome)
	}
	select {
	case d := <-fixture.policy.decisions:
		t.Fatalf("replayed result was classified again: %s", d)
	default:
	}
	resultFrame(second.Calls[0].CallID, second.Calls[0].Name, 210)
	if decision := receiveRealtimeCU(t, fixture.policy.decisions, "second effect settlement"); decision != policyelements.IntentDispositionSucceeded {
		t.Fatal(decision)
	}
	gate.recording.await(t, "settlement.outcome", func(e element.Envelope) bool {
		o, ok := e.Payload.(policyelements.IntentSettlementOutcome)
		return ok && o.Code == "terminal_acknowledged"
	})
	for _, captured := range []uint64{220, 230, 240} {
		sendRealtimeCUConsequenceFrame(t, fixture.runtime, fixture.sink, captured)
	}
	if fixture.model.count.Load() != 2 {
		t.Fatal("settled intent produced more model work")
	}
}

type realtimeCUReplayEnvelope struct {
	envelope element.Envelope
	output   element.OutputPort
	release  func()
}
type realtimeCUReplayGate struct {
	factoryName, portName string
	hold, input           bool
	recording             *scenarioAddressingGraphRecorder
	captured              chan realtimeCUReplayEnvelope
}

func (gate *realtimeCUReplayGate) install(t *testing.T, config *graphlaunch.Config) {
	if gate.factoryName == "" {
		gate.factoryName = "policy.TemporalEvidenceAdmission"
	}
	if gate.portName == "" {
		gate.portName = "admitted"
	}
	instrumentScenarioAddressingFactory(t, config, "policy.IntentSettlement", gate.recording, "outcome", "terminal")
	instrumentScenarioAddressingFactory(t, config, "policy.IntentDispositionProducer", gate.recording, "outcome")
	instrumentScenarioAddressingFactory(t, config, realtimecu.ActivationReference, gate.recording, "outcome")
	for index := range config.Catalog.Assembly.Implementations {
		registration := &config.Catalog.Assembly.Implementations[index]
		if registration.Factory.Descriptor().Name == gate.factoryName {
			registration.Factory = realtimeCUReplayFactory{Factory: registration.Factory, gate: gate}
			return
		}
	}
	t.Fatalf("production factory missing: %s", gate.factoryName)
}

type realtimeCUReplayFactory struct {
	element.Factory
	gate *realtimeCUReplayGate
}

func (f realtimeCUReplayFactory) ValidateConfig(source json.RawMessage) error {
	return f.Factory.(element.ConfigValidator).ValidateConfig(source)
}
func (f realtimeCUReplayFactory) Mount(ctx context.Context, mount element.MountContext) (element.Runnable, error) {
	mount.Ports = realtimeCUReplayPorts{Ports: mount.Ports, gate: f.gate}
	return f.Factory.Mount(ctx, mount)
}

type realtimeCUReplayPorts struct {
	element.Ports
	gate *realtimeCUReplayGate
}

func (p realtimeCUReplayPorts) Input(name string) (element.InputPort, error) {
	input, err := p.Ports.Input(name)
	if err != nil || !p.gate.input || name != p.gate.portName {
		return input, err
	}
	return realtimeCUHeldInput{InputPort: input, gate: p.gate}, nil
}

type realtimeCUHeldInput struct {
	element.InputPort
	gate *realtimeCUReplayGate
}

func (p realtimeCUHeldInput) Receive(ctx context.Context) (element.Envelope, error) {
	envelope, err := p.InputPort.Receive(ctx)
	if err != nil {
		return envelope, err
	}
	released := make(chan struct{})
	var once sync.Once
	capture := realtimeCUReplayEnvelope{envelope: envelope.Clone(), release: func() { once.Do(func() { close(released) }) }}
	select {
	case p.gate.captured <- capture:
	case <-ctx.Done():
		return element.Envelope{}, context.Cause(ctx)
	}
	select {
	case <-released:
		return envelope, nil
	case <-ctx.Done():
		return element.Envelope{}, context.Cause(ctx)
	}
}

func (p realtimeCUReplayPorts) Output(name string) (element.OutputPort, error) {
	output, err := p.Ports.Output(name)
	if err != nil || p.gate.input || name != p.gate.portName {
		return output, err
	}
	return realtimeCUReplayOutput{OutputPort: output, gate: p.gate}, nil
}

type realtimeCUReplayOutput struct {
	element.OutputPort
	gate *realtimeCUReplayGate
}

func (p realtimeCUReplayOutput) Broadcast(ctx context.Context, envelope element.Envelope) (element.SendResult, error) {
	released := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	capture := realtimeCUReplayEnvelope{envelope.Clone(), p.OutputPort, release}
	if p.gate.hold {
		select {
		case p.gate.captured <- capture:
		case <-ctx.Done():
			return element.SendResult{}, context.Cause(ctx)
		}
		select {
		case <-released:
		case <-ctx.Done():
			return element.SendResult{}, context.Cause(ctx)
		}
	}
	delivery, err := p.OutputPort.Broadcast(ctx, envelope)
	if err == nil && !p.gate.hold {
		select {
		case p.gate.captured <- capture:
		case <-ctx.Done():
			return delivery, context.Cause(ctx)
		}
	}
	return delivery, err
}

// Each altered value travels on the real producer's output lane. Refusal must
// leave the genuine decision usable; rejecting bad input must not poison or
// prematurely clear the active action.
func TestRealtimeComputerUseAlteredSettlementBoundariesLeaveRealEffectPending(t *testing.T) {
	for _, test := range []struct {
		name, factory, port, outcome string
		alter                        func(element.Envelope) element.Envelope
	}{
		{"probe result", "policy.IntentDispositionRetry", "attempt", "settlement_producer.outcome", func(e element.Envelope) element.Envelope {
			p := e.Payload.(policyelements.IntentSettlementProbe)
			p.Result.CallID = "different-call"
			e.Payload = p
			return e
		}},
		{"disposition result", "policy.IntentDispositionProducer", "disposition", "settlement.outcome", func(e element.Envelope) element.Envelope {
			d := e.Payload.(policyelements.IntentDisposition)
			d.Probe.Result.CallID = "different-call"
			e.Payload = d
			return e
		}},
		{"disposition detector", "policy.IntentDispositionProducer", "disposition", "settlement.outcome", func(e element.Envelope) element.Envelope {
			d := e.Payload.(policyelements.IntentDisposition)
			d.Detector.Revision = "different-revision"
			e.Payload = d
			return e
		}},
		{"terminal generation", "policy.IntentSettlement", "terminal", "activation.outcome", func(e element.Envelope) element.Envelope { e.RunID = "different-generation"; return e }},
		{"terminal prefix", "policy.IntentSettlement", "terminal", "activation.outcome", func(e element.Envelope) element.Envelope {
			d := e.Payload.(policyelements.IntentSettlementDecision)
			d.Evidence.Prefix.Digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
			e.Payload = d
			return e
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			gate := &realtimeCUReplayGate{factoryName: test.factory, portName: test.port, hold: true, recording: newScenarioAddressingGraphRecorder(), captured: make(chan realtimeCUReplayEnvelope, 4)}
			fixture := newFailedEffectCancellationFixtureWithGate(t, 0, gate)
			sendRealtimeCUIntentWithFreshScreen(t, fixture.runtime, fixture.sink, 90, 100, 110)
			receiveRealtimeCU(t, fixture.model.invocations, "initial model")
			call := receiveRealtimeCU(t, fixture.sink.calls, "initial effect")
			if err := fixture.runtime.ToolResult(t.Context(), trajectory.ToolResult{CallID: call.Calls[0].CallID, Name: call.Calls[0].Name, Output: json.RawMessage(`{"clicked":true}`)}); err != nil {
				t.Fatal(err)
			}
			receiveRealtimeCU(t, fixture.observer.consequences, "consequence request")
			sendRealtimeCUConsequenceFrame(t, fixture.runtime, fixture.sink, 200)
			held := receiveRealtimeCU(t, gate.captured, "held settlement boundary")
			t.Cleanup(held.release)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			if _, err := held.output.Broadcast(ctx, test.alter(held.envelope.Clone())); err != nil {
				t.Fatal(err)
			}
			gate.recording.await(t, test.outcome, func(e element.Envelope) bool {
				switch o := e.Payload.(type) {
				case policyelements.IntentDispositionProducerOutcome:
					return o.Kind == policyelements.IntentDispositionProducerRefused
				case policyelements.IntentSettlementOutcome:
					return o.Operation == "disposition" && o.Kind == policyelements.IntentSettlementRefused
				case policyelements.GenerationOutcome:
					return o.Kind == policyelements.GenerationRefused && (o.Code == "invalid_settlement_decision" || o.Code == "invalid_settlement_envelope")
				}
				return false
			})
			for _, e := range gate.recording.records("activation.outcome") {
				if o, ok := e.Payload.(policyelements.GenerationOutcome); ok && (o.Code == "intent_succeeded" || o.Code == "intent_failed") {
					t.Fatal("altered evidence cleared the real effect")
				}
			}
			if fixture.model.count.Load() != 1 {
				t.Fatal("altered settlement reactivated cognition")
			}
			held.release()
			gate.recording.await(t, "settlement.outcome", func(e element.Envelope) bool {
				o, ok := e.Payload.(policyelements.IntentSettlementOutcome)
				return ok && o.Code == "terminal_acknowledged"
			})
			if fixture.model.count.Load() != 1 {
				t.Fatal("terminal settlement reactivated cognition")
			}
		})
	}
}

func TestRealtimeComputerUseDelayedTerminalCannotClearNewIntent(t *testing.T) {
	gate := &realtimeCUReplayGate{factoryName: "policy.IntentSettlement", portName: "terminal", recording: newScenarioAddressingGraphRecorder(), captured: make(chan realtimeCUReplayEnvelope, 8)}
	fixture := newFailedEffectCancellationFixtureWithGate(t, 0, gate)
	fixture.policy.mu.Lock()
	fixture.policy.choices = []policyelements.IntentDispositionKind{policyelements.IntentDispositionSucceeded, policyelements.IntentDispositionSucceeded}
	fixture.policy.mu.Unlock()
	sendRealtimeCUIntentWithFreshScreen(t, fixture.runtime, fixture.sink, 90, 100, 110)
	receiveRealtimeCU(t, fixture.model.invocations, "first model")
	first := receiveRealtimeCU(t, fixture.sink.calls, "first effect")
	completeRealtimeCUEffectSuccessfully(t, fixture, first, 200)
	gate.recording.await(t, "settlement.outcome", func(e element.Envelope) bool {
		o, ok := e.Payload.(policyelements.IntentSettlementOutcome)
		return ok && o.Code == "terminal_acknowledged"
	})
	old := receiveRealtimeCU(t, gate.captured, "old terminal decision")
	sendRealtimeCUIntentWithFreshScreen(t, fixture.runtime, fixture.sink, 290, 300, 310)
	if n := receiveRealtimeCU(t, fixture.model.invocations, "new intent model"); n != 2 {
		t.Fatal(n)
	}
	next := receiveRealtimeCU(t, fixture.sink.calls, "new intent effect")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for range 3 {
		if _, err := old.output.Broadcast(ctx, old.envelope); err != nil {
			t.Fatal(err)
		}
	}
	gate.recording.await(t, "activation.outcome", func(e element.Envelope) bool {
		o, ok := e.Payload.(policyelements.GenerationOutcome)
		return ok && o.Code == "duplicate_settlement_decision"
	})
	completeRealtimeCUEffectSuccessfully(t, fixture, next, 320)
	gate.recording.await(t, "settlement.outcome", func(e element.Envelope) bool {
		o, ok := e.Payload.(policyelements.IntentSettlementOutcome)
		return ok && o.Code == "terminal_acknowledged" && o.ResultItemID != old.envelope.Payload.(policyelements.IntentSettlementDecision).Probe.Result.TrajectoryItemID
	})
	if fixture.model.count.Load() != 2 {
		t.Fatal("duplicate terminal created work")
	}
}

func TestRealtimeComputerUseTerminalBeforeModelResultKeepsExactEffect(t *testing.T) {
	gate := &realtimeCUReplayGate{factoryName: realtimecu.ActivationReference, portName: "result", input: true, recording: newScenarioAddressingGraphRecorder(), captured: make(chan realtimeCUReplayEnvelope, 4)}
	fixture := newFailedEffectCancellationFixtureWithGate(t, 0, gate)
	sendRealtimeCUIntentWithFreshScreen(t, fixture.runtime, fixture.sink, 90, 100, 110)
	receiveRealtimeCU(t, fixture.model.invocations, "initial model")
	call := receiveRealtimeCU(t, fixture.sink.calls, "effect before activation's model result")
	held := receiveRealtimeCU(t, gate.captured, "held model result")
	t.Cleanup(held.release)
	completeRealtimeCUEffectSuccessfully(t, fixture, call, 200)
	gate.recording.await(t, "activation.outcome", func(e element.Envelope) bool {
		o, ok := e.Payload.(policyelements.GenerationOutcome)
		return ok && o.Code == "settlement_waiting_for_model_result" && o.GenerationID == call.InvocationID
	})
	for _, e := range gate.recording.records("settlement.outcome") {
		if o, ok := e.Payload.(policyelements.IntentSettlementOutcome); ok && o.Code == "terminal_acknowledged" {
			t.Fatal("settlement acknowledged before its model result arrived")
		}
	}
	held.release()
	gate.recording.await(t, "settlement.outcome", func(e element.Envelope) bool {
		o, ok := e.Payload.(policyelements.IntentSettlementOutcome)
		return ok && o.Code == "terminal_acknowledged"
	})
	if fixture.model.count.Load() != 1 {
		t.Fatal("reordered result created new work")
	}
}

func TestRealtimeComputerUseCancellationOvertakesTerminalDelivery(t *testing.T) {
	gate := &realtimeCUReplayGate{factoryName: realtimecu.ActivationReference, portName: "settlement", input: true, recording: newScenarioAddressingGraphRecorder(), captured: make(chan realtimeCUReplayEnvelope, 4)}
	fixture := newFailedEffectCancellationFixtureWithGate(t, 0, gate)
	sendRealtimeCUIntentWithFreshScreen(t, fixture.runtime, fixture.sink, 90, 100, 110)
	receiveRealtimeCU(t, fixture.model.invocations, "initial model")
	call := receiveRealtimeCU(t, fixture.sink.calls, "initial effect")
	completeRealtimeCUEffectSuccessfully(t, fixture, call, 200)
	held := receiveRealtimeCU(t, gate.captured, "terminal awaiting delivery")
	t.Cleanup(held.release)
	canceled := make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go func() { canceled <- fixture.runtime.Cancel(ctx, "user cancels while terminal is delayed") }()
	gate.recording.await(t, "settlement.outcome", func(e element.Envelope) bool {
		o, ok := e.Payload.(policyelements.IntentSettlementOutcome)
		return ok && o.Operation == "cancel" && o.Code == "terminal_ack_pending"
	})
	select {
	case err := <-canceled:
		t.Fatalf("cancellation completed before exact terminal cleanup: %v", err)
	default:
	}
	held.release()
	if err := receiveRealtimeCU(t, canceled, "cancellation completion"); err != nil {
		t.Fatal(err)
	}
	gate.recording.await(t, "settlement.outcome", func(e element.Envelope) bool {
		o, ok := e.Payload.(policyelements.IntentSettlementOutcome)
		return ok && o.Code == "canceled_after_terminal_ack"
	})
	for _, captured := range []uint64{210, 220, 230} {
		sendRealtimeCUConsequenceFrame(t, fixture.runtime, fixture.sink, captured)
	}
	if fixture.model.count.Load() != 1 {
		t.Fatal("late terminal resumed canceled intent")
	}
}

func TestRealtimeComputerUseAcknowledgementMustMatchPendingTerminal(t *testing.T) {
	gate := &realtimeCUReplayGate{factoryName: realtimecu.ActivationReference, portName: "settlement_ack", hold: true, recording: newScenarioAddressingGraphRecorder(), captured: make(chan realtimeCUReplayEnvelope, 4)}
	fixture := newFailedEffectCancellationFixtureWithGate(t, 0, gate)
	sendRealtimeCUIntentWithFreshScreen(t, fixture.runtime, fixture.sink, 90, 100, 110)
	receiveRealtimeCU(t, fixture.model.invocations, "initial model")
	call := receiveRealtimeCU(t, fixture.sink.calls, "initial effect")
	completeRealtimeCUEffectSuccessfully(t, fixture, call, 200)
	held := receiveRealtimeCU(t, gate.captured, "pending terminal acknowledgement")
	t.Cleanup(held.release)
	bad := held.envelope.Clone()
	ack := bad.Payload.(policyelements.IntentSettlementAcknowledgement)
	ack.GenerationID = "different-generation"
	bad.Payload = ack
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := held.output.Broadcast(ctx, bad); err != nil {
		t.Fatal(err)
	}
	gate.recording.await(t, "settlement.outcome", func(e element.Envelope) bool {
		o, ok := e.Payload.(policyelements.IntentSettlementOutcome)
		return ok && o.Operation == "ack" && o.Kind == policyelements.IntentSettlementRefused
	})
	for _, e := range gate.recording.records("settlement.outcome") {
		if o, ok := e.Payload.(policyelements.IntentSettlementOutcome); ok && o.Code == "terminal_acknowledged" {
			t.Fatal("mismatched acknowledgement released settlement")
		}
	}
	held.release()
	gate.recording.await(t, "settlement.outcome", func(e element.Envelope) bool {
		o, ok := e.Payload.(policyelements.IntentSettlementOutcome)
		return ok && o.Code == "terminal_acknowledged"
	})
	for range 3 {
		if _, err := held.output.Broadcast(ctx, held.envelope); err != nil {
			t.Fatal(err)
		}
	}
	gate.recording.await(t, "settlement.outcome", func(e element.Envelope) bool {
		o, ok := e.Payload.(policyelements.IntentSettlementOutcome)
		return ok && o.Code == "duplicate_acknowledgement"
	})
	if fixture.model.count.Load() != 1 {
		t.Fatal("acknowledgement replay created another effect")
	}
}
