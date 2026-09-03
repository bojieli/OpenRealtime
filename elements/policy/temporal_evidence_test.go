package policy_test

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const temporalEvidenceGraph = `graph temporal_evidence_test {
    policy.TemporalEvidenceAdmission :: admission;
    input committed = admission.committed;
    output admitted = admission.admitted;
    output outcome = admission.outcome;
}
`

func TestTemporalEvidenceAdmissionContractAndConfigAreExplicit(t *testing.T) {
	descriptor := policyelements.TemporalEvidenceAdmissionDescriptor()
	if err := descriptor.Validate(); err != nil {
		t.Fatal(err)
	}
	if descriptor.Name != "policy.TemporalEvidenceAdmission" || descriptor.Revision != 1 ||
		descriptor.ConfigSchema != "schema://openrealtime/policy/temporal-evidence-admission-config/v1" {
		t.Fatalf("temporal evidence descriptor = %+v", descriptor)
	}
	wantPorts := map[string]string{
		"committed": "Event<trajectory.ObservationCommitOutcome>",
		"admitted":  "Stream<policy.AdmittedTemporalEvidence>",
		"outcome":   "Event<policy.TemporalEvidenceAdmissionOutcome>",
	}
	for name, want := range wantPorts {
		port, found := descriptor.Port(name)
		if !found || port.Type.String() != want || port.LossAllowed {
			t.Fatalf("temporal evidence port %s = %+v, want lossless %s", name, port, want)
		}
	}
	if !reflect.DeepEqual(descriptor.Reaction.Triggers, []string{"committed"}) ||
		!reflect.DeepEqual(descriptor.Reaction.Outcomes, []string{"admitted", "outcome"}) ||
		descriptor.Reaction.MaxConcurrency != 1 {
		t.Fatalf("temporal evidence reaction = %+v", descriptor.Reaction)
	}
	if len(descriptor.Dependencies) != 3 || descriptor.Dependencies[0].Name != stateelements.TrajectoryStoreService {
		t.Fatalf("temporal evidence dependencies = %+v", descriptor.Dependencies)
	}

	registrations, err := policyelements.FactoryRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	var validator element.ConfigValidator
	for _, registration := range registrations {
		if registration.Profile.Reference != descriptor.Name {
			continue
		}
		if registration.Profile.Artifact.ID !=
			"builtin://openrealtime/elements/policy.TemporalEvidenceAdmission" ||
			registration.Profile.Artifact.Revision != "implementation:1" {
			t.Fatalf("temporal evidence registration = %+v", registration.Profile)
		}
		validator = registration.Factory.(element.ConfigValidator)
	}
	if validator == nil {
		t.Fatal("temporal evidence factory is absent from the built-in registry")
	}
	for _, source := range []string{
		`{"mode":"immediate"}`,
		`{"mode":"after_intent","required":[{"observer":"vision","source":"camera"}]}`,
		`{"mode":"after_intent","source_set":"explicit","required":[{"observer":"vision","source":"camera"}]}`,
		`{"mode":"after_intent","source_set":"observed_before_intent"}`,
	} {
		if err := validator.ValidateConfig(json.RawMessage(source)); err != nil {
			t.Errorf("valid config %s: %v", source, err)
		}
	}
	for _, source := range []string{
		`{}`,
		`{"mode":"later"}`,
		`{"mode":"immediate","required":[{"observer":"vision","source":"camera"}]}`,
		`{"mode":"after_intent"}`,
		`{"mode":"after_intent","source_set":"observed_before_intent","required":[{"observer":"vision","source":"camera"}]}`,
		`{"mode":"after_intent","required":[{"observer":"vision","source":"camera"},{"observer":"vision","source":"camera"}]}`,
		`{"mode":"after_intent","required":[{"observer":"","source":"camera"}]}`,
		`{"mode":"immediate","unknown":true}`,
	} {
		if err := validator.ValidateConfig(json.RawMessage(source)); err == nil {
			t.Errorf("invalid config was accepted: %s", source)
		}
	}
}

