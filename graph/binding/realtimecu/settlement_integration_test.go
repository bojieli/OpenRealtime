package realtimecu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// mountedSettlementGraph is deliberately an isolated subgraph rather than a
// second production topology. In particular, activation.admitted has exactly
// one source: evidence released by IntentSettlement. The probe Tee makes test
// inspection explicit, and the Mux makes an indeterminate retry an authored
// control path instead of a hidden timer in either policy element.
//
// This checkpoint covers the real gate, producer, and activation lifecycle,
// their exact no-bypass topology, continuation/terminal ordering, retained
// visual media, explicit indeterminate retry, and shutdown ownership. It does
// not cover the session-cancellation coordinator, the production graph and
// lock/profile identities, forged cross-session traffic, saturation and
// publication failure, the stable WebSocket endpoint, or any live behavioral
// benchmark; those remain separate acceptance gates.
const mountedSettlementGraph = `graph realtime_cu_settlement_integration_test {
    policy.IntentSettlement :: settlement;
    policy.IntentDispositionProducer :: producer;
    policy.RealtimeComputerUseActivation :: activation;
    flow.Tee :: probe_copy;
    flow.Mux :: probe_requests;
    flow.Drop :: settlement_state_sink;
    flow.Drop :: producer_state_sink;
    flow.Drop :: producer_outcome_sink;
    flow.Drop :: producer_resolution_sink;
    flow.Drop :: activation_authority_sink;
    flow.Drop :: activation_state_sink;
    flow.Drop :: activation_disposition_sink;

    settlement.probe -> probe_copy.in;
    probe_copy.out -> probe_requests.in;
    probe_requests.out -> producer.probe;
    producer.disposition -> settlement.disposition;
    settlement.admitted -> activation.admitted;
    settlement.cleanup -> activation.effect_cleanup;
    settlement.terminal -> activation.settlement;
    activation.settlement_ack -> settlement.ack;
    settlement.state -> settlement_state_sink.in;
    producer.state -> producer_state_sink.in;
    producer.outcome -> producer_outcome_sink.in;
    producer.resolved -> producer_resolution_sink.in;
    activation.authority -> activation_authority_sink.in;
    activation.state -> activation_state_sink.in;
    activation.disposition_append -> activation_disposition_sink.in;

    input evidence = settlement.evidence;
    input retry_probe = probe_requests.in;
    input settlement_reset = settlement.reset;
    input settlement_cancel = settlement.cancel;
    input producer_cancel = producer.cancel;
    input activation_cancel = activation.cancel;
    input result = activation.result;
    input effect_terminal = activation.effect_terminal;
    input disposition_committed = activation.disposition_committed;
    input disposition_rejected = activation.disposition_rejected;

    output probe = probe_copy.out;
    output settlement_outcome = settlement.outcome;
    output trigger = activation.trigger;
    output activation_outcome = activation.outcome;
}
`

var mountedSettlementDetector = policyelements.IntentDetectorIdentity{
	Reference: "settlement-primary", Revision: "v1",
	ConfigurationDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
}

type mountedSettlementDecider struct {
	descriptor policyelements.SemanticDeciderDescriptor

	mu       sync.Mutex
	choices  []policyelements.IntentDispositionKind
	requests []coreinteraction.Decision
	closed   atomic.Bool
}

func (decider *mountedSettlementDecider) Name() string { return "mounted-settlement-test" }

func (decider *mountedSettlementDecider) Descriptor() policyelements.SemanticDeciderDescriptor {
	return decider.descriptor
}

