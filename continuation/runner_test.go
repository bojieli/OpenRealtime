package continuation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/bojieli/OpenRealtime/trajectory"
)

type scriptedProvider struct {
	descriptor Descriptor
	events     []Event
	completion Completion
	err        error
}

type blockingProvider struct {
	descriptor Descriptor
	started    chan Request
	release    chan struct{}
}

func (provider *blockingProvider) Descriptor() Descriptor { return provider.descriptor }

func (provider *blockingProvider) Continue(_ context.Context, request Request, emit Emit) (Completion, error) {
	provider.started <- request
	<-provider.release
	if err := emit(Event{Kind: EventAssistantDelta, Text: "stale answer"}); err != nil {
		return Completion{}, err
	}
	return Completion{}, nil
}

func (provider scriptedProvider) Descriptor() Descriptor { return provider.descriptor }

func (provider scriptedProvider) Continue(_ context.Context, _ Request, emit Emit) (Completion, error) {
	for _, event := range provider.events {
		if err := emit(event); err != nil {
			return Completion{}, err
		}
	}
	return provider.completion, provider.err
}

func TestRunnerAppendsOneInterleavedTurn(t *testing.T) {
	t.Parallel()
	store := trajectory.NewStore()
	if err := store.Append(trajectory.Item{ID: "user", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "Check the calendar."}); err != nil {
		t.Fatal(err)
	}
	var now uint64
	var id uint64
	runner, err := NewRunner(RunnerConfig{
		Store: store, RetainReasoning: true,
		Now:    func() uint64 { now++; return now },
		NextID: func(prefix string) string { id++; return fmt.Sprintf("%s-%d", prefix, id) },
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := scriptedProvider{
		descriptor: Descriptor{Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow, Effort: EffortHigh, Streaming: true, NativeStateType: "test-state", RetainsToolCalls: true, ExecutableTools: true},
		events: []Event{
			{Kind: EventReasoningDelta, Text: "I should inspect it."},
			{Kind: EventAssistantDelta, Text: "I'll check."},
			{Kind: EventToolCall, ToolCall: &trajectory.ToolCall{CallID: "call-1", Name: "calendar.read", Arguments: json.RawMessage(`{"day":"Tuesday"}`)}},
		},
		completion: Completion{StopReason: "tool_call", ProviderStateType: "test-state", ProviderState: json.RawMessage(`{"opaque":true}`)},
	}
	result, err := runner.Run(context.Background(), provider, Invocation{
		Instruction: "Continue.", MaxOutputTokens: 256,
		Tools: []ToolDefinition{{Name: "calendar.read", Description: "Read calendar.", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.AssistantText != "I'll check." || len(result.ToolCalls) != 1 || result.EndVersion != 5 || !result.Committed {
		t.Fatalf("unexpected result: %#v", result)
	}
	items := store.Snapshot().Items
	if items[1].Kind != trajectory.KindInstruction || items[2].Kind != trajectory.KindReasoning || items[3].Kind != trajectory.KindAssistant || items[4].Kind != trajectory.KindToolCall {
		t.Fatalf("unexpected trajectory kinds: %#v", items)
	}
	if items[2].ProviderStateType != "test-state" || len(items[3].ProviderState) != 0 {
		t.Fatal("native state was not attached exactly once")
	}
}

func TestRunnerRejectsStaleSafePointWithoutPublishingOutput(t *testing.T) {
	t.Parallel()
	store := trajectory.NewStore()
	if err := store.Append(trajectory.Item{
		ID: "user-1", Kind: trajectory.KindObservation, MonotonicNS: 1,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "first",
	}); err != nil {
		t.Fatal(err)
	}
	var now uint64 = 1
	runner, err := NewRunner(RunnerConfig{Store: store, Now: func() uint64 { now++; return now }})
	if err != nil {
		t.Fatal(err)
	}
	provider := &blockingProvider{
		descriptor: Descriptor{Provider: "test", Model: "fast", Phase: trajectory.PhaseFast, Effort: EffortMinimal, Streaming: true},
		started:    make(chan Request, 1), release: make(chan struct{}),
	}
	type outcome struct {
		result RunResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, runErr := runner.Run(context.Background(), provider, Invocation{Instruction: "Respond."}, nil)
		done <- outcome{result: result, err: runErr}
	}()
	request := <-provider.started
	if request.Trajectory.Version != 2 || len(request.Trajectory.Items) != 2 || request.Trajectory.Items[1].Kind != trajectory.KindInstruction {
		t.Fatalf("provider did not receive virtual instruction prefix: %#v", request.Trajectory)
	}
	if got := store.Snapshot().Version; got != 1 {
		t.Fatalf("in-flight continuation published instruction early: version=%d", got)
	}
	if err := store.Append(trajectory.Item{
		ID: "user-2", Kind: trajectory.KindObservation, MonotonicNS: 3,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "new event",
	}); err != nil {
		t.Fatal(err)
	}
	close(provider.release)
	got := <-done
	if !errors.Is(got.err, ErrStalePrefix) || got.result.Committed || !got.result.Interrupted || len(got.result.AppendedIDs) != 0 {
		t.Fatalf("stale result escaped: result=%#v err=%v", got.result, got.err)
	}
	snapshot := store.Snapshot()
	if snapshot.Version != 2 || snapshot.Items[1].ID != "user-2" {
		t.Fatalf("stale continuation changed canonical trajectory: %#v", snapshot)
	}
}

func TestRunnerRetainsInterruptedPartialAssistant(t *testing.T) {
	t.Parallel()
	store := trajectory.NewStore()
	runner, err := NewRunner(RunnerConfig{Store: store, Now: func() uint64 { return 1 }})
	if err != nil {
		t.Fatal(err)
	}
	provider := scriptedProvider{
		descriptor: Descriptor{Provider: "test", Model: "fast", Phase: trajectory.PhaseFast, Effort: EffortMinimal, Streaming: true},
		events:     []Event{{Kind: EventAssistantDelta, Text: "partial"}},
		err:        errors.New("connection lost"),
	}
	result, err := runner.Run(context.Background(), provider, Invocation{Instruction: "Respond."}, nil)
	if err == nil || !result.Interrupted {
		t.Fatalf("expected interrupted provider error, result=%#v err=%v", result, err)
	}
	items := store.Snapshot().Items
	if len(items) != 2 || !items[1].Interrupted || items[1].Content != "partial" {
		t.Fatalf("partial assistant was not retained: %#v", items)
	}
}

func TestRunnerDoesNotPublishInterruptedToolCall(t *testing.T) {
	t.Parallel()
	store := trajectory.NewStore()
	runner, err := NewRunner(RunnerConfig{Store: store, Now: func() uint64 { return 1 }})
	if err != nil {
		t.Fatal(err)
	}
	provider := scriptedProvider{
		descriptor: Descriptor{
			Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow,
			Effort: EffortHigh, Streaming: true, RetainsToolCalls: true, ExecutableTools: true,
		},
		events: []Event{
			{Kind: EventAssistantDelta, Text: "I will inspect it."},
			{Kind: EventToolCall, ToolCall: &trajectory.ToolCall{CallID: "call-unsafe", Name: "lookup", Arguments: json.RawMessage(`{}`)}},
		},
		err: errors.New("stream failed before the terminal safe point"),
	}
	result, err := runner.Run(context.Background(), provider, Invocation{
		Instruction: "Continue.",
		Tools:       []ToolDefinition{{Name: "lookup", Description: "Lookup.", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}, nil)
	if err == nil || !result.Interrupted || len(result.ToolCalls) != 0 {
		t.Fatalf("interrupted call escaped: result=%#v err=%v", result, err)
	}
	items := store.Snapshot().Items
	if len(items) != 2 || items[1].Kind != trajectory.KindAssistant {
		t.Fatalf("unexpected interrupted trajectory: %#v", items)
	}
}

func TestRunnerObserverCanCancelStream(t *testing.T) {
	t.Parallel()
	store := trajectory.NewStore()
	runner, err := NewRunner(RunnerConfig{Store: store, Now: func() uint64 { return 1 }})
	if err != nil {
		t.Fatal(err)
	}
	provider := scriptedProvider{
		descriptor: Descriptor{Provider: "test", Model: "fast", Phase: trajectory.PhaseFast, Effort: EffortMinimal, Streaming: true},
		events:     []Event{{Kind: EventAssistantDelta, Text: "one"}, {Kind: EventAssistantDelta, Text: "two"}},
	}
	want := errors.New("stop")
	_, err = runner.Run(context.Background(), provider, Invocation{Instruction: "Respond."}, func(Event) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want observer error", err)
	}
}

func TestRunnerRejectsToolCallFromNonExecutableProvider(t *testing.T) {
	t.Parallel()
	store := trajectory.NewStore()
	runner, err := NewRunner(RunnerConfig{Store: store, Now: func() uint64 { return 1 }})
	if err != nil {
		t.Fatal(err)
	}
	provider := scriptedProvider{
		descriptor: Descriptor{
			Provider: "test", Model: "fast", Phase: trajectory.PhaseFast,
			Effort: EffortMinimal, Streaming: true,
		},
		events: []Event{{Kind: EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "call-unauthorized", Name: "lookup", Arguments: json.RawMessage(`{}`),
		}}},
	}
	result, err := runner.Run(context.Background(), provider, Invocation{Instruction: "Respond."}, nil)
	if err == nil || len(result.ToolCalls) != 0 {
		t.Fatalf("unauthorized tool call escaped: result=%#v err=%v", result, err)
	}
	for _, item := range store.Snapshot().Items {
		if item.Kind == trajectory.KindToolCall {
			t.Fatalf("unauthorized tool call was appended: %#v", item)
		}
	}
}

func TestRunnerRejectsUndeclaredToolCall(t *testing.T) {
	t.Parallel()
	store := trajectory.NewStore()
	runner, err := NewRunner(RunnerConfig{Store: store, Now: func() uint64 { return 1 }})
	if err != nil {
		t.Fatal(err)
	}
	provider := scriptedProvider{
		descriptor: Descriptor{
			Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow,
			Effort: EffortHigh, Streaming: true, ToolAuthority: ToolAuthorityExecute,
		},
		events: []Event{{Kind: EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "call-unknown", Name: "unknown", Arguments: json.RawMessage(`{}`),
		}}},
	}
	result, err := runner.Run(context.Background(), provider, Invocation{
		Instruction: "Continue.", Tools: []ToolDefinition{{Name: "lookup", Description: "Lookup.", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}, nil)
	if err == nil || len(result.ToolCalls) != 0 {
		t.Fatalf("undeclared tool escaped: result=%#v err=%v", result, err)
	}
	for _, item := range store.Snapshot().Items {
		if item.Kind == trajectory.KindToolCall {
			t.Fatalf("undeclared tool was committed: %#v", item)
		}
	}
}

func TestRunnerRecordsProposalWithoutExecutionAuthorityOrNativeCallState(t *testing.T) {
	t.Parallel()
	store := trajectory.NewStore()
	runner, err := NewRunner(RunnerConfig{Store: store, Now: func() uint64 { return 1 }})
	if err != nil {
		t.Fatal(err)
	}
	provider := scriptedProvider{
		descriptor: Descriptor{
			Provider: "test", Model: "fast", Phase: trajectory.PhaseFast,
			Effort: EffortMinimal, Streaming: true, NativeStateType: "test-state",
			RetainsToolCalls: true, ToolAuthority: ToolAuthorityPropose,
		},
		events: []Event{{Kind: EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "proposal-1", Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`),
		}}},
		completion: Completion{ProviderStateType: "test-state", ProviderState: json.RawMessage(`{"tool_calls":["proposal-1"]}`)},
	}
	result, err := runner.Run(context.Background(), provider, Invocation{
		Instruction: "Respond.", Tools: []ToolDefinition{{Name: "lookup", Description: "Lookup.", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToolProposals) != 1 || len(result.ToolCalls) != 0 {
		t.Fatalf("proposal authority escaped as execution: %#v", result)
	}
	items := store.Snapshot().Items
	if len(items) != 2 || items[1].Kind != trajectory.KindToolProposal || len(items[1].ProviderState) != 0 {
		t.Fatalf("proposal was not portably isolated: %#v", items)
	}
	if err := store.Append(trajectory.Item{
		ID: "result", Kind: trajectory.KindToolResult, MonotonicNS: 2,
		Producer:   trajectory.Producer{Phase: trajectory.PhaseTool},
		ToolResult: &trajectory.ToolResult{CallID: "proposal-1", Name: "lookup", Output: json.RawMessage(`{"value":7}`)},
	}); err == nil {
		t.Fatal("proposal unexpectedly accepted an executable tool result")
	}
}
