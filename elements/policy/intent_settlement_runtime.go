package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	intentSettlementRuntimeID          = "builtin://openrealtime/elements/policy.IntentSettlement"
	intentSettlementRuntimeRevision    = "implementation:2"
	intentSettlementInvalidInputItemID = "intent-settlement-invalid-input"
)

type intentSettlementFactory struct{}

var (
	_ element.Factory         = intentSettlementFactory{}
	_ element.ConfigValidator = intentSettlementFactory{}
	_ element.Runnable        = (*intentSettlementRunner)(nil)
)

func (intentSettlementFactory) Descriptor() element.Descriptor {
	return IntentSettlementDescriptor()
}

func (intentSettlementFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeIntentSettlementConfig(source)
	return err
}

func (intentSettlementFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	if err := validatePolicyIdentifier("intent settlement instance ID", mount.InstanceID, true); err != nil {
		return nil, fmt.Errorf("policy.IntentSettlement instance ID: %w", err)
	}
	config, err := decodeIntentSettlementConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("policy.IntentSettlement %s config: %w", mount.InstanceID, err)
	}
	storeValue, _, found := mount.Services.Lookup(stateelements.TrajectoryStoreService)
	if !found {
		return nil, fmt.Errorf("policy.IntentSettlement %s has no canonical trajectory store", mount.InstanceID)
	}
	storeService, ok := storeValue.(*stateelements.TrajectoryStoreServiceValue)
	if !ok || storeService == nil || storeService.Store == nil {
		return nil, fmt.Errorf("canonical trajectory store service has type %T", storeValue)
	}
	if err := validatePolicyIdentifier(
		"canonical trajectory store session ID", storeService.SessionID, true,
	); err != nil {
		return nil, fmt.Errorf("policy.IntentSettlement %s: %w", mount.InstanceID, err)
	}
	clockValue, _, found := mount.Services.Lookup(graphruntime.ClockServiceName)
	if !found {
		return nil, errors.New("intent settlement has no runtime clock service")
	}
	clock, ok := clockValue.(graphruntime.Clock)
	if !ok || clock == nil {
		return nil, fmt.Errorf("runtime clock service has type %T", clockValue)
	}
	sequenceValue, _, found := mount.Services.Lookup(graphruntime.SequenceServiceName)
	if !found {
		return nil, errors.New("intent settlement has no runtime sequence service")
	}
	sequences, ok := sequenceValue.(*graphruntime.SequenceAllocator)
	if !ok || sequences == nil {
		return nil, fmt.Errorf("runtime sequence service has type %T", sequenceValue)
	}
	ports, err := intentSettlementPortsFrom(mount.Ports)
	if err != nil {
		return nil, err
	}
	return &intentSettlementRunner{
		instance: mount.InstanceID, sessionID: storeService.SessionID,
		config: config, store: storeService.Store,
		clock: clock, sequences: sequences, resolution: mount.Resolution, ports: ports,
		records:      make(map[intentSettlementKey]*intentSettlementRecord),
		revocations:  make(map[intentSettlementKey]intentSettlementRevocation),
		acknowledged: make(map[string]struct{}),
		state: IntentSettlementState{
			MaxTrackedIntents: config.MaxTrackedIntents, CancellationMemory: config.CancelMemory,
		},
	}, nil
}

type intentSettlementPorts struct {
	evidence, disposition, ack, reset, cancel          element.InputPort
	admitted, cleanup, probe, terminal, state, outcome element.OutputPort
}

func intentSettlementPortsFrom(ports element.Ports) (intentSettlementPorts, error) {
	if ports == nil {
		return intentSettlementPorts{}, errors.New("policy.IntentSettlement has nil ports")
	}
	var result intentSettlementPorts
	for _, entry := range []struct {
		name string
		port *element.InputPort
	}{
		{"evidence", &result.evidence}, {"disposition", &result.disposition}, {"ack", &result.ack},
		{"reset", &result.reset}, {"cancel", &result.cancel},
	} {
		port, err := ports.Input(entry.name)
		if err != nil {
			return intentSettlementPorts{}, err
		}
		*entry.port = port
	}
	for _, entry := range []struct {
		name string
		port *element.OutputPort
	}{
		{"admitted", &result.admitted}, {"cleanup", &result.cleanup},
		{"probe", &result.probe}, {"terminal", &result.terminal},
		{"state", &result.state}, {"outcome", &result.outcome},
	} {
		port, err := ports.Output(entry.name)
		if err != nil {
			return intentSettlementPorts{}, err
		}
		*entry.port = port
	}
	return result, nil
}

type intentSettlementKey struct {
	session string
	intent  string
}

type pendingIntentSettlement struct {
	envelope element.Envelope
	evidence AdmittedTemporalEvidence
	probe    IntentSettlementProbe
}

type intentSettlementRecord struct {
	intent             TemporalEvidenceItemIdentity
	pending            *pendingIntentSettlement
	lastDisposition    *IntentDisposition
	awaitingAck        *IntentSettlementDecision
	terminal           *IntentSettlementDecision
	replacement        *pendingIntentSettlement
	deferredRetirement *intentSettlementDeferredRetirement
}

type intentSettlementRevocation struct {
	intent       TemporalEvidenceItemIdentity
	cancellation IntentSettlementCancellation
	envelope     element.Envelope
}

type intentSettlementDecisionWitness struct {
	disposition       *IntentDisposition
	cancellation      *IntentSettlementCancellation
	reset             *IntentSettlementAddress
	supersedingIntent *TemporalEvidenceItemIdentity
}

type intentSettlementDeferredRetirement struct {
	kind         IntentSettlementDecisionKind
	itemID       string
	reset        *IntentSettlementAddress
	cancellation *IntentSettlementCancellation
}

type intentSettlementRunner struct {
	instance   string
	sessionID  string
	config     IntentSettlementConfig
	store      *trajectory.Store
	clock      graphruntime.Clock
	sequences  *graphruntime.SequenceAllocator
	resolution element.ResolutionReporter
	ports      intentSettlementPorts

	records           map[intentSettlementKey]*intentSettlementRecord
	revocations       map[intentSettlementKey]intentSettlementRevocation
	acknowledged      map[string]struct{}
	acknowledgedOrder []string
	state             IntentSettlementState
}

type intentSettlementInput struct {
	kind     string
	envelope element.Envelope
}

func (runner *intentSettlementRunner) Run(parent context.Context) error {
	if err := liveidentity.Report(runner.resolution, liveidentity.Artifact{
		ID: intentSettlementRuntimeID, Revision: intentSettlementRuntimeRevision,
	}, nil); err != nil {
		return err
	}
	// Startup is the genesis state for this actor, so it deliberately has no
	// synthetic parent derived by appending to the (bounded) instance ID.
	if err := runner.publishState(parent, element.Envelope{}); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	// Receipt on inputs is concurrent, but all state transitions are serialized
	// by this unbuffered actor handoff. Actor receipt is the linearization point:
	// two controls with no happens-before relationship may legally linearize in
	// either order, and the resulting lineage records which order won. A graph
	// requiring cancel-before-continue precedence must serialize those controls
	// in an upstream coordinator. Once a lossless admission broadcast begins it
	// cannot be retracted here; production coordination must also cancel any
	// downstream activation when cancellation linearizes after release.
	inputs := make(chan intentSettlementInput)
	failures := make(chan error, 5)
	var wait sync.WaitGroup
	for _, source := range []struct {
		kind string
		port element.InputPort
	}{
		{"evidence", runner.ports.evidence}, {"disposition", runner.ports.disposition},
		{"ack", runner.ports.ack},
		{"reset", runner.ports.reset}, {"cancel", runner.ports.cancel},
	} {
		wait.Add(1)
		go receiveIntentSettlementInputs(ctx, source.kind, source.port, inputs, failures, &wait)
	}
	defer func() {
		cancel(nil)
		wait.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			return err
		case input := <-inputs:
			var err error
			switch input.kind {
			case "evidence":
				err = runner.acceptEvidence(ctx, input.envelope)
			case "disposition":
				err = runner.acceptDisposition(ctx, input.envelope)
			case "ack":
				err = runner.acceptAcknowledgement(ctx, input.envelope)
			case "reset":
				err = runner.acceptReset(ctx, input.envelope)
			case "cancel":
				err = runner.acceptCancel(ctx, input.envelope)
			default:
				err = fmt.Errorf("unknown intent-settlement input %q", input.kind)
			}
			if err != nil {
				return err
			}
		}
	}
}

