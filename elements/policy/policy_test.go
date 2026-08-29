package policy_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const generationPolicyGraph = `graph generation_policy_test {
    policy.GenerateOnObservation :: activation;
    input context = activation.context;
    input committed = activation.committed;
    input cancel = activation.cancel;
    output trigger = activation.trigger;
    output state = activation.state;
    output outcome = activation.outcome;
}
`

func TestGenerateOnObservationDescriptorMakesActivationJoinExplicit(t *testing.T) {
	descriptor := policyelements.GenerateOnObservationDescriptor()
	if err := descriptor.Validate(); err != nil {
		t.Fatal(err)
	}
	wantPorts := map[string]string{
		"context":   "State<trajectory.Snapshot>",
		"committed": "Event<trajectory.ObservationCommitOutcome>",
		"cancel":    "Interrupt<policy.GenerationAddress>",
		"trigger":   "Trigger<cognition.Generate>",
		"state":     "State<policy.GenerationState>",
		"outcome":   "Event<policy.GenerationOutcome>",
	}
	for name, want := range wantPorts {
		port, found := descriptor.Port(name)
		if !found || port.Type.String() != want {
			t.Fatalf("port %s = %+v, want %s", name, port, want)
		}
	}
	if !reflect.DeepEqual(descriptor.Reaction.Triggers, []string{"context", "committed"}) ||
		!reflect.DeepEqual(descriptor.Reaction.Interrupts, []string{"cancel"}) ||
		!descriptor.Reaction.BreaksCycles || descriptor.Reaction.MaxConcurrency != 1 {
		t.Fatalf("reaction = %+v", descriptor.Reaction)
	}
}

func TestGenerateOnObservationWaitsForExactCommittedContext(t *testing.T) {
	harness := mountPolicy(t, validPolicyConfig("fast"))
	defer harness.stop(t)
	startup := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.GenerationState)
	if startup.Role != "fast" || startup.ContextVersion != 0 || len(startup.Pending) != 0 {
		t.Fatalf("startup state = %+v", startup)
	}

	commit := committedObservation("microphone", "observation-trigger", "trajectory-observation-7", 1, 7)
	sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-envelope", Payload: commit,
	})
	pending := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.GenerationState)
	if len(pending.Pending) != 1 || pending.Pending[0].StreamID != "microphone" ||
		pending.Pending[0].ContextVersion != 1 {
		t.Fatalf("pending state = %+v", pending)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "trigger"))

	sendPolicy(t, harness.ingress(t, "context"), contextEnvelope("context-version-1", commit, true))
	triggerEnvelope := receivePolicy(t, harness.egress(t, "trigger"))
	payload, ok := triggerEnvelope.Payload.(cognitionelements.Generate)
	if !ok {
		t.Fatalf("trigger payload type = %T", triggerEnvelope.Payload)
	}
	version := payload.ExpectedContextVersion
	if version == nil || *version != 1 || payload.ExpectedContextItemID != "context-version-1" ||
		payload.Invocation.SourceRevision != 7 || payload.Invocation.Instruction != "answer" ||
		triggerEnvelope.RunID == "" || triggerEnvelope.CancellationScope != triggerEnvelope.RunID ||
		!strings.HasSuffix(triggerEnvelope.ItemID, ":trigger") ||
		!containsPolicy(triggerEnvelope.CausalParents, "commit-envelope") ||
		!containsPolicy(triggerEnvelope.CausalParents, "context-version-1") {
		t.Fatalf("generation trigger = %+v payload %+v", triggerEnvelope, payload)
	}
	outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.GenerationOutcome)
	if outcome.Kind != policyelements.GenerationEmitted || outcome.GenerationID != triggerEnvelope.RunID ||
		outcome.Role != "fast" || outcome.ContextVersion != 1 || outcome.SourceRevision != 7 ||
		outcome.TriggerItemID != "observation-trigger" {
		t.Fatalf("generation outcome = %+v", outcome)
	}
	settled := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.GenerationState)
	if settled.Emitted != 1 || len(settled.Pending) != 0 || settled.ContextVersion != 1 {
		t.Fatalf("settled state = %+v", settled)
	}
	assertPolicyLiveResolution(t, harness.mounted)
}

