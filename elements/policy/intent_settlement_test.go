package policy_test

import (
	"context"
	"encoding/json"
	"fmt"
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

const intentSettlementGraph = `graph intent_settlement_test {
    policy.IntentSettlement :: settlement;
    input evidence = settlement.evidence;
    input disposition = settlement.disposition;
    input ack = settlement.ack;
    input reset = settlement.reset;
    input cancel = settlement.cancel;
    output admitted = settlement.admitted;
    output cleanup = settlement.cleanup;
    output probe = settlement.probe;
    output terminal = settlement.terminal;
    output state = settlement.state;
    output outcome = settlement.outcome;
}
`

const chainedIntentSettlementGraph = `graph chained_intent_settlement_test {
    policy.IntentSettlement :: first;
    policy.IntentSettlement :: second;

    input evidence = first.evidence;
    input first_disposition = first.disposition;
    input first_ack = first.ack;
    input first_reset = first.reset;
    input first_cancel = first.cancel;

    first.admitted -> second.evidence;

    input second_disposition = second.disposition;
    input second_ack = second.ack;
    input second_reset = second.reset;
    input second_cancel = second.cancel;

    output first_probe = first.probe;
    output first_terminal = first.terminal;
    output first_state = first.state;
    output first_outcome = first.outcome;
    output first_cleanup = first.cleanup;
    output admitted = second.admitted;
    output cleanup = second.cleanup;
    output second_probe = second.probe;
    output second_terminal = second.terminal;
    output second_state = second.state;
    output second_outcome = second.outcome;
}
`

var settlementDetector = policyelements.IntentDetectorIdentity{
	Reference: "settlement-primary", Revision: "v1",
	ConfigurationDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
}

func TestIntentSettlementContractAndConfigAreExplicit(t *testing.T) {
	descriptor := policyelements.IntentSettlementDescriptor()
	if err := descriptor.Validate(); err != nil {
		t.Fatal(err)
	}
	if descriptor.Name != "policy.IntentSettlement" || descriptor.Revision != 2 ||
		descriptor.ConfigSchema != "schema://openrealtime/policy/intent-settlement-config/v1" ||
		descriptor.StateSchema != "schema://openrealtime/policy/intent-settlement-state/v2" {
		t.Fatalf("intent settlement descriptor = %+v", descriptor)
	}
	wantPorts := []element.Port{
		{Name: "evidence", Direction: element.Input, Type: policyelements.AdmittedTemporalEvidenceType(), Cardinality: element.One, Required: true, DefaultDepth: 32},
		{Name: "disposition", Direction: element.Input, Type: policyelements.IntentDispositionType(), Cardinality: element.One, Required: true, DefaultDepth: 16},
		{Name: "ack", Direction: element.Input, Type: policyelements.IntentSettlementAcknowledgementType(), Cardinality: element.One, Required: true, DefaultDepth: 16},
		{Name: "reset", Direction: element.Input, Type: policyelements.IntentSettlementResetType(), Cardinality: element.One, Required: true, DefaultDepth: 16},
		{Name: "cancel", Direction: element.Input, Type: policyelements.IntentSettlementCancelType(), Cardinality: element.One, Required: true, DefaultDepth: 16},
		{Name: "admitted", Direction: element.Output, Type: policyelements.AdmittedTemporalEvidenceType(), Cardinality: element.One, Required: true, DefaultDepth: 32},
		{Name: "cleanup", Direction: element.Output, Type: policyelements.IntentSettlementCleanupType(), Cardinality: element.One, Required: true, DefaultDepth: 16},
		{Name: "probe", Direction: element.Output, Type: policyelements.IntentSettlementProbeType(), Cardinality: element.One, Required: true, DefaultDepth: 16},
		{Name: "terminal", Direction: element.Output, Type: policyelements.IntentSettlementDecisionType(), Cardinality: element.One, Required: true, DefaultDepth: 16},
		{Name: "state", Direction: element.Output, Type: policyelements.IntentSettlementStateType(), Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
		{Name: "outcome", Direction: element.Output, Type: policyelements.IntentSettlementOutcomeType(), Cardinality: element.One, Required: true, DefaultDepth: 32},
	}
	if !reflect.DeepEqual(descriptor.Ports, wantPorts) {
		t.Fatalf("intent settlement ports = %+v, want %+v", descriptor.Ports, wantPorts)
	}
	wantReaction := element.Reaction{
		Triggers:       []string{"evidence", "disposition", "ack"},
		Interrupts:     []string{"reset", "cancel"},
		Outcomes:       []string{"admitted", "cleanup", "probe", "terminal", "state", "outcome"},
		MaxConcurrency: 1, BreaksCycles: true,
	}
	if !reflect.DeepEqual(descriptor.Reaction, wantReaction) ||
		!reflect.DeepEqual(descriptor.Dependencies, []element.Dependency{
			{Name: stateelements.TrajectoryStoreService},
			{Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName},
		}) || !reflect.DeepEqual(descriptor.Effects, []element.Effect{
		{Name: "policy.intent-settlement.memory", Reversible: true},
	}) {
		t.Fatalf("intent settlement reaction = %+v", descriptor.Reaction)
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
			"builtin://openrealtime/elements/policy.IntentSettlement" ||
			registration.Profile.Artifact.Revision != "implementation:2" {
			t.Fatalf("intent settlement registration = %+v", registration.Profile)
		}
		validator = registration.Factory.(element.ConfigValidator)
	}
	if validator == nil {
		t.Fatal("intent settlement factory is absent from the built-in registry")
	}
	valid := settlementConfigJSON(t, 64)
	if err := validator.ValidateConfig(valid); err != nil {
		t.Fatalf("valid intent settlement config: %v", err)
	}
	for _, source := range []string{
		`{}`,
		`{"expected_admission":{"mode":"immediate"},"candidate_sources":[{"observer":"vision","source":"screen"}],"detector":{"reference":"settlement-primary","revision":"v1","configuration_digest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}`,
		`{"expected_admission":{"mode":"after_intent","source_set":"explicit","required":[{"observer":"vision","source":"screen"}]},"candidate_sources":[],"detector":{"reference":"settlement-primary","revision":"v1","configuration_digest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}`,
		`{"expected_admission":{"mode":"after_intent","source_set":"explicit","required":[{"observer":"vision","source":"screen"}]},"candidate_sources":[{"observer":"vision","source":"screen"},{"observer":"vision","source":"screen"}],"detector":{"reference":"settlement-primary","revision":"v1","configuration_digest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}`,
		`{"expected_admission":{"mode":"after_intent","source_set":"explicit","required":[{"observer":"vision","source":"screen"}]},"candidate_sources":[{"observer":"vision","source":"screen"}],"detector":{"reference":"settlement-primary","revision":"v1","configuration_digest":"not-a-digest"}}`,
		`{"expected_admission":{"mode":"after_intent","source_set":"explicit","required":[{"observer":"vision","source":"screen"}]},"candidate_sources":[{"observer":"vision","source":"screen"}],"detector":{"reference":"settlement-primary","revision":"v1","configuration_digest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},"max_tracked_intents":0}`,
		`{"expected_admission":{"mode":"after_intent","source_set":"explicit","required":[{"observer":"vision","source":"screen"}]},"candidate_sources":[{"observer":"vision","source":"screen"}],"detector":{"reference":"settlement-primary","revision":"v1","configuration_digest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},"unknown":true}`,
	} {
		if err := validator.ValidateConfig(json.RawMessage(source)); err == nil {
			t.Errorf("invalid intent settlement config was accepted: %s", source)
		}
	}
}

func TestIntentSettlementMountRequiresTrustedSessionIdentity(t *testing.T) {
	registry := graphruntime.NewRegistry()
	if err := policyelements.RegisterFactories(registry); err != nil {
		t.Fatal(err)
	}
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(
		stateelements.TrajectoryStoreService,
		&stateelements.TrajectoryStoreServiceValue{Store: trajectory.NewStore()},
	); err != nil {
		t.Fatal(err)
	}
	_, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph:    compilePolicySource(t, "intent-settlement-session-test.ortg", []byte(intentSettlementGraph)),
		Registry: registry, Services: services,
		Values: map[string]json.RawMessage{"settlement": settlementConfigJSON(t, 4)},
	})
	if err == nil || !strings.Contains(err.Error(), "trajectory store session ID") {
		t.Fatalf("mount without trusted session identity error = %v", err)
	}
}

func TestIntentSettlementMountRejectsInvalidInstanceIdentity(t *testing.T) {
	registrations, err := policyelements.FactoryRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	var factory element.Factory
	for _, registration := range registrations {
		if registration.Profile.Reference == "policy.IntentSettlement" {
			factory = registration.Factory
			break
		}
	}
	if factory == nil {
		t.Fatal("intent settlement factory is absent from the built-in registry")
	}
	for _, instanceID := range []string{"", "invalid instance", strings.Repeat("x", 257)} {
		_, err := factory.Mount(context.Background(), element.MountContext{InstanceID: instanceID})
		if err == nil || !strings.Contains(err.Error(), "instance ID") || len(err.Error()) > 512 {
			t.Fatalf("mount with invalid instance %q error = %v", instanceID, err)
		}
	}
}

func TestIntentSettlementStartupBoundsMaximumInstanceIdentity(t *testing.T) {
	instance := strings.Repeat("x", 256)
	var clock atomic.Uint64
	harness := mountIntentSettlementWithInstance(
		t, trajectory.NewStore(), 4, func() uint64 { return clock.Add(100) }, instance,
	)
	defer harness.stop(t)
	envelope := receivePolicy(t, harness.egress(t, "state"))
	state, ok := envelope.Payload.(policyelements.IntentSettlementState)
	if !ok || state.Revision != 0 {
		t.Fatalf("maximum-instance startup state = %+v", envelope.Payload)
	}
	for _, parent := range envelope.CausalParents {
		if len(parent) > 256 {
			t.Fatalf("maximum-instance startup parent has %d bytes", len(parent))
		}
	}
}

func TestIntentSettlementPassesOrdinaryAndFailedEffectEvidenceWithoutStateLeak(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		failed bool
	}{
		{name: "ordinary observation"},
		{name: "failed effect", failed: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, evidence := settlementEvidenceFixture(t, testCase.failed, false)
			harness := mountIntentSettlement(t, store, 1)
			defer harness.stop(t)
			consumeIntentSettlementStartup(t, harness)
			envelope := settlementEvidenceEnvelope("ordinary-evidence", "session-a", 10, evidence)
			sendPolicy(t, harness.ingress(t, "evidence"), envelope)
			admitted := receivePolicy(t, harness.egress(t, "admitted"))
			if admitted.ItemID != envelope.ItemID ||
				!reflect.DeepEqual(admitted.Payload, evidence) {
				t.Fatalf("ordinary admission = %+v", admitted)
			}
			outcome := intentSettlementOutcome(t, harness)
			if outcome.Kind != policyelements.IntentSettlementAdmitted ||
				outcome.Code != "not_post_effect_candidate" {
				t.Fatalf("ordinary outcome = %+v", outcome)
			}
			state := intentSettlementState(t, harness)
			if state.TrackedIntents != 0 || state.PendingIntents != 0 || state.TerminalIntents != 0 ||
				state.Admitted != 1 || state.Saturated {
				t.Fatalf("ordinary state leaked a record: %+v", state)
			}
			assertNoPolicyEnvelope(t, harness.egress(t, "probe"))
			assertNoPolicyEnvelope(t, harness.egress(t, "terminal"))
		})
	}
}