func (decider *mountedSettlementDecider) Decide(
	ctx context.Context, request coreinteraction.Decision,
) (coreinteraction.Outcome, error) {
	if err := context.Cause(ctx); err != nil {
		return coreinteraction.Outcome{}, err
	}
	copy := request
	copy.Options = slices.Clone(request.Options)
	copy.Images = make([]coreinteraction.Image, len(request.Images))
	for index := range request.Images {
		copy.Images[index] = request.Images[index]
		copy.Images[index].Bytes = slices.Clone(request.Images[index].Bytes)
	}
	decider.mu.Lock()
	defer decider.mu.Unlock()
	decider.requests = append(decider.requests, copy)
	if len(decider.choices) == 0 {
		return coreinteraction.Outcome{}, errors.New("mounted settlement test has no scripted disposition")
	}
	choice := decider.choices[0]
	decider.choices = decider.choices[1:]
	index := slices.Index(request.Options, string(choice))
	if index < 0 {
		return coreinteraction.Outcome{}, fmt.Errorf("scripted disposition %q is not offered", choice)
	}
	return coreinteraction.Outcome{Index: index, Option: string(choice)}, nil
}

func (decider *mountedSettlementDecider) Close() error {
	decider.closed.Store(true)
	return nil
}

func (decider *mountedSettlementDecider) snapshotRequests() []coreinteraction.Decision {
	decider.mu.Lock()
	defer decider.mu.Unlock()
	result := make([]coreinteraction.Decision, len(decider.requests))
	copy(result, decider.requests)
	return result
}

type mountedSettlementHarness struct {
	mounted  *graphruntime.Mounted
	done     <-chan error
	cancel   context.CancelFunc
	fixture  *activationTestFixture
	decider  *mountedSettlementDecider
	mediaMu  sync.Mutex
	mediaIDs []string
}

type mountedSettlementTurn struct {
	intent         policyelements.TemporalEvidenceItemIdentity
	intentItemID   string
	runID          string
	contextVersion uint64
}

func TestMountedSettlementCompositionTopologyHasNoActivationBypass(t *testing.T) {
	graph := compileMountedSettlementGraph(t)
	wantEdges := [][4]string{
		{"settlement", "probe", "probe_copy", "in"},
		{"probe_copy", "out", "probe_requests", "in"},
		{"probe_requests", "out", "producer", "probe"},
		{"producer", "disposition", "settlement", "disposition"},
		{"settlement", "admitted", "activation", "admitted"},
		{"settlement", "cleanup", "activation", "effect_cleanup"},
		{"settlement", "terminal", "activation", "settlement"},
		{"activation", "settlement_ack", "settlement", "ack"},
	}
	for _, want := range wantEdges {
		if !mountedSettlementHasEdge(graph, want[0], want[1], want[2], want[3]) {
			t.Errorf("settlement graph lacks %s.%s -> %s.%s", want[0], want[1], want[2], want[3])
		}
	}
	incoming := 0
	for _, edge := range graph.Edges {
		if edge.To.Node == "activation" && edge.To.Port == "admitted" {
			incoming++
			if edge.From.Node != "settlement" || edge.From.Port != "admitted" {
				t.Errorf("activation admission bypass = %s -> %s", edge.From.String(), edge.To.String())
			}
		}
	}
	for _, boundary := range graph.Boundaries {
		if boundary.Direction == ir.InputBoundary &&
			boundary.Endpoint.Node == "activation" && boundary.Endpoint.Port == "admitted" {
			t.Errorf("activation.admitted is exposed as input boundary %q", boundary.Name)
		}
	}
	if incoming != 1 {
		t.Fatalf("activation.admitted incoming edges = %d, want exactly 1", incoming)
	}
}

func TestMountedSettlementCompositionContinueUsesExactMediaAndReactivatesOnce(t *testing.T) {
	harness := mountSettlementComposition(t, []policyelements.IntentDispositionKind{
		policyelements.IntentDispositionContinue,
	})
	turn := harness.startTurn(t)
	consequence := harness.appendSuccessfulConsequence(t, turn, "continue-call", true)
	harness.send(t, "result", activationResultEnvelope(
		turn.runID, turn.contextVersion,
		[]cognitionelements.ToolProposal{activationTestProposal("continue-call")},
	))
	harness.send(t, "evidence", consequence)
	harness.receive(t, "probe")

	outcome := harness.receiveSettlementOutcomeCode(t, "continue")
	if outcome.Kind != policyelements.IntentSettlementAdmitted ||
		outcome.Disposition != policyelements.IntentDispositionContinue {
		t.Fatalf("continue outcome = %+v", outcome)
	}
	next := harness.receive(t, "trigger")
	if next.RunID == "" || next.RunID == turn.runID {
		t.Fatalf("continued settlement trigger = %+v", next)
	}
	harness.assertNoEnvelope(t, "trigger", 75*time.Millisecond)
	harness.assertExactMedia(t, 1)
}

