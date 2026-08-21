package eventloop

import (
	"fmt"
	"slices"

	"github.com/bojieli/OpenRealtime/trajectory"
)

// Batch is the exact event group handed to one processor invocation.
//
// A batch may span more than one commit: events that were committed while a
// policy deferred acting on them are carried forward and handed to the run
// that the deferral owed. Each commit keeps its own batch marker inside the
// trajectory, so a model that receives two carried-forward groups can still
// see that they arrived as two groups.
type Batch struct {
	Events       []Event
	Items        []trajectory.Item
	StartVersion uint64
	EndVersion   uint64
	// Triage is the strongest triage among the events in the batch. A parallel
	// batch is one the processor may handle without disturbing work in flight.
	Triage Triage
	// Deferred reports that some events in this batch were committed during an
	// earlier safe point and waited for a wake-up before being acted upon.
	Deferred bool
}

func (batch Batch) empty() bool { return len(batch.Items) == 0 && len(batch.Events) == 0 }

// SourceRevision is the newest perception revision represented in the batch.
func (batch Batch) SourceRevision() uint64 {
	var newest uint64
	for _, item := range batch.Items {
		newest = max(newest, item.SourceRevision)
	}
	return newest
}

// Contains reports whether the batch committed at least one item of kind.
func (batch Batch) Contains(kind trajectory.Kind) bool {
	return slices.ContainsFunc(batch.Items, func(item trajectory.Item) bool { return item.Kind == kind })
}

// merge concatenates committed batches in commit order. It is how deferred
// work rejoins the run it was waiting for.
func merge(batches []Batch) Batch {
	if len(batches) == 0 {
		return Batch{}
	}
	merged := Batch{
		StartVersion: batches[0].StartVersion,
		EndVersion:   batches[len(batches)-1].EndVersion,
		Triage:       TriageQueue,
	}
	for index, batch := range batches {
		merged.Events = append(merged.Events, batch.Events...)
		merged.Items = append(merged.Items, batch.Items...)
		merged.Deferred = merged.Deferred || batch.Deferred || index < len(batches)-1
		if batch.Triage == TriageCancel {
			merged.Triage = TriageCancel
		} else if batch.Triage == TriageParallel && merged.Triage != TriageCancel {
			merged.Triage = TriageParallel
		}
	}
	return merged
}

// markBatch stamps a commit group so it is legible as a group.
//
// A model handed four events at one safe point attends to the last one unless
// the group is marked, which is why this is part of the committed log rather
// than a rendering convention a compiler could forget to apply. A single item
// needs no marker: one event is not a batch.
func markBatch(batchID string, items []trajectory.Item) {
	if len(items) < 2 {
		return
	}
	for index := range items {
		if items[index].Event == nil {
			continue
		}
		items[index].Event.BatchID = batchID
		items[index].Event.BatchIndex = index
		items[index].Event.BatchSize = len(items)
	}
}

