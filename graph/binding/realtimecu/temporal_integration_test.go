package realtimecu

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const temporalActivationTestGraph = `graph realtime_cu_temporal_activation_test {
    policy.TemporalEvidenceAdmission :: evidence;
    policy.RealtimeComputerUseActivation :: activation;
    evidence.admitted -> activation.admitted;
    input committed = evidence.committed;
    input cancel = activation.cancel;
    input result = activation.result;
    input effect_terminal = activation.effect_terminal;
    input disposition_committed = activation.disposition_committed;
    input disposition_rejected = activation.disposition_rejected;
    output evidence_outcome = evidence.outcome;
    output trigger = activation.trigger;
    output authority = activation.authority;
    output state = activation.state;
    output activation_outcome = activation.outcome;
    output disposition_append = activation.disposition_append;
}
`

func TestMountedTemporalActivationRejectsStaleCameraDespiteFreshScreen(t *testing.T) {
	store := trajectory.NewStore()
	appendTemporalActivationItems(t, store,
		temporalActivationObserver("screen-before", "vision", SourceScreen, 10, 1),
		temporalActivationObserver("camera-before", "vision", SourceCamera, 11, 2),
		temporalActivationUser("intent", 20, 3),
	)
	harness := mountTemporalActivation(t, store, `{
		"mode":"after_intent","source_set":"observed_before_intent"
	}`)

	freshScreen := temporalActivationObserver("screen-after", "vision", SourceScreen, 30, 4, "intent")
	appendTemporalActivationItems(t, store, freshScreen)
	harness.sendCommit(t, store, freshScreen, 4)
	refused := harness.receiveEvidenceOutcome(t)
	if refused.Kind != policyelements.TemporalEvidenceAdmissionRefused ||
		refused.Code != "mismatched_evidence" || refused.RequiredObservations != 2 ||
		refused.QualifiedObservations != 1 {
		t.Fatalf("fresh-screen/stale-camera outcome = %+v", refused)
	}
	harness.assertNoTrigger(t)

	freshCamera := temporalActivationObserver("camera-after", "vision", SourceCamera, 31, 5, "intent")
	appendTemporalActivationItems(t, store, freshCamera)
	harness.sendCommit(t, store, freshCamera, 5)
	admitted := harness.receiveEvidenceOutcome(t)
	if admitted.Kind != policyelements.TemporalEvidenceAdmissionAdmitted ||
		admitted.RequiredObservations != 2 || admitted.QualifiedObservations != 2 {
		t.Fatalf("fresh camera outcome = %+v", admitted)
	}
	trigger := harness.receive(t, "trigger")
	if trigger.RunID == "" || trigger.CancellationScope != trigger.RunID ||
		!containsString(trigger.CausalParents, "screen-after") ||
		!containsString(trigger.CausalParents, "camera-after") ||
		!containsString(trigger.CausalParents, "intent") {
		t.Fatalf("temporally admitted trigger = %+v", trigger)
	}
}