func TestMountedSettlementCompositionTerminalAcknowledgesWithoutReactivation(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		disposition policyelements.IntentDispositionKind
		code        string
	}{
		{name: "succeeded", disposition: policyelements.IntentDispositionSucceeded, code: "intent_succeeded"},
		{name: "failed", disposition: policyelements.IntentDispositionFailed, code: "intent_failed"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			harness := mountSettlementComposition(t, []policyelements.IntentDispositionKind{testCase.disposition})
			turn := harness.startTurn(t)
			consequence := harness.appendSuccessfulConsequence(t, turn, testCase.name+"-call", true)
			harness.send(t, "result", activationResultEnvelope(
				turn.runID, turn.contextVersion,
				[]cognitionelements.ToolProposal{activationTestProposal(testCase.name + "-call")},
			))
			harness.send(t, "evidence", consequence)
			harness.receive(t, "probe")

			settled := harness.receiveSettlementOutcomeCode(t, "terminal_acknowledged")
			if settled.Kind != policyelements.IntentSettlementSuppressed ||
				settled.Disposition != testCase.disposition {
				t.Fatalf("terminal acknowledgement outcome = %+v", settled)
			}
			activation := harness.receiveActivationOutcomeCode(t, testCase.code)
			if activation.GenerationID != turn.runID {
				t.Fatalf("terminal activation outcome = %+v", activation)
			}
			harness.assertNoEnvelope(t, "trigger", 75*time.Millisecond)

			// Later same-intent cadence is stopped by the terminal gate. If the
			// graph had a second admission edge, this observation would be able to
			// select the durable intent and open another generation.
			later, _ := harness.fixture.appendVisual(
				t, testCase.name+"-later-screen", "later unchanged cadence", turn.intentItemID,
			)
			later = afterIntentAdmissionEnvelope(later, turn.intent)
			harness.send(t, "evidence", later)
			if suppressed := harness.receiveSettlementOutcomeCode(t, "intent_terminal"); suppressed.Kind != policyelements.IntentSettlementSuppressed {
				t.Fatalf("terminal cadence outcome = %+v", suppressed)
			}
			harness.assertNoEnvelope(t, "trigger", 75*time.Millisecond)
			harness.assertExactMedia(t, 1)
		})
	}
}

func TestMountedSettlementCompositionIndeterminateWaitsForExplicitRetry(t *testing.T) {
	harness := mountSettlementComposition(t, []policyelements.IntentDispositionKind{
		policyelements.IntentDispositionIndeterminate,
		policyelements.IntentDispositionContinue,
	})
	turn := harness.startTurn(t)
	consequence := harness.appendSuccessfulConsequence(t, turn, "retry-call", true)
	harness.send(t, "result", activationResultEnvelope(
		turn.runID, turn.contextVersion,
		[]cognitionelements.ToolProposal{activationTestProposal("retry-call")},
	))
	harness.send(t, "evidence", consequence)
	probe := harness.receive(t, "probe")

	if outcome := harness.receiveSettlementOutcomeCode(t, "indeterminate"); outcome.Kind != policyelements.IntentSettlementHeld ||
		outcome.Disposition != policyelements.IntentDispositionIndeterminate {
		t.Fatalf("indeterminate outcome = %+v", outcome)
	}
	harness.assertNoEnvelope(t, "trigger", 75*time.Millisecond)
	if requests := harness.decider.snapshotRequests(); len(requests) != 1 {
		t.Fatalf("indeterminate classification requests = %d, want 1 before explicit retry", len(requests))
	}

	// The graph-authored retry lane replays the exact typed probe. No element
	// interprets indeterminate as success or invents a retry policy internally.
	harness.send(t, "retry_probe", probe)
	if outcome := harness.receiveSettlementOutcomeCode(t, "continue"); outcome.Kind != policyelements.IntentSettlementAdmitted {
		t.Fatalf("retried continuation outcome = %+v", outcome)
	}
	if trigger := harness.receive(t, "trigger"); trigger.RunID == "" || trigger.RunID == turn.runID {
		t.Fatalf("retried continuation trigger = %+v", trigger)
	}
	harness.assertNoEnvelope(t, "trigger", 75*time.Millisecond)
	if requests := harness.decider.snapshotRequests(); len(requests) != 2 {
		t.Fatalf("classification requests after explicit retry = %d, want 2", len(requests))
	}
	harness.assertExactMedia(t, 2)
}