func TestIntentSettlementContinueReleasesExactHeldEvidence(t *testing.T) {
	store, evidence := settlementEvidenceFixture(t, false, true)
	harness := mountIntentSettlement(t, store, 4)
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)
	envelope := settlementEvidenceEnvelope("candidate-evidence", "session-a", 10, evidence)
	sendPolicy(t, harness.ingress(t, "evidence"), envelope)
	probeEnvelope, probe := intentSettlementProbeEnvelope(t, harness)
	if probe.SessionID != envelope.SessionID || probe.Issuer != "settlement" ||
		probe.ProbeID == "" || probe.Sequence == 0 || probe.IssuedNS == 0 ||
		probeEnvelope.Sequence != probe.Sequence ||
		probe.Detector != settlementDetector ||
		!reflect.DeepEqual(probe.Evidence, evidence) || probe.DurableIntent != *evidence.DurableIntent ||
		probe.TriggerObservation != evidence.TriggerObservation || probe.Prefix != evidence.Prefix ||
		probe.Result.InvocationID != "generation-1" || probe.Result.CallID != "call-1" {
		t.Fatalf("settlement probe = %+v", probe)
	}
	config := policyelements.IntentSettlementConfig{
		ExpectedAdmission: policyelements.TemporalEvidenceAdmissionConfig{
			Mode:      policyelements.TemporalEvidenceAdmissionAfterIntent,
			SourceSet: policyelements.TemporalEvidenceSourceSetExplicit,
			Required:  []policyelements.TemporalEvidenceRequirement{{Observer: "vision", Source: "screen"}},
		},
		CandidateSources: []policyelements.TemporalEvidenceRequirement{{Observer: "vision", Source: "screen"}},
		Detector:         settlementDetector,
	}
	if err := policyelements.VerifyIntentSettlementProbe(store.Snapshot(), probe, config); err != nil {
		t.Fatalf("verify emitted settlement probe: %v", err)
	}
	encodedProbe, err := json.Marshal(probe)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrippedProbe policyelements.IntentSettlementProbe
	if err := json.Unmarshal(encodedProbe, &roundTrippedProbe); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundTrippedProbe, probe) {
		t.Fatalf("settlement probe changed across JSON round trip: got=%+v want=%+v", roundTrippedProbe, probe)
	}
	if err := policyelements.VerifyIntentSettlementProbe(
		store.Snapshot(), roundTrippedProbe, config,
	); err != nil {
		t.Fatalf("verify round-tripped settlement probe: %v", err)
	}
	forgedProbe := probe
	forgedProbe.Result.CallID = "forged-call"
	if err := policyelements.VerifyIntentSettlementProbe(store.Snapshot(), forgedProbe, config); err == nil {
		t.Fatal("settlement probe verifier accepted a forged call identity")
	}
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementHeld || outcome.Code != "awaiting_disposition" {
		t.Fatalf("held outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.TrackedIntents != 1 || state.PendingIntents != 1 {
		t.Fatalf("held state = %+v", state)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))

	foreignDisposition := settlementDisposition(probe, policyelements.IntentDispositionContinue)
	foreignDisposition.Probe.SessionID = "foreign-session"
	foreignEnvelope := settlementDispositionEnvelope("foreign-payload", foreignDisposition)
	foreignEnvelope.SessionID = "session-a"
	sendPolicy(t, harness.ingress(t, "disposition"), foreignEnvelope)
	foreignOutcome := intentSettlementOutcome(t, harness)
	if foreignOutcome.Kind != policyelements.IntentSettlementRefused ||
		foreignOutcome.Code != "session_mismatch" || foreignOutcome.DurableIntentItemID != "" ||
		foreignOutcome.TriggerObservationItemID != "" || foreignOutcome.ResultItemID != "" ||
		foreignOutcome.ProbeID != "" {
		t.Fatalf("foreign nested disposition outcome = %+v", foreignOutcome)
	}
	_ = intentSettlementState(t, harness)
	assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))

	disposition := settlementDisposition(probe, policyelements.IntentDispositionContinue)
	dispositionEnvelope := settlementDispositionEnvelope("continue", disposition)
	sendPolicy(t, harness.ingress(t, "disposition"), dispositionEnvelope)
	admitted := receivePolicy(t, harness.egress(t, "admitted"))
	if admitted.ItemID == envelope.ItemID || admitted.ItemID == dispositionEnvelope.ItemID ||
		admitted.ItemID == probe.ProbeID ||
		!strings.HasPrefix(admitted.ItemID, "intent-settlement-admitted:sha256:") ||
		len(admitted.ItemID) > 256 || admitted.Sequence != envelope.Sequence ||
		admitted.SourceID != envelope.SourceID ||
		!reflect.DeepEqual(admitted.Payload, evidence) {
		t.Fatalf("continued admission = %+v", admitted)
	}
	wantParents := []string{envelope.ItemID, dispositionEnvelope.ItemID, probe.ProbeID}
	if !reflect.DeepEqual(admitted.CausalParents, wantParents) {
		t.Fatalf("continued admission lineage = %v, want %v", admitted.CausalParents, wantParents)
	}
	outcome := intentSettlementOutcome(t, harness)
	if outcome.Kind != policyelements.IntentSettlementAdmitted || outcome.Code != "continue" ||
		outcome.Disposition != policyelements.IntentDispositionContinue {
		t.Fatalf("continue outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.TrackedIntents != 0 ||
		state.PendingIntents != 0 || state.Admitted != 1 {
		t.Fatalf("continue state = %+v", state)
	}

	forged := admitted.Clone()
	forged.CausalParents[0] = "changed-evidence-parent"
	verifier := mountIntentSettlement(t, store, 4)
	defer verifier.stop(t)
	consumeIntentSettlementStartup(t, verifier)
	sendPolicy(t, verifier.ingress(t, "evidence"), forged)
	if outcome := intentSettlementOutcome(t, verifier); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "invalid_envelope" {
		t.Fatalf("changed-metadata admission outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, verifier)
	assertNoPolicyEnvelope(t, verifier.egress(t, "probe"))
	assertNoPolicyEnvelope(t, verifier.egress(t, "admitted"))
	crossType := settlementDispositionEnvelope(admitted.ItemID, disposition)
	sendPolicy(t, harness.ingress(t, "disposition"), crossType)
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "invalid_envelope" {
		t.Fatalf("cross-type admission identity outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)

	// Continue is not a terminal latch: the same intent may legitimately
	// produce another result-linked effect and classification probe.
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"candidate-evidence-again", "session-a", 11, evidence,
	))
	second := intentSettlementProbe(t, harness)
	if second.ProbeID == probe.ProbeID || second.IssuedNS <= probe.IssuedNS {
		t.Fatalf("continued intent did not get a fresh probe: first=%+v second=%+v", probe, second)
	}
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)
}

func TestIntentSettlementProbeSequenceRejectsStaleDispositionWithConstantClock(t *testing.T) {
	store, evidence := settlementEvidenceFixture(t, false, true)
	harness := mountIntentSettlementWithClock(t, store, 4, func() uint64 { return 100 })
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)

	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"constant-clock-first", "session-a", 10, evidence,
	))
	first := intentSettlementProbe(t, harness)
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)
	oldDisposition := settlementDisposition(first, policyelements.IntentDispositionContinue)
	sendPolicy(t, harness.ingress(t, "disposition"), settlementDispositionEnvelope(
		"constant-clock-first-continue", oldDisposition,
	))
	_ = receivePolicy(t, harness.egress(t, "admitted"))
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)

	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"constant-clock-replay", "session-a", 11, evidence,
	))
	second := intentSettlementProbe(t, harness)
	if second.IssuedNS != first.IssuedNS || second.Sequence <= first.Sequence ||
		second.ProbeID == first.ProbeID {
		t.Fatalf("constant-clock replay did not receive a fresh probe: first=%+v second=%+v",
			first, second)
	}
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)
	sendPolicy(t, harness.ingress(t, "disposition"), settlementDispositionEnvelope(
		"stale-constant-clock-disposition", oldDisposition,
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "probe_mismatch" {
		t.Fatalf("stale constant-clock disposition outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.PendingIntents != 1 ||
		state.TrackedIntents != 1 {
		t.Fatalf("stale constant-clock disposition changed pending state = %+v", state)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))
}

func TestIntentSettlementAdmittedOutputComposesWithAnotherGate(t *testing.T) {
	store, evidence := settlementEvidenceFixture(t, false, true)
	harness := mountChainedIntentSettlement(t, store, 4)
	defer harness.stop(t)
	for _, boundary := range []string{"first_state", "second_state"} {
		envelope := receivePolicy(t, harness.egress(t, boundary))
		state, ok := envelope.Payload.(policyelements.IntentSettlementState)
		if !ok || state.Revision != 0 || state.TrackedIntents != 0 {
			t.Fatalf("%s startup state = %+v", boundary, envelope.Payload)
		}
	}

	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"chained-evidence", "session-a", 41, evidence,
	))
	firstProbeEnvelope := receivePolicy(t, harness.egress(t, "first_probe"))
	firstProbe, ok := firstProbeEnvelope.Payload.(policyelements.IntentSettlementProbe)
	if !ok || firstProbe.Issuer != "first" || firstProbeEnvelope.Sequence != firstProbe.Sequence {
		t.Fatalf("first chained probe = %+v", firstProbeEnvelope)
	}
	_ = receivePolicy(t, harness.egress(t, "first_outcome"))
	_ = receivePolicy(t, harness.egress(t, "first_state"))
	firstDisposition := settlementDisposition(firstProbe, policyelements.IntentDispositionContinue)
	sendPolicy(t, harness.ingress(t, "first_disposition"), settlementDispositionEnvelope(
		"chained-continue", firstDisposition,
	))
	firstOutcomeEnvelope := receivePolicy(t, harness.egress(t, "first_outcome"))
	firstOutcome, ok := firstOutcomeEnvelope.Payload.(policyelements.IntentSettlementOutcome)
	if !ok || firstOutcome.Code != "continue" {
		t.Fatalf("first chained outcome = %+v", firstOutcomeEnvelope.Payload)
	}
	_ = receivePolicy(t, harness.egress(t, "first_state"))

	secondProbeEnvelope := receivePolicy(t, harness.egress(t, "second_probe"))
	secondProbe, ok := secondProbeEnvelope.Payload.(policyelements.IntentSettlementProbe)
	if !ok || secondProbe.Issuer != "second" || secondProbe.ProbeID == firstProbe.ProbeID ||
		secondProbeEnvelope.Sequence != secondProbe.Sequence ||
		!reflect.DeepEqual(secondProbe.Evidence, evidence) ||
		!slices.ContainsFunc(secondProbeEnvelope.CausalParents, func(parent string) bool {
			return strings.HasPrefix(parent, "intent-settlement-admitted:sha256:")
		}) {
		t.Fatalf("second chained probe = envelope=%+v probe=%+v", secondProbeEnvelope, secondProbe)
	}
	secondOutcomeEnvelope := receivePolicy(t, harness.egress(t, "second_outcome"))
	secondOutcome, ok := secondOutcomeEnvelope.Payload.(policyelements.IntentSettlementOutcome)
	if !ok || secondOutcome.Code != "awaiting_disposition" {
		t.Fatalf("second chained outcome = %+v", secondOutcomeEnvelope.Payload)
	}
	secondStateEnvelope := receivePolicy(t, harness.egress(t, "second_state"))
	secondState, ok := secondStateEnvelope.Payload.(policyelements.IntentSettlementState)
	if !ok || secondState.PendingIntents != 1 || secondState.TrackedIntents != 1 {
		t.Fatalf("second chained state = %+v", secondStateEnvelope.Payload)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))
}

func TestIntentSettlementActorReceiptLinearizesCancellationAndContinue(t *testing.T) {
	t.Run("cancel received first refuses continuation", func(t *testing.T) {
		store, evidence := settlementEvidenceFixture(t, false, true)
		harness := mountIntentSettlement(t, store, 4)
		defer harness.stop(t)
		consumeIntentSettlementStartup(t, harness)
		sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
			"cancel-first-evidence", "session-a", 10, evidence,
		))
		probe := intentSettlementProbe(t, harness)
		_ = intentSettlementOutcome(t, harness)
		_ = intentSettlementState(t, harness)
		cancellation := policyelements.IntentSettlementCancellation{
			SessionID: "session-a", DurableIntent: *evidence.DurableIntent,
			Reason: "participant canceled",
		}
		sendPolicy(t, harness.ingress(t, "cancel"), settlementCancelEnvelope(
			"cancel-before-continue", cancellation,
		))
		terminalEnvelope, decision := intentSettlementDecisionEnvelope(t, harness)
		assertIntentSettlementLineage(t, terminalEnvelope, "cancel-before-continue", probe.ProbeID)
		if decision.Kind != policyelements.IntentSettlementDecisionCanceled {
			t.Fatalf("cancel-first decision = %+v", decision)
		}
		_ = intentSettlementOutcome(t, harness)
		_ = intentSettlementState(t, harness)
		sendPolicy(t, harness.ingress(t, "disposition"), settlementDispositionEnvelope(
			"continue-after-cancel",
			settlementDisposition(probe, policyelements.IntentDispositionContinue),
		))
		if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
			outcome.Code != "conflicting_terminal_disposition" {
			t.Fatalf("cancel-first continuation outcome = %+v", outcome)
		}
		_ = intentSettlementState(t, harness)
		assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))
	})

	t.Run("continue received first then cancellation forwards cleanup only", func(t *testing.T) {
		store, evidence := settlementEvidenceFixture(t, false, true)
		harness := mountIntentSettlement(t, store, 4)
		defer harness.stop(t)
		consumeIntentSettlementStartup(t, harness)
		sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
			"continue-first-evidence", "session-a", 10, evidence,
		))
		probe := intentSettlementProbe(t, harness)
		_ = intentSettlementOutcome(t, harness)
		_ = intentSettlementState(t, harness)
		sendPolicy(t, harness.ingress(t, "disposition"), settlementDispositionEnvelope(
			"continue-before-cancel",
			settlementDisposition(probe, policyelements.IntentDispositionContinue),
		))
		admitted := receivePolicy(t, harness.egress(t, "admitted"))
		assertIntentSettlementLineage(t, admitted, "continue-before-cancel", probe.ProbeID)
		_ = intentSettlementOutcome(t, harness)
		if state := intentSettlementState(t, harness); state.Admitted != 1 {
			t.Fatalf("continue-first state = %+v", state)
		}
		cancellation := policyelements.IntentSettlementCancellation{
			SessionID: "session-a", DurableIntent: *evidence.DurableIntent,
			Reason: "participant canceled after release",
		}
		sendPolicy(t, harness.ingress(t, "cancel"), settlementCancelEnvelope(
			"cancel-after-continue", cancellation,
		))
		if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementIgnored ||
			outcome.Code != "cancellation_recorded" {
			t.Fatalf("continue-first cancellation outcome = %+v", outcome)
		}
		_ = intentSettlementState(t, harness)
		sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
			"same-intent-after-cancel", "session-a", 11, evidence,
		))
		cleanup := receivePolicy(t, harness.egress(t, "cleanup"))
		assertIntentSettlementCleanup(
			t, cleanup, evidence, cancellation, "same-intent-after-cancel", "cancel-after-continue",
		)
		if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementCleanupForwarded ||
			outcome.Code != "canceled_successful_effect_cleanup" {
			t.Fatalf("continue-first replay outcome = %+v", outcome)
		}
		if state := intentSettlementState(t, harness); state.Admitted != 1 || state.Cleanups != 1 ||
			state.CancellationRequests != 1 || state.CancellationsCompleted != 1 {
			t.Fatalf("continue-first replay state = %+v", state)
		}
		assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))
		assertNoPolicyEnvelope(t, harness.egress(t, "probe"))
	})
}

func TestIntentSettlementCausalParentBoundaryIsClosedUnderAddedLineage(t *testing.T) {
	store, evidence := settlementEvidenceFixture(t, false, true)
	harness := mountIntentSettlement(t, store, 4)
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)
	envelope := settlementEvidenceEnvelope("parent-boundary", "session-a", 10, evidence)
	for index := 0; index < 62; index++ {
		envelope.CausalParents = append(envelope.CausalParents, fmt.Sprintf("parent-%d", index))
	}
	sendPolicy(t, harness.ingress(t, "evidence"), envelope)
	probeEnvelope, probe := intentSettlementProbeEnvelope(t, harness)
	if len(probeEnvelope.CausalParents) != 63 {
		t.Fatalf("probe causal parents = %d, want 63", len(probeEnvelope.CausalParents))
	}
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)
	disposition := settlementDisposition(probe, policyelements.IntentDispositionContinue)
	sendPolicy(t, harness.ingress(t, "disposition"), settlementDispositionEnvelope(
		"parent-boundary-continue", disposition,
	))
	admitted := receivePolicy(t, harness.egress(t, "admitted"))
	if len(admitted.CausalParents) != 3 {
		t.Fatalf("continued admission causal parents = %d, want 3", len(admitted.CausalParents))
	}
	unique := make(map[string]struct{}, len(admitted.CausalParents))
	for _, parent := range admitted.CausalParents {
		unique[parent] = struct{}{}
	}
	if len(unique) != len(admitted.CausalParents) {
		t.Fatalf("continued admission has duplicate causal parents: %v", admitted.CausalParents)
	}
	assertIntentSettlementLineage(t, admitted, "parent-boundary-continue", probe.ProbeID)
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)

	for _, testCase := range []struct {
		name    string
		parents []string
	}{
		{name: "overflow", parents: func() []string {
			parents := make([]string, 63)
			for index := range parents {
				parents[index] = fmt.Sprintf("overflow-%d", index)
			}
			return parents
		}()},
		{name: "duplicate", parents: []string{"duplicate", "duplicate"}},
		{name: "self", parents: []string{"invalid-parent-envelope"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			other := mountIntentSettlement(t, store, 4)
			defer other.stop(t)
			consumeIntentSettlementStartup(t, other)
			invalid := settlementEvidenceEnvelope(
				"invalid-parent-envelope", "session-a", 11, evidence,
			)
			invalid.CausalParents = testCase.parents
			sendPolicy(t, other.ingress(t, "evidence"), invalid)
			if outcome := intentSettlementOutcome(t, other); outcome.Kind != policyelements.IntentSettlementRefused ||
				outcome.Code != "invalid_envelope" {
				t.Fatalf("invalid causal parents outcome = %+v", outcome)
			}
			_ = intentSettlementState(t, other)
			assertNoPolicyEnvelope(t, other.egress(t, "probe"))
		})
	}
}

