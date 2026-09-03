package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	temporalEvidenceRuntimeID       = "builtin://openrealtime/elements/policy.TemporalEvidenceAdmission"
	temporalEvidenceRuntimeRevision = "implementation:1"
)

type temporalEvidenceAdmissionFactory struct{}

var (
	_ element.Factory         = temporalEvidenceAdmissionFactory{}
	_ element.ConfigValidator = temporalEvidenceAdmissionFactory{}
	_ element.Runnable        = (*temporalEvidenceAdmissionRunner)(nil)
)

func (temporalEvidenceAdmissionFactory) Descriptor() element.Descriptor {
	return TemporalEvidenceAdmissionDescriptor()
}

func (temporalEvidenceAdmissionFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeTemporalEvidenceAdmissionConfig(source)
	return err
}

func (temporalEvidenceAdmissionFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeTemporalEvidenceAdmissionConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("policy.TemporalEvidenceAdmission %s config: %w", mount.InstanceID, err)
	}
	storeValue, _, found := mount.Services.Lookup(stateelements.TrajectoryStoreService)
	if !found {
		return nil, fmt.Errorf(
			"policy.TemporalEvidenceAdmission %s has no canonical trajectory store", mount.InstanceID)
	}
	storeService, ok := storeValue.(*stateelements.TrajectoryStoreServiceValue)
	if !ok || storeService == nil || storeService.Store == nil {
		return nil, fmt.Errorf("canonical trajectory store service has type %T", storeValue)
	}
	clockValue, _, found := mount.Services.Lookup(graphruntime.ClockServiceName)
	if !found {
		return nil, errors.New("temporal evidence admission has no runtime clock service")
	}
	clock, ok := clockValue.(graphruntime.Clock)
	if !ok || clock == nil {
		return nil, fmt.Errorf("runtime clock service has type %T", clockValue)
	}
	sequenceValue, _, found := mount.Services.Lookup(graphruntime.SequenceServiceName)
	if !found {
		return nil, errors.New("temporal evidence admission has no runtime sequence service")
	}
	sequences, ok := sequenceValue.(*graphruntime.SequenceAllocator)
	if !ok || sequences == nil {
		return nil, fmt.Errorf("runtime sequence service has type %T", sequenceValue)
	}
	committed, err := mount.Ports.Input("committed")
	if err != nil {
		return nil, err
	}
	admitted, err := mount.Ports.Output("admitted")
	if err != nil {
		return nil, err
	}
	outcome, err := mount.Ports.Output("outcome")
	if err != nil {
		return nil, err
	}
	return &temporalEvidenceAdmissionRunner{
		instance: mount.InstanceID, config: config, store: storeService.Store,
		clock: clock, sequences: sequences, committed: committed,
		admitted: admitted, outcome: outcome, resolution: mount.Resolution,
	}, nil
}

type temporalEvidenceAdmissionRunner struct {
	instance   string
	config     TemporalEvidenceAdmissionConfig
	store      *trajectory.Store
	clock      graphruntime.Clock
	sequences  *graphruntime.SequenceAllocator
	committed  element.InputPort
	admitted   element.OutputPort
	outcome    element.OutputPort
	resolution element.ResolutionReporter
}

func (runner *temporalEvidenceAdmissionRunner) Run(ctx context.Context) error {
	if err := liveidentity.Report(runner.resolution, liveidentity.Artifact{
		ID: temporalEvidenceRuntimeID, Revision: temporalEvidenceRuntimeRevision,
	}, nil); err != nil {
		return err
	}
	for {
		envelope, err := runner.committed.Receive(ctx)
		if generationTerminal(ctx, err) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := runner.accept(ctx, envelope); err != nil {
			return err
		}
	}
}