func TestMountedSettlementCompositionTerminalBeforeModelResultIsDeferred(t *testing.T) {
	harness := mountSettlementComposition(t, []policyelements.IntentDispositionKind{
		policyelements.IntentDispositionSucceeded,
	})
	turn := harness.startTurn(t)
	consequence := harness.appendSuccessfulConsequence(t, turn, "early-terminal-call", true)

	// Canonical proposal/call/result evidence already exists, but activation's
	// independently forked SafeModelResult copy is intentionally delayed.
	harness.send(t, "evidence", consequence)
	harness.receive(t, "probe")
	if outcome := harness.receiveActivationOutcomeCode(t, "settlement_waiting_for_model_result"); outcome.GenerationID != turn.runID {
		t.Fatalf("early terminal activation outcome = %+v", outcome)
	}
	harness.assertNoEnvelope(t, "trigger", 75*time.Millisecond)

	harness.send(t, "result", activationResultEnvelope(
		turn.runID, turn.contextVersion,
		[]cognitionelements.ToolProposal{activationTestProposal("early-terminal-call")},
	))
	if outcome := harness.receiveSettlementOutcomeCode(t, "terminal_acknowledged"); outcome.Kind != policyelements.IntentSettlementSuppressed {
		t.Fatalf("deferred terminal acknowledgement = %+v", outcome)
	}
	if outcome := harness.receiveActivationOutcomeCode(t, "intent_succeeded"); outcome.GenerationID != turn.runID {
		t.Fatalf("deferred terminal completion = %+v", outcome)
	}
	harness.assertNoEnvelope(t, "trigger", 75*time.Millisecond)
	harness.assertExactMedia(t, 1)
}