func TestIntentSettlementSanitizesRefusalMetadataAndPinsStateSession(t *testing.T) {
	store, _ := settlementEvidenceFixture(t, false, true)
	harness := mountIntentSettlement(t, store, 4)
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)

	oversized := strings.Repeat("x", 4096)
	parents := []string{"retained-parent", "retained-parent", "invalid parent"}
	for index := 0; index < 128; index++ {
		parents = append(parents, oversized)
	}
	forged := policyelements.IntentDisposition{
		Probe: policyelements.IntentSettlementProbe{
			ProbeID: oversized, SessionID: oversized,
			DurableIntent:      policyelements.TemporalEvidenceItemIdentity{TrajectoryItemID: oversized},
			TriggerObservation: policyelements.TemporalEvidenceItemIdentity{TrajectoryItemID: oversized},
			Result:             policyelements.IntentSettlementResultIdentity{TrajectoryItemID: oversized},
		},
		Detector: settlementDetector, Kind: policyelements.IntentDispositionContinue,
	}
	input := element.Envelope{
		Type: policyelements.IntentDispositionType(), ItemID: oversized,
		SessionID: "foreign-session", SourceID: oversized, OpportunityID: oversized,
		RunID: oversized, TraceID: oversized, CancellationScope: oversized,
		CausalParents: parents, Payload: forged,
	}
	sendPolicy(t, harness.ingress(t, "disposition"), input)
	outcomeEnvelope, outcome := intentSettlementOutcomeEnvelope(t, harness)
	if outcome.Kind != policyelements.IntentSettlementRefused || outcome.Code != "invalid_envelope" {
		t.Fatalf("hostile refusal outcome = %+v", outcome)
	}
	if outcome.SessionID != "session-a" || outcome.DurableIntentItemID != "" ||
		outcome.TriggerObservationItemID != "" || outcome.ResultItemID != "" || outcome.ProbeID != "" {
		t.Fatalf("hostile identities escaped into refusal payload: %+v", outcome)
	}
	if outcomeEnvelope.SessionID != "session-a" || outcomeEnvelope.SourceID != "" ||
		outcomeEnvelope.OpportunityID != "" || outcomeEnvelope.RunID != "" ||
		outcomeEnvelope.TraceID != "" || outcomeEnvelope.CancellationScope != "" ||
		strings.Contains(outcomeEnvelope.ItemID, oversized) ||
		slices.Contains(outcomeEnvelope.CausalParents, oversized) {
		t.Fatalf("hostile metadata escaped into refusal envelope: %+v", outcomeEnvelope)
	}
	if slices.Contains(outcomeEnvelope.CausalParents, "retained-parent") ||
		!reflect.DeepEqual(outcomeEnvelope.CausalParents, []string{"intent-settlement-invalid-input"}) ||
		slices.Contains(outcomeEnvelope.CausalParents, outcomeEnvelope.ItemID) {
		t.Fatalf("refusal lineage was not canonically bounded: %+v", outcomeEnvelope)
	}
	stateEnvelope := receivePolicy(t, harness.egress(t, "state"))
	state, ok := stateEnvelope.Payload.(policyelements.IntentSettlementState)
	if !ok {
		t.Fatalf("intent settlement state payload = %T", stateEnvelope.Payload)
	}
	if state.Refused != 1 || state.TrackedIntents != 0 ||
		stateEnvelope.SessionID != "session-a" || stateEnvelope.SourceID != "" ||
		stateEnvelope.OpportunityID != "" || stateEnvelope.RunID != "" ||
		stateEnvelope.TraceID != "" || stateEnvelope.CancellationScope != "" ||
		strings.Contains(stateEnvelope.ItemID, oversized) ||
		slices.Contains(stateEnvelope.CausalParents, oversized) ||
		slices.Contains(stateEnvelope.CausalParents, stateEnvelope.ItemID) {
		t.Fatalf("state output inherited hostile control metadata: envelope=%+v state=%+v",
			stateEnvelope, state)
	}
}

func TestIntentSettlementPreflightsNestedPayloadsBeforeRetainingThem(t *testing.T) {
	store, evidence := settlementEvidenceFixture(t, false, true)
	harness := mountIntentSettlement(t, store, 4)
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)

	evidence.QualifyingObservations = make(
		[]policyelements.TemporalEvidenceItemIdentity, 33,
	)
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"oversized-observation-set", "session-a", 10, evidence,
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "invalid_evidence" {
		t.Fatalf("oversized nested evidence outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.TrackedIntents != 0 ||
		state.PendingIntents != 0 || state.Refused != 1 {
		t.Fatalf("oversized nested evidence changed retained state = %+v", state)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "probe"))
}

func TestIntentSettlementGeneratedOutputsCannotNameThemselvesAsParents(t *testing.T) {
	store, evidence := settlementEvidenceFixture(t, false, true)
	predictor := mountIntentSettlementWithClock(t, store, 4, func() uint64 { return 100 })
	consumeIntentSettlementStartup(t, predictor)
	predictedInput := settlementEvidenceEnvelope("generated-cycle", "session-a", 10, evidence)
	sendPolicy(t, predictor.ingress(t, "evidence"), predictedInput)
	_, _ = intentSettlementProbeEnvelope(t, predictor)
	predictedOutcome, _ := intentSettlementOutcomeEnvelope(t, predictor)
	predictedState := receivePolicy(t, predictor.egress(t, "state"))
	predictor.stop(t)

	harness := mountIntentSettlementWithClock(t, store, 4, func() uint64 { return 100 })
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)

	input := settlementEvidenceEnvelope("generated-cycle", "session-a", 10, evidence)
	input.CausalParents = []string{predictedOutcome.ItemID, predictedState.ItemID}
	sendPolicy(t, harness.ingress(t, "evidence"), input)
	probeEnvelope, _ := intentSettlementProbeEnvelope(t, harness)
	if slices.Contains(probeEnvelope.CausalParents, probeEnvelope.ItemID) {
		t.Fatalf("probe output is self-causal: %+v", probeEnvelope)
	}
	outcomeEnvelope, _ := intentSettlementOutcomeEnvelope(t, harness)
	if outcomeEnvelope.ItemID != predictedOutcome.ItemID ||
		!strings.HasPrefix(outcomeEnvelope.ItemID, "intent-settlement-outcome:sha256:") ||
		len(outcomeEnvelope.ItemID) > 256 ||
		slices.Contains(outcomeEnvelope.CausalParents, outcomeEnvelope.ItemID) {
		t.Fatalf("outcome output is self-causal: %+v", outcomeEnvelope)
	}
	stateEnvelope := receivePolicy(t, harness.egress(t, "state"))
	if stateEnvelope.ItemID != predictedState.ItemID ||
		!strings.HasPrefix(stateEnvelope.ItemID, "intent-settlement-state:sha256:") ||
		len(stateEnvelope.ItemID) > 256 ||
		slices.Contains(stateEnvelope.CausalParents, stateEnvelope.ItemID) {
		t.Fatalf("state output is self-causal: %+v", stateEnvelope)
	}
}

func TestIntentSettlementRejectsPredictedProbeLineageWithoutStateLeak(t *testing.T) {
	store, evidence := settlementEvidenceFixture(t, false, true)
	newClock := func() func() uint64 {
		var clock atomic.Uint64
		return func() uint64 { return clock.Add(100) }
	}
	predictor := mountIntentSettlementWithClock(t, store, 4, newClock())
	consumeIntentSettlementStartup(t, predictor)
	sendPolicy(t, predictor.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"predict-probe", "session-a", 10, evidence,
	))
	predicted := intentSettlementProbe(t, predictor)
	_ = intentSettlementOutcome(t, predictor)
	_ = intentSettlementState(t, predictor)
	predictor.stop(t)

	harness := mountIntentSettlementWithClock(t, store, 4, newClock())
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)
	input := settlementEvidenceEnvelope("colliding-probe", "session-a", 11, evidence)
	input.CausalParents = []string{predicted.ProbeID}
	sendPolicy(t, harness.ingress(t, "evidence"), input)
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "invalid_probe" {
		t.Fatalf("predicted probe collision outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.TrackedIntents != 0 ||
		state.PendingIntents != 0 {
		t.Fatalf("predicted probe collision leaked state = %+v", state)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "probe"))
}