func (runner *intentSettlementRunner) acceptEvidence(ctx context.Context, envelope element.Envelope) error {
	evidence, ok := intentSettlementEvidencePayload(envelope.Payload)
	if !ok || !envelope.Type.Equal(AdmittedTemporalEvidenceType()) {
		return runner.refuse(ctx, envelope, "evidence", "invalid_evidence",
			fmt.Sprintf("evidence payload has type %T", envelope.Payload), IntentSettlementProbe{})
	}
	if err := validateIntentSettlementInputEnvelope("evidence", envelope); err != nil {
		return runner.refuse(ctx, envelope, "evidence", "invalid_envelope", err.Error(), IntentSettlementProbe{})
	}
	if err := preflightIntentSettlementEvidence(evidence); err != nil {
		return runner.refuse(ctx, envelope, "evidence", "invalid_evidence", err.Error(), IntentSettlementProbe{})
	}
	if err := verifyIntentSettlementReleasedAdmissionEnvelope(envelope, evidence); err != nil {
		return runner.refuse(ctx, envelope, "evidence", "invalid_envelope", err.Error(), IntentSettlementProbe{})
	}
	if err := runner.requireMountedSession(envelope.SessionID); err != nil {
		return runner.refuse(ctx, envelope, "evidence", "session_mismatch", err.Error(), IntentSettlementProbe{})
	}
	evidence = cloneIntentSettlementEvidence(evidence)
	snapshot := runner.store.Snapshot()
	if err := VerifyAdmittedTemporalEvidence(snapshot, evidence, runner.config.ExpectedAdmission); err != nil {
		return runner.refuse(ctx, envelope, "evidence", "invalid_temporal_evidence", err.Error(), IntentSettlementProbe{})
	}
	if err := validateIntentSettlementEvidenceProjection(evidence); err != nil {
		return runner.refuse(ctx, envelope, "evidence", "invalid_temporal_evidence", err.Error(), IntentSettlementProbe{})
	}
	if evidence.DurableIntent == nil {
		return runner.refuse(ctx, envelope, "evidence", "missing_durable_intent",
			"settlement evidence has no durable intent", IntentSettlementProbe{})
	}
	key := intentSettlementKey{session: envelope.SessionID, intent: evidence.DurableIntent.TrajectoryItemID}
	if revoked, found := runner.revocations[key]; found {
		if revoked.intent != *evidence.DurableIntent {
			return runner.refuse(ctx, envelope, "evidence", "revocation_identity_conflict",
				"the canceled intent item ID resolves to a different canonical identity",
				IntentSettlementProbe{})
		}
		successful, effectConsequence, err := intentSettlementCanceledEffectConsequence(
			snapshot, evidence, runner.config.CandidateSources,
		)
		if err != nil {
			return runner.refuse(ctx, envelope, "evidence", "invalid_revoked_effect_evidence",
				err.Error(), IntentSettlementProbe{})
		}
		if effectConsequence {
			// Once an intent is canceled, even a successful canonical effect must
			// not enter disposition policy or resume cognition. Forward only its
			// independently verified, directly linked consequence so downstream
			// activation can retire the matching canceled-effect race record. Keep
			// the exact intent revocation in this gate: admission here is cleanup
			// authority, not permission to resume cognition.
			code := "canceled_failed_effect_cleanup"
			message := "verified failed-effect consequence forwarded only for canceled bookkeeping cleanup"
			if successful {
				code = "canceled_successful_effect_cleanup"
				message = "verified successful-effect consequence forwarded only for canceled bookkeeping cleanup"
			}
			runner.state.Cleanups++
			before := runner.state.Revision
			runner.bumpState()
			cleanup := IntentSettlementCleanup{
				Evidence:           cloneIntentSettlementEvidence(evidence),
				Cancellation:       revoked.cancellation,
				EvidenceItemID:     envelope.ItemID,
				CancellationItemID: revoked.envelope.ItemID,
			}
			if err := runner.publishCleanup(ctx, envelope, cleanup); err != nil {
				return err
			}
			return runner.publishTransition(ctx, envelope, IntentSettlementOutcome{
				Kind: IntentSettlementCleanupForwarded, Operation: "evidence", SessionID: envelope.SessionID,
				DurableIntentItemID:      key.intent,
				TriggerObservationItemID: evidence.TriggerObservation.TrajectoryItemID,
				Code:                     code,
				Message:                  message,
			}, before)
		}
		return runner.ignore(ctx, envelope, "evidence", "intent_revoked",
			"settlement evidence belongs to the exactly canceled durable intent",
			IntentSettlementProbe{})
	}
	record := runner.records[key]
	if record != nil && (record.terminal != nil || record.awaitingAck != nil) {
		runner.state.Suppressed++
		before := runner.state.Revision
		runner.pruneSupersededIntentRevocations(envelope.SessionID, *evidence.DurableIntent)
		runner.bumpState()
		return runner.publishTransition(ctx, envelope, IntentSettlementOutcome{
			Kind: IntentSettlementSuppressed, Operation: "evidence", SessionID: envelope.SessionID,
			DurableIntentItemID: key.intent, TriggerObservationItemID: evidence.TriggerObservation.TrajectoryItemID,
			Disposition: intentSettlementRecordDisposition(record), Code: "intent_terminal",
			Message: "later evidence for the terminal durable intent was suppressed",
		}, before)
	}
	if record != nil && record.pending != nil {
		runner.state.Suppressed++
		before := runner.state.Revision
		runner.pruneSupersededIntentRevocations(envelope.SessionID, *evidence.DurableIntent)
		runner.bumpState()
		return runner.publishTransition(ctx, envelope, IntentSettlementOutcome{
			Kind: IntentSettlementSuppressed, Operation: "evidence", SessionID: envelope.SessionID,
			DurableIntentItemID: key.intent, TriggerObservationItemID: evidence.TriggerObservation.TrajectoryItemID,
			ProbeID: record.pending.probe.ProbeID, Code: "decision_pending",
			Message: "later same-intent evidence was suppressed while the exact post-effect decision is pending",
		}, before)
	}
	result, candidate, err := intentSettlementCandidate(snapshot, evidence, runner.config.CandidateSources)
	if err != nil {
		return runner.refuse(ctx, envelope, "evidence", "invalid_post_effect_evidence", err.Error(), IntentSettlementProbe{})
	}
	var probe IntentSettlementProbe
	if candidate {
		probe, err = runner.makeProbe(envelope, evidence, result)
		if err != nil {
			return runner.refuse(ctx, envelope, "evidence", "invalid_probe", err.Error(), IntentSettlementProbe{})
		}
		if probe.ProbeID == envelope.ItemID || slices.Contains(envelope.CausalParents, probe.ProbeID) {
			return runner.refuse(ctx, envelope, "evidence", "invalid_probe",
				"settlement probe identity collides with its triggering lineage", IntentSettlementProbe{})
		}
	}
	heldForRetirement, err := runner.retireOtherIntents(ctx, envelope, evidence, key)
	if err != nil {
		return err
	}
	if heldForRetirement {
		return nil
	}
	record = runner.records[key]
	if !candidate {
		runner.state.Admitted++
		before := runner.state.Revision
		runner.pruneSupersededIntentRevocations(envelope.SessionID, *evidence.DurableIntent)
		runner.bumpState()
		if err := runner.publishAdmission(ctx, envelope, evidence); err != nil {
			return err
		}
		return runner.publishTransition(ctx, envelope, IntentSettlementOutcome{
			Kind: IntentSettlementAdmitted, Operation: "evidence", SessionID: envelope.SessionID,
			DurableIntentItemID: key.intent, TriggerObservationItemID: evidence.TriggerObservation.TrajectoryItemID,
			Code: "not_post_effect_candidate",
		}, before)
	}
	if record == nil {
		if len(runner.records) >= runner.config.MaxTrackedIntents {
			runner.state.Refused++
			before := runner.state.Revision
			runner.bumpState()
			return runner.publishTransition(ctx, envelope, IntentSettlementOutcome{
				Kind: IntentSettlementRefused, Operation: "evidence", SessionID: envelope.SessionID,
				DurableIntentItemID: key.intent, Code: "capacity_exhausted",
				Message: "intent settlement capacity is exhausted; live safety state was not evicted",
			}, before)
		}
	}
	runner.pruneSupersededIntentRevocations(envelope.SessionID, *evidence.DurableIntent)
	if record == nil {
		record = &intentSettlementRecord{intent: *evidence.DurableIntent}
		runner.records[key] = record
	}
	retained := envelope.Clone()
	retained.Payload = cloneIntentSettlementEvidence(evidence)
	record.pending = &pendingIntentSettlement{
		envelope: retained, evidence: cloneIntentSettlementEvidence(evidence), probe: probe,
	}
	runner.state.Held++
	before := runner.state.Revision
	runner.bumpState()
	if err := runner.publishProbe(ctx, envelope, probe); err != nil {
		return err
	}
	return runner.publishTransition(ctx, envelope, IntentSettlementOutcome{
		Kind: IntentSettlementHeld, Operation: "evidence", SessionID: envelope.SessionID,
		DurableIntentItemID: key.intent, TriggerObservationItemID: evidence.TriggerObservation.TrajectoryItemID,
		ResultItemID: result.TrajectoryItemID, ProbeID: probe.ProbeID, Code: "awaiting_disposition",
	}, before)
}

// intentSettlementCanceledEffectConsequence recognizes the exact result-linked
// consequence of an effect whose durable intent is already revoked. No such
// evidence may enter disposition policy, but cancellation cleanup still needs
// a rigorously authenticated safe point. This verifier therefore checks the
// same exact intent -> call -> result -> consequence lineage independently;
// callers may only use a matched result to forward already-revoked evidence
// downstream. The first return reports whether that result was successful.
func intentSettlementCanceledEffectConsequence(
	snapshot trajectory.Snapshot, evidence AdmittedTemporalEvidence,
	candidateSources []TemporalEvidenceRequirement,
) (bool, bool, error) {
	if evidence.DurableIntent == nil || evidence.Prefix.Version == 0 ||
		evidence.Prefix.Version > snapshot.Version ||
		evidence.TriggerObservation.StoreVersion == 0 ||
		evidence.TriggerObservation.StoreVersion > evidence.Prefix.Version ||
		evidence.DurableIntent.StoreVersion == 0 ||
		evidence.DurableIntent.StoreVersion > evidence.Prefix.Version {
		return false, false, errors.New("revoked effect evidence has invalid intent, trigger, or prefix position")
	}
	prefix := snapshot.Items[:evidence.Prefix.Version]
	triggerIndex := int(evidence.TriggerObservation.StoreVersion - 1)
	intentIndex := int(evidence.DurableIntent.StoreVersion - 1)
	trigger := prefix[triggerIndex]
	intent := prefix[intentIndex]
	if trigger.ID != evidence.TriggerObservation.TrajectoryItemID || trigger.Observation == nil ||
		intent.ID != evidence.DurableIntent.TrajectoryItemID || intent.Event == nil {
		return false, false, errors.New("revoked effect evidence does not name its exact canonical trigger and intent")
	}
	pair := TemporalEvidenceRequirement{
		Observer: trigger.Observation.Observer, Source: trigger.Observation.Source,
	}
	if !slices.Contains(candidateSources, pair) {
		return false, false, nil
	}
	resultIndex := -1
	for _, parentID := range trigger.CausalParentIDs {
		for index := triggerIndex - 1; index >= 0; index-- {
			item := prefix[index]
			if item.ID != parentID || item.Kind != trajectory.KindToolResult || item.ToolResult == nil {
				continue
			}
			if resultIndex != -1 {
				return false, false, errors.New("revoked effect consequence directly names multiple canonical tool results")
			}
			resultIndex = index
			break
		}
	}
	if resultIndex == -1 {
		return false, false, nil
	}
	result := prefix[resultIndex]
	successful := result.ToolResult.Error == ""
	if successful {
		if len(result.ToolResult.Output) == 0 || !json.Valid(result.ToolResult.Output) {
			return false, false, errors.New("revoked successful canonical result has no valid output")
		}
	} else {
		if strings.TrimSpace(result.ToolResult.Error) == "" {
			return false, false, errors.New("revoked failed canonical result has an empty error")
		}
		if len(result.ToolResult.Output) != 0 {
			return false, false, errors.New("revoked failed canonical result also carries successful output")
		}
	}
	for _, identity := range []struct {
		label string
		value string
	}{
		{"revoked result trajectory item ID", result.ID},
		{"revoked result invocation ID", result.InvocationID},
		{"revoked result call ID", result.ToolResult.CallID},
		{"revoked result tool", result.ToolResult.Name},
	} {
		if err := validatePolicyIdentifier(identity.label, identity.value, true); err != nil {
			return false, false, err
		}
	}
	if resultIndex <= intentIndex || !temporalCausalAncestor(prefix, intentIndex, resultIndex) {
		return false, false, errors.New("revoked result does not descend from the exact canceled intent")
	}
	matchingCalls := 0
	matchingCallIndex := -1
	for index := intentIndex + 1; index < resultIndex; index++ {
		item := prefix[index]
		if item.Kind == trajectory.KindToolCall && item.ToolCall != nil &&
			item.InvocationID == result.InvocationID &&
			item.ToolCall.CallID == result.ToolResult.CallID &&
			item.ToolCall.Name == result.ToolResult.Name {
			matchingCalls++
			matchingCallIndex = index
		}
	}
	if matchingCalls != 1 {
		return false, false, fmt.Errorf(
			"revoked result has %d exact canonical calls under the canceled intent", matchingCalls,
		)
	}
	if !temporalCausalAncestor(prefix, intentIndex, matchingCallIndex) ||
		!slices.Contains(result.CausalParentIDs, prefix[matchingCallIndex].ID) {
		return false, false, errors.New("revoked result is not a direct child of its exact intent-descended call")
	}
	consequences := 0
	consequenceIndex := -1
	for index := resultIndex + 1; index <= triggerIndex; index++ {
		item := prefix[index]
		if item.Kind != trajectory.KindObservation || item.Observation == nil ||
			item.Observation.Observer != pair.Observer || item.Observation.Source != pair.Source ||
			!slices.Contains(item.CausalParentIDs, result.ID) {
			continue
		}
		consequences++
		consequenceIndex = index
	}
	if consequences != 1 || consequenceIndex != triggerIndex {
		return false, false, fmt.Errorf(
			"revoked result has %d direct consequences on observer/source %q/%q before the trigger",
			consequences, pair.Observer, pair.Source,
		)
	}
	return successful, true, nil
}