func (runner *temporalEvidenceAdmissionRunner) accept(
	ctx context.Context, envelope element.Envelope,
) error {
	commit, ok := observationCommitPayload(envelope.Payload)
	if !ok {
		return runner.publishOutcome(ctx, envelope, TemporalEvidenceAdmissionOutcome{
			Kind: TemporalEvidenceAdmissionRefused, Mode: runner.config.Mode,
			Code: "invalid_payload", Message: fmt.Sprintf("commit payload has type %T", envelope.Payload),
		})
	}
	base := TemporalEvidenceAdmissionOutcome{
		Mode: runner.config.Mode, TriggerItemID: commit.TriggerItemID,
		TrajectoryItemID: commit.TrajectoryItemID,
	}
	if commit.Kind != stateelements.ObservationCommitted {
		base.Kind = TemporalEvidenceAdmissionIgnored
		base.Code = "observation_not_committed"
		base.Message = "observation commit outcome did not cross the canonical trajectory boundary"
		return runner.publishOutcome(ctx, envelope, base)
	}
	if err := validatePolicyIdentifier("commit session ID", envelope.SessionID, true); err != nil {
		base.Kind = TemporalEvidenceAdmissionRefused
		base.Code = "invalid_commit_envelope"
		base.Message = err.Error()
		return runner.publishOutcome(ctx, envelope, base)
	}
	if err := validatePolicyIdentifier("commit envelope item ID", envelope.ItemID, true); err != nil {
		base.Kind = TemporalEvidenceAdmissionRefused
		base.Code = "invalid_commit_envelope"
		base.Message = err.Error()
		return runner.publishOutcome(ctx, envelope, base)
	}
	if err := validateCommit(commit); err != nil {
		base.Kind = TemporalEvidenceAdmissionRefused
		base.Code = "invalid_commit"
		base.Message = err.Error()
		return runner.publishOutcome(ctx, envelope, base)
	}
	admission, evaluation := evaluateTemporalEvidence(runner.store.Snapshot(), runner.config, commit)
	base.DurableIntentItemID = evaluation.intentID
	base.RequiredObservations = evaluation.required
	base.QualifiedObservations = evaluation.qualified
	if evaluation.err != nil {
		base.Kind = TemporalEvidenceAdmissionRefused
		base.Code = evaluation.code
		base.Message = evaluation.err.Error()
		return runner.publishOutcome(ctx, envelope, base)
	}
	if err := runner.publishAdmission(ctx, envelope, admission); err != nil {
		return err
	}
	base.Kind = TemporalEvidenceAdmissionAdmitted
	base.Code = "admitted"
	return runner.publishOutcome(ctx, envelope, base)
}

type temporalEvidenceEvaluation struct {
	intentID  string
	required  int
	qualified int
	code      string
	err       error
}