func mountSettlementComposition(
	t *testing.T, choices []policyelements.IntentDispositionKind,
) *mountedSettlementHarness {
	t.Helper()
	fixture := newActivationTestFixture(t)
	descriptor := policyelements.SemanticDeciderDescriptor{
		Provider: "test-provider", Model: "test-model", Protocol: "enum-v1",
		Revision:            mountedSettlementDetector.Revision,
		ConfigurationDigest: mountedSettlementDetector.ConfigurationDigest,
		Vision:              true, DecisionTimeoutMS: 1_000,
	}
	decider := &mountedSettlementDecider{descriptor: descriptor, choices: slices.Clone(choices)}
	semanticRegistry := policyelements.NewSemanticDeciderRegistry()
	if err := semanticRegistry.Register(mountedSettlementDetector.Reference, descriptor,
		func() (policyelements.SemanticDecider, error) { return decider, nil }); err != nil {
		t.Fatal(err)
	}
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(stateelements.TrajectoryStoreService,
		&stateelements.TrajectoryStoreServiceValue{
			Store: fixture.store, SessionID: activationTestSession,
		}); err != nil {
		t.Fatal(err)
	}
	if _, err := services.Set(policyelements.SemanticDeciderRegistryService, semanticRegistry); err != nil {
		t.Fatal(err)
	}
	harness := &mountedSettlementHarness{fixture: fixture, decider: decider}
	if _, err := services.Set(cognitionelements.MediaResolverService,
		continuation.MediaResolver(func(handle string) (continuation.Media, error) {
			harness.mediaMu.Lock()
			harness.mediaIDs = append(harness.mediaIDs, handle)
			harness.mediaMu.Unlock()
			if handle != "settlement-frame" {
				return continuation.Media{}, fmt.Errorf("unexpected media handle %q", handle)
			}
			return continuation.Media{MIMEType: "image/png", Bytes: []byte{1, 2, 3, 4}}, nil
		})); err != nil {
		t.Fatal(err)
	}

	registry, err := elements.RuntimeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	registrations, err := ElementFactoryRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, registration := range registrations {
		if err := registry.RegisterFactory(registration); err != nil {
			t.Fatal(err)
		}
	}
	settlementConfig := mountedSettlementConfig()
	values := map[string]json.RawMessage{
		"settlement": mustMountedSettlementJSON(t, settlementConfig),
		"producer": mustMountedSettlementJSON(t, policyelements.IntentDispositionProducerConfig{
			ExpectedSettlement: settlementConfig, DirectVisualInput: true,
			MaxEvidenceBytes: 4096, MaxMediaBytes: 64, MaxMediaItems: 1,
			MaxPending: 4, TerminalMemory: 8, CancelMemory: 8,
		}),
		"activation": mustMountedSettlementJSON(t, ActivationConfig{
			GenerateOnObservationConfig: policyelements.GenerateOnObservationConfig{
				Role: "computer-use", Invocation: continuation.Invocation{
					Instruction: "act on admitted evidence", MaxOutputTokens: 64,
				}, TerminalMemory: 32, CancelMemory: 16,
			},
			ExpectedAdmission:  settlementConfig.ExpectedAdmission,
			ExpectedSettlement: &settlementConfig,
		}),
	}
	var clock atomic.Uint64
	clock.Store(1_000_000)
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: compileMountedSettlementGraph(t), Registry: registry,
		Services: services, Values: values,
		Now: func() uint64 { return clock.Add(10) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	harness.mounted, harness.done, harness.cancel = mounted, done, cancel
	t.Cleanup(func() { harness.stop(t) })
	return harness
}

func compileMountedSettlementGraph(t *testing.T) ir.Graph {
	t.Helper()
	catalog, err := elements.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterElementDescriptors(catalog); err != nil {
		t.Fatal(err)
	}
	parsed, err := syntax.Parse("realtime-cu-settlement-integration-test.ortg", []byte(mountedSettlementGraph))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}
	return compiled.Graph
}

func mountedSettlementConfig() policyelements.IntentSettlementConfig {
	return policyelements.IntentSettlementConfig{
		ExpectedAdmission: policyelements.TemporalEvidenceAdmissionConfig{
			Mode:      policyelements.TemporalEvidenceAdmissionAfterIntent,
			SourceSet: policyelements.TemporalEvidenceSourceSetObservedBeforeIntent,
		},
		CandidateSources: []policyelements.TemporalEvidenceRequirement{{
			Observer: "vision", Source: SourceScreen,
		}},
		Detector: mountedSettlementDetector, MaxTrackedIntents: 8, CancelMemory: 8,
	}
}

func mustMountedSettlementJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func mountedSettlementHasEdge(
	graph ir.Graph, fromNode, fromPort, toNode, toPort string,
) bool {
	for _, edge := range graph.Edges {
		if edge.From.Node == fromNode && edge.From.Port == fromPort &&
			edge.To.Node == toNode && edge.To.Port == toPort {
			return true
		}
	}
	return false
}