func TestMountedTemporalActivationCancellationRevokesOldIntentButAllowsNewIntent(t *testing.T) {
	store := trajectory.NewStore()
	appendTemporalActivationItems(t, store,
		temporalActivationObserver("screen-before", "vision", SourceScreen, 10, 1),
		temporalActivationUser("old-intent", 20, 2),
	)
	harness := mountTemporalActivation(t, store, `{
		"mode":"after_intent","source_set":"observed_before_intent"
	}`)
	harness.send(t, "cancel", element.Envelope{
		Type: policyelements.GenerationCancelType(), ItemID: "cancel-old-intent",
		SessionID: activationTestSession, Sequence: 3,
		Payload: policyelements.GenerationCancel{
			StreamID: activationTestSession, Reason: "participant canceled",
		},
	})
	canceled := harness.receiveActivationOutcome(t)
	if canceled.Kind != policyelements.GenerationCanceled || canceled.Code != "intent_revoked" {
		t.Fatalf("cancellation outcome = %+v", canceled)
	}
	harness.receive(t, "state")

	oldFresh := temporalActivationObserver("old-intent-screen", "vision", SourceScreen, 30, 3, "old-intent")
	appendTemporalActivationItems(t, store, oldFresh)
	harness.sendCommit(t, store, oldFresh, 4)
	if outcome := harness.receiveEvidenceOutcome(t); outcome.Kind != policyelements.TemporalEvidenceAdmissionAdmitted {
		t.Fatalf("policy did not admit fresh old-intent evidence: %+v", outcome)
	}
	revoked := harness.receiveActivationOutcome(t)
	if revoked.Kind != policyelements.GenerationIgnored || revoked.Code != "intent_revoked" {
		t.Fatalf("post-cancel old-intent admission = %+v", revoked)
	}
	harness.receive(t, "state")
	harness.assertNoTrigger(t)

	newIntent := temporalActivationUser("new-intent", 40, 4)
	appendTemporalActivationItems(t, store, newIntent)
	harness.sendCommit(t, store, newIntent, 5)
	if outcome := harness.receiveEvidenceOutcome(t); outcome.Kind != policyelements.TemporalEvidenceAdmissionRefused ||
		outcome.Code != "stale_evidence" {
		t.Fatalf("new intent without refreshed screen = %+v", outcome)
	}
	newFresh := temporalActivationObserver("new-intent-screen", "vision", SourceScreen, 50, 5, "new-intent")
	appendTemporalActivationItems(t, store, newFresh)
	harness.sendCommit(t, store, newFresh, 6)
	if outcome := harness.receiveEvidenceOutcome(t); outcome.Kind != policyelements.TemporalEvidenceAdmissionAdmitted {
		t.Fatalf("new-intent evidence outcome = %+v", outcome)
	}
	trigger := harness.receive(t, "trigger")
	if !containsString(trigger.CausalParents, "new-intent") ||
		containsString(trigger.CausalParents, "old-intent") {
		t.Fatalf("new-intent trigger parents = %v", trigger.CausalParents)
	}
}

func TestMountedTemporalActivationImmediateModePreservesDirectComposition(t *testing.T) {
	store := trajectory.NewStore()
	user := temporalActivationUser("direct-intent", 0, 1)
	appendTemporalActivationItems(t, store, user)
	harness := mountTemporalActivation(t, store, `{"mode":"immediate"}`)
	harness.sendCommit(t, store, user, 1)
	if outcome := harness.receiveEvidenceOutcome(t); outcome.Kind != policyelements.TemporalEvidenceAdmissionAdmitted ||
		outcome.Mode != policyelements.TemporalEvidenceAdmissionImmediate {
		t.Fatalf("immediate evidence outcome = %+v", outcome)
	}
	trigger := harness.receive(t, "trigger")
	if trigger.RunID == "" || !containsString(trigger.CausalParents, "direct-intent") {
		t.Fatalf("immediate direct trigger = %+v", trigger)
	}
}