func evaluateTemporalEvidence(
	snapshot trajectory.Snapshot,
	config TemporalEvidenceAdmissionConfig,
	commit stateelements.ObservationCommitOutcome,
) (AdmittedTemporalEvidence, temporalEvidenceEvaluation) {
	refuse := func(code string, err error) (AdmittedTemporalEvidence, temporalEvidenceEvaluation) {
		return AdmittedTemporalEvidence{}, temporalEvidenceEvaluation{code: code, err: err}
	}
	if err := trajectory.VerifyPrefix(snapshot, commit.Context.Prefix); err != nil {
		return refuse("prefix_mismatch", fmt.Errorf("verify committed trajectory prefix: %w", err))
	}
	if commit.StoreVersion > uint64(len(snapshot.Items)) || commit.StoreVersion == 0 {
		return refuse("invalid_store_version", fmt.Errorf(
			"committed store version %d is outside canonical snapshot version %d",
			commit.StoreVersion, snapshot.Version))
	}
	if config.Mode == TemporalEvidenceAdmissionAfterIntent && commit.StoreVersion != snapshot.Version {
		return refuse("stale_trigger_prefix", fmt.Errorf(
			"after-intent trigger prefix version %d is not current canonical version %d",
			commit.StoreVersion, snapshot.Version))
	}
	prefix := snapshot.Items[:commit.StoreVersion]
	triggerIndex := len(prefix) - 1
	trigger := prefix[triggerIndex]
	if trigger.ID != commit.TrajectoryItemID || trigger.Kind != trajectory.KindObservation ||
		trigger.Event == nil || trigger.Event.EventID != commit.TriggerItemID ||
		trigger.SourceRevision != commit.SourceRevision {
		return refuse("trigger_mismatch", errors.New(
			"commit does not name the exact event-backed observation at its prefix tail"))
	}
	triggerIdentity, err := temporalEvidenceIdentity(trigger, triggerIndex)
	if err != nil {
		return refuse("invalid_trigger_observation", err)
	}
	admission := AdmittedTemporalEvidence{
		Mode: config.Mode, SourceSet: config.SourceSet, TriggerCommit: commit,
		TriggerObservation: triggerIdentity, Prefix: commit.Context.Prefix,
	}
	if config.Mode == TemporalEvidenceAdmissionImmediate {
		return admission, temporalEvidenceEvaluation{}
	}

	intentIndex, intent, err := latestDurableIntent(prefix)
	if err != nil {
		return refuse("missing_durable_intent", err)
	}
	intentIdentity, err := temporalEvidenceIdentity(intent, intentIndex)
	if err != nil {
		return refuse("invalid_durable_intent", err)
	}
	evaluation := temporalEvidenceEvaluation{intentID: intent.ID}
	if triggerIndex != intentIndex && !temporalCausalAncestor(prefix, intentIndex, triggerIndex) {
		evaluation.code = "trigger_not_causal"
		evaluation.err = errors.New("trigger observation is not a causal descendant of the exact durable intent")
		return AdmittedTemporalEvidence{}, evaluation
	}
	requirements := slices.Clone(config.Required)
	if config.SourceSet == TemporalEvidenceSourceSetObservedBeforeIntent {
		epochStart := temporalEvidenceEpochStart(prefix, intentIndex)
		requirements, err = observedSourceSet(prefix, epochStart, intentIndex)
		if err != nil {
			evaluation.code = "invalid_observed_source_set"
			evaluation.err = err
			return AdmittedTemporalEvidence{}, evaluation
		}
		if len(requirements) == 0 {
			evaluation.code = "empty_observed_source_set"
			evaluation.err = errors.New(
				"no observer/source pair exists before the durable intent to freeze")
			return AdmittedTemporalEvidence{}, evaluation
		}
	}
	if len(requirements) == 0 || len(requirements) > maximumTemporalEvidenceRequirements {
		evaluation.code = "invalid_required_source_set"
		evaluation.err = fmt.Errorf("required source set contains %d pairs", len(requirements))
		return AdmittedTemporalEvidence{}, evaluation
	}
	evaluation.required = len(requirements)
	selected := make([]TemporalEvidenceItemIdentity, 0, len(requirements))
	selectedIDs := make(map[string]struct{}, len(requirements)+1)
	selectedIDs[intent.ID] = struct{}{}
	for _, requirement := range requirements {
		identity, failure := qualifyTemporalEvidence(prefix, intentIndex, intent, requirement)
		if failure.err != nil {
			failure.intentID = intent.ID
			failure.required = len(requirements)
			failure.qualified = len(selected)
			return AdmittedTemporalEvidence{}, failure
		}
		if _, aliased := selectedIDs[identity.TrajectoryItemID]; aliased {
			evaluation.qualified = len(selected)
			evaluation.code = "aliased_evidence"
			evaluation.err = fmt.Errorf(
				"trajectory item %q cannot identify both intent and qualifying evidence",
				identity.TrajectoryItemID)
			return AdmittedTemporalEvidence{}, evaluation
		}
		selectedIDs[identity.TrajectoryItemID] = struct{}{}
		selected = append(selected, identity)
	}
	evaluation.qualified = len(selected)
	admission.DurableIntent = &intentIdentity
	admission.QualifyingObservations = selected
	return admission, evaluation
}