func (runner *intentSettlementRunner) acceptDisposition(
	ctx context.Context, envelope element.Envelope,
) error {
	disposition, ok := intentDispositionPayload(envelope.Payload)
	if !ok || !envelope.Type.Equal(IntentDispositionType()) {
		return runner.refuse(ctx, envelope, "disposition", "invalid_disposition",
			fmt.Sprintf("disposition payload has type %T", envelope.Payload), IntentSettlementProbe{})
	}
	probe := disposition.Probe
	if err := validateIntentSettlementInputEnvelope("disposition", envelope); err != nil {
		return runner.refuse(ctx, envelope, "disposition", "invalid_envelope", err.Error(), probe)
	}
	if err := preflightIntentDisposition(disposition); err != nil {
		return runner.refuse(ctx, envelope, "disposition", "invalid_disposition", err.Error(), probe)
	}
	disposition = cloneIntentDisposition(disposition)
	probe = disposition.Probe
	if envelope.SessionID != runner.sessionID {
		return runner.refuse(ctx, envelope, "disposition", "session_mismatch",
			"disposition crossed the mounted trajectory session", probe)
	}
	if envelope.SourceID != disposition.Detector.Reference {
		return runner.refuse(ctx, envelope, "disposition", "detector_mismatch",
			"disposition envelope source does not match its detector reference", probe)
	}
	if envelope.SessionID == "" || envelope.SessionID != probe.SessionID {
		return runner.refuse(ctx, envelope, "disposition", "session_mismatch",
			"disposition envelope and probe sessions differ", probe)
	}
	if !slices.Contains(envelope.CausalParents, probe.ProbeID) {
		return runner.refuse(ctx, envelope, "disposition", "invalid_disposition",
			"disposition does not directly name its exact probe as a causal parent", probe)
	}
	key := intentSettlementKey{session: probe.SessionID, intent: probe.DurableIntent.TrajectoryItemID}
	record := runner.records[key]
	if record == nil {
		return runner.refuse(ctx, envelope, "disposition", "unknown_probe",
			"no settlement record exists for the disposition", probe)
	}
	if record.awaitingAck != nil {
		if record.awaitingAck.Disposition != nil &&
			reflect.DeepEqual(*record.awaitingAck.Disposition, disposition) {
			return runner.ignore(ctx, envelope, "disposition", "duplicate_terminal_disposition",
				"the terminal decision is already awaiting activation acknowledgement", probe)
		}
		return runner.refuse(ctx, envelope, "disposition", "conflicting_terminal_disposition",
			"a different terminal decision is already awaiting acknowledgement", probe)
	}
	if record.terminal != nil {
		if record.terminal.Disposition != nil &&
			reflect.DeepEqual(*record.terminal.Disposition, disposition) {
			return runner.ignore(ctx, envelope, "disposition", "duplicate_disposition",
				"the same disposition is already terminal", probe)
		}
		return runner.refuse(ctx, envelope, "disposition", "conflicting_disposition",
			"the durable intent already has a different terminal disposition", probe)
	}
	if record.pending == nil || !reflect.DeepEqual(record.pending.probe, probe) {
		return runner.refuse(ctx, envelope, "disposition", "probe_mismatch",
			"disposition does not echo the exact pending probe", probe)
	}
	if err := validateIntentDisposition(disposition, runner.config.Detector); err != nil {
		return runner.refuse(ctx, envelope, "disposition", "invalid_disposition", err.Error(), probe)
	}
	snapshot := runner.store.Snapshot()
	if err := VerifyIntentSettlementProbe(snapshot, probe, runner.config); err != nil {
		return runner.refuse(ctx, envelope, "disposition", "stale_settlement_evidence", err.Error(), probe)
	}
	if record.lastDisposition != nil {
		if reflect.DeepEqual(*record.lastDisposition, disposition) {
			return runner.ignore(ctx, envelope, "disposition", "duplicate_indeterminate_disposition",
				"the exact nonterminal disposition was already applied", probe)
		}
		if disposition.DecisionStartedNS < record.lastDisposition.DecisionFinishedNS ||
			disposition.DecisionFinishedNS <= record.lastDisposition.DecisionFinishedNS {
			return runner.refuse(ctx, envelope, "disposition", "non_monotonic_disposition",
				"disposition timing does not follow the last accepted classification", probe)
		}
	}
	pending := record.pending
	switch disposition.Kind {
	case IntentDispositionContinue:
		if envelope.ItemID == pending.envelope.ItemID {
			return runner.refuse(ctx, envelope, "disposition", "invalid_disposition",
				"continuation item ID would make the released admission self-causal", probe)
		}
		released, err := runner.makeReleasedAdmission(
			pending.envelope, envelope, probe, pending.evidence,
		)
		if err != nil {
			return runner.refuse(ctx, envelope, "disposition", "invalid_released_admission",
				err.Error(), probe)
		}
		if err := runner.publishReleasedAdmission(ctx, released); err != nil {
			return err
		}
		delete(runner.records, key)
		runner.state.Admitted++
		before := runner.state.Revision
		runner.bumpState()
		return runner.publishTransition(ctx, envelope, IntentSettlementOutcome{
			Kind: IntentSettlementAdmitted, Operation: "disposition", SessionID: probe.SessionID,
			DurableIntentItemID:      probe.DurableIntent.TrajectoryItemID,
			TriggerObservationItemID: probe.TriggerObservation.TrajectoryItemID,
			ResultItemID:             probe.Result.TrajectoryItemID, ProbeID: probe.ProbeID,
			Disposition: disposition.Kind, Code: "continue",
			Message: "the verified post-effect evidence was released to downstream activation",
		}, before)
	case IntentDispositionSucceeded, IntentDispositionFailed:
		before := runner.state.Revision
		decisionKind := intentDispositionDecisionKind(disposition.Kind)
		decision, err := runner.makeTerminalDecision(
			decisionKind, pending,
			intentSettlementDecisionWitness{disposition: &disposition}, before,
		)
		if err != nil {
			return runner.refuse(ctx, envelope, "disposition", "invalid_terminal_decision", err.Error(), probe)
		}
		record.awaitingAck = &decision
		runner.bumpState()
		if err := runner.publishTerminal(ctx, envelope, decision); err != nil {
			return err
		}
		return runner.publishTransition(ctx, envelope, IntentSettlementOutcome{
			Kind: IntentSettlementHeld, Operation: "disposition", SessionID: probe.SessionID,
			DurableIntentItemID:      probe.DurableIntent.TrajectoryItemID,
			TriggerObservationItemID: probe.TriggerObservation.TrajectoryItemID,
			ResultItemID:             probe.Result.TrajectoryItemID, ProbeID: probe.ProbeID,
			Disposition: disposition.Kind, Code: "awaiting_activation_ack",
			Message: "the terminal disposition is held until downstream activation acknowledges exact cleanup",
		}, before)
	case IntentDispositionIndeterminate:
		copy := cloneIntentDisposition(disposition)
		record.lastDisposition = &copy
		runner.state.Indeterminate++
		before := runner.state.Revision
		runner.bumpState()
		return runner.publishTransition(ctx, envelope, IntentSettlementOutcome{
			Kind: IntentSettlementHeld, Operation: "disposition", SessionID: probe.SessionID,
			DurableIntentItemID:      probe.DurableIntent.TrajectoryItemID,
			TriggerObservationItemID: probe.TriggerObservation.TrajectoryItemID,
			ResultItemID:             probe.Result.TrajectoryItemID, ProbeID: probe.ProbeID,
			Disposition: disposition.Kind, Code: "indeterminate",
			Message: "indeterminate evidence remains held and cannot be interpreted as task success",
		}, before)
	default:
		return runner.refuse(ctx, envelope, "disposition", "unknown_disposition",
			fmt.Sprintf("unknown intent disposition %q", disposition.Kind), probe)
	}
}

func (runner *intentSettlementRunner) acceptAcknowledgement(
	ctx context.Context, envelope element.Envelope,
) error {
	ack, ok := intentSettlementAcknowledgementPayload(envelope.Payload)
	if !ok || !envelope.Type.Equal(IntentSettlementAcknowledgementType()) {
		return runner.refuse(ctx, envelope, "ack", "invalid_acknowledgement",
			fmt.Sprintf("acknowledgement payload has type %T", envelope.Payload), IntentSettlementProbe{})
	}
	decision := ack.Decision
	probe := decision.Probe
	if err := validateIntentSettlementInputEnvelope("acknowledgement", envelope); err != nil {
		return runner.refuse(ctx, envelope, "ack", "invalid_envelope", err.Error(), probe)
	}
	if err := preflightIntentSettlementAcknowledgement(ack); err != nil {
		return runner.refuse(ctx, envelope, "ack", "invalid_acknowledgement", err.Error(), probe)
	}
	ack.Decision = cloneIntentSettlementDecision(ack.Decision)
	decision = ack.Decision
	probe = decision.Probe
	if envelope.SessionID != runner.sessionID {
		return runner.refuse(ctx, envelope, "ack", "session_mismatch",
			"acknowledgement crossed the mounted trajectory session", probe)
	}
	if decision.SessionID != runner.sessionID || probe.SessionID != runner.sessionID {
		return runner.refuse(ctx, envelope, "ack", "session_mismatch",
			"acknowledgement decision or probe crossed the mounted trajectory session", probe)
	}
	if ack.AcknowledgedNS == 0 || ack.AcknowledgedNS < decision.FinishedNS ||
		ack.GenerationID != decision.InvocationID || envelope.SessionID != decision.SessionID ||
		envelope.RunID != decision.InvocationID || envelope.CancellationScope != decision.InvocationID ||
		!slices.Contains(envelope.CausalParents, decision.TerminalID) {
		return runner.refuse(ctx, envelope, "ack", "invalid_acknowledgement",
			"acknowledgement has an invalid time, session, run, cancellation scope, or terminal lineage", probe)
	}
	wantTerminalID, err := intentSettlementTerminalID(decision)
	if err != nil || decision.TerminalID != wantTerminalID {
		return runner.refuse(ctx, envelope, "ack", "invalid_settlement_decision",
			"acknowledgement terminal identity does not match its decision", probe)
	}
	if _, found := runner.acknowledged[decision.TerminalID]; found {
		return runner.ignore(ctx, envelope, "ack", "duplicate_acknowledgement",
			"the exact terminal acknowledgement was already applied", probe)
	}
	key := intentSettlementKey{session: decision.SessionID, intent: probe.DurableIntent.TrajectoryItemID}
	record := runner.records[key]
	if record == nil || record.awaitingAck == nil ||
		!reflect.DeepEqual(*record.awaitingAck, decision) {
		return runner.refuse(ctx, envelope, "ack", "acknowledgement_mismatch",
			"acknowledgement does not echo the exact pending terminal decision", probe)
	}
	if err := VerifyIntentSettlementDecision(runner.store.Snapshot(), decision, runner.config); err != nil {
		return runner.refuse(ctx, envelope, "ack", "invalid_settlement_decision", err.Error(), probe)
	}
	replacement := record.replacement
	deferred := cloneIntentSettlementDeferredRetirement(record.deferredRetirement)
	retire := decision.Kind == IntentSettlementDecisionSuperseded ||
		decision.Kind == IntentSettlementDecisionReset ||
		decision.Kind == IntentSettlementDecisionCanceled || deferred != nil || replacement != nil
	if !retire && (decision.Kind == IntentSettlementDecisionSucceeded ||
		decision.Kind == IntentSettlementDecisionFailed) {
		if decision.Disposition == nil {
			return runner.refuse(ctx, envelope, "ack", "invalid_acknowledgement",
				"terminal task decision has no accepted disposition", probe)
		}
		copy := cloneIntentSettlementDecision(decision)
		record.terminal = &copy
	}
	runner.rememberAcknowledged(decision)
	record.pending = nil
	record.awaitingAck = nil
	record.replacement = nil
	record.deferredRetirement = nil
	before := runner.state.Revision
	if decision.Kind == IntentSettlementDecisionReset ||
		(deferred != nil && deferred.kind == IntentSettlementDecisionReset) {
		runner.state.Resets++
	}
	if decision.Kind == IntentSettlementDecisionCanceled ||
		(deferred != nil && deferred.kind == IntentSettlementDecisionCanceled) {
		runner.state.CancellationsCompleted++
	}
	runner.bumpState()
	if retire {
		delete(runner.records, key)
		runner.updateCounts()
		code := string(decision.Kind)
		message := "downstream activation acknowledged retirement of the old intent"
		outcomeKind := IntentSettlementReset
		if decision.Kind == IntentSettlementDecisionCanceled ||
			(deferred != nil && deferred.kind == IntentSettlementDecisionCanceled) {
			outcomeKind = IntentSettlementCanceled
		}
		if deferred != nil && deferred.kind == IntentSettlementDecisionCanceled &&
			decision.Kind != IntentSettlementDecisionCanceled {
			code = "canceled_after_terminal_ack"
			message = "downstream activation acknowledged the exact effect before cancellation retired the intent"
		} else if deferred != nil && deferred.kind == IntentSettlementDecisionReset &&
			decision.Kind != IntentSettlementDecisionReset {
			code = "reset_after_terminal_ack"
			message = "downstream activation acknowledged the exact effect before reset retired the intent"
		} else if replacement != nil &&
			decision.Kind != IntentSettlementDecisionSuperseded &&
			decision.Kind != IntentSettlementDecisionReset &&
			decision.Kind != IntentSettlementDecisionCanceled {
			code = "terminal_acknowledged_replaced"
			message = "downstream activation cleared the terminal effect; the newer intent may now proceed"
		}
		transitionCause := envelope.Clone()
		transitionCause.CausalParents = appendUnique(
			transitionCause.CausalParents, decision.TerminalID,
		)
		if deferred != nil {
			transitionCause.CausalParents = appendUnique(
				transitionCause.CausalParents, deferred.itemID,
			)
		}
		if err := runner.publishTransition(ctx, transitionCause, IntentSettlementOutcome{
			Kind: outcomeKind, Operation: "ack", SessionID: decision.SessionID,
			DurableIntentItemID: key.intent, TriggerObservationItemID: probe.TriggerObservation.TrajectoryItemID,
			ResultItemID: probe.Result.TrajectoryItemID, ProbeID: probe.ProbeID,
			Code: code, Message: message,
		}, before); err != nil {
			return err
		}
		if replacement != nil {
			return runner.acceptEvidence(ctx, replacement.envelope)
		}
		return nil
	}
	return runner.publishTransition(ctx, envelope, IntentSettlementOutcome{
		Kind: IntentSettlementSuppressed, Operation: "ack", SessionID: decision.SessionID,
		DurableIntentItemID: key.intent, TriggerObservationItemID: probe.TriggerObservation.TrajectoryItemID,
		ResultItemID: probe.Result.TrajectoryItemID, ProbeID: probe.ProbeID,
		Disposition: decision.Disposition.Kind, Code: "terminal_acknowledged",
		Message: "downstream activation cleared the exact effect; same-intent evidence remains quiescent",
	}, before)
}