func TestObservationCommitTimestampsOnlyUntimedFinalUserIntent(t *testing.T) {
	for _, test := range []struct {
		name        string
		observation perception.Observation
		captureNS   uint64
		clockNS     uint64
		want        uint64
	}{
		{
			name: "typed final user fallback",
			observation: perception.Observation{
				Text: "typed task", Observer: textObserverName, Source: "text",
				Authority: trajectory.AuthorityUser, Revision: 1, Final: true,
			},
			want: 1,
		},
		{
			name: "positive user source time preserved",
			observation: perception.Observation{
				Text: "spoken task", Observer: "vision", Source: SourceMicrophone,
				Authority: trajectory.AuthorityUser, Revision: 1, Final: true, OccurredNS: 41,
			},
			clockNS: 99, want: 41,
		},
		{
			name: "capture time precedes commit fallback",
			observation: perception.Observation{
				Text: "typed task", Observer: textObserverName, Source: "text",
				Authority: trajectory.AuthorityUser, Revision: 1, Final: true,
			},
			captureNS: 17, clockNS: 99, want: 17,
		},
		{
			name: "observer remains untimed",
			observation: perception.Observation{
				Text: "screen", Observer: "vision", Source: SourceScreen,
				Authority: trajectory.AuthorityObserver, Revision: 1, Final: true,
			},
			clockNS: 99, want: 0,
		},
		{
			name: "provisional user remains untimed",
			observation: perception.Observation{
				Text: "speaking", Observer: "vision", Source: SourceMicrophone,
				Authority: trajectory.AuthorityUser, Revision: 1, Provisional: true,
			},
			clockNS: 99, want: 0,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			appendPort := &recordingOutputPort{name: "append", typeName: stateelements.AppendType()}
			runner := &observationCommitRunner{
				instance: "timestamp-test", namespace: "timestamp-test-revisions",
				clock:     graphruntime.ClockFunc(func() uint64 { return test.clockNS }),
				sequences: graphruntime.NewSequenceAllocator(), ports: observationCommitPorts{
					appendOutput: appendPort,
				},
				committed: make(map[string]map[uint64]committedObservation),
			}
			if err := runner.startObservation(context.Background(), queuedObservation{
				envelope: element.Envelope{
					ItemID: "timestamp-observation", SessionID: activationTestSession,
					CaptureNS: test.captureNS,
				},
				observation: test.observation, streamID: "timestamp-stream",
			}); err != nil {
				t.Fatal(err)
			}
			requests := appendPort.snapshot()
			if len(requests) != 1 {
				t.Fatalf("append requests = %d, want 1", len(requests))
			}
			request := requests[0].Payload.(stateelements.Append)
			if got := request.Items[0].Event.OccurredNS; got != test.want {
				t.Fatalf("committed occurrence time = %d, want %d", got, test.want)
			}
		})
	}
}

type temporalActivationHarness struct {
	mounted *graphruntime.Mounted
	done    <-chan error
}