func (harness *mountedSettlementHarness) startTurn(t *testing.T) mountedSettlementTurn {
	t.Helper()
	intentEnvelope, intentCommit := harness.fixture.appendUser(
		t, "mounted-settlement-intent", "click the warning",
	)
	intent := intentEnvelope.Payload.(policyelements.AdmittedTemporalEvidence).TriggerObservation
	initial, initialCommit := harness.fixture.appendVisual(
		t, "mounted-settlement-initial-screen", "warning visible", intentCommit.TrajectoryItemID,
	)
	initial = afterIntentAdmissionEnvelope(initial, intent)
	harness.send(t, "evidence", initial)
	trigger := harness.receive(t, "trigger")
	payload, ok := trigger.Payload.(cognitionelements.Generate)
	if !ok || trigger.RunID == "" || payload.ExpectedContextVersion == nil ||
		*payload.ExpectedContextVersion != initialCommit.StoreVersion {
		t.Fatalf("initial settlement trigger = %+v payload=%+v", trigger, payload)
	}
	if outcome := harness.receiveSettlementOutcomeCode(t, "not_post_effect_candidate"); outcome.Kind != policyelements.IntentSettlementAdmitted {
		t.Fatalf("initial settlement admission = %+v", outcome)
	}
	return mountedSettlementTurn{
		intent: intent, intentItemID: intentCommit.TrajectoryItemID,
		runID: trigger.RunID, contextVersion: initialCommit.StoreVersion,
	}
}

func (harness *mountedSettlementHarness) appendSuccessfulConsequence(
	t *testing.T, turn mountedSettlementTurn, callID string, withMedia bool,
) element.Envelope {
	t.Helper()
	proposal := activationTestProposal(callID)
	snapshot := harness.fixture.store.Snapshot()
	if turn.contextVersion == 0 || turn.contextVersion > snapshot.Version {
		t.Fatalf("turn context version %d outside trajectory %d", turn.contextVersion, snapshot.Version)
	}
	contextTail := snapshot.Items[turn.contextVersion-1]
	call := proposal.Call
	call.Arguments = slices.Clone(proposal.Call.Arguments)
	proposalItem := trajectory.Item{
		ID: "mounted-proposal-" + callID, Kind: trajectory.KindToolProposal,
		MonotonicNS:     harness.fixture.nextMonotonicNS(),
		CausalParentIDs: []string{contextTail.ID}, SourceRevision: contextTail.SourceRevision,
		InvocationID: turn.runID, Producer: trajectory.Producer{Phase: trajectory.PhaseFast},
		ToolCall: &call,
	}
	harness.fixture.appendRaw(t, proposalItem)
	callItem := trajectory.Item{
		ID: "mounted-call-" + callID, Kind: trajectory.KindToolCall,
		MonotonicNS:     harness.fixture.nextMonotonicNS(),
		CausalParentIDs: []string{proposalItem.ID}, SourceRevision: proposalItem.SourceRevision,
		InvocationID: turn.runID, Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime},
		ToolCall: &call,
	}
	harness.fixture.appendRaw(t, callItem)
	resultItem := trajectory.Item{
		ID: "mounted-result-" + callID, Kind: trajectory.KindToolResult,
		MonotonicNS:     harness.fixture.nextMonotonicNS(),
		CausalParentIDs: []string{callItem.ID}, SourceRevision: proposalItem.SourceRevision,
		InvocationID: turn.runID, Producer: trajectory.Producer{Phase: trajectory.PhaseTool},
		ToolResult: &trajectory.ToolResult{
			CallID: call.CallID, Name: call.Name, Output: json.RawMessage(`{"ok":true}`),
		},
	}
	harness.fixture.appendRaw(t, resultItem)
	consequence := harness.fixture.visualItem(
		"mounted-consequence-"+callID, "warning dismissed", turn.intentItemID,
	)
	consequence.CausalParentIDs = []string{turn.intentItemID, resultItem.ID}
	if withMedia {
		consequence.Observation.Media = []trajectory.MediaRef{{
			Handle: "settlement-frame", MIMEType: "image/png", Source: SourceScreen,
			Width: 2, Height: 2, Bytes: 4, CapturedNS: consequence.Event.OccurredNS,
		}}
	}
	envelope, _ := harness.fixture.appendObservation(t, consequence, "screen-stream")
	return afterIntentAdmissionEnvelope(envelope, turn.intent)
}

func (harness *mountedSettlementHarness) send(
	t *testing.T, boundary string, envelope element.Envelope,
) {
	t.Helper()
	port, err := harness.mounted.Ingress(boundary)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := port.Broadcast(ctx, envelope)
	if err != nil {
		t.Fatalf("send %s: %v", boundary, err)
	}
	if result.Delivered != 1 || result.Dropped != 0 {
		t.Fatalf("send %s delivered %d and dropped %d", boundary, result.Delivered, result.Dropped)
	}
}