func TestIntentSettlementTerminalRequiresExactAcknowledgementAndStaysQuiescent(t *testing.T) {
	store, evidence := settlementEvidenceFixture(t, false, true)
	harness := mountIntentSettlement(t, store, 4)
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"terminal-evidence", "session-a", 10, evidence,
	))
	probe := intentSettlementProbe(t, harness)
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)

	disposition := settlementDisposition(probe, policyelements.IntentDispositionSucceeded)
	sendPolicy(t, harness.ingress(t, "disposition"), settlementDispositionEnvelope("succeeded", disposition))
	decisionEnvelope, decision := intentSettlementDecisionEnvelope(t, harness)
	assertIntentSettlementLineage(t, decisionEnvelope, "succeeded", probe.ProbeID)
	if decision.Kind != policyelements.IntentSettlementDecisionSucceeded ||
		decision.TerminalID == "" || decision.InvocationID != probe.Result.InvocationID ||
		decision.Disposition == nil || !reflect.DeepEqual(*decision.Disposition, disposition) ||
		!reflect.DeepEqual(decision.Evidence, evidence) ||
		decision.StateRevisionAfter != decision.StateRevisionBefore+1 {
		t.Fatalf("terminal decision = %+v", decision)
	}
	verifyConfig := settlementExpectedConfig()
	if err := policyelements.VerifyIntentSettlementDecision(store.Snapshot(), decision, verifyConfig); err != nil {
		t.Fatalf("verify emitted settlement decision: %v", err)
	}
	forgedDecision := decision
	forgedDecision.InvocationID = "forged-generation"
	if err := policyelements.VerifyIntentSettlementDecision(
		store.Snapshot(), forgedDecision, verifyConfig,
	); err == nil {
		t.Fatal("settlement decision verifier accepted a forged invocation")
	}
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementHeld || outcome.Code != "awaiting_activation_ack" {
		t.Fatalf("pre-ack outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))
	foreignDecision := decision
	foreignDecision.SessionID = "foreign-session"
	foreignDecision.Probe.SessionID = "foreign-session"
	foreignAck := policyelements.IntentSettlementAcknowledgement{
		Decision: foreignDecision, GenerationID: foreignDecision.InvocationID,
		AcknowledgedNS: foreignDecision.FinishedNS + 1,
	}
	foreignAckEnvelope := settlementAckEnvelope("foreign-decision-ack", foreignAck)
	foreignAckEnvelope.SessionID = "session-a"
	sendPolicy(t, harness.ingress(t, "ack"), foreignAckEnvelope)
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "session_mismatch" || outcome.DurableIntentItemID != "" ||
		outcome.TriggerObservationItemID != "" || outcome.ResultItemID != "" || outcome.ProbeID != "" {
		t.Fatalf("foreign nested acknowledgement outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	conflicting := settlementDisposition(probe, policyelements.IntentDispositionFailed)
	sendPolicy(t, harness.ingress(t, "disposition"), settlementDispositionEnvelope(
		"conflicting-terminal", conflicting,
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "conflicting_terminal_disposition" {
		t.Fatalf("conflicting terminal outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	duplicateInput := settlementDispositionEnvelope("duplicate-terminal", disposition)
	duplicateInput.CausalParents = append(duplicateInput.CausalParents, decision.TerminalID)
	sendPolicy(t, harness.ingress(t, "disposition"), duplicateInput)
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementIgnored ||
		outcome.Code != "duplicate_terminal_disposition" {
		t.Fatalf("duplicate terminal outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	assertNoPolicyEnvelope(t, harness.egress(t, "terminal"))
	collidingDuplicate := settlementDispositionEnvelope(decision.TerminalID, disposition)
	sendPolicy(t, harness.ingress(t, "disposition"), collidingDuplicate)
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "invalid_envelope" {
		t.Fatalf("reserved terminal identity collision outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	assertNoPolicyEnvelope(t, harness.egress(t, "terminal"))

	wrong := policyelements.IntentSettlementAcknowledgement{
		Decision: decision, GenerationID: "other-generation", AcknowledgedNS: decision.FinishedNS + 1,
	}
	sendPolicy(t, harness.ingress(t, "ack"), settlementAckEnvelope("wrong-ack", wrong))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused || outcome.Code != "invalid_acknowledgement" {
		t.Fatalf("wrong ack outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	unlinkedAck := policyelements.IntentSettlementAcknowledgement{
		Decision: decision, GenerationID: decision.InvocationID,
		AcknowledgedNS: decision.FinishedNS + 1,
	}
	unlinkedAckEnvelope := settlementAckEnvelope("unlinked-ack", unlinkedAck)
	unlinkedAckEnvelope.CausalParents = nil
	sendPolicy(t, harness.ingress(t, "ack"), unlinkedAckEnvelope)
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "invalid_acknowledgement" {
		t.Fatalf("unlinked ack outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)

	// Later cadence is suppressed both before and after acknowledgement.
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"pre-ack-cadence", "session-a", 11, evidence,
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementSuppressed || outcome.Code != "intent_terminal" {
		t.Fatalf("pre-ack cadence outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))

	ack := policyelements.IntentSettlementAcknowledgement{
		Decision: decision, GenerationID: decision.InvocationID, AcknowledgedNS: decision.FinishedNS + 1,
	}
	misaddressedAck := settlementAckEnvelope("misaddressed-ack", ack)
	misaddressedAck.RunID = "other-generation"
	sendPolicy(t, harness.ingress(t, "ack"), misaddressedAck)
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "invalid_acknowledgement" {
		t.Fatalf("misaddressed ack outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	sendPolicy(t, harness.ingress(t, "ack"), settlementAckEnvelope("exact-ack", ack))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementSuppressed || outcome.Code != "terminal_acknowledged" {
		t.Fatalf("exact ack outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.TerminalIntents != 1 ||
		state.PendingIntents != 0 || state.TrackedIntents != 1 {
		t.Fatalf("acknowledged terminal state = %+v", state)
	}

	sendPolicy(t, harness.ingress(t, "ack"), settlementAckEnvelope("duplicate-ack", ack))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementIgnored || outcome.Code != "duplicate_acknowledgement" {
		t.Fatalf("duplicate ack outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"post-ack-cadence", "session-a", 12, evidence,
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementSuppressed || outcome.Code != "intent_terminal" {
		t.Fatalf("post-ack cadence outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))

	reset := policyelements.IntentSettlementAddress{
		SessionID: "session-a", DurableIntent: *evidence.DurableIntent, Reason: "new task",
	}
	sendPolicy(t, harness.ingress(t, "reset"), element.Envelope{
		Type: policyelements.IntentSettlementResetType(), ItemID: "exact-reset",
		SessionID: "session-a", Payload: reset,
	})
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementReset || outcome.Code != "reset" {
		t.Fatalf("terminal reset outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.TrackedIntents != 0 ||
		state.TerminalIntents != 0 {
		t.Fatalf("terminal reset state = %+v", state)
	}
}

func TestIntentSettlementIndeterminateFailsClosedButCanBeResolved(t *testing.T) {
	store, evidence := settlementEvidenceFixture(t, false, true)
	harness := mountIntentSettlement(t, store, 4)
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"indeterminate-evidence", "session-a", 10, evidence,
	))
	probe := intentSettlementProbe(t, harness)
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)

	indeterminate := settlementDisposition(probe, policyelements.IntentDispositionIndeterminate)
	indeterminate.DecisionStartedNS = probe.IssuedNS + 10
	indeterminate.DecisionFinishedNS = probe.IssuedNS + 20
	sendPolicy(t, harness.ingress(t, "disposition"), settlementDispositionEnvelope(
		"indeterminate", indeterminate,
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementHeld || outcome.Code != "indeterminate" {
		t.Fatalf("indeterminate outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.Indeterminate != 1 || state.PendingIntents != 1 {
		t.Fatalf("indeterminate state = %+v", state)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))
	assertNoPolicyEnvelope(t, harness.egress(t, "terminal"))
	sendPolicy(t, harness.ingress(t, "disposition"), settlementDispositionEnvelope(
		"duplicate-indeterminate", indeterminate,
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementIgnored ||
		outcome.Code != "duplicate_indeterminate_disposition" {
		t.Fatalf("duplicate indeterminate outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.Indeterminate != 1 {
		t.Fatalf("duplicate indeterminate changed state = %+v", state)
	}
	stale := settlementDisposition(probe, policyelements.IntentDispositionSucceeded)
	sendPolicy(t, harness.ingress(t, "disposition"), settlementDispositionEnvelope("stale-terminal", stale))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "non_monotonic_disposition" {
		t.Fatalf("non-monotonic disposition outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	assertNoPolicyEnvelope(t, harness.egress(t, "terminal"))

	resolved := settlementDisposition(probe, policyelements.IntentDispositionFailed)
	resolved.DecisionStartedNS = indeterminate.DecisionFinishedNS
	resolved.DecisionFinishedNS = resolved.DecisionStartedNS + 1
	sendPolicy(t, harness.ingress(t, "disposition"), settlementDispositionEnvelope("failed", resolved))
	decisionEnvelope, decision := intentSettlementDecisionEnvelope(t, harness)
	assertIntentSettlementLineage(t, decisionEnvelope, "failed", probe.ProbeID)
	if decision.Kind != policyelements.IntentSettlementDecisionFailed {
		t.Fatalf("resolved terminal decision = %+v", decision)
	}
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)
}

func TestIntentSettlementRejectsForgedReorderedAndAmbiguousEvidence(t *testing.T) {
	store, evidence := settlementEvidenceFixture(t, false, true)
	harness := mountIntentSettlement(t, store, 4)
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)
	forgedEvidence := evidence
	forgedEvidence.TriggerObservation.TrajectoryItemID = "forged-observation"
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"forged-temporal-evidence", "session-a", 9, forgedEvidence,
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "invalid_temporal_evidence" {
		t.Fatalf("forged temporal evidence outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	assertNoPolicyEnvelope(t, harness.egress(t, "probe"))
	forgedProjection := evidence
	forgedProjection.TriggerCommit.Message = "unauthenticated commit diagnostic"
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"forged-commit-projection", "session-a", 9, forgedProjection,
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "invalid_temporal_evidence" {
		t.Fatalf("forged commit projection outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	assertNoPolicyEnvelope(t, harness.egress(t, "probe"))
	oversizedEvidence := evidence
	oversizedEvidence.TriggerCommit.Message = strings.Repeat("x", 1<<20)
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"oversized-evidence", "session-a", 9, oversizedEvidence,
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "invalid_evidence" {
		t.Fatalf("oversized evidence outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	assertNoPolicyEnvelope(t, harness.egress(t, "probe"))
	oversized := settlementEvidenceEnvelope("oversized-envelope", "session-a", 9, evidence)
	oversized.CausalParents = make([]string, 65)
	for index := range oversized.CausalParents {
		oversized.CausalParents[index] = fmt.Sprintf("parent-%d", index)
	}
	sendPolicy(t, harness.ingress(t, "evidence"), oversized)
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "invalid_envelope" {
		t.Fatalf("oversized envelope outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	assertNoPolicyEnvelope(t, harness.egress(t, "probe"))

	unknownProbe := policyelements.IntentSettlementProbe{
		ProbeID: "unknown", SessionID: "session-a", DurableIntent: *evidence.DurableIntent,
	}
	sendPolicy(t, harness.ingress(t, "disposition"), settlementDispositionEnvelope(
		"before-probe", settlementDisposition(unknownProbe, policyelements.IntentDispositionContinue),
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused || outcome.Code != "unknown_probe" {
		t.Fatalf("pre-probe disposition outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)

	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"forgery-evidence", "session-a", 10, evidence,
	))
	probe := intentSettlementProbe(t, harness)
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)
	unlinked := settlementDispositionEnvelope(
		"unlinked-disposition",
		settlementDisposition(probe, policyelements.IntentDispositionSucceeded),
	)
	unlinked.CausalParents = nil
	sendPolicy(t, harness.ingress(t, "disposition"), unlinked)
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "invalid_disposition" {
		t.Fatalf("unlinked disposition outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	wrongDetector := settlementDisposition(probe, policyelements.IntentDispositionSucceeded)
	wrongDetector.Detector.Revision = "other-revision"
	sendPolicy(t, harness.ingress(t, "disposition"), settlementDispositionEnvelope(
		"wrong-detector", wrongDetector,
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "invalid_disposition" {
		t.Fatalf("wrong detector outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	forged := settlementDisposition(probe, policyelements.IntentDispositionSucceeded)
	forged.Probe.Result.CallID = "forged-call"
	sendPolicy(t, harness.ingress(t, "disposition"), settlementDispositionEnvelope("forged", forged))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused || outcome.Code != "probe_mismatch" {
		t.Fatalf("forged disposition outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	invalidTime := settlementDisposition(probe, policyelements.IntentDispositionSucceeded)
	invalidTime.DecisionStartedNS = probe.IssuedNS - 1
	sendPolicy(t, harness.ingress(t, "disposition"), settlementDispositionEnvelope(
		"invalid-time", invalidTime,
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused || outcome.Code != "invalid_disposition" {
		t.Fatalf("invalid timing outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)

	ambiguousStore, ambiguousEvidence := settlementEvidenceFixture(t, false, true)
	first := ambiguousStore.Snapshot().Items[4]
	first.ID = "duplicate-consequence"
	first.MonotonicNS = 6
	first.SourceRevision = 6
	first.Event = &trajectory.EventMetadata{
		EventID: "duplicate-consequence-event", Type: "vision.endpoint", Source: "vision",
		Channel: "screen", OccurredNS: 31,
	}
	appendTemporalItems(t, ambiguousStore, first)
	ambiguousEvidence = admitSettlementEvidence(t, ambiguousStore, 6)
	ambiguousHarness := mountIntentSettlement(t, ambiguousStore, 4)
	defer ambiguousHarness.stop(t)
	consumeIntentSettlementStartup(t, ambiguousHarness)
	sendPolicy(t, ambiguousHarness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"ambiguous-evidence", "session-a", 10, ambiguousEvidence,
	))
	if outcome := intentSettlementOutcome(t, ambiguousHarness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "invalid_post_effect_evidence" {
		t.Fatalf("ambiguous consequence outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, ambiguousHarness)
	assertNoPolicyEnvelope(t, ambiguousHarness.egress(t, "probe"))
}

func TestIntentSettlementRejectsResultWhoseMatchingCallIsOutsideIntentCausality(t *testing.T) {
	store := trajectory.NewStore()
	appendTemporalItems(t, store,
		temporalObserver("old-screen", "old-screen-event", "vision", "screen", 10, 1),
		temporalIntent("intent-1", "intent-1-event", 20, 2),
		trajectory.Item{
			ID: "cross-branch-call", Kind: trajectory.KindToolCall, MonotonicNS: 3,
			CausalParentIDs: []string{"old-screen"}, InvocationID: "generation-cross",
			Producer: trajectory.Producer{Phase: trajectory.PhaseFast},
			ToolCall: &trajectory.ToolCall{CallID: "call-cross", Name: "computer.click", Arguments: json.RawMessage(`{"x":1}`)},
		},
		trajectory.Item{
			ID: "cross-branch-result", Kind: trajectory.KindToolResult, MonotonicNS: 4,
			CausalParentIDs: []string{"cross-branch-call", "intent-1"}, InvocationID: "generation-cross",
			Producer:   trajectory.Producer{Phase: trajectory.PhaseTool},
			ToolResult: &trajectory.ToolResult{CallID: "call-cross", Name: "computer.click", Output: json.RawMessage(`{"ok":true}`)},
		},
		temporalObserver("post-screen", "post-screen-event", "vision", "screen", 30, 5,
			"intent-1", "cross-branch-result"),
	)
	evidence := admitSettlementEvidence(t, store, 5)
	harness := mountIntentSettlement(t, store, 4)
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"cross-branch-evidence", "session-a", 10, evidence,
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "invalid_post_effect_evidence" {
		t.Fatalf("cross-branch call outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	assertNoPolicyEnvelope(t, harness.egress(t, "probe"))
}

func TestIntentSettlementRejectsNoncanonicalResultIdentities(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]trajectory.Item)
	}{
		{
			name: "oversized result item ID",
			mutate: func(items []trajectory.Item) {
				identifier := strings.Repeat("r", 257)
				items[3].ID = identifier
				items[4].CausalParentIDs[1] = identifier
			},
		},
		{
			name: "invalid UTF-8 invocation ID",
			mutate: func(items []trajectory.Item) {
				identifier := string([]byte{0xff})
				items[2].InvocationID = identifier
				items[3].InvocationID = identifier
			},
		},
		{
			name: "oversized call ID",
			mutate: func(items []trajectory.Item) {
				identifier := strings.Repeat("c", 257)
				items[2].ToolCall.CallID = identifier
				items[3].ToolResult.CallID = identifier
			},
		},
		{
			name: "noncanonical tool name",
			mutate: func(items []trajectory.Item) {
				items[2].ToolCall.Name = "computer click"
				items[3].ToolResult.Name = "computer click"
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			base, _ := settlementEvidenceFixture(t, false, true)
			items := base.Snapshot().Items
			testCase.mutate(items)
			store := trajectory.NewStore()
			appendTemporalItems(t, store, items...)
			evidence := admitSettlementEvidence(t, store, store.Snapshot().Version)
			harness := mountIntentSettlement(t, store, 4)
			defer harness.stop(t)
			consumeIntentSettlementStartup(t, harness)
			sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
				"noncanonical-result", "session-a", 10, evidence,
			))
			if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
				outcome.Code != "invalid_post_effect_evidence" {
				t.Fatalf("noncanonical result outcome = %+v", outcome)
			}
			_ = intentSettlementState(t, harness)
			assertNoPolicyEnvelope(t, harness.egress(t, "probe"))
		})
	}
}

func TestIntentSettlementPendingResetRequiresActivationAcknowledgement(t *testing.T) {
	store, evidence := settlementEvidenceFixture(t, false, true)
	harness := mountIntentSettlement(t, store, 4)
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"reset-evidence", "session-a", 10, evidence,
	))
	probe := intentSettlementProbe(t, harness)
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)
	reset := policyelements.IntentSettlementAddress{
		SessionID: "session-a", DurableIntent: *evidence.DurableIntent, Reason: "operator reset",
	}
	if err := policyelements.VerifyIntentSettlementReset(store.Snapshot(), reset); err != nil {
		t.Fatalf("verify canonical reset: %v", err)
	}
	oversizedReset := reset
	oversizedReset.Reason = strings.Repeat("r", 1025)
	if err := policyelements.VerifyIntentSettlementReset(store.Snapshot(), oversizedReset); err == nil {
		t.Fatal("settlement reset verifier accepted an oversized reason")
	}
	oversizedResetIdentity := reset
	oversizedResetIdentity.DurableIntent.TrajectoryItemID = strings.Repeat("i", 1<<20)
	if err := policyelements.VerifyIntentSettlementReset(
		store.Snapshot(), oversizedResetIdentity,
	); err == nil || len(err.Error()) > 2048 {
		t.Fatalf("settlement reset verifier did not bound hostile identity diagnostics: %v", err)
	}
	sendPolicy(t, harness.ingress(t, "reset"), element.Envelope{
		Type: policyelements.IntentSettlementResetType(), ItemID: "pending-reset",
		SessionID: "session-a", Payload: reset,
	})
	decisionEnvelope, decision := intentSettlementDecisionEnvelope(t, harness)
	assertIntentSettlementLineage(t, decisionEnvelope, "pending-reset", probe.ProbeID)
	if decision.Kind != policyelements.IntentSettlementDecisionReset ||
		decision.Disposition != nil || decision.Reset == nil || *decision.Reset != reset ||
		decision.Probe.ProbeID != probe.ProbeID {
		t.Fatalf("pending reset decision = %+v", decision)
	}
	if err := policyelements.VerifyIntentSettlementDecision(
		store.Snapshot(), decision, settlementExpectedConfig(),
	); err != nil {
		t.Fatalf("verify pending reset decision: %v", err)
	}
	missingWitness := decision
	missingWitness.Reset = nil
	if err := policyelements.VerifyIntentSettlementDecision(
		store.Snapshot(), missingWitness, settlementExpectedConfig(),
	); err == nil {
		t.Fatal("settlement decision verifier accepted a reset without its exact witness")
	}
	if outcome := intentSettlementOutcome(t, harness); outcome.Code != "awaiting_activation_ack" {
		t.Fatalf("pending reset outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))
	ack := policyelements.IntentSettlementAcknowledgement{
		Decision: decision, GenerationID: decision.InvocationID,
		AcknowledgedNS: decision.FinishedNS + 1,
	}
	sendPolicy(t, harness.ingress(t, "ack"), settlementAckEnvelope("pending-reset-ack", ack))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementReset ||
		outcome.Code != "reset" {
		t.Fatalf("pending reset ack outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.TrackedIntents != 0 {
		t.Fatalf("pending reset ack state = %+v", state)
	}
	sendPolicy(t, harness.ingress(t, "ack"), settlementAckEnvelope("duplicate-reset-ack", ack))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementIgnored ||
		outcome.Code != "duplicate_acknowledgement" {
		t.Fatalf("duplicate reset acknowledgement = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
}

func TestIntentSettlementResetDuringTerminalWaitsForExactAcknowledgement(t *testing.T) {
	store, evidence := settlementEvidenceFixture(t, false, true)
	harness := mountIntentSettlement(t, store, 4)
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"terminal-before-reset", "session-a", 10, evidence,
	))
	probe := intentSettlementProbe(t, harness)
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)
	sendPolicy(t, harness.ingress(t, "disposition"), settlementDispositionEnvelope(
		"success-before-reset", settlementDisposition(probe, policyelements.IntentDispositionSucceeded),
	))
	_, decision := intentSettlementDecisionEnvelope(t, harness)
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)
	reset := policyelements.IntentSettlementAddress{
		SessionID: "session-a", DurableIntent: *evidence.DurableIntent,
		Reason: "operator reset during terminal cleanup",
	}
	sendPolicy(t, harness.ingress(t, "reset"), element.Envelope{
		Type: policyelements.IntentSettlementResetType(), ItemID: "reset-during-terminal",
		SessionID: "session-a", Payload: reset,
	})
	deferredEnvelope, outcome := intentSettlementOutcomeEnvelope(t, harness)
	if outcome.Kind != policyelements.IntentSettlementIgnored || outcome.Code != "terminal_ack_pending" {
		t.Fatalf("reset-during-terminal outcome = %+v", outcome)
	}
	assertIntentSettlementLineage(
		t, deferredEnvelope, "reset-during-terminal", decision.TerminalID,
	)
	if state := intentSettlementState(t, harness); state.TrackedIntents != 1 ||
		state.Resets != 0 {
		t.Fatalf("reset-during-terminal state = %+v", state)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "terminal"))
	ack := policyelements.IntentSettlementAcknowledgement{
		Decision: decision, GenerationID: decision.InvocationID,
		AcknowledgedNS: decision.FinishedNS + 1,
	}
	sendPolicy(t, harness.ingress(t, "ack"), settlementAckEnvelope("terminal-reset-ack", ack))
	completionEnvelope, completion := intentSettlementOutcomeEnvelope(t, harness)
	if completion.Kind != policyelements.IntentSettlementReset ||
		completion.Code != "reset_after_terminal_ack" {
		t.Fatalf("terminal reset acknowledgement = %+v", completion)
	}
	assertIntentSettlementLineage(
		t, completionEnvelope, "reset-during-terminal", decision.TerminalID,
	)
	if state := intentSettlementState(t, harness); state.TrackedIntents != 0 ||
		state.Resets != 1 {
		t.Fatalf("terminal reset completion state = %+v", state)
	}
	sendPolicy(t, harness.ingress(t, "ack"), settlementAckEnvelope("duplicate-terminal-reset-ack", ack))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementIgnored ||
		outcome.Code != "duplicate_acknowledgement" {
		t.Fatalf("duplicate terminal reset acknowledgement = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
}

func TestIntentSettlementExactCancellationTombstonesQueuedEvidenceButAllowsNewIntent(t *testing.T) {
	store, evidence := settlementEvidenceFixture(t, false, true)
	harness := mountIntentSettlement(t, store, 1)
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)
	cancellation := policyelements.IntentSettlementCancellation{
		SessionID: "session-a", DurableIntent: *evidence.DurableIntent,
		Reason: "participant canceled",
	}
	if err := policyelements.VerifyIntentSettlementCancellation(store.Snapshot(), cancellation); err != nil {
		t.Fatalf("verify canonical cancellation: %v", err)
	}
	forged := cancellation
	forged.DurableIntent.StoreVersion++
	if err := policyelements.VerifyIntentSettlementCancellation(store.Snapshot(), forged); err == nil {
		t.Fatal("cancellation verifier accepted a forged durable-intent position")
	}
	oversizedReason := cancellation
	oversizedReason.Reason = strings.Repeat("x", 1025)
	if err := policyelements.VerifyIntentSettlementCancellation(
		store.Snapshot(), oversizedReason,
	); err == nil {
		t.Fatal("cancellation verifier accepted an oversized reason")
	}
	oversizedIdentity := cancellation
	oversizedIdentity.DurableIntent.TrajectoryItemID = strings.Repeat("i", 1<<20)
	if err := policyelements.VerifyIntentSettlementCancellation(
		store.Snapshot(), oversizedIdentity,
	); err == nil || len(err.Error()) > 2048 {
		t.Fatalf("cancellation verifier did not bound hostile identity diagnostics: %v", err)
	}
	invalidReason := cancellation
	invalidReason.Reason = string([]byte{0xff})
	if err := policyelements.VerifyIntentSettlementCancellation(
		store.Snapshot(), invalidReason,
	); err == nil {
		t.Fatal("cancellation verifier accepted an invalid UTF-8 reason")
	}
	sendPolicy(t, harness.ingress(t, "cancel"), settlementCancelEnvelope("pre-cancel", cancellation))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementIgnored ||
		outcome.Code != "cancellation_recorded" || outcome.DurableIntentItemID != "intent-1" {
		t.Fatalf("pre-cancel outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.CancellationRequests != 1 ||
		state.CancellationsCompleted != 1 ||
		state.CancellationEntries != 1 || state.TrackedIntents != 0 {
		t.Fatalf("pre-cancel state = %+v", state)
	}
	sendPolicy(t, harness.ingress(t, "cancel"), settlementCancelEnvelope("duplicate-pre-cancel", cancellation))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementIgnored ||
		outcome.Code != "duplicate_cancellation" {
		t.Fatalf("duplicate cancel outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.CancellationRequests != 1 ||
		state.CancellationsCompleted != 1 || state.CancellationEntries != 1 {
		t.Fatalf("duplicate cancel changed state = %+v", state)
	}
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"queued-canceled-evidence", "session-a", 10, evidence,
	))
	queuedCleanup := receivePolicy(t, harness.egress(t, "cleanup"))
	assertIntentSettlementCleanup(
		t, queuedCleanup, evidence, cancellation, "queued-canceled-evidence", "pre-cancel",
	)
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementCleanupForwarded ||
		outcome.Code != "canceled_successful_effect_cleanup" {
		t.Fatalf("queued canceled evidence outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	assertNoPolicyEnvelope(t, harness.egress(t, "probe"))
	assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))

	appendTemporalItems(t, store,
		temporalIntent("intent-2", "intent-2-event", 40, 6),
		temporalObserver("screen-intent-2", "screen-intent-2-event", "vision", "screen", 50, 7, "intent-2"),
	)
	newEvidence := admitSettlementEvidence(t, store, 7)
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"new-intent-evidence", "session-a", 11, newEvidence,
	))
	admitted := receivePolicy(t, harness.egress(t, "admitted"))
	if !reflect.DeepEqual(admitted.Payload, newEvidence) {
		t.Fatalf("new intent admission = %+v", admitted)
	}
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementAdmitted {
		t.Fatalf("new intent outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.CancellationEntries != 0 {
		t.Fatalf("superseded cancellation was not pruned = %+v", state)
	}
	newCancellation := policyelements.IntentSettlementCancellation{
		SessionID: "session-a", DurableIntent: *newEvidence.DurableIntent,
		Reason: "cancel newer intent",
	}
	sendPolicy(t, harness.ingress(t, "cancel"), settlementCancelEnvelope("new-intent-cancel", newCancellation))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementIgnored ||
		outcome.Code != "cancellation_recorded" {
		t.Fatalf("post-prune cancellation outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.CancellationRequests != 2 ||
		state.CancellationsCompleted != 2 ||
		state.CancellationEntries != 1 {
		t.Fatalf("post-prune cancellation state = %+v", state)
	}
}

func TestIntentSettlementPendingCancellationRequiresExactAcknowledgement(t *testing.T) {
	store, evidence := settlementEvidenceFixture(t, false, true)
	harness := mountIntentSettlement(t, store, 4)
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"cancel-pending-evidence", "session-a", 10, evidence,
	))
	probe := intentSettlementProbe(t, harness)
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)
	cancellation := policyelements.IntentSettlementCancellation{
		SessionID: "session-a", DurableIntent: *evidence.DurableIntent,
		Reason: "participant canceled",
	}
	sendPolicy(t, harness.ingress(t, "cancel"), settlementCancelEnvelope("cancel-pending", cancellation))
	decisionEnvelope, decision := intentSettlementDecisionEnvelope(t, harness)
	assertIntentSettlementLineage(t, decisionEnvelope, "cancel-pending", probe.ProbeID)
	if decision.Kind != policyelements.IntentSettlementDecisionCanceled ||
		decision.Cancellation == nil || *decision.Cancellation != cancellation ||
		decision.Probe.ProbeID != probe.ProbeID {
		t.Fatalf("canceled terminal decision = %+v", decision)
	}
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementHeld ||
		outcome.Code != "awaiting_activation_ack" {
		t.Fatalf("pending cancellation outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))
	sendPolicy(t, harness.ingress(t, "disposition"), settlementDispositionEnvelope(
		"late-continue", settlementDisposition(probe, policyelements.IntentDispositionContinue),
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "conflicting_terminal_disposition" {
		t.Fatalf("late continuation outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	ack := policyelements.IntentSettlementAcknowledgement{
		Decision: decision, GenerationID: decision.InvocationID,
		AcknowledgedNS: decision.FinishedNS + 1,
	}
	sendPolicy(t, harness.ingress(t, "ack"), settlementAckEnvelope("cancel-ack", ack))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementCanceled ||
		outcome.Code != "canceled" {
		t.Fatalf("cancellation acknowledgement outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.TrackedIntents != 0 ||
		state.CancellationEntries != 1 || state.AcknowledgedTerminals != 1 ||
		state.CancellationRequests != 1 || state.CancellationsCompleted != 1 {
		t.Fatalf("cancellation acknowledgement state = %+v", state)
	}
	sendPolicy(t, harness.ingress(t, "ack"), settlementAckEnvelope("duplicate-cancel-ack", ack))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementIgnored ||
		outcome.Code != "duplicate_acknowledgement" {
		t.Fatalf("duplicate cancellation acknowledgement = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
}

func TestIntentSettlementCountsSupersededActiveCancellationOnCompletion(t *testing.T) {
	store, evidence := settlementEvidenceFixture(t, false, true)
	harness := mountIntentSettlement(t, store, 4)
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"active-before-new-authority", "session-a", 10, evidence,
	))
	probe := intentSettlementProbe(t, harness)
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)

	// The newer user authority makes an old tombstone unnecessary, but it does
	// not make cleanup of the old already-active result unnecessary.
	appendTemporalItems(t, store, temporalIntent(
		"intent-2", "intent-2-event", 40, store.Snapshot().Version+1,
	))
	cancellation := policyelements.IntentSettlementCancellation{
		SessionID: "session-a", DurableIntent: *evidence.DurableIntent,
		Reason: "cancel superseded active intent",
	}
	sendPolicy(t, harness.ingress(t, "cancel"), settlementCancelEnvelope(
		"cancel-superseded-active", cancellation,
	))
	terminalEnvelope, decision := intentSettlementDecisionEnvelope(t, harness)
	assertIntentSettlementLineage(t, terminalEnvelope, "cancel-superseded-active", probe.ProbeID)
	if decision.Kind != policyelements.IntentSettlementDecisionCanceled {
		t.Fatalf("superseded active cancellation decision = %+v", decision)
	}
	_ = intentSettlementOutcome(t, harness)
	if state := intentSettlementState(t, harness); state.CancellationRequests != 1 ||
		state.CancellationsCompleted != 0 || state.CancellationEntries != 0 {
		t.Fatalf("superseded active cancellation pending state = %+v", state)
	}
	sendPolicy(t, harness.ingress(t, "cancel"), settlementCancelEnvelope(
		"duplicate-cancel-superseded-active", cancellation,
	))
	_ = intentSettlementOutcome(t, harness)
	if state := intentSettlementState(t, harness); state.CancellationRequests != 1 ||
		state.CancellationsCompleted != 0 {
		t.Fatalf("duplicate superseded cancellation changed counters = %+v", state)
	}
	ack := policyelements.IntentSettlementAcknowledgement{
		Decision: decision, GenerationID: decision.InvocationID,
		AcknowledgedNS: decision.FinishedNS + 1,
	}
	sendPolicy(t, harness.ingress(t, "ack"), settlementAckEnvelope(
		"superseded-active-cancel-ack", ack,
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementCanceled || outcome.Code != "canceled" {
		t.Fatalf("superseded active cancellation acknowledgement = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.CancellationRequests != 1 ||
		state.CancellationsCompleted != 1 || state.TrackedIntents != 0 {
		t.Fatalf("superseded active cancellation completed state = %+v", state)
	}
}

func TestIntentSettlementFailedCancellationDecisionDoesNotMutateRevocationState(t *testing.T) {
	store, evidence := settlementEvidenceFixture(t, false, true)
	var clock atomic.Uint64
	var failClock atomic.Bool
	harness := mountIntentSettlementWithClock(t, store, 4, func() uint64 {
		if failClock.Load() {
			return 0
		}
		return clock.Add(100)
	})
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"clock-failure-evidence", "session-a", 10, evidence,
	))
	probe := intentSettlementProbe(t, harness)
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)
	cancellation := policyelements.IntentSettlementCancellation{
		SessionID: "session-a", DurableIntent: *evidence.DurableIntent,
		Reason: "cancel while clock fails",
	}
	failClock.Store(true)
	sendPolicy(t, harness.ingress(t, "cancel"), settlementCancelEnvelope(
		"clock-failure-cancel", cancellation,
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "invalid_terminal_decision" {
		t.Fatalf("clock-failure cancellation outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.CancellationRequests != 0 ||
		state.CancellationsCompleted != 0 || state.CancellationEntries != 0 ||
		state.PendingIntents != 1 {
		t.Fatalf("clock-failure cancellation mutated safety state = %+v", state)
	}
	failClock.Store(false)
	assertNoPolicyEnvelope(t, harness.egress(t, "terminal"))
	sendPolicy(t, harness.ingress(t, "disposition"), settlementDispositionEnvelope(
		"continue-after-refused-cancel",
		settlementDisposition(probe, policyelements.IntentDispositionContinue),
	))
	admitted := receivePolicy(t, harness.egress(t, "admitted"))
	assertIntentSettlementLineage(
		t, admitted, "continue-after-refused-cancel", probe.ProbeID,
	)
	_ = intentSettlementOutcome(t, harness)
	if state := intentSettlementState(t, harness); state.Admitted != 1 ||
		state.TrackedIntents != 0 {
		t.Fatalf("post-refusal continuation state = %+v", state)
	}
}

func TestIntentSettlementFailedProbeConstructionDoesNotRetainEmptyRecord(t *testing.T) {
	store, evidence := settlementEvidenceFixture(t, false, true)
	var clock atomic.Uint64
	var failClock atomic.Bool
	harness := mountIntentSettlementWithClock(t, store, 1, func() uint64 {
		if failClock.Load() {
			return 0
		}
		return clock.Add(100)
	})
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)
	failClock.Store(true)
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"failed-probe-construction", "session-a", 10, evidence,
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "invalid_probe" {
		t.Fatalf("failed probe construction outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.TrackedIntents != 0 ||
		state.PendingIntents != 0 || state.Saturated {
		t.Fatalf("failed probe construction retained an empty record = %+v", state)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "probe"))

	failClock.Store(false)
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"probe-after-clock-recovery", "session-a", 11, evidence,
	))
	_ = intentSettlementProbe(t, harness)
	_ = intentSettlementOutcome(t, harness)
	if state := intentSettlementState(t, harness); state.TrackedIntents != 1 ||
		state.PendingIntents != 1 || !state.Saturated {
		t.Fatalf("clock recovery did not use the full live capacity = %+v", state)
	}
}

func TestIntentSettlementRejectedNewEvidenceDoesNotPruneCancellationTombstones(t *testing.T) {
	store, oldEvidence := settlementEvidenceFixture(t, false, true)
	var clock atomic.Uint64
	var failClock atomic.Bool
	harness := mountIntentSettlementWithClock(t, store, 4, func() uint64 {
		if failClock.Load() {
			return 0
		}
		return clock.Add(100)
	})
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)
	oldCancellation := policyelements.IntentSettlementCancellation{
		SessionID: "session-a", DurableIntent: *oldEvidence.DurableIntent,
		Reason: "retain until newer input is accepted",
	}
	sendPolicy(t, harness.ingress(t, "cancel"), settlementCancelEnvelope(
		"old-cancellation", oldCancellation,
	))
	_ = intentSettlementOutcome(t, harness)
	if state := intentSettlementState(t, harness); state.CancellationEntries != 1 ||
		state.CancellationsCompleted != 1 {
		t.Fatalf("old cancellation was not recorded = %+v", state)
	}

	appendTemporalItems(t, store,
		temporalIntent("new-intent", "new-intent-event", 40, 6),
		trajectory.Item{
			ID: "new-call", Kind: trajectory.KindToolCall, MonotonicNS: 7,
			CausalParentIDs: []string{"new-intent"}, InvocationID: "new-generation",
			Producer: trajectory.Producer{Phase: trajectory.PhaseFast},
			ToolCall: &trajectory.ToolCall{
				CallID: "new-call-id", Name: "computer.click", Arguments: json.RawMessage(`{"x":2}`),
			},
		},
		trajectory.Item{
			ID: "new-result", Kind: trajectory.KindToolResult, MonotonicNS: 8,
			CausalParentIDs: []string{"new-call"}, InvocationID: "new-generation",
			Producer: trajectory.Producer{Phase: trajectory.PhaseTool},
			ToolResult: &trajectory.ToolResult{
				CallID: "new-call-id", Name: "computer.click", Output: json.RawMessage(`{"ok":true}`),
			},
		},
		temporalObserver(
			"new-screen", "new-screen-event", "vision", "screen", 50, 9,
			"new-intent", "new-result",
		),
	)
	newEvidence := admitSettlementEvidence(t, store, 9)
	failClock.Store(true)
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"rejected-newer-evidence", "session-a", 11, newEvidence,
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "invalid_probe" {
		t.Fatalf("rejected newer evidence outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.CancellationEntries != 1 ||
		state.TrackedIntents != 0 || state.PendingIntents != 0 {
		t.Fatalf("rejected newer evidence pruned safety state = %+v", state)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "probe"))
}

func TestIntentSettlementFailedSupersessionDoesNotRetainRejectedReplacement(t *testing.T) {
	store, firstEvidence := settlementEvidenceFixture(t, false, true)
	var clock atomic.Uint64
	var failClock atomic.Bool
	harness := mountIntentSettlementWithClock(t, store, 4, func() uint64 {
		if failClock.Load() {
			return 0
		}
		return clock.Add(100)
	})
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"pending-before-failed-supersession", "session-a", 10, firstEvidence,
	))
	firstProbe := intentSettlementProbe(t, harness)
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)

	appendTemporalItems(t, store,
		temporalIntent("failed-replacement-intent", "failed-replacement-event", 40, 6),
		temporalObserver(
			"failed-replacement-screen", "failed-replacement-screen-event", "vision", "screen",
			50, 7, "failed-replacement-intent",
		),
	)
	replacement := admitSettlementEvidence(t, store, 7)
	failClock.Store(true)
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"rejected-replacement", "session-a", 11, replacement,
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused ||
		outcome.Code != "invalid_terminal_decision" {
		t.Fatalf("failed supersession outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.TrackedIntents != 1 ||
		state.PendingIntents != 1 {
		t.Fatalf("failed supersession changed the live record = %+v", state)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "terminal"))

	failClock.Store(false)
	cancellation := policyelements.IntentSettlementCancellation{
		SessionID: "session-a", DurableIntent: *firstEvidence.DurableIntent,
		Reason: "retire original after failed supersession",
	}
	sendPolicy(t, harness.ingress(t, "cancel"), settlementCancelEnvelope(
		"cancel-after-failed-supersession", cancellation,
	))
	decision := intentSettlementDecision(t, harness)
	if decision.Kind != policyelements.IntentSettlementDecisionCanceled ||
		decision.Probe.ProbeID != firstProbe.ProbeID {
		t.Fatalf("post-failure cancellation decision = %+v", decision)
	}
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)
	ack := policyelements.IntentSettlementAcknowledgement{
		Decision: decision, GenerationID: decision.InvocationID,
		AcknowledgedNS: decision.FinishedNS + 1,
	}
	sendPolicy(t, harness.ingress(t, "ack"), settlementAckEnvelope(
		"ack-after-failed-supersession", ack,
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementCanceled ||
		outcome.Code != "canceled" {
		t.Fatalf("post-failure cancellation acknowledgement = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.TrackedIntents != 0 ||
		state.CancellationsCompleted != 1 {
		t.Fatalf("post-failure cancellation state = %+v", state)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))
	assertNoPolicyEnvelope(t, harness.egress(t, "probe"))
}

func TestIntentSettlementRejectsForeignFirstInputWithoutSelectingMountedSession(t *testing.T) {
	store, evidence := settlementEvidenceFixture(t, false, true)
	harness := mountIntentSettlement(t, store, 4)
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)

	foreign := settlementEvidenceEnvelope("foreign-first-evidence", "session-b", 10, evidence)
	foreign.SourceID = "foreign-source"
	foreign.OpportunityID = "foreign-opportunity"
	foreign.RunID = "foreign-run"
	foreign.TraceID = "foreign-trace"
	foreign.CancellationScope = "foreign-scope"
	foreign.CausalParents = []string{"foreign-parent"}
	sendPolicy(t, harness.ingress(t, "evidence"), foreign)
	foreignOutcomeEnvelope, outcome := intentSettlementOutcomeEnvelope(t, harness)
	if outcome.Kind != policyelements.IntentSettlementRefused || outcome.Code != "session_mismatch" {
		t.Fatalf("foreign-first evidence outcome = %+v", outcome)
	}
	if foreignOutcomeEnvelope.SessionID != "session-a" || foreignOutcomeEnvelope.SourceID != "" ||
		foreignOutcomeEnvelope.OpportunityID != "" || foreignOutcomeEnvelope.RunID != "" ||
		foreignOutcomeEnvelope.TraceID != "" || foreignOutcomeEnvelope.CancellationScope != "" ||
		slices.Contains(foreignOutcomeEnvelope.CausalParents, foreign.ItemID) ||
		slices.Contains(foreignOutcomeEnvelope.CausalParents, "foreign-parent") {
		t.Fatalf("foreign refusal retained cross-session addressing: %+v", foreignOutcomeEnvelope)
	}
	foreignStateEnvelope := receivePolicy(t, harness.egress(t, "state"))
	state, ok := foreignStateEnvelope.Payload.(policyelements.IntentSettlementState)
	if !ok {
		t.Fatalf("intent settlement state payload = %T", foreignStateEnvelope.Payload)
	}
	if state.TrackedIntents != 0 || state.CancellationEntries != 0 ||
		foreignStateEnvelope.SessionID != "session-a" || foreignStateEnvelope.SourceID != "" ||
		foreignStateEnvelope.RunID != "" || foreignStateEnvelope.CancellationScope != "" ||
		slices.Contains(foreignStateEnvelope.CausalParents, foreign.ItemID) ||
		slices.Contains(foreignStateEnvelope.CausalParents, "foreign-parent") {
		t.Fatalf("foreign-first evidence changed safety state = %+v", state)
	}
	foreignCancellation := policyelements.IntentSettlementCancellation{
		SessionID: "session-b", DurableIntent: *evidence.DurableIntent,
		Reason: "foreign first cancellation",
	}
	sendPolicy(t, harness.ingress(t, "cancel"), settlementCancelEnvelope(
		"foreign-first-cancel", foreignCancellation,
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused || outcome.Code != "session_mismatch" {
		t.Fatalf("foreign-first cancellation outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.CancellationRequests != 0 ||
		state.CancellationsCompleted != 0 || state.CancellationEntries != 0 {
		t.Fatalf("foreign-first cancellation changed safety state = %+v", state)
	}

	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"legitimate-after-foreign", "session-a", 11, evidence,
	))
	if probe := intentSettlementProbe(t, harness); probe.SessionID != "session-a" {
		t.Fatalf("legitimate session probe = %+v", probe)
	}
	_ = intentSettlementOutcome(t, harness)
	if state := intentSettlementState(t, harness); state.TrackedIntents != 1 ||
		state.PendingIntents != 1 {
		t.Fatalf("legitimate session state = %+v", state)
	}
}

func TestIntentSettlementRejectsCrossSessionInputsWithoutEvictingLiveSafetyState(t *testing.T) {
	store, evidence := settlementEvidenceFixture(t, false, true)
	harness := mountIntentSettlement(t, store, 1)
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"capacity-first", "session-a", 10, evidence,
	))
	probe := intentSettlementProbe(t, harness)
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)

	// A mounted canonical trajectory is session-scoped. The same canonical
	// identity under another envelope session is rejected and cannot clear this
	// session's held candidate.
	foreignCancel := policyelements.IntentSettlementCancellation{
		SessionID: "session-b", DurableIntent: *evidence.DurableIntent,
		Reason: "other session canceled",
	}
	sendPolicy(t, harness.ingress(t, "cancel"), element.Envelope{
		Type: policyelements.IntentSettlementCancelType(), ItemID: "wrong-cancel", SessionID: "session-b",
		CancellationScope: foreignCancel.DurableIntent.TrajectoryItemID, Payload: foreignCancel,
	})
	if outcome := intentSettlementOutcome(t, harness); outcome.Code != "session_mismatch" ||
		outcome.Kind != policyelements.IntentSettlementRefused {
		t.Fatalf("unrelated cancellation outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)

	// Evidence from another session is likewise rejected before it can compete
	// for or evict the live record.
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"capacity-second", "session-c", 12, evidence,
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Kind != policyelements.IntentSettlementRefused || outcome.Code != "session_mismatch" {
		t.Fatalf("cross-session evidence outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.TrackedIntents != 1 || !state.Saturated {
		t.Fatalf("live state changed by cross-session input = %+v", state)
	}

	valid := settlementDisposition(probe, policyelements.IntentDispositionSucceeded)
	sendPolicy(t, harness.ingress(t, "disposition"), settlementDispositionEnvelope("still-live", valid))
	decisionEnvelope, decision := intentSettlementDecisionEnvelope(t, harness)
	assertIntentSettlementLineage(t, decisionEnvelope, "still-live", probe.ProbeID)
	if decision.Probe.ProbeID != probe.ProbeID {
		t.Fatalf("capacity evicted live probe: %+v", decision)
	}
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)

	// Exact cancellation cannot bypass the already-issued terminal barrier. It
	// is retained until activation acknowledges that effect, after which queued
	// evidence for the canceled intent remains revoked.
	exactCancel := policyelements.IntentSettlementCancellation{
		SessionID: "session-a", DurableIntent: *evidence.DurableIntent,
		Reason: "participant canceled",
	}
	sendPolicy(t, harness.ingress(t, "cancel"), element.Envelope{
		Type: policyelements.IntentSettlementCancelType(), ItemID: "exact-cancel", SessionID: "session-a",
		CancellationScope: exactCancel.DurableIntent.TrajectoryItemID, Payload: exactCancel,
	})
	deferredCancelEnvelope, outcome := intentSettlementOutcomeEnvelope(t, harness)
	if outcome.Code != "terminal_ack_pending" || outcome.Kind != policyelements.IntentSettlementHeld {
		t.Fatalf("exact cancellation outcome = %+v", outcome)
	}
	assertIntentSettlementLineage(
		t, deferredCancelEnvelope, "exact-cancel", decision.TerminalID,
	)
	if state := intentSettlementState(t, harness); state.TrackedIntents != 1 ||
		state.CancellationRequests != 1 || state.CancellationsCompleted != 0 {
		t.Fatalf("exact cancellation state = %+v", state)
	}
	ack := policyelements.IntentSettlementAcknowledgement{
		Decision: decision, GenerationID: decision.InvocationID,
		AcknowledgedNS: decision.FinishedNS + 1,
	}
	sendPolicy(t, harness.ingress(t, "ack"), settlementAckEnvelope("canceled-terminal-ack", ack))
	cancelCompletionEnvelope, cancelCompletion := intentSettlementOutcomeEnvelope(t, harness)
	if cancelCompletion.Code != "canceled_after_terminal_ack" ||
		cancelCompletion.Kind != policyelements.IntentSettlementCanceled {
		t.Fatalf("canceled terminal acknowledgement = %+v", cancelCompletion)
	}
	assertIntentSettlementLineage(
		t, cancelCompletionEnvelope, "exact-cancel", decision.TerminalID,
	)
	if state := intentSettlementState(t, harness); state.TrackedIntents != 0 ||
		state.CancellationsCompleted != 1 {
		t.Fatalf("canceled terminal state = %+v", state)
	}
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"canceled-in-flight", "session-a", 13, evidence,
	))
	inFlightCleanup := receivePolicy(t, harness.egress(t, "cleanup"))
	assertIntentSettlementCleanup(
		t, inFlightCleanup, evidence, exactCancel, "canceled-in-flight", "exact-cancel",
	)
	if outcome := intentSettlementOutcome(t, harness); outcome.Code != "canceled_successful_effect_cleanup" ||
		outcome.Kind != policyelements.IntentSettlementCleanupForwarded {
		t.Fatalf("canceled in-flight evidence outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	assertNoPolicyEnvelope(t, harness.egress(t, "probe"))
	assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))
}

func TestIntentSettlementRevokedIntentForwardsOnlyExactEffectCleanupEvidence(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		failed       bool
		resultLinked bool
		forwarded    bool
		code         string
	}{
		{name: "exact failed consequence", failed: true, forwarded: true,
			code: "canceled_failed_effect_cleanup"},
		{name: "ordinary cadence", code: "intent_revoked"},
		{name: "successful consequence", resultLinked: true, forwarded: true,
			code: "canceled_successful_effect_cleanup"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, evidence := settlementEvidenceFixture(t, testCase.failed, testCase.resultLinked)
			harness := mountIntentSettlement(t, store, 2)
			defer harness.stop(t)
			consumeIntentSettlementStartup(t, harness)
			cancellation := policyelements.IntentSettlementCancellation{
				SessionID: "session-a", DurableIntent: *evidence.DurableIntent,
				Reason: "participant canceled",
			}
			const cancellationItemID = "effect-cleanup-cancel"
			sendPolicy(t, harness.ingress(t, "cancel"),
				settlementCancelEnvelope(cancellationItemID, cancellation))
			_ = intentSettlementOutcome(t, harness)
			_ = intentSettlementState(t, harness)

			envelope := settlementEvidenceEnvelope(
				"revoked-failed-effect-evidence", "session-a", 10, evidence,
			)
			sendPolicy(t, harness.ingress(t, "evidence"), envelope)
			if testCase.forwarded {
				cleanup := receivePolicy(t, harness.egress(t, "cleanup"))
				assertIntentSettlementCleanup(
					t, cleanup, evidence, cancellation, envelope.ItemID, cancellationItemID,
				)
				assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))
			} else {
				assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))
				assertNoPolicyEnvelope(t, harness.egress(t, "cleanup"))
			}
			outcome := intentSettlementOutcome(t, harness)
			if outcome.Code != testCase.code ||
				(testCase.forwarded && outcome.Kind != policyelements.IntentSettlementCleanupForwarded) ||
				(!testCase.forwarded && outcome.Kind != policyelements.IntentSettlementIgnored) {
				t.Fatalf("revoked %s outcome = %+v", testCase.name, outcome)
			}
			state := intentSettlementState(t, harness)
			if state.CancellationEntries != 1 || state.TrackedIntents != 0 ||
				state.PendingIntents != 0 || state.TerminalIntents != 0 {
				t.Fatalf("revoked %s changed tombstone or settlement records: %+v", testCase.name, state)
			}
			assertNoPolicyEnvelope(t, harness.egress(t, "probe"))
			assertNoPolicyEnvelope(t, harness.egress(t, "terminal"))
		})
	}
}

func TestIntentSettlementCleanupVerifierRejectsHostileControlsAndAcceptsDelayedPrefix(t *testing.T) {
	store, evidence := settlementEvidenceFixture(t, false, true)
	harness := mountIntentSettlement(t, store, 4)
	consumeIntentSettlementStartup(t, harness)
	cancellation := policyelements.IntentSettlementCancellation{
		SessionID: "session-a", DurableIntent: *evidence.DurableIntent,
		Reason: "participant canceled",
	}
	const cancellationItemID = "cleanup-verifier-cancel"
	sendPolicy(t, harness.ingress(t, "cancel"),
		settlementCancelEnvelope(cancellationItemID, cancellation))
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)
	evidenceEnvelope := settlementEvidenceEnvelope(
		"cleanup-verifier-evidence", "session-a", 10, evidence,
	)
	sendPolicy(t, harness.ingress(t, "evidence"), evidenceEnvelope)
	validEnvelope := receivePolicy(t, harness.egress(t, "cleanup"))
	validCleanup := validEnvelope.Payload.(policyelements.IntentSettlementCleanup)
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)
	harness.stop(t)

	config := settlementExpectedConfig()
	if err := policyelements.VerifyIntentSettlementCleanup(
		store.Snapshot(), validEnvelope, validCleanup, config,
	); err != nil {
		t.Fatalf("verify canonical cleanup: %v", err)
	}
	// Cleanup evidence is immutable historical authority. A later final user
	// intent must not invalidate it; cancellation is still checked against this
	// complete current snapshot.
	appendTemporalItems(t, store, temporalIntent("cleanup-newer-intent", "cleanup-newer-event", 40, 6))
	current := store.Snapshot()
	if err := policyelements.VerifyIntentSettlementCleanup(
		current, validEnvelope, validCleanup, config,
	); err != nil {
		t.Fatalf("delayed cleanup against current snapshot: %v", err)
	}

	for _, testCase := range []struct {
		name   string
		mutate func(*element.Envelope, *policyelements.IntentSettlementCleanup)
	}{
		{name: "envelope type", mutate: func(envelope *element.Envelope, _ *policyelements.IntentSettlementCleanup) {
			envelope.Type = policyelements.AdmittedTemporalEvidenceType()
		}},
		{name: "item ID digest", mutate: func(envelope *element.Envelope, _ *policyelements.IntentSettlementCleanup) {
			envelope.ItemID += "-forged"
		}},
		{name: "sequence", mutate: func(envelope *element.Envelope, _ *policyelements.IntentSettlementCleanup) {
			envelope.Sequence++
		}},
		{name: "source", mutate: func(envelope *element.Envelope, _ *policyelements.IntentSettlementCleanup) {
			envelope.SourceID = "foreign-settlement"
		}},
		{name: "cross session envelope", mutate: func(envelope *element.Envelope, _ *policyelements.IntentSettlementCleanup) {
			envelope.SessionID = "session-b"
		}},
		{name: "cancellation scope", mutate: func(envelope *element.Envelope, _ *policyelements.IntentSettlementCleanup) {
			envelope.CancellationScope = "other-intent"
		}},
		{name: "evidence item identity", mutate: func(_ *element.Envelope, cleanup *policyelements.IntentSettlementCleanup) {
			cleanup.EvidenceItemID = "other-evidence"
		}},
		{name: "cancellation item identity", mutate: func(_ *element.Envelope, cleanup *policyelements.IntentSettlementCleanup) {
			cleanup.CancellationItemID = "other-cancel"
		}},
		{name: "missing evidence parent", mutate: func(envelope *element.Envelope, _ *policyelements.IntentSettlementCleanup) {
			envelope.CausalParents = []string{cancellationItemID}
		}},
		{name: "missing cancellation parent", mutate: func(envelope *element.Envelope, _ *policyelements.IntentSettlementCleanup) {
			envelope.CausalParents = []string{evidenceEnvelope.ItemID}
		}},
		{name: "duplicate parent", mutate: func(envelope *element.Envelope, _ *policyelements.IntentSettlementCleanup) {
			envelope.CausalParents = append(envelope.CausalParents, evidenceEnvelope.ItemID)
		}},
		{name: "cancellation identity", mutate: func(_ *element.Envelope, cleanup *policyelements.IntentSettlementCleanup) {
			cleanup.Cancellation.DurableIntent.TriggerItemID = "forged-intent-event"
		}},
		{name: "durable intent mismatch", mutate: func(_ *element.Envelope, cleanup *policyelements.IntentSettlementCleanup) {
			cleanup.Evidence.DurableIntent.SourceRevision++
		}},
		{name: "evidence prefix", mutate: func(_ *element.Envelope, cleanup *policyelements.IntentSettlementCleanup) {
			cleanup.Evidence.Prefix.Digest = "sha256:" + strings.Repeat("0", 64)
			cleanup.Evidence.TriggerCommit.Context.Prefix = cleanup.Evidence.Prefix
		}},
		{name: "evidence trigger", mutate: func(_ *element.Envelope, cleanup *policyelements.IntentSettlementCleanup) {
			cleanup.Evidence.TriggerObservation.TriggerItemID = "forged-consequence-event"
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			envelope, cleanup := cloneSettlementCleanupControl(validEnvelope, validCleanup)
			testCase.mutate(&envelope, &cleanup)
			envelope.Payload = cleanup
			if err := policyelements.VerifyIntentSettlementCleanup(
				current, envelope, cleanup, config,
			); err == nil {
				t.Fatalf("cleanup verifier accepted hostile %s", testCase.name)
			}
		})
	}

	for _, testCase := range []struct {
		name   string
		mutate func(*trajectory.Snapshot)
	}{
		{name: "result has both status forms", mutate: func(snapshot *trajectory.Snapshot) {
			item := settlementTrajectoryItem(t, snapshot.Items, "result-item-1")
			item.ToolResult.Error = "forged failure alongside output"
		}},
		{name: "result has no status", mutate: func(snapshot *trajectory.Snapshot) {
			item := settlementTrajectoryItem(t, snapshot.Items, "result-item-1")
			item.ToolResult.Output = nil
			item.ToolResult.Error = " "
		}},
		{name: "result invocation", mutate: func(snapshot *trajectory.Snapshot) {
			settlementTrajectoryItem(t, snapshot.Items, "result-item-1").InvocationID = "other-generation"
		}},
		{name: "result call", mutate: func(snapshot *trajectory.Snapshot) {
			settlementTrajectoryItem(t, snapshot.Items, "result-item-1").ToolResult.CallID = "other-call"
		}},
		{name: "result tool", mutate: func(snapshot *trajectory.Snapshot) {
			settlementTrajectoryItem(t, snapshot.Items, "result-item-1").ToolResult.Name = "computer.type"
		}},
		{name: "result parent lineage", mutate: func(snapshot *trajectory.Snapshot) {
			settlementTrajectoryItem(t, snapshot.Items, "result-item-1").CausalParentIDs = []string{"intent-1"}
		}},
		{name: "consequence parent lineage", mutate: func(snapshot *trajectory.Snapshot) {
			settlementTrajectoryItem(t, snapshot.Items, "post-screen").CausalParentIDs = []string{"intent-1"}
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			forgedSnapshot := trajectory.Snapshot{
				Version: current.Version, Items: slices.Clone(current.Items),
			}
			// Clone nested trajectory payloads before mutating the defensive test
			// snapshot; different hostile cases must remain independent.
			payload, err := json.Marshal(forgedSnapshot)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(payload, &forgedSnapshot); err != nil {
				t.Fatal(err)
			}
			testCase.mutate(&forgedSnapshot)
			identity, err := trajectory.IdentifyPrefix(forgedSnapshot, validCleanup.Evidence.Prefix.Version)
			if err != nil {
				t.Fatal(err)
			}
			envelope, cleanup := cloneSettlementCleanupControl(validEnvelope, validCleanup)
			cleanup.Evidence.Prefix = identity
			cleanup.Evidence.TriggerCommit.Context.Prefix = identity
			envelope.Payload = cleanup
			if err := policyelements.VerifyIntentSettlementCleanup(
				forgedSnapshot, envelope, cleanup, config,
			); err == nil {
				t.Fatalf("cleanup verifier accepted hostile %s", testCase.name)
			}
		})
	}
}

func TestIntentSettlementNewIntentWaitsForExactRetirementAcknowledgement(t *testing.T) {
	store, firstEvidence := settlementEvidenceFixture(t, false, true)
	harness := mountIntentSettlement(t, store, 4)
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"first-intent", "session-a", 10, firstEvidence,
	))
	firstProbe := intentSettlementProbe(t, harness)
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)

	appendTemporalItems(t, store,
		temporalIntent("intent-2", "intent-2-event", 40, 6),
		temporalObserver("intent-2-screen", "intent-2-screen-event", "vision", "screen", 50, 7, "intent-2"),
	)
	secondEvidence := admitSettlementEvidence(t, store, 7)
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"second-intent", "session-a", 11, secondEvidence,
	))
	retirement := intentSettlementDecision(t, harness)
	if retirement.Kind != policyelements.IntentSettlementDecisionSuperseded ||
		retirement.SupersedingIntent == nil ||
		*retirement.SupersedingIntent != *secondEvidence.DurableIntent ||
		retirement.Probe.ProbeID != firstProbe.ProbeID {
		t.Fatalf("supersession decision = %+v", retirement)
	}
	if err := policyelements.VerifyIntentSettlementDecision(
		store.Snapshot(), retirement, settlementExpectedConfig(),
	); err != nil {
		t.Fatalf("verify supersession decision: %v", err)
	}
	missingWitness := retirement
	missingWitness.SupersedingIntent = nil
	if err := policyelements.VerifyIntentSettlementDecision(
		store.Snapshot(), missingWitness, settlementExpectedConfig(),
	); err == nil {
		t.Fatal("settlement decision verifier accepted supersession without newer authority")
	}
	if outcome := intentSettlementOutcome(t, harness); outcome.Code != "prior_intent_superseded" {
		t.Fatalf("supersession outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))

	ack := policyelements.IntentSettlementAcknowledgement{
		Decision: retirement, GenerationID: retirement.InvocationID,
		AcknowledgedNS: retirement.FinishedNS + 1,
	}
	sendPolicy(t, harness.ingress(t, "ack"), settlementAckEnvelope("retirement-ack", ack))
	if outcome := intentSettlementOutcome(t, harness); outcome.Code != "superseded" {
		t.Fatalf("retirement ack outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	admitted := receivePolicy(t, harness.egress(t, "admitted"))
	if !reflect.DeepEqual(admitted.Payload, secondEvidence) {
		t.Fatalf("replacement admission = %+v", admitted)
	}
	if outcome := intentSettlementOutcome(t, harness); outcome.Code != "not_post_effect_candidate" {
		t.Fatalf("replacement outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.TrackedIntents != 0 {
		t.Fatalf("replacement state = %+v", state)
	}
}

func TestIntentSettlementNewIntentQueuedAfterTerminalDecisionRunsAfterAck(t *testing.T) {
	store, firstEvidence := settlementEvidenceFixture(t, false, true)
	harness := mountIntentSettlement(t, store, 4)
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"first-terminal-intent", "session-a", 10, firstEvidence,
	))
	probe := intentSettlementProbe(t, harness)
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)
	sendPolicy(t, harness.ingress(t, "disposition"), settlementDispositionEnvelope(
		"first-terminal-disposition",
		settlementDisposition(probe, policyelements.IntentDispositionSucceeded),
	))
	decision := intentSettlementDecision(t, harness)
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)

	appendTemporalItems(t, store,
		temporalIntent("intent-after-terminal", "intent-after-terminal-event", 40, 6),
		temporalObserver(
			"screen-after-terminal", "screen-after-terminal-event", "vision", "screen",
			50, 7, "intent-after-terminal",
		),
	)
	replacementEvidence := admitSettlementEvidence(t, store, 7)
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"queued-after-terminal", "session-a", 11, replacementEvidence,
	))
	if outcome := intentSettlementOutcome(t, harness); outcome.Code != "prior_intent_ack_pending" ||
		outcome.Kind != policyelements.IntentSettlementHeld {
		t.Fatalf("queued replacement outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	assertNoPolicyEnvelope(t, harness.egress(t, "admitted"))

	ack := policyelements.IntentSettlementAcknowledgement{
		Decision: decision, GenerationID: decision.InvocationID,
		AcknowledgedNS: decision.FinishedNS + 1,
	}
	sendPolicy(t, harness.ingress(t, "ack"), settlementAckEnvelope("terminal-replacement-ack", ack))
	if outcome := intentSettlementOutcome(t, harness); outcome.Code != "terminal_acknowledged_replaced" ||
		outcome.Kind != policyelements.IntentSettlementReset {
		t.Fatalf("terminal replacement ack outcome = %+v", outcome)
	}
	_ = intentSettlementState(t, harness)
	admitted := receivePolicy(t, harness.egress(t, "admitted"))
	if !reflect.DeepEqual(admitted.Payload, replacementEvidence) {
		t.Fatalf("post-terminal replacement admission = %+v", admitted)
	}
	if outcome := intentSettlementOutcome(t, harness); outcome.Code != "not_post_effect_candidate" {
		t.Fatalf("post-terminal replacement outcome = %+v", outcome)
	}
	if state := intentSettlementState(t, harness); state.TrackedIntents != 0 {
		t.Fatalf("post-terminal replacement state = %+v", state)
	}
}

func mountIntentSettlement(t *testing.T, store *trajectory.Store, maximum int) policyHarness {
	t.Helper()
	var clock atomic.Uint64
	return mountIntentSettlementWithClock(t, store, maximum, func() uint64 {
		return clock.Add(100)
	})
}

func mountChainedIntentSettlement(
	t *testing.T, store *trajectory.Store, maximum int,
) policyHarness {
	t.Helper()
	registry := graphruntime.NewRegistry()
	if err := policyelements.RegisterFactories(registry); err != nil {
		t.Fatal(err)
	}
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(stateelements.TrajectoryStoreService,
		&stateelements.TrajectoryStoreServiceValue{Store: store, SessionID: "session-a"}); err != nil {
		t.Fatal(err)
	}
	var clock atomic.Uint64
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph:    compilePolicySource(t, "chained-intent-settlement-test.ortg", []byte(chainedIntentSettlementGraph)),
		Registry: registry,
		Services: services,
		Values: map[string]json.RawMessage{
			"first":  settlementConfigJSON(t, maximum),
			"second": settlementConfigJSON(t, maximum),
		},
		Now: func() uint64 { return clock.Add(100) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	return policyHarness{mounted: mounted, done: done, cancel: cancel}
}

func mountIntentSettlementWithClock(
	t *testing.T, store *trajectory.Store, maximum int, now func() uint64,
) policyHarness {
	return mountIntentSettlementWithInstance(t, store, maximum, now, "settlement")
}

func mountIntentSettlementWithInstance(
	t *testing.T, store *trajectory.Store, maximum int, now func() uint64, instance string,
) policyHarness {
	t.Helper()
	registry := graphruntime.NewRegistry()
	if err := policyelements.RegisterFactories(registry); err != nil {
		t.Fatal(err)
	}
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(stateelements.TrajectoryStoreService,
		&stateelements.TrajectoryStoreServiceValue{Store: store, SessionID: "session-a"}); err != nil {
		t.Fatal(err)
	}
	source := strings.Replace(intentSettlementGraph, " :: settlement;", " :: "+instance+";", 1)
	source = strings.ReplaceAll(source, "settlement.", instance+".")
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph:    compilePolicySource(t, "intent-settlement-test.ortg", []byte(source)),
		Registry: registry, Services: services,
		Values: map[string]json.RawMessage{instance: settlementConfigJSON(t, maximum)},
		Now:    now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	return policyHarness{mounted: mounted, done: done, cancel: cancel}
}

func settlementConfigJSON(t *testing.T, maximum int) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(policyelements.IntentSettlementConfig{
		ExpectedAdmission: policyelements.TemporalEvidenceAdmissionConfig{
			Mode:      policyelements.TemporalEvidenceAdmissionAfterIntent,
			SourceSet: policyelements.TemporalEvidenceSourceSetExplicit,
			Required:  []policyelements.TemporalEvidenceRequirement{{Observer: "vision", Source: "screen"}},
		},
		CandidateSources:  []policyelements.TemporalEvidenceRequirement{{Observer: "vision", Source: "screen"}},
		Detector:          settlementDetector,
		MaxTrackedIntents: maximum,
		CancelMemory:      maximum,
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func settlementExpectedConfig() policyelements.IntentSettlementConfig {
	return policyelements.IntentSettlementConfig{
		ExpectedAdmission: policyelements.TemporalEvidenceAdmissionConfig{
			Mode:      policyelements.TemporalEvidenceAdmissionAfterIntent,
			SourceSet: policyelements.TemporalEvidenceSourceSetExplicit,
			Required: []policyelements.TemporalEvidenceRequirement{
				{Observer: "vision", Source: "screen"},
			},
		},
		CandidateSources: []policyelements.TemporalEvidenceRequirement{
			{Observer: "vision", Source: "screen"},
		},
		Detector: settlementDetector,
	}
}

func settlementEvidenceFixture(
	t *testing.T, failedResult, resultLinked bool,
) (*trajectory.Store, policyelements.AdmittedTemporalEvidence) {
	t.Helper()
	store := trajectory.NewStore()
	items := []trajectory.Item{
		temporalObserver("old-screen", "old-screen-event", "vision", "screen", 10, 1),
		temporalIntent("intent-1", "intent-1-event", 20, 2),
	}
	if resultLinked || failedResult {
		items = append(items,
			trajectory.Item{
				ID: "call-item-1", Kind: trajectory.KindToolCall, MonotonicNS: 3,
				CausalParentIDs: []string{"intent-1"}, InvocationID: "generation-1",
				Producer: trajectory.Producer{Phase: trajectory.PhaseFast},
				ToolCall: &trajectory.ToolCall{
					CallID: "call-1", Name: "computer.click", Arguments: json.RawMessage(`{"x":1}`),
				},
			},
		)
		result := trajectory.Item{
			ID: "result-item-1", Kind: trajectory.KindToolResult, MonotonicNS: 4,
			CausalParentIDs: []string{"call-item-1"}, InvocationID: "generation-1",
			Producer: trajectory.Producer{Phase: trajectory.PhaseTool},
			ToolResult: &trajectory.ToolResult{
				CallID: "call-1", Name: "computer.click", Output: json.RawMessage(`{"ok":true}`),
			},
		}
		if failedResult {
			result.ToolResult.Output = nil
			result.ToolResult.Error = "target rejected the effect"
		}
		items = append(items, result)
	}
	parents := []string{"intent-1"}
	if resultLinked || failedResult {
		parents = append(parents, "result-item-1")
	}
	items = append(items, temporalObserver(
		"post-screen", "post-screen-event", "vision", "screen", 30,
		uint64(len(items)+1), parents...,
	))
	appendTemporalItems(t, store, items...)
	return store, admitSettlementEvidence(t, store, store.Snapshot().Version)
}

func admitSettlementEvidence(
	t *testing.T, store *trajectory.Store, version uint64,
) policyelements.AdmittedTemporalEvidence {
	t.Helper()
	harness := mountTemporalEvidence(t, store,
		`{"mode":"after_intent","source_set":"explicit","required":[{"observer":"vision","source":"screen"}]}`)
	commit := temporalCommit(t, store, version)
	sendPolicy(t, harness.ingress(t, "committed"), temporalCommitEnvelope("settlement-source", commit))
	evidence := receivePolicy(t, harness.egress(t, "admitted")).Payload.(policyelements.AdmittedTemporalEvidence)
	_ = temporalOutcome(t, harness)
	harness.stop(t)
	return evidence
}

func settlementEvidenceEnvelope(
	itemID, sessionID string, sequence uint64, evidence policyelements.AdmittedTemporalEvidence,
) element.Envelope {
	return element.Envelope{
		Type: policyelements.AdmittedTemporalEvidenceType(), ItemID: itemID,
		SessionID: sessionID, Sequence: sequence, Payload: evidence,
	}
}

func assertIntentSettlementCleanup(
	t *testing.T, envelope element.Envelope,
	evidence policyelements.AdmittedTemporalEvidence,
	cancellation policyelements.IntentSettlementCancellation,
	evidenceItemID, cancellationItemID string,
) {
	t.Helper()
	cleanup, ok := envelope.Payload.(policyelements.IntentSettlementCleanup)
	if !ok || !envelope.Type.Equal(policyelements.IntentSettlementCleanupType()) {
		t.Fatalf("settlement cleanup has payload/type %T/%s", envelope.Payload, envelope.Type.String())
	}
	if !reflect.DeepEqual(cleanup.Evidence, evidence) ||
		!reflect.DeepEqual(cleanup.Cancellation, cancellation) ||
		cleanup.CancellationItemID != cancellationItemID ||
		envelope.CancellationScope != cancellation.DurableIntent.TrajectoryItemID ||
		!slices.Contains(envelope.CausalParents, evidenceItemID) ||
		!slices.Contains(envelope.CausalParents, cancellationItemID) {
		t.Fatalf("settlement cleanup = %+v envelope=%+v", cleanup, envelope)
	}
}

func cloneSettlementCleanupControl(
	envelope element.Envelope, cleanup policyelements.IntentSettlementCleanup,
) (element.Envelope, policyelements.IntentSettlementCleanup) {
	result := envelope.Clone()
	cleanup.Evidence.QualifyingObservations = slices.Clone(cleanup.Evidence.QualifyingObservations)
	if cleanup.Evidence.DurableIntent != nil {
		intent := *cleanup.Evidence.DurableIntent
		cleanup.Evidence.DurableIntent = &intent
	}
	result.Payload = cleanup
	return result, cleanup
}

func settlementTrajectoryItem(
	t *testing.T, items []trajectory.Item, itemID string,
) *trajectory.Item {
	t.Helper()
	for index := range items {
		if items[index].ID == itemID {
			return &items[index]
		}
	}
	t.Fatalf("trajectory fixture has no item %q", itemID)
	return nil
}

func settlementDisposition(
	probe policyelements.IntentSettlementProbe, kind policyelements.IntentDispositionKind,
) policyelements.IntentDisposition {
	return policyelements.IntentDisposition{
		Probe: probe, Detector: settlementDetector, Kind: kind,
		DecisionStartedNS: probe.IssuedNS, DecisionFinishedNS: probe.IssuedNS + 1,
	}
}

func settlementDispositionEnvelope(
	itemID string, disposition policyelements.IntentDisposition,
) element.Envelope {
	return element.Envelope{
		Type: policyelements.IntentDispositionType(), ItemID: itemID,
		SessionID: disposition.Probe.SessionID, SourceID: disposition.Detector.Reference,
		CausalParents: []string{disposition.Probe.ProbeID}, Payload: disposition,
	}
}

func settlementAckEnvelope(
	itemID string, acknowledgement policyelements.IntentSettlementAcknowledgement,
) element.Envelope {
	return element.Envelope{
		Type: policyelements.IntentSettlementAcknowledgementType(), ItemID: itemID,
		SessionID:         acknowledgement.Decision.SessionID,
		RunID:             acknowledgement.Decision.InvocationID,
		CancellationScope: acknowledgement.Decision.InvocationID,
		CausalParents:     []string{acknowledgement.Decision.TerminalID},
		Payload:           acknowledgement,
	}
}

func settlementCancelEnvelope(
	itemID string, cancellation policyelements.IntentSettlementCancellation,
) element.Envelope {
	return element.Envelope{
		Type: policyelements.IntentSettlementCancelType(), ItemID: itemID,
		SessionID:         cancellation.SessionID,
		CancellationScope: cancellation.DurableIntent.TrajectoryItemID,
		Payload:           cancellation,
	}
}

func consumeIntentSettlementStartup(t *testing.T, harness policyHarness) {
	t.Helper()
	state := intentSettlementState(t, harness)
	if state.Revision != 0 || state.TrackedIntents != 0 || state.MaxTrackedIntents == 0 {
		t.Fatalf("intent settlement startup state = %+v", state)
	}
	assertIntentSettlementLiveResolution(t, harness.mounted)
}

func intentSettlementProbe(t *testing.T, harness policyHarness) policyelements.IntentSettlementProbe {
	t.Helper()
	_, probe := intentSettlementProbeEnvelope(t, harness)
	return probe
}

func intentSettlementProbeEnvelope(
	t *testing.T, harness policyHarness,
) (element.Envelope, policyelements.IntentSettlementProbe) {
	t.Helper()
	envelope := receivePolicy(t, harness.egress(t, "probe"))
	probe, ok := envelope.Payload.(policyelements.IntentSettlementProbe)
	if !ok {
		t.Fatalf("intent settlement probe payload = %T", envelope.Payload)
	}
	return envelope, probe
}

func intentSettlementDecision(
	t *testing.T, harness policyHarness,
) policyelements.IntentSettlementDecision {
	t.Helper()
	_, decision := intentSettlementDecisionEnvelope(t, harness)
	return decision
}

func intentSettlementDecisionEnvelope(
	t *testing.T, harness policyHarness,
) (element.Envelope, policyelements.IntentSettlementDecision) {
	t.Helper()
	envelope := receivePolicy(t, harness.egress(t, "terminal"))
	decision, ok := envelope.Payload.(policyelements.IntentSettlementDecision)
	if !ok {
		t.Fatalf("intent settlement decision payload = %T", envelope.Payload)
	}
	return envelope, decision
}

func assertIntentSettlementLineage(
	t *testing.T, envelope element.Envelope, triggerItemID, probeID string,
) {
	t.Helper()
	if !slices.Contains(envelope.CausalParents, triggerItemID) ||
		!slices.Contains(envelope.CausalParents, probeID) {
		t.Fatalf(
			"settlement output %q lineage = %v, want trigger %q and probe %q",
			envelope.ItemID, envelope.CausalParents, triggerItemID, probeID,
		)
	}
}

func intentSettlementOutcome(
	t *testing.T, harness policyHarness,
) policyelements.IntentSettlementOutcome {
	t.Helper()
	_, outcome := intentSettlementOutcomeEnvelope(t, harness)
	return outcome
}

func intentSettlementOutcomeEnvelope(
	t *testing.T, harness policyHarness,
) (element.Envelope, policyelements.IntentSettlementOutcome) {
	t.Helper()
	envelope := receivePolicy(t, harness.egress(t, "outcome"))
	outcome, ok := envelope.Payload.(policyelements.IntentSettlementOutcome)
	if !ok {
		t.Fatalf("intent settlement outcome payload = %T", envelope.Payload)
	}
	return envelope, outcome
}

func intentSettlementState(t *testing.T, harness policyHarness) policyelements.IntentSettlementState {
	t.Helper()
	envelope := receivePolicy(t, harness.egress(t, "state"))
	state, ok := envelope.Payload.(policyelements.IntentSettlementState)
	if !ok {
		t.Fatalf("intent settlement state payload = %T", envelope.Payload)
	}
	return state
}

func assertIntentSettlementLiveResolution(t *testing.T, mounted *graphruntime.Mounted) {
	t.Helper()
	resolution := mounted.Live().Nodes["settlement"].Resolution
	if resolution == nil || resolution.RuntimeEvidence != inspect.EvidenceLive ||
		resolution.Runtime.ID != "builtin://openrealtime/elements/policy.IntentSettlement" ||
		resolution.Runtime.Revision != "implementation:2" ||
		resolution.CapabilitiesEvidence != inspect.EvidenceLive || len(resolution.Capabilities) != 0 {
		t.Fatalf("intent settlement live resolution = %+v", resolution)
	}
}

func TestIntentSettlementProbeEvidenceIsDefensivelyCloned(t *testing.T) {
	store, evidence := settlementEvidenceFixture(t, false, true)
	harness := mountIntentSettlement(t, store, 4)
	defer harness.stop(t)
	consumeIntentSettlementStartup(t, harness)
	original := *evidence.DurableIntent
	sendPolicy(t, harness.ingress(t, "evidence"), settlementEvidenceEnvelope(
		"clone-evidence", "session-a", 10, evidence,
	))
	probe := intentSettlementProbe(t, harness)
	_ = intentSettlementOutcome(t, harness)
	_ = intentSettlementState(t, harness)
	evidence.DurableIntent.TrajectoryItemID = "caller-mutated"
	evidence.QualifyingObservations[0].TrajectoryItemID = "caller-mutated"
	if probe.Evidence.DurableIntent == nil || *probe.Evidence.DurableIntent != original ||
		slices.ContainsFunc(probe.Evidence.QualifyingObservations, func(identity policyelements.TemporalEvidenceItemIdentity) bool {
			return identity.TrajectoryItemID == "caller-mutated"
		}) {
		t.Fatalf("probe aliases caller-owned evidence: %+v", probe)
	}
}