func latestDurableIntent(prefix []trajectory.Item) (int, trajectory.Item, error) {
	for index := len(prefix) - 1; index >= 0; index-- {
		item := prefix[index]
		if item.Kind != trajectory.KindObservation || trajectory.AuthorityOf(item) != trajectory.AuthorityUser {
			continue
		}
		if item.Producer.Phase != trajectory.PhaseUser || item.Event == nil {
			return 0, trajectory.Item{}, fmt.Errorf(
				"latest user-authority item %q is not canonical user observation evidence", item.ID)
		}
		if item.Event.Type != item.Event.Source+".endpoint" {
			return 0, trajectory.Item{}, fmt.Errorf(
				"latest user observation %q is not a final durable intent", item.ID)
		}
		if item.Event.OccurredNS == 0 {
			return 0, trajectory.Item{}, fmt.Errorf(
				"final user observation %q has no source occurrence time", item.ID)
		}
		return index, item, nil
	}
	return 0, trajectory.Item{}, errors.New("canonical prefix contains no final user intent")
}

// temporalEvidenceEpochStart returns the first item after the preceding final
// user intent. Dynamic discovery is thereby scoped to observations associated
// with the new intent's input cohort instead of accumulating every sensor ever
// used in a long-lived session.
func temporalEvidenceEpochStart(prefix []trajectory.Item, intentIndex int) int {
	for index := intentIndex - 1; index >= 0; index-- {
		item := prefix[index]
		if item.Kind == trajectory.KindObservation && item.Producer.Phase == trajectory.PhaseUser &&
			trajectory.AuthorityOf(item) == trajectory.AuthorityUser && item.Event != nil &&
			item.Event.Type == item.Event.Source+".endpoint" {
			return index + 1
		}
	}
	return 0
}

func observedSourceSet(
	prefix []trajectory.Item, start, end int,
) ([]TemporalEvidenceRequirement, error) {
	result := make([]TemporalEvidenceRequirement, 0)
	seen := make(map[TemporalEvidenceRequirement]struct{})
	for index := start; index < end; index++ {
		item := prefix[index]
		if item.Kind != trajectory.KindObservation || trajectory.AuthorityOf(item) != trajectory.AuthorityObserver {
			continue
		}
		identity, err := temporalEvidenceIdentity(item, index)
		if err != nil {
			return nil, fmt.Errorf("pre-intent observation %q: %w", item.ID, err)
		}
		requirement := TemporalEvidenceRequirement{Observer: identity.Observer, Source: identity.Source}
		if _, duplicate := seen[requirement]; duplicate {
			continue
		}
		if len(result) == maximumTemporalEvidenceRequirements {
			return nil, fmt.Errorf(
				"observed source set exceeds %d distinct pairs", maximumTemporalEvidenceRequirements)
		}
		seen[requirement] = struct{}{}
		result = append(result, requirement)
	}
	return result, nil
}