func TestTemporalEvidenceAdmissionImmediateStillAttestsCanonicalTrigger(t *testing.T) {
	store := trajectory.NewStore()
	appendTemporalItems(t, store, temporalObserver(
		"camera-before-intent", "camera-event", "vision", "camera", 10, 1,
	))
	// A later append must not invalidate an exact immutable historical prefix.
	commit := temporalCommit(t, store, 1)
	appendTemporalItems(t, store, temporalInstruction("later-runtime-item", 2))
	harness := mountTemporalEvidence(t, store, `{"mode":"immediate"}`)
	defer harness.stop(t)

	sendPolicy(t, harness.ingress(t, "committed"), temporalCommitEnvelope("immediate-commit", commit))
	admittedEnvelope := receivePolicy(t, harness.egress(t, "admitted"))
	admitted, ok := admittedEnvelope.Payload.(policyelements.AdmittedTemporalEvidence)
	if !ok {
		t.Fatalf("immediate admitted payload = %T", admittedEnvelope.Payload)
	}
	if admitted.Mode != policyelements.TemporalEvidenceAdmissionImmediate ||
		admitted.DurableIntent != nil || admitted.QualifyingObservations != nil ||
		!reflect.DeepEqual(admitted.TriggerCommit, commit) || admitted.Prefix != commit.Context.Prefix ||
		admitted.TriggerObservation.TrajectoryItemID != "camera-before-intent" ||
		admitted.TriggerObservation.Observer != "vision" ||
		admitted.TriggerObservation.Source != "camera" ||
		admitted.TriggerObservation.StoreVersion != 1 {
		t.Fatalf("immediate admission = %+v", admitted)
	}
	for _, parent := range []string{
		"immediate-commit", commit.Context.StateItemID, commit.TrajectoryItemID,
	} {
		if !slices.Contains(admittedEnvelope.CausalParents, parent) {
			t.Errorf("immediate admission lacks causal parent %q: %v", parent, admittedEnvelope.CausalParents)
		}
	}
	outcome := temporalOutcome(t, harness)
	if outcome.Kind != policyelements.TemporalEvidenceAdmissionAdmitted || outcome.Code != "admitted" {
		t.Fatalf("immediate outcome = %+v", outcome)
	}
	assertTemporalEvidenceLiveResolution(t, harness.mounted)
}