func (runner *intentSettlementRunner) acceptReset(ctx context.Context, envelope element.Envelope) error {
	address, ok := intentSettlementAddressPayload(envelope.Payload)
	if !ok || !envelope.Type.Equal(IntentSettlementResetType()) {
		return runner.refuse(ctx, envelope, "reset", "invalid_reset",
			fmt.Sprintf("settlement reset payload has type %T", envelope.Payload), IntentSettlementProbe{})
	}
	if err := validateIntentSettlementInputEnvelope("reset", envelope); err != nil {
		return runner.refuse(ctx, envelope, "reset", "invalid_reset", err.Error(), IntentSettlementProbe{})
	}
	if err := preflightIntentSettlementAddress("settlement reset", address); err != nil {
		return runner.refuse(ctx, envelope, "reset", "invalid_reset", err.Error(), IntentSettlementProbe{})
	}
	if envelope.SessionID == "" || envelope.SessionID != address.SessionID {
		return runner.refuse(ctx, envelope, "reset", "invalid_reset",
			"settlement reset requires one exact envelope and payload session", IntentSettlementProbe{})
	}
	if envelope.SessionID != runner.sessionID {
		return runner.refuse(ctx, envelope, "reset", "session_mismatch",
			"reset crossed the mounted trajectory session", IntentSettlementProbe{})
	}
	if err := VerifyIntentSettlementReset(runner.store.Snapshot(), address); err != nil {
		return runner.refuse(ctx, envelope, "reset", "invalid_reset", err.Error(), IntentSettlementProbe{})
	}
	key := intentSettlementKey{session: address.SessionID, intent: address.DurableIntent.TrajectoryItemID}
	record := runner.records[key]
	if record == nil || record.intent != address.DurableIntent {
		return runner.ignore(ctx, envelope, "reset", "stale_reset",
			"reset does not address the current exact durable intent", IntentSettlementProbe{})
	}
	if record.awaitingAck != nil {
		if record.deferredRetirement == nil {
			copy := address
			record.deferredRetirement = &intentSettlementDeferredRetirement{
				kind: IntentSettlementDecisionReset, itemID: envelope.ItemID, reset: &copy,
			}
		} else if record.deferredRetirement.kind == IntentSettlementDecisionCanceled {
			return runner.ignore(ctx, envelope, "reset", "cancellation_pending",
				"a stronger exact cancellation is already retained until terminal acknowledgement",
				record.awaitingAck.Probe)
		} else if record.deferredRetirement.reset == nil ||
			*record.deferredRetirement.reset != address {
			return runner.refuse(ctx, envelope, "reset", "conflicting_reset",
				"a different exact reset is already retained until terminal acknowledgement",
				record.awaitingAck.Probe)
		}
		deferredCause := envelope.Clone()
		deferredCause.CausalParents = appendUnique(
			deferredCause.CausalParents, record.awaitingAck.TerminalID,
		)
		return runner.ignore(ctx, deferredCause, "reset", "terminal_ack_pending",
			"the exact reset is retained until the pending terminal decision is acknowledged", record.awaitingAck.Probe)
	}
	if record.pending != nil {
		before := runner.state.Revision
		decision, err := runner.makeTerminalDecision(
			IntentSettlementDecisionReset, record.pending,
			intentSettlementDecisionWitness{reset: &address}, before,
		)
		if err != nil {
			return runner.refuse(ctx, envelope, "reset", "invalid_terminal_decision", err.Error(), record.pending.probe)
		}
		record.awaitingAck = &decision
		runner.bumpState()
		if err := runner.publishTerminal(ctx, envelope, decision); err != nil {
			return err
		}
		return runner.publishTransition(ctx, envelope, IntentSettlementOutcome{
			Kind: IntentSettlementHeld, Operation: "reset", SessionID: key.session,
			DurableIntentItemID: key.intent, ProbeID: decision.Probe.ProbeID,
			Code:    "awaiting_activation_ack",
			Message: "the pending result-linked effect must be cleared before reset completes",
		}, before)
	}
	delete(runner.records, key)
	runner.state.Resets++
	before := runner.state.Revision
	runner.bumpState()
	return runner.publishTransition(ctx, envelope, IntentSettlementOutcome{
		Kind: IntentSettlementReset, Operation: "reset", SessionID: key.session,
		DurableIntentItemID: key.intent, Code: "reset",
		Message: "the exactly addressed settlement epoch was reset",
	}, before)
}

func (runner *intentSettlementRunner) acceptCancel(ctx context.Context, envelope element.Envelope) error {
	cancel, ok := intentSettlementCancellationPayload(envelope.Payload)
	if !ok || !envelope.Type.Equal(IntentSettlementCancelType()) {
		return runner.refuse(ctx, envelope, "cancel", "invalid_cancel",
			fmt.Sprintf("settlement cancellation payload has type %T", envelope.Payload), IntentSettlementProbe{})
	}
	if err := validateIntentSettlementInputEnvelope("cancel", envelope); err != nil {
		return runner.refuse(ctx, envelope, "cancel", "invalid_cancel", err.Error(), IntentSettlementProbe{})
	}
	if err := preflightIntentSettlementCancellation(cancel); err != nil {
		return runner.refuse(ctx, envelope, "cancel", "invalid_cancel", err.Error(), IntentSettlementProbe{})
	}
	if envelope.SessionID == "" || envelope.SessionID != cancel.SessionID ||
		envelope.CancellationScope != cancel.DurableIntent.TrajectoryItemID {
		return runner.refuse(ctx, envelope, "cancel", "invalid_cancel",
			"settlement cancellation requires one exact envelope, session, and durable-intent scope",
			IntentSettlementProbe{})
	}
	if err := runner.requireMountedSession(cancel.SessionID); err != nil {
		return runner.refuse(ctx, envelope, "cancel", "session_mismatch", err.Error(), IntentSettlementProbe{})
	}
	snapshot := runner.store.Snapshot()
	if err := VerifyIntentSettlementCancellation(snapshot, cancel); err != nil {
		return runner.refuse(ctx, envelope, "cancel", "invalid_cancel", err.Error(), IntentSettlementProbe{})
	}
	cancel.Reason = boundedPolicyReason(cancel.Reason)
	key := intentSettlementKey{session: cancel.SessionID, intent: cancel.DurableIntent.TrajectoryItemID}
	record := runner.records[key]
	activeMatch := record != nil && record.intent == cancel.DurableIntent
	superseded := intentSettlementCancellationSuperseded(snapshot, cancel.DurableIntent)
	var pendingDecision *IntentSettlementDecision
	var pendingDecisionBefore uint64
	if activeMatch && record.awaitingAck == nil && record.pending != nil {
		pendingDecisionBefore = runner.state.Revision
		decision, err := runner.makeTerminalDecision(
			IntentSettlementDecisionCanceled, record.pending,
			intentSettlementDecisionWitness{cancellation: &cancel}, pendingDecisionBefore,
		)
		if err != nil {
			return runner.refuse(ctx, envelope, "cancel", "invalid_terminal_decision",
				err.Error(), record.pending.probe)
		}
		pendingDecision = &decision
	}
	revoked, exists := runner.revocations[key]
	if exists && revoked.intent != cancel.DurableIntent {
		return runner.refuse(ctx, envelope, "cancel", "revocation_identity_conflict",
			"the cancellation item ID conflicts with an existing canonical revocation",
			IntentSettlementProbe{})
	}
	prunable := runner.canonicalIntentRevocationsToPrune(snapshot)
	retainedRevocations := len(runner.revocations) - len(prunable)
	for _, candidate := range prunable {
		if candidate == key {
			exists = false
			break
		}
	}
	alreadyPending := activeMatch && ((record.deferredRetirement != nil &&
		record.deferredRetirement.kind == IntentSettlementDecisionCanceled) ||
		(record.awaitingAck != nil && record.awaitingAck.Kind == IntentSettlementDecisionCanceled))
	newRequest := !exists && !alreadyPending && (!superseded || activeMatch)
	if !superseded && !exists && retainedRevocations >= runner.config.CancelMemory {
		return runner.refuse(ctx, envelope, "cancel", "cancellation_capacity_exhausted",
			"the session cancellation tombstone is full and no live identity was evicted",
			IntentSettlementProbe{})
	}
	for _, candidate := range prunable {
		delete(runner.revocations, candidate)
	}
	if !superseded && !exists {
		retainedEnvelope := envelope.Clone()
		retainedEnvelope.Payload = cancel
		runner.revocations[key] = intentSettlementRevocation{
			intent: cancel.DurableIntent, cancellation: cancel, envelope: retainedEnvelope,
		}
	}
	if newRequest {
		runner.state.CancellationRequests++
	}
	var replacement *pendingIntentSettlement
	var matchedProbe IntentSettlementProbe
	removed := false
	waiting := false
	if record != nil && record.intent == cancel.DurableIntent {
		if record.awaitingAck != nil {
			if record.deferredRetirement == nil ||
				record.deferredRetirement.kind != IntentSettlementDecisionCanceled {
				copy := cancel
				record.deferredRetirement = &intentSettlementDeferredRetirement{
					kind: IntentSettlementDecisionCanceled, itemID: envelope.ItemID,
					cancellation: &copy,
				}
			}
			matchedProbe = record.awaitingAck.Probe
			waiting = true
		} else if pendingDecision != nil {
			decision := *pendingDecision
			record.awaitingAck = &decision
			runner.bumpState()
			if err := runner.publishTerminal(ctx, envelope, decision); err != nil {
				return err
			}
			return runner.publishTransition(ctx, envelope, IntentSettlementOutcome{
				Kind: IntentSettlementHeld, Operation: "cancel", SessionID: key.session,
				DurableIntentItemID:      key.intent,
				TriggerObservationItemID: decision.Probe.TriggerObservation.TrajectoryItemID,
				ResultItemID:             decision.Probe.Result.TrajectoryItemID, ProbeID: decision.Probe.ProbeID,
				Code:    "awaiting_activation_ack",
				Message: "cancellation is held until activation acknowledges cleanup of the exact completed effect",
			}, pendingDecisionBefore)
		} else {
			replacement = record.replacement
			matchedProbe = intentSettlementRecordProbe(record)
			delete(runner.records, key)
			removed = true
		}
	}
	if newRequest && !waiting {
		runner.state.CancellationsCompleted++
	}
	before := runner.state.Revision
	runner.bumpState()
	kind := IntentSettlementIgnored
	code := "cancellation_recorded"
	message := "exact cancellation was recorded for queued settlement evidence"
	if waiting {
		kind = IntentSettlementHeld
		code = "terminal_ack_pending"
		message = "cancellation is retained until activation acknowledges the exact pending terminal decision"
	} else if removed {
		kind = IntentSettlementCanceled
		code = "intent_revoked"
		message = boundedPolicyReason(cancel.Reason)
	} else if exists {
		code = "duplicate_cancellation"
		message = "the exact durable-intent cancellation was already recorded"
	}
	if superseded && !waiting && !removed {
		code = "superseded_cancellation"
		message = "the exactly addressed intent is already superseded by newer canonical user authority"
	}
	transitionCause := envelope.Clone()
	if waiting && record != nil && record.awaitingAck != nil {
		transitionCause.CausalParents = appendUnique(
			transitionCause.CausalParents, record.awaitingAck.TerminalID,
		)
	}
	if err := runner.publishTransition(ctx, transitionCause, IntentSettlementOutcome{
		Kind: kind, Operation: "cancel", SessionID: cancel.SessionID,
		DurableIntentItemID:      cancel.DurableIntent.TrajectoryItemID,
		TriggerObservationItemID: matchedProbe.TriggerObservation.TrajectoryItemID,
		ResultItemID:             matchedProbe.Result.TrajectoryItemID, ProbeID: matchedProbe.ProbeID,
		Code: code, Message: message,
	}, before); err != nil {
		return err
	}
	if replacement != nil {
		return runner.acceptEvidence(ctx, replacement.envelope)
	}
	return nil
}