func TestGenerateOnObservationRejectsForgedSameLengthContext(t *testing.T) {
	harness := mountPolicy(t, validPolicyConfig("deliberative"))
	defer harness.stop(t)
	_ = receivePolicy(t, harness.egress(t, "state"))
	commit := committedObservation("camera", "frame-trigger", "real-trajectory-item", 1, 11)
	sendPolicy(t, harness.ingress(t, "context"), contextEnvelope("context-one", commit, false))
	_ = receivePolicy(t, harness.egress(t, "state"))
	sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-forged-context", Payload: commit,
	})
	outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.GenerationOutcome)
	if outcome.Kind != policyelements.GenerationRefused || outcome.Code != "context_commit_mismatch" ||
		!strings.Contains(outcome.Message, "does not match committed trajectory item") {
		t.Fatalf("forged context outcome = %+v", outcome)
	}
	state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.GenerationState)
	if state.Refused != 1 || len(state.Pending) != 0 {
		t.Fatalf("forged context state = %+v", state)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "trigger"))
}

func TestGenerateOnObservationRetainsOnlyImmutableContextAttestation(t *testing.T) {
	harness := mountPolicy(t, validPolicyConfig("fast"))
	defer harness.stop(t)
	_ = receivePolicy(t, harness.egress(t, "state"))
	commit := committedObservation("microphone", "observation-trigger", "trajectory-item", 1, 13)
	snapshot := trajectory.Snapshot{Version: 1, Items: []trajectory.Item{{
		ID: commit.TrajectoryItemID, Kind: trajectory.KindObservation,
		SourceRevision: commit.SourceRevision,
		Event:          &trajectory.EventMetadata{EventID: commit.TriggerItemID},
	}}}
	sendPolicy(t, harness.ingress(t, "context"), element.Envelope{
		Type: stateelements.SnapshotType(), ItemID: "context-stable", Payload: snapshot,
	})
	_ = receivePolicy(t, harness.egress(t, "state"))
	// Acceptance is complete. Mutating a hostile producer's retained slice must
	// neither race with nor change the policy's already sampled attestation.
	snapshot.Items[0].ID = "mutated"
	snapshot.Items[0].SourceRevision = 999
	snapshot.Items[0].Event.EventID = "mutated"
	sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-stable", Payload: commit,
	})
	trigger := receivePolicy(t, harness.egress(t, "trigger"))
	if trigger.RunID == "" {
		t.Fatalf("stable context trigger = %+v", trigger)
	}
	outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.GenerationOutcome)
	if outcome.Kind != policyelements.GenerationEmitted {
		t.Fatalf("stable context outcome = %+v", outcome)
	}
	_ = receivePolicy(t, harness.egress(t, "state"))
}

func TestGenerateOnObservationHonorsPreCancellationAndIdentityDrift(t *testing.T) {
	t.Run("pre-cancellation", func(t *testing.T) {
		harness := mountPolicy(t, validPolicyConfig("fast"))
		defer harness.stop(t)
		_ = receivePolicy(t, harness.egress(t, "state"))
		sendPolicy(t, harness.ingress(t, "cancel"), element.Envelope{
			Type: policyelements.GenerationCancelType(), ItemID: "cancel-before-commit",
			Payload: policyelements.GenerationCancel{StreamID: "microphone", Reason: "barge-in"},
		})
		recorded := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.GenerationOutcome)
		if recorded.Kind != policyelements.GenerationIgnored || recorded.Code != "cancel_recorded" {
			t.Fatalf("recorded cancel = %+v", recorded)
		}
		_ = receivePolicy(t, harness.egress(t, "state"))
		commit := committedObservation("microphone", "observation-trigger", "trajectory-item", 1, 3)
		sendPolicy(t, harness.ingress(t, "context"), contextEnvelope("context-one", commit, true))
		_ = receivePolicy(t, harness.egress(t, "state"))
		sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
			Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-after-cancel", Payload: commit,
		})
		canceled := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.GenerationOutcome)
		if canceled.Kind != policyelements.GenerationCanceled || canceled.Message != "barge-in" {
			t.Fatalf("pre-canceled outcome = %+v", canceled)
		}
		state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.GenerationState)
		if state.Canceled != 1 || len(state.Pending) != 0 {
			t.Fatalf("pre-canceled state = %+v", state)
		}
		assertNoPolicyEnvelope(t, harness.egress(t, "trigger"))
	})

	t.Run("same-version identity drift", func(t *testing.T) {
		harness := mountPolicy(t, validPolicyConfig("fast"))
		defer harness.stop(t)
		_ = receivePolicy(t, harness.egress(t, "state"))
		empty := element.Envelope{
			Type: stateelements.SnapshotType(), ItemID: "empty-context-a", Payload: trajectory.Snapshot{},
		}
		sendPolicy(t, harness.ingress(t, "context"), empty)
		_ = receivePolicy(t, harness.egress(t, "state"))
		empty.ItemID = "empty-context-b"
		sendPolicy(t, harness.ingress(t, "context"), empty)
		outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.GenerationOutcome)
		if outcome.Kind != policyelements.GenerationRefused || outcome.Code != "context_identity_drift" {
			t.Fatalf("identity drift outcome = %+v", outcome)
		}
		_ = receivePolicy(t, harness.egress(t, "state"))
	})
}