func TestTemporalEvidenceAdmissionExplicitPairFailsClosed(t *testing.T) {
	config := `{"mode":"after_intent","source_set":"explicit","required":[{"observer":"vision","source":"camera"}]}`
	tests := []struct {
		name       string
		post       []trajectory.Item
		wantCode   string
		admitted   bool
		withoutPre bool
	}{
		{
			name: "fresh exact camera", admitted: true,
			post: []trajectory.Item{temporalObserver(
				"fresh-camera", "fresh-camera-event", "vision", "camera", 110, 3, "intent",
			)},
		},
		{
			name: "fresh screen cannot satisfy camera", wantCode: "mismatched_evidence",
			post: []trajectory.Item{temporalObserver(
				"fresh-screen", "fresh-screen-event", "vision", "screen", 110, 3, "intent",
			)},
		},
		{
			name: "post-commit stale capture", wantCode: "stale_evidence",
			post: []trajectory.Item{temporalObserver(
				"stale-camera", "stale-camera-event", "vision", "camera", 99, 3, "intent",
			)},
		},
		{
			name: "zero source time", wantCode: "stale_evidence",
			post: []trajectory.Item{temporalObserver(
				"untimed-camera", "untimed-camera-event", "vision", "camera", 0, 3, "intent",
			)},
		},
		{
			name: "wrong observer cannot satisfy source", wantCode: "mismatched_evidence",
			post: []trajectory.Item{temporalObserver(
				"wrong-observer-camera", "wrong-observer-event", "other-vision", "camera", 110, 3, "intent",
			)},
		},
		{
			name: "missing observer and source", wantCode: "missing_evidence", withoutPre: true,
			post: []trajectory.Item{temporalObserver(
				"unrelated-observation", "unrelated-event", "other-vision", "screen", 110, 3, "intent",
			)},
		},
		{
			name: "noncausal camera cannot satisfy", wantCode: "evidence_not_causal",
			post: []trajectory.Item{
				temporalObserver("noncausal-camera", "noncausal-camera-event", "vision", "camera", 110, 3),
				temporalObserver("causal-screen", "causal-screen-event", "vision", "screen", 111, 4, "intent"),
			},
		},
		{
			name: "newest malformed pair shadows older valid pair", wantCode: "mismatched_evidence",
			post: []trajectory.Item{
				temporalObserver("older-valid-camera", "older-valid-event", "vision", "camera", 110, 3, "intent"),
				func() trajectory.Item {
					item := temporalObserver("newer-malformed-camera", "newer-malformed-event", "vision", "camera", 111, 4, "intent")
					item.Event.Source = "aliased-observer"
					return item
				}(),
				temporalObserver("latest-screen", "latest-screen-event", "vision", "screen", 112, 5, "intent"),
			},
		},
		{
			name: "newest stale pair shadows older valid pair", wantCode: "stale_evidence",
			post: []trajectory.Item{
				temporalObserver("older-valid-camera", "older-valid-event", "vision", "camera", 110, 3, "intent"),
				temporalObserver("newer-stale-camera", "newer-stale-event", "vision", "camera", 90, 4, "intent"),
				temporalObserver("latest-screen", "latest-screen-event", "vision", "screen", 112, 5, "intent"),
			},
		},
		{
			name: "producer alias cannot claim observer", wantCode: "mismatched_evidence",
			post: []trajectory.Item{
				func() trajectory.Item {
					item := temporalObserver("aliased-camera", "aliased-camera-event", "vision", "camera", 110, 3, "intent")
					item.Producer.Provider = "other-vision"
					return item
				}(),
				temporalObserver("latest-screen", "latest-screen-event", "vision", "screen", 112, 4, "intent"),
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			store := trajectory.NewStore()
			if testCase.withoutPre {
				appendTemporalItems(t, store, temporalIntent("intent", "intent-event", 100, 2))
			} else {
				appendTemporalItems(t, store,
					temporalObserver("old-camera", "old-camera-event", "vision", "camera", 50, 1),
					temporalIntent("intent", "intent-event", 100, 2),
				)
			}
			appendTemporalItems(t, store, testCase.post...)
			harness := mountTemporalEvidence(t, store, config)
			defer harness.stop(t)
			commit := temporalCommit(t, store, store.Snapshot().Version)
			sendPolicy(t, harness.ingress(t, "committed"), temporalCommitEnvelope("explicit-commit", commit))
			if !testCase.admitted {
				outcome := temporalOutcome(t, harness)
				if outcome.Kind != policyelements.TemporalEvidenceAdmissionRefused ||
					outcome.Code != testCase.wantCode || outcome.DurableIntentItemID != "intent" ||
					outcome.RequiredObservations != 1 {
					t.Fatalf("explicit refusal = %+v", outcome)
				}
				assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))
				return
			}
			admitted := receivePolicy(t, harness.egress(t, "admitted")).Payload.(policyelements.AdmittedTemporalEvidence)
			if admitted.DurableIntent == nil || admitted.DurableIntent.TrajectoryItemID != "intent" ||
				admitted.DurableIntent.StoreVersion != 2 || admitted.DurableIntent.OccurredNS != 100 ||
				len(admitted.QualifyingObservations) != 1 ||
				admitted.QualifyingObservations[0].TrajectoryItemID != "fresh-camera" ||
				admitted.QualifyingObservations[0].StoreVersion != 3 ||
				admitted.QualifyingObservations[0].OccurredNS != 110 ||
				admitted.QualifyingObservations[0].Observer != "vision" ||
				admitted.QualifyingObservations[0].Source != "camera" {
				t.Fatalf("explicit admission = %+v", admitted)
			}
			outcome := temporalOutcome(t, harness)
			if outcome.Kind != policyelements.TemporalEvidenceAdmissionAdmitted ||
				outcome.RequiredObservations != 1 || outcome.QualifiedObservations != 1 {
				t.Fatalf("explicit admitted outcome = %+v", outcome)
			}
		})
	}
}

