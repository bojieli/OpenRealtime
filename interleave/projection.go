package interleave

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// SlowContextPolicy names a preregistered model-visible context condition.
// Canonical is the production contract. The other policies are scientific
// controls over typed provenance; none creates a second store or agent.
type SlowContextPolicy string

const (
	SlowContextCanonical   SlowContextPolicy = "canonical"
	SlowContextContentOnly SlowContextPolicy = "content-only"
	SlowContextIndependent SlowContextPolicy = "independent"
)

// ParseSlowContextPolicy accepts only the three declared context conditions.
func ParseSlowContextPolicy(value string) (SlowContextPolicy, error) {
	policy := SlowContextPolicy(strings.ToLower(strings.TrimSpace(value)))
	switch policy {
	case SlowContextCanonical, SlowContextContentOnly, SlowContextIndependent:
		return policy, nil
	default:
		return "", fmt.Errorf("unknown slow context policy %q", value)
	}
}

// SlowContextProjection returns nil for the exact canonical production path.
// Controls receive a pure projection and still commit into the same canonical
// store through continuation.Runner's version check.
func SlowContextProjection(policy SlowContextPolicy) (continuation.TrajectoryProjection, error) {
	parsed, err := ParseSlowContextPolicy(string(policy))
	if err != nil {
		return nil, err
	}
	if parsed == SlowContextCanonical {
		return nil, nil
	}
	return func(snapshot trajectory.Snapshot) (trajectory.Snapshot, error) {
		return ProjectSlowContext(snapshot, parsed)
	}, nil
}

// ProjectSlowContext derives one registered control from typed item kinds and
// producer provenance only. It never examines natural-language content.
//
// Content-only retains fast assistant speech but removes fast reasoning,
// proposals, and opaque native state. Independent removes every fast-produced
// item, while preserving observations, prior authoritative slow work, calls,
// and results. Runtime instructions and assistant-state transitions belonging
// to removed fast items are removed as well.
func ProjectSlowContext(snapshot trajectory.Snapshot, policy SlowContextPolicy) (trajectory.Snapshot, error) {
	if policy == SlowContextCanonical {
		return cloneProjectedSnapshot(snapshot), nil
	}
	if policy != SlowContextContentOnly && policy != SlowContextIndependent {
		return trajectory.Snapshot{}, errors.New("slow context projection requires a declared control policy")
	}

	fastInvocations := make(map[string]struct{})
	fastAssistants := make(map[string]struct{})
	for _, item := range snapshot.Items {
		if item.Producer.Phase != trajectory.PhaseFast {
			continue
		}
		if item.InvocationID != "" {
			fastInvocations[item.InvocationID] = struct{}{}
		}
		if item.Kind == trajectory.KindAssistant {
			fastAssistants[item.ID] = struct{}{}
		}
	}

	projected := trajectory.Snapshot{Version: snapshot.Version}
	for _, item := range snapshot.Items {
		if item.Producer.Phase == trajectory.PhaseFast {
			if policy == SlowContextIndependent || item.Kind != trajectory.KindAssistant {
				continue
			}
			item.ProviderStateType = ""
			item.ProviderState = nil
		}
		if item.Kind == trajectory.KindInstruction {
			if _, fast := fastInvocations[item.InvocationID]; fast {
				continue
			}
		}
		if item.Kind == trajectory.KindAssistantState && item.AssistantState != nil {
			if _, fast := fastAssistants[item.AssistantState.AssistantItemID]; fast && policy == SlowContextIndependent {
				continue
			}
		}
		projected.Items = append(projected.Items, cloneProjectedItem(item))
	}

	retained := make(map[string]struct{}, len(projected.Items))
	for _, item := range projected.Items {
		retained[item.ID] = struct{}{}
	}
	for index := range projected.Items {
		parents := projected.Items[index].CausalParentIDs[:0]
		for _, parent := range projected.Items[index].CausalParentIDs {
			if _, exists := retained[parent]; exists {
				parents = append(parents, parent)
			}
		}
		projected.Items[index].CausalParentIDs = parents
	}
	return projected, nil
}

func cloneProjectedSnapshot(snapshot trajectory.Snapshot) trajectory.Snapshot {
	result := trajectory.Snapshot{Version: snapshot.Version, Items: make([]trajectory.Item, len(snapshot.Items))}
	for index, item := range snapshot.Items {
		result.Items[index] = cloneProjectedItem(item)
	}
	return result
}

func cloneProjectedItem(item trajectory.Item) trajectory.Item {
	item.CausalParentIDs = slices.Clone(item.CausalParentIDs)
	item.ProviderState = slices.Clone(item.ProviderState)
	if item.ToolCall != nil {
		call := *item.ToolCall
		call.Arguments = slices.Clone(call.Arguments)
		item.ToolCall = &call
	}
	if item.ToolResult != nil {
		result := *item.ToolResult
		result.Output = slices.Clone(result.Output)
		item.ToolResult = &result
	}
	if item.AssistantState != nil {
		state := *item.AssistantState
		item.AssistantState = &state
	}
	if item.Repair != nil {
		repair := *item.Repair
		item.Repair = &repair
	}
	if item.Event != nil {
		event := *item.Event
		item.Event = &event
	}
	return item
}