// pruneSupersededIntentRevocations is safe only after the complete admission
// has been verified against the current snapshot. That verification proves
// current is the newest user authority, so any delayed evidence for an older
// intent will now fail independent temporal verification even without its
// cancellation tombstone.
func (runner *intentSettlementRunner) pruneSupersededIntentRevocations(
	sessionID string, current TemporalEvidenceItemIdentity,
) {
	for key, revoked := range runner.revocations {
		if key.session == sessionID && revoked.intent.StoreVersion < current.StoreVersion {
			delete(runner.revocations, key)
		}
	}
}

func (runner *intentSettlementRunner) canonicalIntentRevocationsToPrune(
	snapshot trajectory.Snapshot,
) []intentSettlementKey {
	result := make([]intentSettlementKey, 0, len(runner.revocations))
	for key, revoked := range runner.revocations {
		if intentSettlementCancellationSuperseded(snapshot, revoked.intent) {
			result = append(result, key)
		}
	}
	return result
}

func intentSettlementCancellationSuperseded(
	snapshot trajectory.Snapshot, canceled TemporalEvidenceItemIdentity,
) bool {
	for index := int(canceled.StoreVersion); index < len(snapshot.Items); index++ {
		item := snapshot.Items[index]
		if item.Kind == trajectory.KindObservation &&
			trajectory.AuthorityOf(item) == trajectory.AuthorityUser {
			return true
		}
	}
	return false
}

func (runner *intentSettlementRunner) requireMountedSession(sessionID string) error {
	if runner.sessionID != sessionID {
		return fmt.Errorf(
			"session %q differs from mounted trajectory session %q", sessionID, runner.sessionID,
		)
	}
	return nil
}

func (runner *intentSettlementRunner) retireOtherIntents(
	ctx context.Context, envelope element.Envelope, evidence AdmittedTemporalEvidence,
	current intentSettlementKey,
) (bool, error) {
	for key, record := range runner.records {
		if key.session != current.session || key.intent == current.intent {
			continue
		}
		if record.awaitingAck != nil {
			runner.retainReplacement(record, envelope, evidence)
			before := runner.state.Revision
			runner.pruneSupersededIntentRevocations(current.session, *evidence.DurableIntent)
			runner.bumpState()
			return true, runner.publishTransition(ctx, envelope, IntentSettlementOutcome{
				Kind: IntentSettlementHeld, Operation: "evidence", SessionID: current.session,
				DurableIntentItemID:      current.intent,
				TriggerObservationItemID: evidence.TriggerObservation.TrajectoryItemID,
				ProbeID:                  record.awaitingAck.Probe.ProbeID, Code: "prior_intent_ack_pending",
				Message: "new-intent evidence is held until activation acknowledges retirement of the prior effect",
			}, before)
		}
		if record.pending != nil {
			before := runner.state.Revision
			decision, err := runner.makeTerminalDecision(
				IntentSettlementDecisionSuperseded, record.pending,
				intentSettlementDecisionWitness{supersedingIntent: evidence.DurableIntent}, before,
			)
			if err != nil {
				return true, runner.refuse(ctx, envelope, "evidence", "invalid_terminal_decision", err.Error(), record.pending.probe)
			}
			runner.retainReplacement(record, envelope, evidence)
			record.awaitingAck = &decision
			runner.pruneSupersededIntentRevocations(current.session, *evidence.DurableIntent)
			runner.bumpState()
			if err := runner.publishTerminal(ctx, envelope, decision); err != nil {
				return true, err
			}
			return true, runner.publishTransition(ctx, envelope, IntentSettlementOutcome{
				Kind: IntentSettlementHeld, Operation: "evidence", SessionID: current.session,
				DurableIntentItemID:      current.intent,
				TriggerObservationItemID: evidence.TriggerObservation.TrajectoryItemID,
				ProbeID:                  decision.Probe.ProbeID, Code: "prior_intent_superseded",
				Message: "new-intent evidence is held until the completed prior effect is retired",
			}, before)
		}
		delete(runner.records, key)
		runner.updateCounts()
	}
	return false, nil
}

func (runner *intentSettlementRunner) retainReplacement(
	record *intentSettlementRecord, envelope element.Envelope, evidence AdmittedTemporalEvidence,
) {
	if record == nil || (record.replacement != nil &&
		record.replacement.evidence.TriggerCommit.StoreVersion >= evidence.TriggerCommit.StoreVersion) {
		return
	}
	retained := envelope.Clone()
	retained.Payload = cloneIntentSettlementEvidence(evidence)
	record.replacement = &pendingIntentSettlement{
		envelope: retained, evidence: cloneIntentSettlementEvidence(evidence),
	}
}

func intentSettlementRecordProbe(record *intentSettlementRecord) IntentSettlementProbe {
	if record == nil {
		return IntentSettlementProbe{}
	}
	switch {
	case record.awaitingAck != nil:
		return cloneIntentSettlementProbe(record.awaitingAck.Probe)
	case record.pending != nil:
		return cloneIntentSettlementProbe(record.pending.probe)
	case record.terminal != nil:
		return cloneIntentSettlementProbe(record.terminal.Probe)
	}
	return IntentSettlementProbe{}
}

// validateIntentSettlementInputEnvelope bounds every piece of envelope
// metadata that can be retained or copied into an output. Two causal-parent
// slots are reserved for the triggering control item and canonical probe.
func validateIntentSettlementInputEnvelope(label string, envelope element.Envelope) error {
	if label != "evidence" && strings.HasPrefix(envelope.ItemID, "intent-settlement-admitted:") {
		return fmt.Errorf(
			"%s item ID uses reserved settlement output namespace %q",
			label, "intent-settlement-admitted:",
		)
	}
	for _, prefix := range []string{
		"intent-settlement-probe:", "intent-settlement-terminal:",
		"intent-settlement-cleanup:", "intent-settlement-outcome:",
		"intent-settlement-state:",
	} {
		if strings.HasPrefix(envelope.ItemID, prefix) {
			return fmt.Errorf("%s item ID uses reserved settlement output namespace %q", label, prefix)
		}
	}
	for _, identity := range []struct {
		field    string
		value    string
		required bool
	}{
		{"item ID", envelope.ItemID, true},
		{"session ID", envelope.SessionID, true},
		{"source ID", envelope.SourceID, false},
		{"opportunity ID", envelope.OpportunityID, false},
		{"run ID", envelope.RunID, false},
		{"trace ID", envelope.TraceID, false},
		{"cancellation scope", envelope.CancellationScope, false},
	} {
		if err := validatePolicyIdentifier(
			label+" "+identity.field, identity.value, identity.required,
		); err != nil {
			return err
		}
	}
	maximumInputParents := maximumIntentSettlementCausalParents - 2
	if len(envelope.CausalParents) > maximumInputParents {
		return fmt.Errorf("%s causal parents exceed %d entries", label, maximumInputParents)
	}
	seen := make(map[string]struct{}, len(envelope.CausalParents))
	for index, parent := range envelope.CausalParents {
		if err := validatePolicyIdentifier(
			fmt.Sprintf("%s causal parent %d", label, index), parent, true,
		); err != nil {
			return err
		}
		if parent == envelope.ItemID {
			return fmt.Errorf("%s causal parent %d is a self-reference", label, index)
		}
		if _, duplicate := seen[parent]; duplicate {
			return fmt.Errorf("%s causal parent %d is duplicated", label, index)
		}
		seen[parent] = struct{}{}
	}
	return nil
}

func (runner *intentSettlementRunner) makeProbe(
	envelope element.Envelope, evidence AdmittedTemporalEvidence,
	result IntentSettlementResultIdentity,
) (IntentSettlementProbe, error) {
	issued := runner.clock.NowNS()
	if issued == 0 {
		return IntentSettlementProbe{}, errors.New("runtime clock returned zero for settlement probe")
	}
	sequence, err := runner.sequences.Next(runner.instance + ".probe")
	if err != nil {
		return IntentSettlementProbe{}, fmt.Errorf("allocate settlement probe sequence: %w", err)
	}
	probe := IntentSettlementProbe{
		Issuer: runner.instance, Sequence: sequence, SessionID: envelope.SessionID,
		Evidence: cloneIntentSettlementEvidence(evidence), DurableIntent: *evidence.DurableIntent,
		TriggerObservation: evidence.TriggerObservation, Result: result,
		Prefix: evidence.Prefix, Detector: runner.config.Detector, IssuedNS: issued,
	}
	probeID, err := intentSettlementProbeID(probe)
	if err != nil {
		return IntentSettlementProbe{}, err
	}
	probe.ProbeID = probeID
	return probe, nil
}