func TestGenerateOnObservationConfigIsStrictAndBounded(t *testing.T) {
	graph := compilePolicySource(t, "policy-config.ortg", []byte(generationPolicyGraph))
	for name, value := range map[string]json.RawMessage{
		"missing role":      json.RawMessage(`{"invocation":{"instruction":"answer"}}`),
		"unknown field":     json.RawMessage(`{"role":"fast","invocation":{"instruction":"answer"},"mystery":true}`),
		"derived revision":  json.RawMessage(`{"role":"fast","invocation":{"instruction":"answer","source_revision":9}}`),
		"empty instruction": json.RawMessage(`{"role":"fast","invocation":{"instruction":""}}`),
		"unbounded pending": json.RawMessage(`{"role":"fast","invocation":{"instruction":"answer"},"max_pending":1000001}`),
	} {
		t.Run(name, func(t *testing.T) {
			registry := graphruntime.NewRegistry()
			if err := policyelements.RegisterFactories(registry); err != nil {
				t.Fatal(err)
			}
			if _, err := graphruntime.Mount(context.Background(), graphruntime.Config{
				Graph: graph, Registry: registry,
				Values: map[string]json.RawMessage{"activation": value},
			}); err == nil {
				t.Fatal("invalid policy config mounted")
			}
		})
	}
}