// compile turns validated events into trajectory items in arrival order.
func (coordinator *Coordinator) compile(snapshot trajectory.Snapshot, events []Event) ([]trajectory.Item, error) {
	all := slices.Clone(snapshot.Items)
	items := make([]trajectory.Item, 0, len(events))
	lastNS := uint64(0)
	parentID := ""
	if len(all) > 0 {
		lastNS = all[len(all)-1].MonotonicNS
		parentID = all[len(all)-1].ID
	}
	nextNS := func() uint64 {
		value := coordinator.now()
		if value < lastNS {
			value = lastNS
		}
		lastNS = value
		return value
	}
	nextItemID := func() string {
		coordinator.idMu.Lock()
		defer coordinator.idMu.Unlock()
		return coordinator.nextID("event-item")
	}

	for _, event := range events {
		metadata := &trajectory.EventMetadata{
			EventID: event.EventID, Type: event.Type, Source: event.Source,
			Channel: event.Channel, OccurredNS: event.OccurredNS, CorrelationID: event.CorrelationID,
			SupersedesRevision: event.SupersedesRevision,
		}
		switch event.Kind {
		case trajectory.KindObservation:
			item := trajectory.Item{
				ID: nextItemID(), Kind: trajectory.KindObservation, MonotonicNS: nextNS(),
				SourceRevision: event.SourceRevision, Producer: event.Producer,
				Content: event.Content, Observation: event.Observation, Event: metadata,
			}
			supersededID := ""
			if event.SupersedesRevision != 0 {
				for index := len(all) - 1; index >= 0; index-- {
					if all[index].Kind == trajectory.KindObservation &&
						all[index].SourceRevision == event.SupersedesRevision {
						supersededID = all[index].ID
						break
					}
				}
				if supersededID == "" {
					return nil, fmt.Errorf("observation supersedes unknown source revision %d", event.SupersedesRevision)
				}
			}
			item.CausalParentIDs = directParents(parentID, supersededID)
			items = append(items, item)
			all = append(all, item)
			parentID = item.ID
		case trajectory.KindAssistantState:
			state := *event.AssistantState
			item := trajectory.Item{
				ID: nextItemID(), Kind: trajectory.KindAssistantState, MonotonicNS: nextNS(),
				SourceRevision: event.SourceRevision, Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime},
				AssistantState: &state, Event: metadata,
			}
			item.CausalParentIDs = directParents(parentID, state.AssistantItemID)
			items = append(items, item)
			all = append(all, item)
			parentID = item.ID
		case trajectory.KindRepair:
			repair := *event.Repair
			item := trajectory.Item{
				ID: nextItemID(), Kind: trajectory.KindRepair, MonotonicNS: nextNS(),
				SourceRevision: event.SourceRevision, Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime},
				Repair: &repair, Event: metadata,
			}
			item.CausalParentIDs = directParents(parentID, repair.TargetAssistantItemID, repair.RepairAssistantItemID)
			items = append(items, item)
			all = append(all, item)
			parentID = item.ID
		case trajectory.KindToolPlaceholder:
			placeholder := *event.ToolPlaceholder
			callItemID := ""
			for index := len(all) - 1; index >= 0; index-- {
				if all[index].Kind == trajectory.KindToolCall && all[index].ToolCall != nil &&
					all[index].ToolCall.CallID == placeholder.CallID {
					callItemID = all[index].ID
					break
				}
			}
			if callItemID == "" {
				return nil, fmt.Errorf("tool placeholder references unknown call %q", placeholder.CallID)
			}
			item := trajectory.Item{
				ID: nextItemID(), Kind: trajectory.KindToolPlaceholder, MonotonicNS: nextNS(),
				SourceRevision: event.SourceRevision, InvocationID: event.InvocationID,
				Producer:        trajectory.Producer{Phase: trajectory.PhaseRuntime},
				ToolPlaceholder: &placeholder, Event: metadata,
			}
			item.CausalParentIDs = directParents(parentID, callItemID)
			items = append(items, item)
			all = append(all, item)
			parentID = item.ID
		case trajectory.KindToolResult:
			compiled, err := coordinator.compileToolResults(all, parentID, nextNS, nextItemID, event, metadata)
			if err != nil {
				return nil, err
			}
			items = append(items, compiled...)
			all = append(all, compiled...)
			parentID = compiled[len(compiled)-1].ID
		}
	}
	return items, nil
}

func (coordinator *Coordinator) compileToolResults(
	all []trajectory.Item,
	parentID string,
	nextNS func() uint64,
	nextItemID func() string,
	event Event,
	metadata *trajectory.EventMetadata,
) ([]trajectory.Item, error) {
	matched, err := trajectory.MatchToolResultBatch(
		trajectory.Snapshot{Version: uint64(len(all)), Items: all},
		event.InvocationID,
		event.ToolResults,
	)
	if err != nil {
		return nil, err
	}

	items := make([]trajectory.Item, 0, len(matched))
	for _, pair := range matched {
		metadataCopy := *metadata
		result := pair.Result
		item := trajectory.Item{
			ID: nextItemID(), Kind: trajectory.KindToolResult, MonotonicNS: nextNS(),
			CausalParentIDs: directParents(parentID, pair.Pending.ItemID),
			SourceRevision:  pair.Pending.SourceRevision, InvocationID: event.InvocationID,
			Producer: trajectory.Producer{Phase: trajectory.PhaseTool}, ToolResult: &result,
			Event: &metadataCopy,
		}
		items = append(items, item)
		parentID = item.ID
	}
	return items, nil
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