func mountTemporalActivation(
	t *testing.T, store *trajectory.Store, evidenceConfig string,
) temporalActivationHarness {
	t.Helper()
	catalog, err := elements.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterElementDescriptors(catalog); err != nil {
		t.Fatal(err)
	}
	parsed, err := syntax.Parse("realtime-cu-temporal-test.ortg", []byte(temporalActivationTestGraph))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
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
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(stateelements.TrajectoryStoreService,
		&stateelements.TrajectoryStoreServiceValue{Store: store}); err != nil {
		t.Fatal(err)
	}
	values := map[string]json.RawMessage{
		"evidence": json.RawMessage(evidenceConfig),
		"activation": json.RawMessage(`{
			"role":"computer-use","invocation":{"instruction":"act on admitted evidence"},
			"terminal_memory":32,"cancel_memory":16
		}`),
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: compiled.Graph, Registry: registry, Services: services, Values: values,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	harness := temporalActivationHarness{mounted: mounted, done: done}
	// Activation publishes its initial state before receiving inputs. Draining it
	// here keeps the depth-one lossless state boundary from backpressuring the
	// first state transition exercised by a mounted test.
	harness.receive(t, "state")
	t.Cleanup(func() {
		cancel()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer closeCancel()
		if err := mounted.Close(closeCtx); err != nil {
			t.Errorf("close temporal activation graph: %v", err)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("temporal activation graph did not stop")
		}
	})
	return harness
}

func (harness temporalActivationHarness) send(
	t *testing.T, boundary string, envelope element.Envelope,
) {
	t.Helper()
	port, err := harness.mounted.Ingress(boundary)
	if err != nil {
		t.Fatal(err)
	}
	result, err := port.Broadcast(context.Background(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	if result.Delivered != 1 || result.Dropped != 0 {
		t.Fatalf("send %s delivered %d and dropped %d", boundary, result.Delivered, result.Dropped)
	}
}

func (harness temporalActivationHarness) sendCommit(
	t *testing.T, store *trajectory.Store, item trajectory.Item, sequence uint64,
) {
	t.Helper()
	snapshot := store.Snapshot()
	prefix, err := trajectory.IdentifyPrefix(snapshot, snapshot.Version)
	if err != nil {
		t.Fatal(err)
	}
	commit := stateelements.ObservationCommitOutcome{
		Kind: stateelements.ObservationCommitted, TriggerItemID: item.Event.EventID,
		TrajectoryItemID: item.ID, StreamID: item.Event.Source + ":" + item.Event.Channel,
		ObservationRevision: item.SourceRevision, SourceRevision: item.SourceRevision,
		StoreVersion: snapshot.Version,
		Context: stateelements.CommittedContext{
			Prefix: prefix, StateItemID: fmt.Sprintf("trajectory-state-%d", snapshot.Version),
		},
	}
	harness.send(t, "committed", element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: item.ID + "-commit",
		SessionID: activationTestSession, Sequence: sequence, Payload: commit,
	})
}

func (harness temporalActivationHarness) receive(
	t *testing.T, boundary string,
) element.Envelope {
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

func (harness temporalActivationHarness) receiveEvidenceOutcome(
	t *testing.T,
) policyelements.TemporalEvidenceAdmissionOutcome {
	t.Helper()
	envelope := harness.receive(t, "evidence_outcome")
	outcome, ok := envelope.Payload.(policyelements.TemporalEvidenceAdmissionOutcome)
	if !ok {
		t.Fatalf("evidence outcome payload = %T", envelope.Payload)
	}
	return outcome
}

func (harness temporalActivationHarness) receiveActivationOutcome(
	t *testing.T,
) policyelements.GenerationOutcome {
	t.Helper()
	envelope := harness.receive(t, "activation_outcome")
	outcome, ok := envelope.Payload.(policyelements.GenerationOutcome)
	if !ok {
		t.Fatalf("activation outcome payload = %T", envelope.Payload)
	}
	return outcome
}

func (harness temporalActivationHarness) assertNoTrigger(t *testing.T) {
	t.Helper()
	port, err := harness.mounted.Egress("trigger")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if envelope, err := port.Receive(ctx); err == nil {
		t.Fatalf("unexpected activation trigger: %+v", envelope)
	}
}

func appendTemporalActivationItems(
	t *testing.T, store *trajectory.Store, items ...trajectory.Item,
) {
	t.Helper()
	if err := store.AppendBatch(items); err != nil {
		t.Fatal(err)
	}
}

func temporalActivationObserver(
	id, observer, source string, occurredNS, revision uint64, parents ...string,
) trajectory.Item {
	return trajectory.Item{
		ID: id, Kind: trajectory.KindObservation, MonotonicNS: revision,
		CausalParentIDs: append([]string(nil), parents...), SourceRevision: revision,
		Producer: trajectory.Producer{Phase: trajectory.PhaseObserver, Provider: observer},
		Content:  observer + " observed " + source,
		Observation: &trajectory.ObservationMeta{
			Observer: observer, Source: source, Authority: trajectory.AuthorityObserver,
		},
		Event: &trajectory.EventMetadata{
			EventID: id + "-event", Type: observer + ".endpoint",
			Source: observer, Channel: source, OccurredNS: occurredNS,
		},
	}
}

func temporalActivationUser(id string, occurredNS, revision uint64) trajectory.Item {
	return trajectory.Item{
		ID: id, Kind: trajectory.KindObservation, MonotonicNS: revision,
		SourceRevision: revision, Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
		Content: "durable user intent",
		Event: &trajectory.EventMetadata{
			EventID: id + "-event", Type: "participant.endpoint",
			Source: "participant", Channel: SourceMicrophone, OccurredNS: occurredNS,
		},
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