func qualifyTemporalEvidence(
	prefix []trajectory.Item,
	intentIndex int,
	intent trajectory.Item,
	requirement TemporalEvidenceRequirement,
) (TemporalEvidenceItemIdentity, temporalEvidenceEvaluation) {
	sawBeforeIntent := false
	sawMismatch := false
	latestExactIndex := -1
	for index, item := range prefix {
		if item.Kind != trajectory.KindObservation || item.Observation == nil ||
			item.Observation.Authority != trajectory.AuthorityObserver {
			continue
		}
		pairMatches := item.Observation.Observer == requirement.Observer &&
			item.Observation.Source == requirement.Source
		if index <= intentIndex {
			if pairMatches {
				sawBeforeIntent = true
			}
			continue
		}
		if !pairMatches {
			if item.Observation.Observer == requirement.Observer ||
				item.Observation.Source == requirement.Source {
				sawMismatch = true
			}
			continue
		}
		latestExactIndex = index
	}
	label := fmt.Sprintf("observer %q source %q", requirement.Observer, requirement.Source)
	if latestExactIndex >= 0 {
		latest := prefix[latestExactIndex]
		identity, err := temporalEvidenceIdentity(latest, latestExactIndex)
		if err != nil {
			return TemporalEvidenceItemIdentity{}, temporalEvidenceEvaluation{
				code: "mismatched_evidence", err: fmt.Errorf("latest %s: %w", label, err),
			}
		}
		if latest.Event.OccurredNS == 0 || latest.Event.OccurredNS < intent.Event.OccurredNS {
			return TemporalEvidenceItemIdentity{}, temporalEvidenceEvaluation{
				code: "stale_evidence", err: fmt.Errorf("latest %s predates the durable intent", label),
			}
		}
		if !temporalCausalAncestor(prefix, intentIndex, latestExactIndex) {
			return TemporalEvidenceItemIdentity{}, temporalEvidenceEvaluation{
				code: "evidence_not_causal", err: fmt.Errorf("latest %s is not a causal descendant of the durable intent", label),
			}
		}
		return identity, temporalEvidenceEvaluation{}
	}
	switch {
	case sawMismatch:
		return TemporalEvidenceItemIdentity{}, temporalEvidenceEvaluation{
			code: "mismatched_evidence", err: fmt.Errorf("no exact post-intent observation matches %s", label),
		}
	case sawBeforeIntent:
		return TemporalEvidenceItemIdentity{}, temporalEvidenceEvaluation{
			code: "stale_evidence", err: fmt.Errorf("%s has only pre-intent evidence", label),
		}
	default:
		return TemporalEvidenceItemIdentity{}, temporalEvidenceEvaluation{
			code: "missing_evidence", err: fmt.Errorf("canonical prefix has no observation for %s", label),
		}
	}
}

func temporalEvidenceIdentity(
	item trajectory.Item, index int,
) (TemporalEvidenceItemIdentity, error) {
	if item.Kind != trajectory.KindObservation || item.Event == nil || item.SourceRevision == 0 {
		return TemporalEvidenceItemIdentity{}, errors.New(
			"temporal evidence requires an event-backed observation with a positive source revision")
	}
	if err := validatePolicyIdentifier("trajectory item ID", item.ID, true); err != nil {
		return TemporalEvidenceItemIdentity{}, err
	}
	if err := validatePolicyIdentifier("observation event ID", item.Event.EventID, true); err != nil {
		return TemporalEvidenceItemIdentity{}, err
	}
	identity := TemporalEvidenceItemIdentity{
		TrajectoryItemID: item.ID, TriggerItemID: item.Event.EventID,
		StoreVersion: uint64(index + 1), SourceRevision: item.SourceRevision,
		OccurredNS: item.Event.OccurredNS, Authority: trajectory.AuthorityOf(item),
	}
	switch identity.Authority {
	case trajectory.AuthorityUser:
		if item.Producer.Phase != trajectory.PhaseUser {
			return TemporalEvidenceItemIdentity{}, errors.New(
				"user-authority temporal evidence was not produced by the user phase")
		}
		identity.Observer = item.Event.Source
		identity.Source = item.Event.Channel
		if item.Observation != nil {
			if item.Observation.Authority != trajectory.AuthorityUser {
				return TemporalEvidenceItemIdentity{}, errors.New("user observation authority is inconsistent")
			}
			identity.Observer = item.Observation.Observer
			identity.Source = item.Observation.Source
			if item.Event.Source != identity.Observer || item.Event.Channel != identity.Source {
				return TemporalEvidenceItemIdentity{}, errors.New(
					"user event identity does not match observation provenance")
			}
		}
	case trajectory.AuthorityObserver:
		if item.Producer.Phase != trajectory.PhaseObserver || item.Observation == nil ||
			item.Observation.Authority != trajectory.AuthorityObserver {
			return TemporalEvidenceItemIdentity{}, errors.New(
				"observer temporal evidence lacks canonical observer provenance")
		}
		identity.Observer = item.Observation.Observer
		identity.Source = item.Observation.Source
		if err := validatePolicyIdentifier("observation observer", identity.Observer, true); err != nil {
			return TemporalEvidenceItemIdentity{}, err
		}
		if err := validatePolicyIdentifier("observation source", identity.Source, true); err != nil {
			return TemporalEvidenceItemIdentity{}, err
		}
		if item.Producer.Provider != identity.Observer || item.Event.Source != identity.Observer ||
			item.Event.Channel != identity.Source {
			return TemporalEvidenceItemIdentity{}, errors.New(
				"observation producer/event identity does not match observer/source provenance")
		}
	default:
		return TemporalEvidenceItemIdentity{}, fmt.Errorf(
			"observation carries unsupported %q authority", identity.Authority)
	}
	return identity, nil
}

