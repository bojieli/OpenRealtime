package interleave

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestEventProcessorRunsFastSlowThenResumesSlowFromToolEvent(t *testing.T) {
	t.Parallel()
	store := trajectory.NewStore()
	catalog := staticCatalog{
		capabilities: []continuation.Capability{{Name: "lookup", Description: "Look up a value.", Available: true, ExecutionPhase: "slow"}},
		tools:        []continuation.ToolDefinition{{Name: "lookup", Description: "Look up a value.", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}
	fast := &sequenceProvider{
		descriptor: descriptor("fast", trajectory.PhaseFast, false),
		scripts: []providerScript{{events: []continuation.Event{
			{Kind: continuation.EventAssistantDelta, Text: "I'll check."},
			{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{CallID: "proposal-1", Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`)}},
		}}},
	}
	slow := &sequenceProvider{
		descriptor: descriptor("slow", trajectory.PhaseSlow, true),
		scripts: []providerScript{
			{events: []continuation.Event{{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{CallID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`)}}}},
			{events: []continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "The value is 7."}}},
		},
	}
	engine, err := New(Config{Store: store, FastProvider: fast, SlowProvider: slow, ToolCatalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	type dispatched struct {
		invocation string
		calls      []trajectory.ToolCall
	}
	var dispatches []dispatched
	var runs []trajectory.Phase
	processor, err := NewProcessor(ProcessorConfig{
		Engine: engine,
		ToolCallSink: func(_ context.Context, invocation string, calls []trajectory.ToolCall) error {
			dispatches = append(dispatches, dispatched{invocation: invocation, calls: calls})
			return nil
		},
		RunObserver: func(phase trajectory.Phase, _ continuation.RunResult) error {
			runs = append(runs, phase)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := eventloop.New(eventloop.Config{Store: store, Processor: processor, MaxPendingEvents: 16, ReservedInterruptEvents: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Submit(eventloop.Event{
		Type: "asr.endpoint", Source: "asr", Channel: "voice", Priority: eventloop.PriorityRoutine,
		Kind: trajectory.KindObservation, SourceRevision: 9,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "What is x?",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.RunNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(dispatches) != 1 || len(dispatches[0].calls) != 1 || dispatches[0].calls[0].CallID != "call-1" {
		t.Fatalf("authoritative slow call was not dispatched: %#v", dispatches)
	}
	if len(fast.Requests()) != 1 || len(slow.Requests()) != 1 {
		t.Fatalf("unexpected first event invocation counts: fast=%d slow=%d", len(fast.Requests()), len(slow.Requests()))
	}
	if _, err := coordinator.Submit(eventloop.Event{
		Type: "tool.results", Source: "environment", Channel: "tool", Priority: eventloop.PriorityRoutine,
		Kind: trajectory.KindToolResult, InvocationID: dispatches[0].invocation,
		ToolResults: []trajectory.ToolResult{{CallID: "call-1", Name: "lookup", Output: json.RawMessage(`{"value":7}`)}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.RunNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fast.Requests()) != 1 || len(slow.Requests()) != 2 {
		t.Fatalf("tool result reran fast or failed to resume slow: fast=%d slow=%d", len(fast.Requests()), len(slow.Requests()))
	}
	if got := slow.Requests()[1].Trajectory; !snapshotContains(got, trajectory.KindAssistant, "I'll check.") ||
		!snapshotContains(got, trajectory.KindToolResult, "") {
		t.Fatalf("resumed slow model did not inherit canonical history: %#v", got)
	}
	if len(runs) != 3 || runs[0] != trajectory.PhaseFast || runs[1] != trajectory.PhaseSlow || runs[2] != trajectory.PhaseSlow {
		t.Fatalf("unexpected canonical continuation sequence: %v", runs)
	}
}