func TestGenerationPolicyReferenceCompilesFromExactLock(t *testing.T) {
	directory := filepath.Join("..", "..", "graphs", "components", "generation-policy")
	topology, err := os.ReadFile(filepath.Join(directory, "agent.ortg"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := syntax.Parse("agent.ortg", topology)
	if err != nil {
		t.Fatal(err)
	}
	catalog := resolve.NewCatalog()
	if err := policyelements.RegisterDescriptors(catalog); err != nil {
		t.Fatal(err)
	}
	updated, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}
	lockPayload, err := os.ReadFile(filepath.Join(directory, "openrealtime.lock"))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := resolve.ParseLock(lockPayload)
	if err != nil {
		t.Fatal(err)
	}
	if !lock.Equal(updated.Lock) {
		want, marshalErr := updated.Lock.Marshal()
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		t.Fatalf("generation-policy lock is stale; generated lock:\n%s", want)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, Lock: lock, ResolutionMode: resolve.Locked,
	})
	if err != nil {
		t.Fatal(err)
	}
	valuesPayload, err := os.ReadFile(filepath.Join(directory, "agent.values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	document, err := graphvalues.ParseYAML("agent.values.yaml", valuesPayload)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := graphvalues.Bind(compiled.Graph, document)
	if err != nil {
		t.Fatal(err)
	}
	for _, boundary := range bound.Graph.Boundaries {
		if strings.Contains(boundary.Type.String(), "audio.") {
			t.Fatalf("generation policy unexpectedly requires audio: %+v", boundary)
		}
	}
}

type policyHarness struct {
	mounted *graphruntime.Mounted
	done    <-chan error
	cancel  context.CancelFunc
}

func mountPolicy(t *testing.T, config json.RawMessage) policyHarness {
	t.Helper()
	registry := graphruntime.NewRegistry()
	if err := policyelements.RegisterFactories(registry); err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph:    compilePolicySource(t, "policy-test.ortg", []byte(generationPolicyGraph)),
		Registry: registry, Values: map[string]json.RawMessage{"activation": config},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	return policyHarness{mounted: mounted, done: done, cancel: cancel}
}

func (harness policyHarness) ingress(t *testing.T, name string) element.OutputPort {
	t.Helper()
	port, err := harness.mounted.Ingress(name)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func (harness policyHarness) egress(t *testing.T, name string) element.InputPort {
	t.Helper()
	port, err := harness.mounted.Egress(name)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func (harness policyHarness) stop(t *testing.T) {
	t.Helper()
	harness.cancel()
	select {
	case err := <-harness.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("generation policy stopped with %v", err)
		}
	case <-time.After(time.Second):
		t.Error("generation policy did not stop")
	}
}

func compilePolicySource(t *testing.T, name string, source []byte) ir.Graph {
	t.Helper()
	parsed, err := syntax.Parse(name, source)
	if err != nil {
		t.Fatal(err)
	}
	catalog := resolve.NewCatalog()
	if err := policyelements.RegisterDescriptors(catalog); err != nil {
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

func validPolicyConfig(role string) json.RawMessage {
	payload, err := json.Marshal(policyelements.GenerateOnObservationConfig{
		Role: role, Invocation: continuation.Invocation{
			Instruction: "answer", MaxOutputTokens: 128,
		},
		MaxPending: 4, TerminalMemory: 8, CancelMemory: 8,
	})
	if err != nil {
		panic(err)
	}
	return payload
}

func committedObservation(
	stream, trigger, item string, version, sourceRevision uint64,
) stateelements.ObservationCommitOutcome {
	return stateelements.ObservationCommitOutcome{
		Kind: stateelements.ObservationCommitted, TriggerItemID: trigger,
		TrajectoryItemID: item, StreamID: stream, ObservationRevision: 1,
		SourceRevision: sourceRevision, StoreVersion: version,
	}
}

func contextEnvelope(
	itemID string, commit stateelements.ObservationCommitOutcome, authentic bool,
) element.Envelope {
	trajectoryItemID := commit.TrajectoryItemID
	if !authentic {
		trajectoryItemID = "forged-trajectory-item"
	}
	items := make([]trajectory.Item, commit.StoreVersion)
	items[commit.StoreVersion-1] = trajectory.Item{
		ID: trajectoryItemID, Kind: trajectory.KindObservation,
		SourceRevision: commit.SourceRevision,
		Event:          &trajectory.EventMetadata{EventID: commit.TriggerItemID},
	}
	return element.Envelope{
		Type: stateelements.SnapshotType(), ItemID: itemID,
		Payload: trajectory.Snapshot{Version: commit.StoreVersion, Items: items},
	}
}

func sendPolicy(t *testing.T, output element.OutputPort, envelope element.Envelope) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := output.Broadcast(ctx, envelope); err != nil {
		t.Fatal(err)
	}
}

func receivePolicy(t *testing.T, input element.InputPort) element.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	envelope, err := input.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func assertNoPolicyEnvelope(t *testing.T, input element.InputPort) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if envelope, err := input.Receive(ctx); err == nil {
		t.Fatalf("unexpected envelope: %+v", envelope)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("empty receive: %v", err)
	}
}

func assertPolicyLiveResolution(t *testing.T, mounted *graphruntime.Mounted) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		resolution := mounted.Live().Nodes["activation"].Resolution
		if resolution != nil && resolution.RuntimeEvidence == inspect.EvidenceLive &&
			resolution.Runtime.ID == "builtin://openrealtime/elements/policy.GenerateOnObservation" &&
			resolution.Runtime.Revision == "implementation:1" &&
			resolution.CapabilitiesEvidence == inspect.EvidenceLive && len(resolution.Capabilities) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("policy live resolution = %+v", resolution)
		}
		time.Sleep(time.Millisecond)
	}
}

func containsPolicy(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
