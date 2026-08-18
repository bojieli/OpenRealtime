package benchspec

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestScoreToolTrajectoryRequiresExactSemanticMultiset(t *testing.T) {
	t.Parallel()
	expected := []ToolCallExpectation{{Name: "records.read", Arguments: json.RawMessage(`{"key":"a"}`)}}
	calls := []trajectory.ToolCall{
		{CallID: "1", Name: "records.read", Arguments: json.RawMessage(`{"key":"wrong"}`)},
		{CallID: "2", Name: "records.read", Arguments: json.RawMessage(`{"key":"a"}`)},
	}
	results := []trajectory.ToolResult{
		{CallID: "1", Name: "records.read", Error: "missing"},
		{CallID: "2", Name: "records.read", Output: json.RawMessage(`{"value":1}`)},
	}
	failures := ScoreToolTrajectory([]string{"records.read"}, expected, calls, results, false)
	joined := strings.Join(failures, " | ")
	if !strings.Contains(joined, "unexpected tool call") || !strings.Contains(joined, "returned an error") {
		t.Fatalf("failures = %v", failures)
	}
}

func TestScoreToolTrajectoryTreatsObjectKeyOrderAsEquivalent(t *testing.T) {
	t.Parallel()
	expected := []ToolCallExpectation{{Name: "tool", Arguments: json.RawMessage(`{"a":1,"b":2}`)}}
	calls := []trajectory.ToolCall{{CallID: "1", Name: "tool", Arguments: json.RawMessage(`{"b":2,"a":1}`)}}
	results := []trajectory.ToolResult{{CallID: "1", Name: "tool", Output: json.RawMessage(`{"ok":true}`)}}
	if failures := ScoreToolTrajectory(nil, expected, calls, results, false); len(failures) != 0 {
		t.Fatalf("failures = %v", failures)
	}
}

func TestScoreToolTrajectoryRequiresExactlyOneMatchingResult(t *testing.T) {
	t.Parallel()
	calls := []trajectory.ToolCall{{CallID: "1", Name: "tool", Arguments: json.RawMessage(`{}`)}}
	tests := []struct {
		name    string
		results []trajectory.ToolResult
		want    string
	}{
		{name: "missing", want: "has no terminal result"},
		{name: "orphan", results: []trajectory.ToolResult{{CallID: "2", Name: "tool"}}, want: "unknown call ID"},
		{name: "wrong name", results: []trajectory.ToolResult{{CallID: "1", Name: "other"}}, want: "does not match"},
		{name: "duplicate", results: []trajectory.ToolResult{{CallID: "1", Name: "tool"}, {CallID: "1", Name: "tool"}}, want: "2 terminal results"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			joined := strings.Join(ScoreToolTrajectory(nil, nil, calls, test.results, true), " | ")
			if !strings.Contains(joined, test.want) {
				t.Fatalf("failures = %q, want %q", joined, test.want)
			}
		})
	}
}

func TestValidateToolCallExpectationsRejectsNonObjectArguments(t *testing.T) {
	t.Parallel()
	if err := ValidateToolCallExpectations([]ToolCallExpectation{{Name: "tool", Arguments: json.RawMessage(`[]`)}}); err == nil {
		t.Fatal("expected non-object arguments to fail")
	}
}