func TestTemporalEvidenceAdmissionObservedBeforeIntentFreezesAndRefreshesEveryPair(t *testing.T) {
	store := trajectory.NewStore()
	appendTemporalItems(t, store,
		temporalObserver("old-screen", "old-screen-event", "vision", "screen", 40, 1),
		temporalObserver("old-camera", "old-camera-event", "vision", "camera", 50, 2),
		temporalIntent("intent", "intent-event", 100, 3),
		temporalObserver("fresh-screen", "fresh-screen-event", "vision", "screen", 110, 4, "intent"),
		temporalObserver("delayed-old-camera", "delayed-old-camera-event", "vision", "camera", 90, 5, "intent"),
	)
	harness := mountTemporalEvidence(t, store,
		`{"mode":"after_intent","source_set":"observed_before_intent"}`)
	defer harness.stop(t)

	stale := temporalCommit(t, store, 5)
	sendPolicy(t, harness.ingress(t, "committed"), temporalCommitEnvelope("dynamic-stale", stale))
	outcome := temporalOutcome(t, harness)
	if outcome.Kind != policyelements.TemporalEvidenceAdmissionRefused ||
		outcome.Code != "stale_evidence" || outcome.RequiredObservations != 2 ||
		outcome.QualifiedObservations != 1 {
		t.Fatalf("dynamic stale outcome = %+v", outcome)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))

	appendTemporalItems(t, store,
		temporalObserver("fresh-camera", "fresh-camera-event", "vision", "camera", 120, 6, "intent"),
	)
	fresh := temporalCommit(t, store, 6)
	sendPolicy(t, harness.ingress(t, "committed"), temporalCommitEnvelope("dynamic-fresh", fresh))
	admitted := receivePolicy(t, harness.egress(t, "admitted")).Payload.(policyelements.AdmittedTemporalEvidence)
	if admitted.SourceSet != policyelements.TemporalEvidenceSourceSetObservedBeforeIntent ||
		admitted.DurableIntent == nil || admitted.DurableIntent.TrajectoryItemID != "intent" ||
		len(admitted.QualifyingObservations) != 2 {
		t.Fatalf("dynamic admission = %+v", admitted)
	}
	wantIDs := []string{"fresh-screen", "fresh-camera"}
	gotIDs := []string{
		admitted.QualifyingObservations[0].TrajectoryItemID,
		admitted.QualifyingObservations[1].TrajectoryItemID,
	}
	if !slices.Equal(gotIDs, wantIDs) || slices.Contains(gotIDs, "old-screen") ||
		slices.Contains(gotIDs, "old-camera") || gotIDs[0] == gotIDs[1] ||
		slices.Contains(gotIDs, admitted.DurableIntent.TrajectoryItemID) {
		t.Fatalf("dynamic qualifying identities = %v", gotIDs)
	}
	outcome = temporalOutcome(t, harness)
	if outcome.Kind != policyelements.TemporalEvidenceAdmissionAdmitted ||
		outcome.RequiredObservations != 2 || outcome.QualifiedObservations != 2 {
		t.Fatalf("dynamic outcome = %+v", outcome)
	}
}