func (harness *mountedSettlementHarness) receive(t *testing.T, boundary string) element.Envelope {
	t.Helper()
	port, err := harness.mounted.Egress(boundary)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	envelope, err := port.Receive(ctx)
	if err != nil {
		select {
		case runErr := <-harness.done:
			t.Fatalf("receive %s after graph stopped: receive=%v run=%v", boundary, err, runErr)
		default:
		}
		t.Fatalf("receive %s: %v", boundary, err)
	}
	return envelope
}

func (harness *mountedSettlementHarness) receiveSettlementOutcomeCode(
	t *testing.T, code string,
) policyelements.IntentSettlementOutcome {
	t.Helper()
	for {
		envelope := harness.receive(t, "settlement_outcome")
		outcome, ok := envelope.Payload.(policyelements.IntentSettlementOutcome)
		if !ok {
			t.Fatalf("settlement outcome payload = %T", envelope.Payload)
		}
		if outcome.Code == code {
			return outcome
		}
		t.Logf("skip settlement outcome while waiting for %q: %+v", code, outcome)
	}
}

func (harness *mountedSettlementHarness) receiveActivationOutcomeCode(
	t *testing.T, code string,
) policyelements.GenerationOutcome {
	t.Helper()
	for {
		envelope := harness.receive(t, "activation_outcome")
		outcome, ok := envelope.Payload.(policyelements.GenerationOutcome)
		if !ok {
			t.Fatalf("activation outcome payload = %T", envelope.Payload)
		}
		if outcome.Code == code {
			return outcome
		}
		t.Logf("skip activation outcome while waiting for %q: %+v", code, outcome)
	}
}

func (harness *mountedSettlementHarness) assertNoEnvelope(
	t *testing.T, boundary string, duration time.Duration,
) {
	t.Helper()
	port, err := harness.mounted.Egress(boundary)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	envelope, err := port.Receive(ctx)
	if err == nil {
		t.Fatalf("unexpected %s envelope: %+v", boundary, envelope)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait for absent %s envelope: %v", boundary, err)
	}
	select {
	case runErr := <-harness.done:
		t.Fatalf("graph stopped while checking absent %s envelope: %v", boundary, runErr)
	default:
	}
}

func (harness *mountedSettlementHarness) assertExactMedia(t *testing.T, count int) {
	t.Helper()
	harness.mediaMu.Lock()
	mediaIDs := slices.Clone(harness.mediaIDs)
	harness.mediaMu.Unlock()
	if len(mediaIDs) != count {
		t.Fatalf("media resolver calls = %v, want %d exact calls", mediaIDs, count)
	}
	for _, handle := range mediaIDs {
		if handle != "settlement-frame" {
			t.Fatalf("media resolver handle = %q", handle)
		}
	}
	requests := harness.decider.snapshotRequests()
	if len(requests) != count {
		t.Fatalf("decider requests = %d, want %d", len(requests), count)
	}
	for _, request := range requests {
		if len(request.Images) != 1 || request.Images[0].MIMEType != "image/png" ||
			!reflect.DeepEqual(request.Images[0].Bytes, []byte{1, 2, 3, 4}) {
			t.Fatalf("decider exact visual input = %+v", request.Images)
		}
	}
}

func (harness *mountedSettlementHarness) stop(t *testing.T) {
	t.Helper()
	if harness.cancel == nil {
		return
	}
	harness.cancel()
	harness.cancel = nil
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer closeCancel()
	if err := harness.mounted.Close(closeCtx); err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("close mounted settlement graph: %v", err)
	}
	select {
	case err := <-harness.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("mounted settlement graph stopped with %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("mounted settlement graph did not stop")
	}
	if !harness.decider.closed.Load() {
		t.Error("mounted settlement graph did not close its independently owned decider")
	}
}