func (runner *intentSettlementRunner) makeTerminalDecision(
	kind IntentSettlementDecisionKind, pending *pendingIntentSettlement,
	witness intentSettlementDecisionWitness, before uint64,
) (IntentSettlementDecision, error) {
	if pending == nil {
		return IntentSettlementDecision{}, errors.New("terminal decision has no pending settlement")
	}
	finished := runner.clock.NowNS()
	minimum := pending.probe.IssuedNS
	if witness.disposition != nil && witness.disposition.DecisionFinishedNS > minimum {
		minimum = witness.disposition.DecisionFinishedNS
	}
	if finished == 0 || finished < minimum {
		return IntentSettlementDecision{}, errors.New(
			"runtime clock is earlier than the evidence used for the terminal decision")
	}
	decision := IntentSettlementDecision{
		Kind: kind, SessionID: pending.probe.SessionID,
		Evidence: cloneIntentSettlementEvidence(pending.evidence), Probe: cloneIntentSettlementProbe(pending.probe),
		InvocationID:        pending.probe.Result.InvocationID,
		StateRevisionBefore: before, StateRevisionAfter: before + 1,
		FinishedNS: finished,
	}
	if witness.disposition != nil {
		copy := cloneIntentDisposition(*witness.disposition)
		decision.Disposition = &copy
	}
	if witness.cancellation != nil {
		copy := *witness.cancellation
		decision.Cancellation = &copy
	}
	if witness.reset != nil {
		copy := *witness.reset
		decision.Reset = &copy
	}
	if witness.supersedingIntent != nil {
		copy := *witness.supersedingIntent
		decision.SupersedingIntent = &copy
	}
	terminalID, err := intentSettlementTerminalID(decision)
	if err != nil {
		return IntentSettlementDecision{}, err
	}
	decision.TerminalID = terminalID
	return decision, nil
}

func intentDispositionDecisionKind(kind IntentDispositionKind) IntentSettlementDecisionKind {
	switch kind {
	case IntentDispositionSucceeded:
		return IntentSettlementDecisionSucceeded
	case IntentDispositionFailed:
		return IntentSettlementDecisionFailed
	default:
		return ""
	}
}

func validateIntentDisposition(
	disposition IntentDisposition, detector IntentDetectorIdentity,
) error {
	if disposition.Detector != detector || disposition.Probe.Detector != detector {
		return errors.New("disposition detector differs from the independently pinned detector")
	}
	switch disposition.Kind {
	case IntentDispositionContinue, IntentDispositionSucceeded,
		IntentDispositionFailed, IntentDispositionIndeterminate:
	default:
		return fmt.Errorf("unknown intent disposition %q", disposition.Kind)
	}
	if disposition.DecisionStartedNS == 0 ||
		disposition.DecisionStartedNS < disposition.Probe.IssuedNS ||
		disposition.DecisionFinishedNS < disposition.DecisionStartedNS {
		return errors.New("disposition decision timestamps are missing or reversed")
	}
	return nil
}

func intentSettlementCandidate(
	snapshot trajectory.Snapshot, evidence AdmittedTemporalEvidence,
	candidateSources []TemporalEvidenceRequirement,
) (IntentSettlementResultIdentity, bool, error) {
	if evidence.DurableIntent == nil || evidence.TriggerObservation.StoreVersion == 0 ||
		evidence.TriggerObservation.StoreVersion > snapshot.Version {
		return IntentSettlementResultIdentity{}, false, errors.New("settlement evidence has invalid intent or trigger position")
	}
	prefix := snapshot.Items[:evidence.Prefix.Version]
	triggerIndex := int(evidence.TriggerObservation.StoreVersion - 1)
	intentIndex := int(evidence.DurableIntent.StoreVersion - 1)
	if triggerIndex < 0 || triggerIndex >= len(prefix) || intentIndex < 0 || intentIndex >= len(prefix) {
		return IntentSettlementResultIdentity{}, false, errors.New("settlement evidence positions are outside the exact prefix")
	}
	trigger := prefix[triggerIndex]
	if trigger.ID != evidence.TriggerObservation.TrajectoryItemID || trigger.Observation == nil {
		return IntentSettlementResultIdentity{}, false, errors.New("settlement trigger does not name an exact canonical observation")
	}
	pair := TemporalEvidenceRequirement{
		Observer: trigger.Observation.Observer, Source: trigger.Observation.Source,
	}
	if !slices.Contains(candidateSources, pair) {
		return IntentSettlementResultIdentity{}, false, nil
	}
	var resultIndex = -1
	for _, parentID := range trigger.CausalParentIDs {
		for index := triggerIndex - 1; index >= 0; index-- {
			item := prefix[index]
			if item.ID != parentID || item.Kind != trajectory.KindToolResult || item.ToolResult == nil {
				continue
			}
			if resultIndex != -1 {
				return IntentSettlementResultIdentity{}, false, errors.New(
					"settlement observation directly names multiple canonical tool results")
			}
			resultIndex = index
			break
		}
	}
	if resultIndex == -1 {
		return IntentSettlementResultIdentity{}, false, nil
	}
	result := prefix[resultIndex]
	if result.ToolResult.Error != "" {
		return IntentSettlementResultIdentity{}, false, nil
	}
	for _, identity := range []struct {
		label string
		value string
	}{
		{"settlement result trajectory item ID", result.ID},
		{"settlement result invocation ID", result.InvocationID},
		{"settlement result call ID", result.ToolResult.CallID},
		{"settlement result tool", result.ToolResult.Name},
	} {
		if err := validatePolicyIdentifier(identity.label, identity.value, true); err != nil {
			return IntentSettlementResultIdentity{}, false, err
		}
	}
	if len(result.ToolResult.Output) == 0 || !json.Valid(result.ToolResult.Output) {
		return IntentSettlementResultIdentity{}, false, errors.New("settlement result is not a complete successful canonical result")
	}
	if !temporalCausalAncestor(prefix, intentIndex, resultIndex) {
		// A completed effect from a prior intent may be linked to an observation
		// under a replacement intent. It must pass through so downstream
		// activation can close the old effect; it is not evidence that the new
		// intent itself succeeded.
		return IntentSettlementResultIdentity{}, false, nil
	}
	matchingCalls := 0
	matchingCallIndex := -1
	for index := intentIndex + 1; index < resultIndex; index++ {
		item := prefix[index]
		if item.Kind == trajectory.KindToolCall && item.ToolCall != nil &&
			item.InvocationID == result.InvocationID &&
			item.ToolCall.CallID == result.ToolResult.CallID &&
			item.ToolCall.Name == result.ToolResult.Name {
			matchingCalls++
			matchingCallIndex = index
		}
	}
	if matchingCalls != 1 {
		return IntentSettlementResultIdentity{}, false, fmt.Errorf(
			"settlement result has %d exact canonical calls under the durable intent", matchingCalls)
	}
	if !temporalCausalAncestor(prefix, intentIndex, matchingCallIndex) {
		return IntentSettlementResultIdentity{}, false, errors.New(
			"settlement result's exact canonical call does not descend from the durable intent")
	}
	if !slices.Contains(result.CausalParentIDs, prefix[matchingCallIndex].ID) {
		return IntentSettlementResultIdentity{}, false, errors.New(
			"settlement result is not a direct causal child of its exact canonical call")
	}
	consequences := 0
	consequenceIndex := -1
	for index := resultIndex + 1; index <= triggerIndex; index++ {
		item := prefix[index]
		if item.Kind != trajectory.KindObservation || item.Observation == nil ||
			item.Observation.Observer != pair.Observer || item.Observation.Source != pair.Source ||
			!slices.Contains(item.CausalParentIDs, result.ID) {
			continue
		}
		consequences++
		consequenceIndex = index
	}
	if consequences != 1 || consequenceIndex != triggerIndex {
		return IntentSettlementResultIdentity{}, false, fmt.Errorf(
			"settlement result has %d direct consequences on observer/source %q/%q before the trigger",
			consequences, pair.Observer, pair.Source,
		)
	}
	return IntentSettlementResultIdentity{
		TrajectoryItemID: result.ID, StoreVersion: uint64(resultIndex + 1),
		InvocationID: result.InvocationID, CallID: result.ToolResult.CallID,
		Tool: result.ToolResult.Name,
	}, true, nil
}

func intentSettlementRecordDisposition(record *intentSettlementRecord) IntentDispositionKind {
	if record == nil {
		return ""
	}
	if record.terminal != nil && record.terminal.Disposition != nil {
		return record.terminal.Disposition.Kind
	}
	if record.awaitingAck != nil && record.awaitingAck.Disposition != nil {
		return record.awaitingAck.Disposition.Kind
	}
	return ""
}

func cloneIntentSettlementEvidence(source AdmittedTemporalEvidence) AdmittedTemporalEvidence {
	result := source
	if source.DurableIntent != nil {
		intent := *source.DurableIntent
		result.DurableIntent = &intent
	}
	result.QualifyingObservations = slices.Clone(source.QualifyingObservations)
	return result
}

func cloneIntentSettlementCleanup(source IntentSettlementCleanup) IntentSettlementCleanup {
	result := source
	result.Evidence = cloneIntentSettlementEvidence(source.Evidence)
	return result
}

func cloneIntentSettlementProbe(source IntentSettlementProbe) IntentSettlementProbe {
	result := source
	result.Evidence = cloneIntentSettlementEvidence(source.Evidence)
	return result
}

func cloneIntentDisposition(source IntentDisposition) IntentDisposition {
	result := source
	result.Probe = cloneIntentSettlementProbe(source.Probe)
	return result
}

func cloneIntentSettlementDecision(source IntentSettlementDecision) IntentSettlementDecision {
	result := source
	result.Evidence = cloneIntentSettlementEvidence(source.Evidence)
	result.Probe = cloneIntentSettlementProbe(source.Probe)
	if source.Disposition != nil {
		disposition := cloneIntentDisposition(*source.Disposition)
		result.Disposition = &disposition
	}
	if source.Cancellation != nil {
		cancellation := *source.Cancellation
		result.Cancellation = &cancellation
	}
	if source.Reset != nil {
		reset := *source.Reset
		result.Reset = &reset
	}
	if source.SupersedingIntent != nil {
		intent := *source.SupersedingIntent
		result.SupersedingIntent = &intent
	}
	return result
}

func cloneIntentSettlementDeferredRetirement(
	source *intentSettlementDeferredRetirement,
) *intentSettlementDeferredRetirement {
	if source == nil {
		return nil
	}
	result := *source
	if source.reset != nil {
		reset := *source.reset
		result.reset = &reset
	}
	if source.cancellation != nil {
		cancellation := *source.cancellation
		result.cancellation = &cancellation
	}
	return &result
}

func intentSettlementEvidencePayload(payload any) (AdmittedTemporalEvidence, bool) {
	switch value := payload.(type) {
	case AdmittedTemporalEvidence:
		return value, true
	case *AdmittedTemporalEvidence:
		if value != nil {
			return *value, true
		}
	}
	return AdmittedTemporalEvidence{}, false
}

func intentDispositionPayload(payload any) (IntentDisposition, bool) {
	switch value := payload.(type) {
	case IntentDisposition:
		return value, true
	case *IntentDisposition:
		if value != nil {
			return *value, true
		}
	}
	return IntentDisposition{}, false
}

func intentSettlementAcknowledgementPayload(payload any) (IntentSettlementAcknowledgement, bool) {
	switch value := payload.(type) {
	case IntentSettlementAcknowledgement:
		return value, true
	case *IntentSettlementAcknowledgement:
		if value != nil {
			return *value, true
		}
	}
	return IntentSettlementAcknowledgement{}, false
}

