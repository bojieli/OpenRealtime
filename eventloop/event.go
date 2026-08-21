// Package eventloop serializes asynchronous external events into one canonical
// trajectory and decides when cognition may act on them.
//
// The loop rests on one invariant, and every policy that defers work depends
// on it:
//
//	Commit is unconditional; acting is conditional; every deferral has a
//	wake-up. An event that has been committed but not yet acted upon must
//	eventually cause a run. Nothing committed is ever silently dropped.
//
// Event sources may run concurrently. They submit typed observations, complete
// tool-result batches, or media commitment transitions, and every one of them
// enters the trajectory the moment it arrives, whatever the conversational
// state. Whether a continuation then runs is a policy decision made by a Gate,
// and a gate that says no leaves the events pending rather than consuming
// them. Wake starts the run that a deferral owes.
//
// The coordinator never derives control from event text: priority is supplied
// by a trusted source, and triage follows from priority alone.
package eventloop

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/bojieli/OpenRealtime/trajectory"
)

// Priority is supplied by a trusted event source or a semantic/acoustic
// classifier. The coordinator never derives it from words in event content.
type Priority string

const (
	// PriorityRoutine waits for the next safe point.
	PriorityRoutine Priority = "routine"
	// PriorityInterrupt cooperatively cancels the work in flight.
	PriorityInterrupt Priority = "interrupt"
	// PriorityParallel is handled alongside the work in flight without
	// disturbing it. It exists for the case the loop could not previously
	// express: answering a quick question while long work continues.
	PriorityParallel Priority = "parallel"
)

// Triage is what an arriving event does to work already in flight. It follows
// from priority alone, which is why deciding it is not a separate policy.
type Triage string

const (
	TriageQueue    Triage = "queue"
	TriageCancel   Triage = "cancel"
	TriageParallel Triage = "parallel"
)

// TriageOf maps priority to its effect on in-flight work.
func TriageOf(priority Priority) Triage {
	switch priority {
	case PriorityInterrupt:
		return TriageCancel
	case PriorityParallel:
		return TriageParallel
	default:
		return TriageQueue
	}
}

// Event is one external occurrence. EventID is assigned on Submit when empty.
// ToolResults is deliberately a batch: one slow invocation's complete set of
// outstanding calls crosses the synchronization boundary atomically.
type Event struct {
	EventID            string
	Type               string
	Source             string
	Channel            string
	CorrelationID      string
	Priority           Priority
	Kind               trajectory.Kind
	OccurredNS         uint64
	SourceRevision     uint64
	SupersedesRevision uint64
	InvocationID       string
	Producer           trajectory.Producer
	Content            string
	Observation        *trajectory.ObservationMeta
	ToolResults        []trajectory.ToolResult
	ToolPlaceholder    *trajectory.ToolPlaceholder
	AssistantState     *trajectory.AssistantState
	Repair             *trajectory.RepairState
}

func validateEvent(event Event) error {
	if strings.TrimSpace(event.Type) == "" || strings.TrimSpace(event.Source) == "" || strings.TrimSpace(event.Channel) == "" {
		return errors.New("event type, source, and channel are required")
	}
	switch event.Priority {
	case PriorityRoutine, PriorityInterrupt, PriorityParallel:
	default:
		return fmt.Errorf("unsupported event priority %q", event.Priority)
	}
	if event.SupersedesRevision != 0 && event.Kind != trajectory.KindObservation {
		return fmt.Errorf("%s event cannot supersede an observation", event.Kind)
	}
	switch event.Kind {
	case trajectory.KindObservation:
		if strings.TrimSpace(event.Content) == "" || event.Producer.Phase == "" ||
			len(event.ToolResults) != 0 || event.AssistantState != nil || event.Repair != nil || event.ToolPlaceholder != nil {
			return errors.New("observation event requires content and producer only")
		}
		if event.SupersedesRevision >= event.SourceRevision && event.SupersedesRevision != 0 {
			return errors.New("observation supersession must name an older source revision")
		}
	case trajectory.KindToolResult:
		if event.InvocationID == "" || len(event.ToolResults) == 0 || event.Content != "" ||
			event.AssistantState != nil || event.Repair != nil || event.ToolPlaceholder != nil {
			return errors.New("tool-result event requires invocation ID and a non-empty result batch")
		}
		for index, result := range event.ToolResults {
			if strings.TrimSpace(result.CallID) == "" || strings.TrimSpace(result.Name) == "" {
				return fmt.Errorf("tool result %d requires call ID and name", index)
			}
			hasOutput := len(result.Output) > 0
			hasError := strings.TrimSpace(result.Error) != ""
			if hasOutput == hasError || hasOutput && !json.Valid(result.Output) {
				return fmt.Errorf("tool result %d requires exactly one valid JSON output or error", index)
			}
		}
	case trajectory.KindToolPlaceholder:
		if event.ToolPlaceholder == nil || event.Content != "" || len(event.ToolResults) != 0 ||
			event.AssistantState != nil || event.Repair != nil {
			return errors.New("tool-placeholder event requires one placeholder transition")
		}
	case trajectory.KindAssistantState:
		if event.AssistantState == nil || event.Content != "" || len(event.ToolResults) != 0 ||
			event.Repair != nil || event.ToolPlaceholder != nil {
			return errors.New("assistant-state event requires one state transition")
		}
	case trajectory.KindRepair:
		if event.Repair == nil || event.Content != "" || len(event.ToolResults) != 0 ||
			event.AssistantState != nil || event.ToolPlaceholder != nil {
			return errors.New("repair event requires one repair transition")
		}
	default:
		return fmt.Errorf("external event cannot directly append trajectory kind %q", event.Kind)
	}
	return nil
}

func cloneEvent(event Event) Event {
	event.ToolResults = slices.Clone(event.ToolResults)
	for index := range event.ToolResults {
		event.ToolResults[index].Output = slices.Clone(event.ToolResults[index].Output)
	}
	if event.AssistantState != nil {
		copied := *event.AssistantState
		event.AssistantState = &copied
	}
	if event.Repair != nil {
		copied := *event.Repair
		event.Repair = &copied
	}
	if event.ToolPlaceholder != nil {
		copied := *event.ToolPlaceholder
		event.ToolPlaceholder = &copied
	}
	if event.Observation != nil {
		copied := *event.Observation
		copied.Media = slices.Clone(event.Observation.Media)
		event.Observation = &copied
	}
	return event
}