func temporalCausalAncestor(items []trajectory.Item, ancestorIndex, descendantIndex int) bool {
	if ancestorIndex < 0 || descendantIndex < 0 || ancestorIndex >= len(items) ||
		descendantIndex >= len(items) || ancestorIndex >= descendantIndex {
		return false
	}
	ancestorID := items[ancestorIndex].ID
	indices := make(map[string]int, descendantIndex+1)
	for index := 0; index <= descendantIndex; index++ {
		indices[items[index].ID] = index
	}
	pending := slices.Clone(items[descendantIndex].CausalParentIDs)
	visited := make(map[string]struct{}, len(pending))
	for len(pending) != 0 {
		last := len(pending) - 1
		current := pending[last]
		pending = pending[:last]
		if current == ancestorID {
			return true
		}
		if _, seen := visited[current]; seen {
			continue
		}
		visited[current] = struct{}{}
		index, found := indices[current]
		if !found || index >= descendantIndex {
			continue
		}
		pending = append(pending, items[index].CausalParentIDs...)
	}
	return false
}

func (runner *temporalEvidenceAdmissionRunner) publishAdmission(
	ctx context.Context, cause element.Envelope, admission AdmittedTemporalEvidence,
) error {
	sequence, err := runner.sequences.Next(runner.instance + ".admitted")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = temporalEvidenceAdmittedType
	envelope.ItemID = fmt.Sprintf("%s:temporal_evidence_admitted:%d", cause.ItemID, sequence)
	envelope.CausalParents = appendUnique(envelope.CausalParents, cause.ItemID)
	envelope.CausalParents = appendUnique(envelope.CausalParents, admission.TriggerCommit.Context.StateItemID)
	envelope.CausalParents = appendUnique(envelope.CausalParents, admission.TriggerObservation.TrajectoryItemID)
	if admission.DurableIntent != nil {
		envelope.CausalParents = appendUnique(envelope.CausalParents, admission.DurableIntent.TrajectoryItemID)
	}
	for _, observation := range admission.QualifyingObservations {
		envelope.CausalParents = appendUnique(envelope.CausalParents, observation.TrajectoryItemID)
	}
	envelope.Payload = admission
	delivery, err := runner.admitted.Broadcast(ctx, envelope)
	if err != nil {
		return err
	}
	if delivery.Delivered < 1 || delivery.Dropped != 0 {
		return fmt.Errorf("temporal evidence admission delivered %d and dropped %d lanes",
			delivery.Delivered, delivery.Dropped)
	}
	return nil
}

func (runner *temporalEvidenceAdmissionRunner) publishOutcome(
	ctx context.Context, cause element.Envelope, outcome TemporalEvidenceAdmissionOutcome,
) error {
	sequence, err := runner.sequences.Next(runner.instance + ".outcome")
	if err != nil {
		return err
	}
	outcome.FinishedNS = runner.clock.NowNS()
	envelope := cause.Clone()
	envelope.Type = temporalEvidenceOutcomeType
	envelope.ItemID = fmt.Sprintf("%s:temporal_evidence_outcome:%d", cause.ItemID, sequence)
	envelope.CausalParents = appendUnique(envelope.CausalParents, cause.ItemID)
	envelope.Payload = outcome
	_, err = runner.outcome.Broadcast(ctx, envelope)
	return err
}