func intentSettlementAddressPayload(payload any) (IntentSettlementAddress, bool) {
	switch value := payload.(type) {
	case IntentSettlementAddress:
		return value, true
	case *IntentSettlementAddress:
		if value != nil {
			return *value, true
		}
	}
	return IntentSettlementAddress{}, false
}

func intentSettlementCancellationPayload(payload any) (IntentSettlementCancellation, bool) {
	switch value := payload.(type) {
	case IntentSettlementCancellation:
		return value, true
	case *IntentSettlementCancellation:
		if value != nil {
			return *value, true
		}
	}
	return IntentSettlementCancellation{}, false
}

func (runner *intentSettlementRunner) bumpState() {
	runner.state.Revision++
	runner.updateCounts()
}

func (runner *intentSettlementRunner) updateCounts() {
	runner.state.TrackedIntents = len(runner.records)
	runner.state.CancellationEntries = len(runner.revocations)
	runner.state.AcknowledgedTerminals = len(runner.acknowledged)
	runner.state.Saturated = len(runner.records) >= runner.config.MaxTrackedIntents ||
		len(runner.revocations) >= runner.config.CancelMemory
	runner.state.PendingIntents = 0
	runner.state.TerminalIntents = 0
	for _, record := range runner.records {
		if record.pending != nil || record.awaitingAck != nil {
			runner.state.PendingIntents++
		}
		if record.terminal != nil {
			runner.state.TerminalIntents++
		}
	}
}

func (runner *intentSettlementRunner) rememberAcknowledged(
	decision IntentSettlementDecision,
) {
	if _, found := runner.acknowledged[decision.TerminalID]; found {
		return
	}
	runner.acknowledged[decision.TerminalID] = struct{}{}
	runner.acknowledgedOrder = append(runner.acknowledgedOrder, decision.TerminalID)
	for len(runner.acknowledgedOrder) > runner.config.MaxTrackedIntents {
		oldest := runner.acknowledgedOrder[0]
		runner.acknowledgedOrder = runner.acknowledgedOrder[1:]
		delete(runner.acknowledged, oldest)
	}
}

func (runner *intentSettlementRunner) publishAdmission(
	ctx context.Context, cause element.Envelope, evidence AdmittedTemporalEvidence,
) error {
	envelope := cause.Clone()
	envelope.Type = AdmittedTemporalEvidenceType()
	envelope.CausalParents = intentSettlementOutputParents(envelope.ItemID, envelope.CausalParents)
	envelope.Payload = cloneIntentSettlementEvidence(evidence)
	return intentSettlementBroadcastExact(ctx, runner.ports.admitted, envelope, "admission")
}

func (runner *intentSettlementRunner) publishCleanup(
	ctx context.Context, cause element.Envelope, cleanup IntentSettlementCleanup,
) error {
	sequence, err := runner.sequences.Next(runner.instance + ".cleanup")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = IntentSettlementCleanupType()
	envelope.SourceID = runner.instance
	envelope.Sequence = sequence
	envelope.CancellationScope = cleanup.Cancellation.DurableIntent.TrajectoryItemID
	envelope.ItemID = intentSettlementGeneratedItemID(
		"cleanup", runner.instance,
		cause.ItemID+"\x00"+cleanup.CancellationItemID, sequence,
	)
	envelope.CausalParents = intentSettlementOutputParents(envelope.ItemID, append(
		slices.Clone(cause.CausalParents), cause.ItemID, cleanup.CancellationItemID,
	))
	envelope.Payload = cloneIntentSettlementCleanup(cleanup)
	return intentSettlementBroadcastExact(ctx, runner.ports.cleanup, envelope, "cleanup")
}

func (runner *intentSettlementRunner) makeReleasedAdmission(
	evidenceCause element.Envelope,
	dispositionCause element.Envelope,
	probe IntentSettlementProbe,
	evidence AdmittedTemporalEvidence,
) (element.Envelope, error) {
	envelope := evidenceCause.Clone()
	envelope.Type = AdmittedTemporalEvidenceType()
	envelope.ItemID = ""
	envelope.CausalParents = intentSettlementOutputParents(envelope.ItemID, []string{
		evidenceCause.ItemID,
		dispositionCause.ItemID,
		probe.ProbeID,
	})
	envelope.Payload = cloneIntentSettlementEvidence(evidence)
	var err error
	envelope.ItemID, err = intentSettlementReleasedAdmissionID(envelope, evidence)
	if err != nil {
		return element.Envelope{}, err
	}
	return envelope, nil
}

func (runner *intentSettlementRunner) publishReleasedAdmission(
	ctx context.Context, envelope element.Envelope,
) error {
	return intentSettlementBroadcastExact(ctx, runner.ports.admitted, envelope, "released admission")
}

func (runner *intentSettlementRunner) publishProbe(
	ctx context.Context, cause element.Envelope, probe IntentSettlementProbe,
) error {
	envelope := cause.Clone()
	envelope.Type = IntentSettlementProbeType()
	envelope.ItemID = probe.ProbeID
	envelope.Sequence = probe.Sequence
	envelope.CausalParents = appendUnique(envelope.CausalParents, cause.ItemID)
	envelope.CausalParents = intentSettlementOutputParents(envelope.ItemID, envelope.CausalParents)
	envelope.Payload = cloneIntentSettlementProbe(probe)
	return intentSettlementBroadcastExact(ctx, runner.ports.probe, envelope, "probe")
}

func (runner *intentSettlementRunner) publishTerminal(
	ctx context.Context, cause element.Envelope, decision IntentSettlementDecision,
) error {
	envelope := cause.Clone()
	envelope.Type = IntentSettlementDecisionType()
	envelope.ItemID = decision.TerminalID
	envelope.SessionID = decision.SessionID
	envelope.RunID = decision.InvocationID
	envelope.CancellationScope = decision.InvocationID
	envelope.CausalParents = appendUnique(envelope.CausalParents, cause.ItemID)
	envelope.CausalParents = appendUnique(envelope.CausalParents, decision.Probe.ProbeID)
	envelope.CausalParents = intentSettlementOutputParents(envelope.ItemID, envelope.CausalParents)
	envelope.Payload = cloneIntentSettlementDecision(decision)
	return intentSettlementBroadcastExact(ctx, runner.ports.terminal, envelope, "terminal decision")
}

func (runner *intentSettlementRunner) refuse(
	ctx context.Context, cause element.Envelope, operation, code, message string,
	probe IntentSettlementProbe,
) error {
	runner.state.Refused++
	before := runner.state.Revision
	runner.bumpState()
	projected := canonicalIntentSettlementRefusalCause(cause)
	if cause.SessionID != runner.sessionID || validateIntentSettlementInputEnvelope(operation, cause) != nil {
		projected.ItemID = intentSettlementInvalidInputItemID
		projected.SourceID = ""
		projected.OpportunityID = ""
		projected.RunID = ""
		projected.Sequence = 0
		projected.CaptureNS = 0
		projected.ReceiveNS = 0
		projected.TraceID = ""
		projected.CancellationScope = ""
		projected.CausalParents = nil
	}
	if cause.SessionID != runner.sessionID || probe.SessionID != runner.sessionID {
		probe = IntentSettlementProbe{}
	}
	projected.SessionID = runner.sessionID
	return runner.publishTransition(ctx, projected, IntentSettlementOutcome{
		Kind: IntentSettlementRefused, Operation: operation, SessionID: projected.SessionID,
		DurableIntentItemID:      safeIntentSettlementIdentifier(probe.DurableIntent.TrajectoryItemID),
		TriggerObservationItemID: safeIntentSettlementIdentifier(probe.TriggerObservation.TrajectoryItemID),
		ResultItemID:             safeIntentSettlementIdentifier(probe.Result.TrajectoryItemID),
		ProbeID:                  safeIntentSettlementIdentifier(probe.ProbeID),
		Code:                     code, Message: boundedPolicyReason(message),
	}, before)
}

func (runner *intentSettlementRunner) ignore(
	ctx context.Context, cause element.Envelope, operation, code, message string,
	probe IntentSettlementProbe,
) error {
	before := runner.state.Revision
	runner.bumpState()
	return runner.publishTransition(ctx, cause, IntentSettlementOutcome{
		Kind: IntentSettlementIgnored, Operation: operation, SessionID: cause.SessionID,
		DurableIntentItemID:      probe.DurableIntent.TrajectoryItemID,
		TriggerObservationItemID: probe.TriggerObservation.TrajectoryItemID,
		ResultItemID:             probe.Result.TrajectoryItemID, ProbeID: probe.ProbeID,
		Code: code, Message: boundedPolicyReason(message),
	}, before)
}

func (runner *intentSettlementRunner) publishTransition(
	ctx context.Context, cause element.Envelope, outcome IntentSettlementOutcome, before uint64,
) error {
	outcome.StateRevisionBefore = before
	outcome.StateRevisionAfter = runner.state.Revision
	outcome.FinishedNS = runner.clock.NowNS()
	sequence, err := runner.sequences.Next(runner.instance + ".outcome")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = IntentSettlementOutcomeType()
	envelope.ItemID = intentSettlementGeneratedItemID("outcome", runner.instance, cause.ItemID, sequence)
	envelope.Sequence = sequence
	envelope.CausalParents = appendUnique(envelope.CausalParents, cause.ItemID)
	envelope.CausalParents = intentSettlementOutputParents(envelope.ItemID, envelope.CausalParents)
	envelope.Payload = outcome
	if err := intentSettlementBroadcastExact(ctx, runner.ports.outcome, envelope, "outcome"); err != nil {
		return err
	}
	return runner.publishState(ctx, cause)
}

func (runner *intentSettlementRunner) publishState(
	ctx context.Context, cause element.Envelope,
) error {
	sequence, err := runner.sequences.Next(runner.instance + ".state")
	if err != nil {
		return err
	}
	itemID := intentSettlementGeneratedItemID("state", runner.instance, cause.ItemID, sequence)
	parents := appendUnique(slices.Clone(cause.CausalParents), cause.ItemID)
	envelope := element.Envelope{
		Type: IntentSettlementStateType(), ItemID: itemID, SessionID: runner.sessionID, Sequence: sequence,
		CausalParents: intentSettlementOutputParents(itemID, parents), Payload: runner.state,
	}
	_, err = runner.ports.state.Broadcast(ctx, envelope)
	return err
}

func canonicalIntentSettlementRefusalCause(cause element.Envelope) element.Envelope {
	projected := element.Envelope{
		ItemID: intentSettlementInvalidInputItemID, Sequence: cause.Sequence,
		CaptureNS: cause.CaptureNS, ReceiveNS: cause.ReceiveNS,
	}
	if itemID := safeIntentSettlementIdentifier(cause.ItemID); itemID != "" {
		projected.ItemID = itemID
	}
	for _, field := range []struct {
		source      string
		destination *string
	}{
		{cause.SessionID, &projected.SessionID},
		{cause.SourceID, &projected.SourceID},
		{cause.OpportunityID, &projected.OpportunityID},
		{cause.RunID, &projected.RunID},
		{cause.TraceID, &projected.TraceID},
		{cause.CancellationScope, &projected.CancellationScope},
	} {
		if field.source == "" {
			continue
		}
		if canonical := safeIntentSettlementIdentifier(field.source); canonical != "" {
			*field.destination = canonical
		}
	}
	for index, parent := range cause.CausalParents {
		if index >= maximumIntentSettlementCausalParents-1 {
			break
		}
		canonical := safeIntentSettlementIdentifier(parent)
		if canonical == "" || canonical == projected.ItemID ||
			slices.Contains(projected.CausalParents, canonical) {
			continue
		}
		projected.CausalParents = append(projected.CausalParents, canonical)
	}
	return projected
}