func TestTemporalEvidenceAdmissionDynamicDiscoveryIsScopedToCurrentIntentCohort(t *testing.T) {
	store := trajectory.NewStore()
	appendTemporalItems(t, store,
		temporalObserver("legacy-depth", "legacy-depth-event", "legacy-vision", "depth", 10, 1),
		temporalIntent("prior-intent", "prior-intent-event", 20, 2),
		temporalObserver("current-old-screen", "current-old-screen-event", "vision", "screen", 30, 3, "prior-intent"),
		temporalIntent("current-intent", "current-intent-event", 40, 4),
		temporalObserver("current-fresh-screen", "current-fresh-screen-event", "vision", "screen", 50, 5, "current-intent"),
	)
	harness := mountTemporalEvidence(t, store,
		`{"mode":"after_intent","source_set":"observed_before_intent"}`)
	defer harness.stop(t)
	commit := temporalCommit(t, store, 5)
	sendPolicy(t, harness.ingress(t, "committed"), temporalCommitEnvelope("cohort-commit", commit))
	admitted := receivePolicy(t, harness.egress(t, "admitted")).Payload.(policyelements.AdmittedTemporalEvidence)
	if len(admitted.QualifyingObservations) != 1 ||
		admitted.QualifyingObservations[0].TrajectoryItemID != "current-fresh-screen" {
		t.Fatalf("cohort-scoped admission = %+v", admitted)
	}
	outcome := temporalOutcome(t, harness)
	if outcome.RequiredObservations != 1 || outcome.QualifiedObservations != 1 {
		t.Fatalf("cohort-scoped outcome = %+v", outcome)
	}
}

func TestTemporalEvidenceAdmissionRejectsTamperedCommitIdentityAndPrefix(t *testing.T) {
	store := trajectory.NewStore()
	appendTemporalItems(t, store, temporalObserver(
		"camera", "camera-event", "vision", "camera", 10, 1,
	))
	valid := temporalCommit(t, store, 1)
	tests := []struct {
		name   string
		mutate func(*stateelements.ObservationCommitOutcome)
		code   string
	}{
		{name: "prefix digest", code: "prefix_mismatch", mutate: func(value *stateelements.ObservationCommitOutcome) {
			value.Context.Prefix.Digest = "sha256:" + strings.Repeat("0", 64)
		}},
		{name: "trajectory item", code: "trigger_mismatch", mutate: func(value *stateelements.ObservationCommitOutcome) {
			value.TrajectoryItemID = "other-item"
		}},
		{name: "event item", code: "trigger_mismatch", mutate: func(value *stateelements.ObservationCommitOutcome) {
			value.TriggerItemID = "other-event"
		}},
		{name: "source revision", code: "trigger_mismatch", mutate: func(value *stateelements.ObservationCommitOutcome) {
			value.SourceRevision++
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			harness := mountTemporalEvidence(t, store, `{"mode":"immediate"}`)
			defer harness.stop(t)
			commit := valid
			testCase.mutate(&commit)
			sendPolicy(t, harness.ingress(t, "committed"), temporalCommitEnvelope("tampered-commit", commit))
			outcome := temporalOutcome(t, harness)
			if outcome.Kind != policyelements.TemporalEvidenceAdmissionRefused || outcome.Code != testCase.code {
				t.Fatalf("tampered outcome = %+v", outcome)
			}
			assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))
		})
	}
}

