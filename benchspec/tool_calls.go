// Package benchspec defines provider-neutral, externally checkable benchmark
// expectations. It contains no model routing or orchestration policy.
package benchspec

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/bojieli/OpenRealtime/trajectory"
)

// ToolTrajectoryScorerVersion identifies the exact action/result scoring
// semantics embedded in benchmark reports.
const ToolTrajectoryScorerVersion = "exact-tool-trajectory-v2"

// ToolCallExpectation identifies one required call by semantic name and JSON
// arguments. Call IDs are intentionally absent because providers allocate them
// at runtime.
type ToolCallExpectation struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ValidateToolCallExpectations rejects ambiguous benchmark specifications.
func ValidateToolCallExpectations(expectations []ToolCallExpectation) error {
	for index, expectation := range expectations {
		if strings.TrimSpace(expectation.Name) == "" {
			return fmt.Errorf("expected tool call %d requires a name", index)
		}
		if _, err := canonicalObject(expectation.Arguments); err != nil {
			return fmt.Errorf("expected tool call %d arguments: %w", index, err)
		}
	}
	return nil
}

// ScoreToolTrajectory checks minimum named calls, an optional exact multiset
// of name/argument pairs, and tool-result failures. Exact expectations reject
// both missing and extra calls. Tool errors fail by default because a repaired
// final answer does not erase wasted or unsafe actions.
func ScoreToolTrajectory(
	requiredNames []string,
	expected []ToolCallExpectation,
	calls []trajectory.ToolCall,
	results []trajectory.ToolResult,
	allowToolErrors bool,
) []string {
	var failures []string
	callNames := make(map[string]string, len(calls))
	resultCounts := make(map[string]int, len(results))
	availableNames := make(map[string]int, len(calls))
	for _, call := range calls {
		availableNames[call.Name]++
		if strings.TrimSpace(call.CallID) == "" {
			failures = append(failures, fmt.Sprintf("tool call %q has no call ID", call.Name))
			continue
		}
		if _, exists := callNames[call.CallID]; exists {
			failures = append(failures, fmt.Sprintf("tool call ID %q is duplicated", call.CallID))
			continue
		}
		callNames[call.CallID] = call.Name
	}
	for _, required := range requiredNames {
		if availableNames[required] == 0 {
			failures = append(failures, fmt.Sprintf("required tool %q was not called", required))
		} else {
			availableNames[required]--
		}
	}

	if len(expected) > 0 {
		expectedCounts := make(map[string]int, len(expected))
		actualCounts := make(map[string]int, len(calls))
		for _, item := range expected {
			key, err := semanticCallKey(item.Name, item.Arguments)
			if err != nil {
				failures = append(failures, fmt.Sprintf("invalid expected tool call %q: %v", item.Name, err))
				continue
			}
			expectedCounts[key]++
		}
		for _, call := range calls {
			key, err := semanticCallKey(call.Name, call.Arguments)
			if err != nil {
				failures = append(failures, fmt.Sprintf("tool call %q has invalid arguments: %v", call.Name, err))
				continue
			}
			actualCounts[key]++
		}
		keys := make([]string, 0, len(expectedCounts)+len(actualCounts))
		seen := make(map[string]struct{}, cap(keys))
		for key := range expectedCounts {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
		for key := range actualCounts {
			if _, exists := seen[key]; !exists {
				keys = append(keys, key)
			}
		}
		slices.Sort(keys)
		for _, key := range keys {
			want, got := expectedCounts[key], actualCounts[key]
			if got < want {
				failures = append(failures, fmt.Sprintf("expected tool call %s missing %d occurrence(s)", printableCallKey(key), want-got))
			}
			if got > want {
				failures = append(failures, fmt.Sprintf("unexpected tool call %s occurred %d extra time(s)", printableCallKey(key), got-want))
			}
		}
	}

	for _, result := range results {
		callName, exists := callNames[result.CallID]
		if !exists {
			failures = append(failures, fmt.Sprintf("tool result %q references unknown call ID %q", result.Name, result.CallID))
		} else if result.Name != callName {
			failures = append(failures, fmt.Sprintf("tool result name %q does not match call %q", result.Name, callName))
		}
		resultCounts[result.CallID]++
		if !allowToolErrors && strings.TrimSpace(result.Error) != "" {
			failures = append(failures, fmt.Sprintf("tool %q returned an error", result.Name))
		}
	}
	for callID, name := range callNames {
		switch resultCounts[callID] {
		case 0:
			failures = append(failures, fmt.Sprintf("tool call %q with ID %q has no terminal result", name, callID))
		case 1:
		default:
			failures = append(failures, fmt.Sprintf("tool call %q with ID %q has %d terminal results", name, callID, resultCounts[callID]))
		}
	}
	return failures
}

func semanticCallKey(name string, arguments json.RawMessage) (string, error) {
	canonical, err := canonicalObject(arguments)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(name) + "\x00" + string(canonical), nil
}

func printableCallKey(key string) string {
	name, arguments, _ := strings.Cut(key, "\x00")
	return fmt.Sprintf("%q(%s)", name, arguments)
}

func canonicalObject(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil, errors.New("must be valid JSON")
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, errors.New("must be one JSON object")
	}
	canonical, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("canonicalize JSON object: %w", err)
	}
	return canonical, nil
}
