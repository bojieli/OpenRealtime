package cascade

import (
	"fmt"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// Repair is what the runtime does when a commitment proves wrong.
//
// Audio that reached the user cannot be unheard. A repair is damage limitation
// rather than reversal, and treating it as a safety margin would be a mistake -
// so the only two things this file does are to make the obligation visible to
// the model, and to record when a correction has actually been produced.
//
// The lifecycle runs across three moments, and it has to, because the log will
// not accept a required repair until every part of the claim is already true:
//
//  1. Supersession notices that heard content is stale and records an
//     obligation in the ledger (audio.go).
//  2. The next safe point raises it as a trajectory item, once the content is
//     recorded as played and the observation that invalidated it is committed.
//     From that moment the slow continuation is instructed to correct it.
//  3. A slow continuation produces the correction, and the obligation is
//     resolved against the assistant item that carries it.

// raiseRepairs turns outstanding ledger obligations into trajectory items.
//
// The repair policy decides here rather than at the moment of invalidation,
// because only here is the honest played duration known: a client that
// truncated playback has by now told us how much it really played, and a
// fragment too short to carry a claim is a different situation from a whole
// sentence.
func (runtime *runtime) raiseRepairs() error {
	obligations := runtime.ledger.Obligations()
	if len(obligations) == 0 {
		return nil
	}
	snapshot := runtime.store.Snapshot()
	if len(snapshot.Items) == 0 {
		return nil
	}
	visibility := trajectory.AssistantVisibility(snapshot)
	pending := make(map[string]struct{})
	for _, repair := range trajectory.PendingRepairs(snapshot) {
		pending[repair.TargetAssistantItemID] = struct{}{}
	}
	revisions := make(map[uint64]struct{})
	assistants := make(map[string]trajectory.Item)
	for _, item := range snapshot.Items {
		switch item.Kind {
		case trajectory.KindObservation:
			revisions[item.SourceRevision] = struct{}{}
		case trajectory.KindAssistant:
			assistants[item.ID] = item
		}
	}

	var items []trajectory.Item
	parentID := snapshot.Items[len(snapshot.Items)-1].ID
	for _, obligation := range obligations {
		if _, known := revisions[obligation.InvalidatedByRevision]; !known {
			// The observation that invalidated this has not committed yet. It
			// will, and the next safe point will find the obligation again.
			continue
		}
		decision := runtime.policies.Repair.Decide(interaction.RepairInput{
			Context: interaction.Context{
				NowNS: runtime.scheduler.NowNS(), Duplex: runtime.duplex.Snapshot(),
			},
			PlayedAudioMS:         obligation.PlayedMS,
			InvalidatedByRevision: obligation.InvalidatedByRevision,
			TargetPhase:           runtime.obligationPhase(obligation),
		})
		if decision.Action != interaction.RepairSpeak {
			// Silent and none both stop here. The difference between them is
			// what the ledger records, not what the conversation hears: a
			// deployment that has chosen not to voice corrections has chosen
			// that, and manufacturing one anyway would be the runtime
			// overruling its own policy.
			runtime.ledger.ResolveObligation(obligation.CommitmentID)
			continue
		}
		raised := false
		for _, targetID := range obligation.AssistantItemIDs {
			target, exists := assistants[targetID]
			if !exists || visibility[targetID] != trajectory.VisibilityPlayed {
				continue
			}
			if _, already := pending[targetID]; already {
				continue
			}
			if obligation.InvalidatedByRevision <= target.SourceRevision || obligation.PlayedMS == 0 {
				continue
			}
			item := trajectory.Item{
				ID: runtime.nextItemID("repair"), Kind: trajectory.KindRepair,
				MonotonicNS:     runtime.scheduler.NowNS(),
				CausalParentIDs: directParents(parentID, targetID),
				SourceRevision:  obligation.InvalidatedByRevision,
				Producer:        trajectory.Producer{Phase: trajectory.PhaseRuntime},
				Repair: &trajectory.RepairState{
					TargetAssistantItemID: targetID, Status: trajectory.RepairRequired,
					PlayedAudioMS: obligation.PlayedMS,
				},
			}
			items = append(items, item)
			pending[targetID] = struct{}{}
			parentID = item.ID
			raised = true
		}
		if !raised {
			continue
		}
	}
	if len(items) == 0 {
		return nil
	}
	if err := runtime.store.AppendBatchAt(snapshot.Version, items); err != nil {
		return fmt.Errorf("raise %d repair obligations: %w", len(items), err)
	}
	return nil
}

// resolveRepairs discharges obligations against the correction that answers
// them.
//
// Only a slow continuation can resolve one. The repair instruction is given to
// the slow provider, its output is the authoritative correction, and a fast
// utterance voicing it is a rendering of that correction rather than the
// correction itself - which is exactly the shape of the two cognition
// boundaries everywhere else.
func (runtime *runtime) resolveRepairs(result continuation.RunResult) error {
	snapshot := runtime.store.Snapshot()
	outstanding := trajectory.PendingRepairs(snapshot)
	if len(outstanding) == 0 || len(snapshot.Items) == 0 {
		return nil
	}
	appended := make(map[string]struct{}, len(result.AppendedIDs))
	for _, id := range result.AppendedIDs {
		appended[id] = struct{}{}
	}
	var correction trajectory.Item
	found := false
	for _, item := range snapshot.Items {
		if item.Kind != trajectory.KindAssistant || item.Producer.Phase != trajectory.PhaseSlow {
			continue
		}
		if _, ours := appended[item.ID]; ours {
			correction, found = item, true
		}
	}
	if !found {
		return nil
	}

	var items []trajectory.Item
	parentID := snapshot.Items[len(snapshot.Items)-1].ID
	for _, repair := range outstanding {
		if correction.ID == repair.TargetAssistantItemID || correction.SourceRevision == 0 {
			continue
		}
		if correction.SourceRevision < revisionOf(snapshot, repair.RequiredItemID) {
			continue
		}
		item := trajectory.Item{
			ID: runtime.nextItemID("repair"), Kind: trajectory.KindRepair,
			MonotonicNS:     runtime.scheduler.NowNS(),
			CausalParentIDs: directParents(parentID, repair.TargetAssistantItemID, correction.ID),
			SourceRevision:  correction.SourceRevision,
			Producer:        trajectory.Producer{Phase: trajectory.PhaseRuntime},
			Repair: &trajectory.RepairState{
				TargetAssistantItemID: repair.TargetAssistantItemID,
				Status:                trajectory.RepairResolved,
				RepairAssistantItemID: correction.ID,
			},
		}
		items = append(items, item)
		parentID = item.ID
	}
	if len(items) == 0 {
		return nil
	}
	if err := runtime.store.AppendBatchAt(snapshot.Version, items); err != nil {
		return fmt.Errorf("resolve %d repair obligations: %w", len(items), err)
	}
	for _, item := range items {
		runtime.resolveObligationFor(item.Repair.TargetAssistantItemID)
	}
	return nil
}

// obligationPhase reports which cognition phase produced the invalidated
// content, so the repair policy can treat a fast guess and a slow answer
// differently if a deployment wants it to.
func (runtime *runtime) obligationPhase(obligation action.Obligation) trajectory.Phase {
	if commitment, exists := runtime.ledger.Lookup(obligation.CommitmentID); exists {
		return commitment.Phase
	}
	return ""
}

func (runtime *runtime) resolveObligationFor(assistantItemID string) {
	for _, obligation := range runtime.ledger.Obligations() {
		for _, id := range obligation.AssistantItemIDs {
			if id == assistantItemID {
				runtime.ledger.ResolveObligation(obligation.CommitmentID)
				return
			}
		}
	}
}

func (runtime *runtime) nextItemID(prefix string) string {
	return idFor(prefix, runtime.sequence.Add(1))
}

func revisionOf(snapshot trajectory.Snapshot, itemID string) uint64 {
	for _, item := range snapshot.Items {
		if item.ID == itemID {
			return item.SourceRevision
		}
	}
	return 0
}

func directParents(ids ...string) []string {
	var result []string
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}