func TestTemporalEvidenceAdmissionAfterIntentRejectsDelayedPrefix(t *testing.T) {
	store := trajectory.NewStore()
	appendTemporalItems(t, store,
		temporalObserver("old-camera", "old-camera-event", "vision", "camera", 50, 1),
		temporalIntent("intent", "intent-event", 100, 2),
		temporalObserver("fresh-camera", "fresh-camera-event", "vision", "camera", 110, 3, "intent"),
	)
	delayed := temporalCommit(t, store, 3)
	appendTemporalItems(t, store,
		temporalObserver("newer-screen", "newer-screen-event", "vision", "screen", 120, 4, "intent"),
	)
	harness := mountTemporalEvidence(t, store,
		`{"mode":"after_intent","required":[{"observer":"vision","source":"camera"}]}`)
	defer harness.stop(t)

	sendPolicy(t, harness.ingress(t, "committed"), temporalCommitEnvelope("delayed-prefix", delayed))
	outcome := temporalOutcome(t, harness)
	if outcome.Kind != policyelements.TemporalEvidenceAdmissionRefused ||
		outcome.Code != "stale_trigger_prefix" {
		t.Fatalf("delayed prefix outcome = %+v", outcome)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))

	current := temporalCommit(t, store, 4)
	sendPolicy(t, harness.ingress(t, "committed"), temporalCommitEnvelope("current-prefix", current))
	admitted := receivePolicy(t, harness.egress(t, "admitted")).Payload.(policyelements.AdmittedTemporalEvidence)
	if len(admitted.QualifyingObservations) != 1 ||
		admitted.QualifyingObservations[0].TrajectoryItemID != "fresh-camera" ||
		admitted.TriggerObservation.TrajectoryItemID != "newer-screen" {
		t.Fatalf("current prefix admission = %+v", admitted)
	}
	if outcome = temporalOutcome(t, harness); outcome.Kind != policyelements.TemporalEvidenceAdmissionAdmitted {
		t.Fatalf("current prefix outcome = %+v", outcome)
	}
}

func TestTemporalEvidenceAdmissionRejectsProvisionalIntentAndMissingStore(t *testing.T) {
	store := trajectory.NewStore()
	provisional := temporalIntent("provisional-intent", "provisional-event", 100, 1)
	provisional.Event.Type = "participant.revision"
	appendTemporalItems(t, store,
		provisional,
		temporalObserver("camera", "camera-event", "vision", "camera", 110, 2, "provisional-intent"),
	)
	harness := mountTemporalEvidence(t, store,
		`{"mode":"after_intent","required":[{"observer":"vision","source":"camera"}]}`)
	defer harness.stop(t)
	commit := temporalCommit(t, store, 2)
	sendPolicy(t, harness.ingress(t, "committed"), temporalCommitEnvelope("provisional-commit", commit))
	outcome := temporalOutcome(t, harness)
	if outcome.Kind != policyelements.TemporalEvidenceAdmissionRefused || outcome.Code != "missing_durable_intent" {
		t.Fatalf("provisional intent outcome = %+v", outcome)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))

	registry := graphruntime.NewRegistry()
	if err := policyelements.RegisterFactories(registry); err != nil {
		t.Fatal(err)
	}
	if _, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph:    compilePolicySource(t, "temporal-evidence-missing-store.ortg", []byte(temporalEvidenceGraph)),
		Registry: registry,
		Values:   map[string]json.RawMessage{"admission": json.RawMessage(`{"mode":"immediate"}`)},
	}); err == nil || !strings.Contains(err.Error(), stateelements.TrajectoryStoreService) {
		t.Fatalf("missing store mount error = %v", err)
	}
}

