package interleavebench

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type benchmarkProvider struct {
	descriptor continuation.Descriptor
}

func (provider benchmarkProvider) Descriptor() continuation.Descriptor { return provider.descriptor }

func (provider benchmarkProvider) Continue(_ context.Context, request continuation.Request, emit continuation.Emit) (continuation.Completion, error) {
	if provider.descriptor.Phase == trajectory.PhaseFast {
		return continuation.Completion{}, emit(continuation.Event{Kind: continuation.EventAssistantDelta, Text: "I'll check."})
	}
	for _, item := range request.Trajectory.Items {
		if item.Kind == trajectory.KindToolResult {
			return continuation.Completion{}, emit(continuation.Event{Kind: continuation.EventAssistantDelta, Text: "The total is 42."})
		}
	}
	return continuation.Completion{StopReason: "tool_calls"}, emit(continuation.Event{
		Kind: continuation.EventToolCall,
		ToolCall: &trajectory.ToolCall{
			CallID: "call-1", Name: "records.read", Arguments: json.RawMessage(`{"key":"total"}`),
		},
	})
}

func TestRunProducesSecretFreePassingReport(t *testing.T) {
	t.Parallel()
	fast := benchmarkProvider{descriptor: continuation.Descriptor{
		Provider: "test", Model: "fast", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, Streaming: true,
		ToolAuthority: continuation.ToolAuthorityPropose,
	}}
	slow := benchmarkProvider{descriptor: continuation.Descriptor{
		Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh, Streaming: true, RetainsToolCalls: true, ExecutableTools: true,
	}}
	report, err := Run(context.Background(), Config{
		FastProvider: fast, SlowProvider: slow, TaskTimeout: time.Second,
		Tasks: []Task{{
			ID: "sum", Prompt: "Read total and report it.",
			Records:            map[string]json.RawMessage{"total": json.RawMessage(`{"value":42}`)},
			ExpectedSubstrings: []string{"42"}, RequiredToolCalls: []string{"records.read"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed != 1 || len(report.Results) != 1 || !report.Results[0].Passed || len(report.Results[0].Invocations) != 3 {
		t.Fatalf("unexpected report: %#v", report)
	}
	if report.Summary.PassRate != 1 || report.Summary.FastFirstEventMS.Count != 1 || report.Summary.TotalToolCalls != 1 {
		t.Fatalf("unexpected aggregate summary: %#v", report.Summary)
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "provider_state") || strings.Contains(string(encoded), "reasoning text") {
		t.Fatalf("private state escaped report: %s", encoded)
	}

	filename := filepath.Join(t.TempDir(), "nested", "report.json")
	if err := WriteReport(filename, report); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filename)
	if err != nil || !strings.Contains(string(data), SchemaVersion) {
		t.Fatalf("report was not published: data=%s err=%v", data, err)
	}
}

func TestLoadTasksRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	filename := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(filename, []byte(`[{"id":"x","prompt":"p","records":{},"expected_substrings":["x"],"unknown":true}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTasks(filename); err == nil {
		t.Fatal("expected unknown task field to fail")
	}
}

func TestCheckedTaskFixtureHasExactToolExpectations(t *testing.T) {
	t.Parallel()
	tasks, err := LoadTasks(filepath.Join("..", "benchmarks", "interleave", "v0.1", "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 4 {
		t.Fatalf("task count = %d", len(tasks))
	}
	for _, task := range tasks {
		if len(task.ExpectedToolCalls) != len(task.RequiredToolCalls) {
			t.Fatalf("task %q expected calls=%d required names=%d", task.ID, len(task.ExpectedToolCalls), len(task.RequiredToolCalls))
		}
	}
}
