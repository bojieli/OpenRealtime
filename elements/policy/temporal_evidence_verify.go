package policy

import (
	"errors"
	"fmt"
	"slices"

	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// ErrAdmittedTemporalEvidenceSuperseded means the admission was valid for its
// immutable prefix, but a later user-authority observation now supersedes its
// durable intent in the current snapshot. Callers may safely discard retained
// work carrying this error without treating the canonical store as corrupt.
var ErrAdmittedTemporalEvidenceSuperseded = errors.New(
	"admitted temporal evidence was superseded by later user evidence",
)

// VerifyAdmittedTemporalEvidence verifies that admission is a faithful,
// content-free description of evidence in snapshot. Consumers should call it
// at their trust boundary instead of trusting identities copied through a
// typed graph port: it re-proves the canonical prefix and trigger, durable
// intent, causal freshness, distinct qualifying pairs, and (when dynamic)
// the complete source set frozen before the intent.
func VerifyAdmittedTemporalEvidence(
	snapshot trajectory.Snapshot, admission AdmittedTemporalEvidence,
) error {
	commit := admission.TriggerCommit
	if admission.Prefix != commit.Context.Prefix {
		return errors.New("temporal admission prefix differs from its trigger commit")
	}
	if commit.Kind != stateelements.ObservationCommitted {
		return errors.New("temporal admission trigger is not a committed observation")
	}
	if err := validateCommit(commit); err != nil {
		return fmt.Errorf("temporal admission trigger: %w", err)
	}
	if commit.StoreVersion > snapshot.Version || commit.StoreVersion > uint64(len(snapshot.Items)) {
		return fmt.Errorf(
			"temporal admission trigger version %d exceeds canonical snapshot version %d",
			commit.StoreVersion, snapshot.Version,
		)
	}
	if err := trajectory.VerifyPrefix(snapshot, admission.Prefix); err != nil {
		return fmt.Errorf("verify temporal admission prefix: %w", err)
	}
	prefix := snapshot.Items[:commit.StoreVersion]
	triggerIndex := len(prefix) - 1
	trigger := prefix[triggerIndex]
	if trigger.ID != commit.TrajectoryItemID || trigger.Kind != trajectory.KindObservation ||
		trigger.Event == nil || trigger.Event.EventID != commit.TriggerItemID ||
		trigger.SourceRevision != commit.SourceRevision {
		return errors.New(
			"temporal admission trigger does not name its exact event-backed prefix tail")
	}
	triggerIdentity, err := temporalEvidenceIdentity(trigger, triggerIndex)
	if err != nil {
		return fmt.Errorf("temporal admission trigger: %w", err)
	}
	if triggerIdentity != admission.TriggerObservation {
		return errors.New(
			"temporal admission trigger identity differs from the canonical prefix tail")
	}

	switch admission.Mode {
	case TemporalEvidenceAdmissionImmediate:
		if admission.SourceSet != "" || admission.DurableIntent != nil ||
			len(admission.QualifyingObservations) != 0 {
			return errors.New("immediate temporal admission carries after-intent evidence")
		}
		return nil
	case TemporalEvidenceAdmissionAfterIntent:
		if admission.SourceSet != TemporalEvidenceSourceSetExplicit &&
			admission.SourceSet != TemporalEvidenceSourceSetObservedBeforeIntent {
			return fmt.Errorf("after-intent temporal admission has source set %q", admission.SourceSet)
		}
		if admission.DurableIntent == nil || len(admission.QualifyingObservations) == 0 ||
			len(admission.QualifyingObservations) > maximumTemporalEvidenceRequirements {
			return errors.New(
				"after-intent temporal admission lacks a bounded durable evidence set")
		}
	default:
		return fmt.Errorf("temporal admission has mode %q", admission.Mode)
	}

	intentIndex, intent, err := temporalEvidenceItem(prefix, *admission.DurableIntent)
	if err != nil {
		return fmt.Errorf("temporal admission durable intent: %w", err)
	}
	if trajectory.AuthorityOf(intent) != trajectory.AuthorityUser ||
		intent.Producer.Phase != trajectory.PhaseUser || intent.Event == nil ||
		intent.Event.Type != intent.Event.Source+".endpoint" || intent.Event.OccurredNS == 0 {
		return errors.New(
			"temporal admission durable intent is not final timestamped user evidence")
	}
	latestUserIndex := -1
	for index := len(prefix) - 1; index >= 0; index-- {
		item := prefix[index]
		if item.Kind == trajectory.KindObservation &&
			trajectory.AuthorityOf(item) == trajectory.AuthorityUser {
			latestUserIndex = index
			break
		}
	}
	if latestUserIndex != intentIndex {
		return errors.New(
			"temporal admission does not name the newest user-authority observation")
	}
	// The admission may have waited in a graph queue while the canonical store
	// advanced. A later user observation, including a provisional revision,
	// supersedes the admitted intent even though it is outside the immutable
	// prefix the gate originally inspected. Later observer/runtime items remain
	// valid so retained evidence can be replayed after a disposition append.
	for index := int(commit.StoreVersion); index < len(snapshot.Items); index++ {
		item := snapshot.Items[index]
		if item.Kind == trajectory.KindObservation &&
			trajectory.AuthorityOf(item) == trajectory.AuthorityUser {
			return fmt.Errorf(
				"%w: durable intent %q was superseded by user observation %q",
				ErrAdmittedTemporalEvidenceSuperseded, intent.ID, item.ID,
			)
		}
	}
	if triggerIndex <= intentIndex ||
		!temporalCausalAncestor(prefix, intentIndex, triggerIndex) {
		return errors.New(
			"temporal admission trigger is not a post-intent causal descendant")
	}

	seenIDs := map[string]struct{}{intent.ID: {}}
	seenPairs := make(map[TemporalEvidenceRequirement]struct{},
		len(admission.QualifyingObservations))
	qualifiedPairs := make([]TemporalEvidenceRequirement, 0,
		len(admission.QualifyingObservations))
	for _, identity := range admission.QualifyingObservations {
		index, item, identityErr := temporalEvidenceItem(prefix, identity)
		if identityErr != nil {
			return fmt.Errorf("temporal admission qualifying observation: %w", identityErr)
		}
		if index <= intentIndex || trajectory.AuthorityOf(item) != trajectory.AuthorityObserver ||
			item.Event == nil || item.Event.OccurredNS == 0 ||
			item.Event.OccurredNS < intent.Event.OccurredNS ||
			!temporalCausalAncestor(prefix, intentIndex, index) {
			return fmt.Errorf(
				"qualifying observation %q is not fresh causal observer evidence", item.ID)
		}
		pair := TemporalEvidenceRequirement{Observer: identity.Observer, Source: identity.Source}
		if _, duplicate := seenIDs[item.ID]; duplicate {
			return fmt.Errorf("temporal admission reuses trajectory item %q", item.ID)
		}
		if _, duplicate := seenPairs[pair]; duplicate {
			return fmt.Errorf(
				"temporal admission repeats observer/source pair %q/%q", pair.Observer, pair.Source)
		}
		latest := -1
		for candidate := intentIndex + 1; candidate < len(prefix); candidate++ {
			observed := prefix[candidate]
			if observed.Kind == trajectory.KindObservation && observed.Observation != nil &&
				observed.Observation.Authority == trajectory.AuthorityObserver &&
				observed.Observation.Observer == pair.Observer &&
				observed.Observation.Source == pair.Source {
				latest = candidate
			}
		}
		if latest != index {
			return fmt.Errorf(
				"qualifying observation %q is not the latest exact observer/source evidence", item.ID)
		}
		seenIDs[item.ID] = struct{}{}
		seenPairs[pair] = struct{}{}
		qualifiedPairs = append(qualifiedPairs, pair)
	}
	if admission.SourceSet == TemporalEvidenceSourceSetObservedBeforeIntent {
		epochStart := temporalEvidenceEpochStart(prefix, intentIndex)
		expected, expectedErr := observedSourceSet(prefix, epochStart, intentIndex)
		if expectedErr != nil {
			return fmt.Errorf("derive dynamic temporal source set: %w", expectedErr)
		}
		if len(expected) == 0 {
			return errors.New("dynamic temporal source set is empty")
		}
		if !slices.Equal(expected, qualifiedPairs) {
			return fmt.Errorf(
				"dynamic temporal source set %v differs from qualifying pairs %v",
				expected, qualifiedPairs,
			)
		}
	}
	return nil
}

func temporalEvidenceItem(
	prefix []trajectory.Item, identity TemporalEvidenceItemIdentity,
) (int, trajectory.Item, error) {
	if identity.StoreVersion == 0 || identity.StoreVersion > uint64(len(prefix)) {
		return 0, trajectory.Item{}, fmt.Errorf(
			"identity position %d exceeds prefix version %d", identity.StoreVersion, len(prefix))
	}
	index := int(identity.StoreVersion - 1)
	item := prefix[index]
	actual, err := temporalEvidenceIdentity(item, index)
	if err != nil {
		return 0, trajectory.Item{}, err
	}
	if actual != identity {
		return 0, trajectory.Item{}, fmt.Errorf(
			"identity for trajectory item %q differs from canonical evidence",
			identity.TrajectoryItemID,
		)
	}
	return index, item, nil
}