func safeIntentSettlementIdentifier(value string) string {
	if len(value) == 0 || len(value) > maximumPolicyIdentifierBytes {
		return ""
	}
	if err := validatePolicyIdentifier("intent settlement projected identifier", value, true); err != nil {
		return ""
	}
	return value
}

type intentSettlementBoundedString struct {
	label string
	value string
	limit int
}

func preflightIntentSettlementStrings(fields ...intentSettlementBoundedString) error {
	for _, field := range fields {
		if len(field.value) > field.limit || !utf8.ValidString(field.value) {
			return fmt.Errorf("%s exceeds %d bytes or is not valid UTF-8", field.label, field.limit)
		}
	}
	return nil
}

func preflightIntentSettlementIdentity(label string, identity TemporalEvidenceItemIdentity) error {
	return preflightIntentSettlementStrings(
		intentSettlementBoundedString{label + " trajectory item ID", identity.TrajectoryItemID, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{label + " trigger item ID", identity.TriggerItemID, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{label + " authority", string(identity.Authority), maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{label + " observer", identity.Observer, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{label + " source", identity.Source, maximumPolicyIdentifierBytes},
	)
}

func preflightIntentSettlementEvidence(evidence AdmittedTemporalEvidence) error {
	if len(evidence.QualifyingObservations) > maximumTemporalEvidenceRequirements {
		return fmt.Errorf("settlement evidence qualifying observations exceed %d entries",
			maximumTemporalEvidenceRequirements)
	}
	if err := preflightIntentSettlementStrings(
		intentSettlementBoundedString{"settlement evidence mode", string(evidence.Mode), maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement evidence source set", string(evidence.SourceSet), maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement commit kind", string(evidence.TriggerCommit.Kind), maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement commit trigger item ID", evidence.TriggerCommit.TriggerItemID, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement commit trajectory item ID", evidence.TriggerCommit.TrajectoryItemID, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement commit stream ID", evidence.TriggerCommit.StreamID, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement commit state item ID", evidence.TriggerCommit.Context.StateItemID, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement commit prefix digest", evidence.TriggerCommit.Context.Prefix.Digest, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement commit code", evidence.TriggerCommit.Code, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement commit message", evidence.TriggerCommit.Message, maximumPolicyReasonBytes},
		intentSettlementBoundedString{"settlement prefix digest", evidence.Prefix.Digest, maximumPolicyIdentifierBytes},
	); err != nil {
		return err
	}
	if err := preflightIntentSettlementIdentity("settlement trigger observation", evidence.TriggerObservation); err != nil {
		return err
	}
	if evidence.DurableIntent != nil {
		if err := preflightIntentSettlementIdentity("settlement durable intent", *evidence.DurableIntent); err != nil {
			return err
		}
	}
	for index, observation := range evidence.QualifyingObservations {
		if err := preflightIntentSettlementIdentity(
			fmt.Sprintf("settlement qualifying observation %d", index), observation,
		); err != nil {
			return err
		}
	}
	return nil
}

func preflightIntentSettlementProbe(probe IntentSettlementProbe) error {
	if err := preflightIntentSettlementEvidence(probe.Evidence); err != nil {
		return err
	}
	if err := preflightIntentSettlementIdentity("settlement probe durable intent", probe.DurableIntent); err != nil {
		return err
	}
	if err := preflightIntentSettlementIdentity("settlement probe trigger observation", probe.TriggerObservation); err != nil {
		return err
	}
	return preflightIntentSettlementStrings(
		intentSettlementBoundedString{"settlement probe ID", probe.ProbeID, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement probe issuer", probe.Issuer, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement probe session ID", probe.SessionID, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement result trajectory item ID", probe.Result.TrajectoryItemID, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement result invocation ID", probe.Result.InvocationID, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement result call ID", probe.Result.CallID, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement result tool", probe.Result.Tool, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement probe prefix digest", probe.Prefix.Digest, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement detector reference", probe.Detector.Reference, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement detector revision", probe.Detector.Revision, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement detector configuration digest", probe.Detector.ConfigurationDigest, maximumPolicyIdentifierBytes},
	)
}

func preflightIntentDisposition(disposition IntentDisposition) error {
	if err := preflightIntentSettlementProbe(disposition.Probe); err != nil {
		return err
	}
	return preflightIntentSettlementStrings(
		intentSettlementBoundedString{"intent disposition kind", string(disposition.Kind), maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"intent disposition detector reference", disposition.Detector.Reference, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"intent disposition detector revision", disposition.Detector.Revision, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"intent disposition detector configuration digest", disposition.Detector.ConfigurationDigest, maximumPolicyIdentifierBytes},
	)
}

func preflightIntentSettlementAddress(label string, address IntentSettlementAddress) error {
	if err := preflightIntentSettlementIdentity(label+" durable intent", address.DurableIntent); err != nil {
		return err
	}
	return preflightIntentSettlementStrings(
		intentSettlementBoundedString{label + " session ID", address.SessionID, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{label + " reason", address.Reason, maximumPolicyReasonBytes},
	)
}

func preflightIntentSettlementCancellation(cancellation IntentSettlementCancellation) error {
	if err := preflightIntentSettlementIdentity("settlement cancellation durable intent", cancellation.DurableIntent); err != nil {
		return err
	}
	return preflightIntentSettlementStrings(
		intentSettlementBoundedString{"settlement cancellation session ID", cancellation.SessionID, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement cancellation reason", cancellation.Reason, maximumPolicyReasonBytes},
	)
}

func preflightIntentSettlementDecision(decision IntentSettlementDecision) error {
	if err := preflightIntentSettlementEvidence(decision.Evidence); err != nil {
		return err
	}
	if err := preflightIntentSettlementProbe(decision.Probe); err != nil {
		return err
	}
	if err := preflightIntentSettlementStrings(
		intentSettlementBoundedString{"settlement terminal ID", decision.TerminalID, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement decision kind", string(decision.Kind), maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement decision session ID", decision.SessionID, maximumPolicyIdentifierBytes},
		intentSettlementBoundedString{"settlement decision invocation ID", decision.InvocationID, maximumPolicyIdentifierBytes},
	); err != nil {
		return err
	}
	if decision.Disposition != nil {
		if err := preflightIntentDisposition(*decision.Disposition); err != nil {
			return err
		}
	}
	if decision.Cancellation != nil {
		if err := preflightIntentSettlementCancellation(*decision.Cancellation); err != nil {
			return err
		}
	}
	if decision.Reset != nil {
		if err := preflightIntentSettlementAddress("settlement reset", *decision.Reset); err != nil {
			return err
		}
	}
	if decision.SupersedingIntent != nil {
		return preflightIntentSettlementIdentity("settlement superseding intent", *decision.SupersedingIntent)
	}
	return nil
}

func preflightIntentSettlementAcknowledgement(ack IntentSettlementAcknowledgement) error {
	if err := preflightIntentSettlementDecision(ack.Decision); err != nil {
		return err
	}
	return preflightIntentSettlementStrings(
		intentSettlementBoundedString{"settlement acknowledgement generation ID", ack.GenerationID, maximumPolicyIdentifierBytes},
	)
}

func intentSettlementOutputParents(itemID string, parents []string) []string {
	if len(parents) > maximumIntentSettlementCausalParents {
		parents = parents[len(parents)-maximumIntentSettlementCausalParents:]
	}
	result := make([]string, 0, len(parents))
	for _, parent := range parents {
		if parent == "" || parent == itemID || slices.Contains(result, parent) {
			continue
		}
		result = append(result, parent)
	}
	if len(result) > maximumIntentSettlementCausalParents {
		result = slices.Clone(result[len(result)-maximumIntentSettlementCausalParents:])
	}
	return result
}

func intentSettlementGeneratedItemID(kind, instance, cause string, sequence uint64) string {
	digest := sha256.Sum256([]byte(
		"openrealtime.policy/intent-settlement/" + kind + "/v1\x00" + instance +
			"\x00" + cause + "\x00" + strconv.FormatUint(sequence, 10),
	))
	return "intent-settlement-" + kind + ":sha256:" + hex.EncodeToString(digest[:])
}

// intentSettlementReleasedAdmissionID binds the complete immutable envelope
// projection instead of asserting that only one nominal producer may emit an
// AdmittedTemporalEvidence value. This lets type-correct settlement gates be
// chained while rejecting reuse of a generated identity with changed metadata.
func intentSettlementReleasedAdmissionID(
	envelope element.Envelope, evidence AdmittedTemporalEvidence,
) (string, error) {
	canonical := struct {
		Schema            string                   `json:"schema"`
		Type              element.Type             `json:"type"`
		SessionID         string                   `json:"session_id"`
		SourceID          string                   `json:"source_id"`
		OpportunityID     string                   `json:"opportunity_id,omitempty"`
		RunID             string                   `json:"run_id,omitempty"`
		Sequence          uint64                   `json:"sequence"`
		CaptureNS         uint64                   `json:"capture_ns,omitempty"`
		ReceiveNS         uint64                   `json:"receive_ns,omitempty"`
		TraceID           string                   `json:"trace_id,omitempty"`
		CancellationScope string                   `json:"cancellation_scope,omitempty"`
		CausalParents     []string                 `json:"causal_parents"`
		Evidence          AdmittedTemporalEvidence `json:"evidence"`
	}{
		Schema:            "openrealtime.policy/intent-settlement/admitted/v1",
		Type:              envelope.Type.Clone(),
		SessionID:         envelope.SessionID,
		SourceID:          envelope.SourceID,
		OpportunityID:     envelope.OpportunityID,
		RunID:             envelope.RunID,
		Sequence:          envelope.Sequence,
		CaptureNS:         envelope.CaptureNS,
		ReceiveNS:         envelope.ReceiveNS,
		TraceID:           envelope.TraceID,
		CancellationScope: envelope.CancellationScope,
		CausalParents:     slices.Clone(envelope.CausalParents),
		Evidence:          cloneIntentSettlementEvidence(evidence),
	}
	payload, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode released settlement admission identity: %w", err)
	}
	digest := sha256.Sum256(payload)
	return "intent-settlement-admitted:sha256:" + hex.EncodeToString(digest[:]), nil
}

func verifyIntentSettlementReleasedAdmissionEnvelope(
	envelope element.Envelope, evidence AdmittedTemporalEvidence,
) error {
	if !strings.HasPrefix(envelope.ItemID, "intent-settlement-admitted:") {
		return nil
	}
	if len(envelope.CausalParents) != 3 {
		return errors.New("released settlement admission lacks its exact causal inputs")
	}
	want, err := intentSettlementReleasedAdmissionID(envelope, evidence)
	if err != nil {
		return err
	}
	if envelope.ItemID != want {
		return errors.New("released settlement admission identity does not match its immutable envelope")
	}
	return nil
}

func intentSettlementBroadcastExact(
	ctx context.Context, output element.OutputPort, envelope element.Envelope, label string,
) error {
	delivery, err := output.Broadcast(ctx, envelope)
	if err != nil {
		return err
	}
	if delivery.Delivered != 1 || delivery.Dropped != 0 {
		return fmt.Errorf("intent settlement %s delivered %d and dropped %d lanes",
			label, delivery.Delivered, delivery.Dropped)
	}
	return nil
}

func receiveIntentSettlementInputs(
	ctx context.Context, kind string, input element.InputPort,
	output chan<- intentSettlementInput, failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := input.Receive(ctx)
		if generationTerminal(ctx, err) {
			return
		}
		if err != nil {
			select {
			case failures <- fmt.Errorf("receive intent settlement %s: %w", kind, err):
			case <-ctx.Done():
			}
			return
		}
		select {
		case output <- intentSettlementInput{kind: kind, envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}