func mountTemporalEvidence(t *testing.T, store *trajectory.Store, config string) policyHarness {
	t.Helper()
	registry := graphruntime.NewRegistry()
	if err := policyelements.RegisterFactories(registry); err != nil {
		t.Fatal(err)
	}
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(stateelements.TrajectoryStoreService,
		&stateelements.TrajectoryStoreServiceValue{Store: store}); err != nil {
		t.Fatal(err)
	}
	var clock atomic.Uint64
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph:    compilePolicySource(t, "temporal-evidence-test.ortg", []byte(temporalEvidenceGraph)),
		Registry: registry, Services: services,
		Values: map[string]json.RawMessage{"admission": json.RawMessage(config)},
		Now:    func() uint64 { return clock.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	return policyHarness{mounted: mounted, done: done, cancel: cancel}
}

func temporalOutcome(t *testing.T, harness policyHarness) policyelements.TemporalEvidenceAdmissionOutcome {
	t.Helper()
	envelope := receivePolicy(t, harness.egress(t, "outcome"))
	outcome, ok := envelope.Payload.(policyelements.TemporalEvidenceAdmissionOutcome)
	if !ok {
		t.Fatalf("temporal evidence outcome payload = %T", envelope.Payload)
	}
	return outcome
}

func appendTemporalItems(t *testing.T, store *trajectory.Store, items ...trajectory.Item) {
	t.Helper()
	if err := store.AppendBatch(items); err != nil {
		t.Fatal(err)
	}
}

func temporalObserver(
	id, eventID, observer, source string, occurredNS, revision uint64, parents ...string,
) trajectory.Item {
	return trajectory.Item{
		ID: id, Kind: trajectory.KindObservation, MonotonicNS: revision,
		CausalParentIDs: slices.Clone(parents), SourceRevision: revision,
		Producer: trajectory.Producer{Phase: trajectory.PhaseObserver, Provider: observer},
		Content:  observer + " observed " + source,
		Observation: &trajectory.ObservationMeta{
			Observer: observer, Source: source, Authority: trajectory.AuthorityObserver,
		},
		Event: &trajectory.EventMetadata{
			EventID: eventID, Type: observer + ".endpoint", Source: observer,
			Channel: source, OccurredNS: occurredNS,
		},
	}
}

func temporalIntent(id, eventID string, occurredNS, revision uint64) trajectory.Item {
	return trajectory.Item{
		ID: id, Kind: trajectory.KindObservation, MonotonicNS: revision,
		SourceRevision: revision, Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
		Content: "durable user intent",
		Event: &trajectory.EventMetadata{
			EventID: eventID, Type: "participant.endpoint", Source: "participant",
			Channel: "microphone", OccurredNS: occurredNS,
		},
	}
}

func temporalInstruction(id string, monotonicNS uint64) trajectory.Item {
	return trajectory.Item{
		ID: id, Kind: trajectory.KindInstruction, MonotonicNS: monotonicNS,
		Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, Content: "runtime instruction",
	}
}

func temporalCommit(
	t *testing.T, store *trajectory.Store, version uint64,
) stateelements.ObservationCommitOutcome {
	t.Helper()
	snapshot := store.Snapshot()
	if version == 0 || version > snapshot.Version {
		t.Fatalf("invalid temporal commit fixture version %d of %d", version, snapshot.Version)
	}
	item := snapshot.Items[version-1]
	if item.Kind != trajectory.KindObservation || item.Event == nil {
		t.Fatalf("temporal commit tail is not an event-backed observation: %+v", item)
	}
	prefix, err := trajectory.IdentifyPrefix(snapshot, version)
	if err != nil {
		t.Fatal(err)
	}
	return stateelements.ObservationCommitOutcome{
		Kind: stateelements.ObservationCommitted, TriggerItemID: item.Event.EventID,
		TrajectoryItemID: item.ID, StreamID: item.Event.Source + ":" + item.Event.Channel,
		ObservationRevision: item.SourceRevision, SourceRevision: item.SourceRevision,
		StoreVersion: version,
		Context: stateelements.CommittedContext{
			Prefix: prefix, StateItemID: "trajectory-state-" + item.ID,
		},
	}
}

func temporalCommitEnvelope(itemID string, commit stateelements.ObservationCommitOutcome) element.Envelope {
	return element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: itemID,
		SessionID: "temporal-evidence-session", Payload: commit,
	}
}

func assertTemporalEvidenceLiveResolution(t *testing.T, mounted *graphruntime.Mounted) {
	t.Helper()
	resolution := mounted.Live().Nodes["admission"].Resolution
	if resolution == nil || resolution.RuntimeEvidence != inspect.EvidenceLive ||
		resolution.Runtime.ID != "builtin://openrealtime/elements/policy.TemporalEvidenceAdmission" ||
		resolution.Runtime.Revision != "implementation:1" ||
		resolution.CapabilitiesEvidence != inspect.EvidenceLive || len(resolution.Capabilities) != 0 {
		t.Fatalf("temporal evidence live resolution = %+v", resolution)
	}
}
