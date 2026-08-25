package continuation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
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

type requestCapturingProvider struct {
	descriptor Descriptor
	request    Request
}

func (provider *requestCapturingProvider) Descriptor() Descriptor { return provider.descriptor }

func (provider *requestCapturingProvider) Continue(_ context.Context, request Request, emit Emit) (Completion, error) {
	provider.request = request
	if err := emit(Event{Kind: EventAssistantDelta, Text: "projected answer"}); err != nil {
		return Completion{}, err
	}
	return Completion{}, nil
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

func TestRunnerProjectionChangesOnlyProviderView(t *testing.T) {
	t.Parallel()
	store := trajectory.NewStore()
	if err := store.AppendBatch([]trajectory.Item{
		{ID: "user", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "question"},
		{ID: "private", Kind: trajectory.KindReasoning, Producer: trajectory.Producer{Phase: trajectory.PhaseFast}, Content: "working"},
	}); err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(RunnerConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	provider := &requestCapturingProvider{descriptor: Descriptor{
		Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow,
		Effort: EffortHigh, Streaming: true,
	}}
	result, err := runner.RunProjected(context.Background(), provider, Invocation{Instruction: "continue"}, func(snapshot trajectory.Snapshot) (trajectory.Snapshot, error) {
		snapshot.Items = snapshot.Items[:1]
		return snapshot, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.request.Trajectory.Items) != 2 || provider.request.Trajectory.Items[0].ID != "user" || provider.request.Trajectory.Version != 3 {
		t.Fatalf("unexpected provider projection: %#v", provider.request.Trajectory)
	}
	canonical := store.Snapshot()
	if !result.Committed || canonical.Version != 4 || canonical.Items[1].ID != "private" || canonical.Items[2].Kind != trajectory.KindInstruction {
		t.Fatalf("projection changed canonical commit: result=%#v snapshot=%#v", result, canonical)
	}
}

func TestRunnerRejectsProjectionThatChangesSemanticContent(t *testing.T) {
	t.Parallel()
	store := trajectory.NewStore()
	if err := store.Append(trajectory.Item{ID: "user", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "original"}); err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(RunnerConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	provider := &requestCapturingProvider{descriptor: Descriptor{
		Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow,
		Effort: EffortHigh, Streaming: true,
	}}
	result, err := runner.RunProjected(context.Background(), provider, Invocation{Instruction: "continue"}, func(snapshot trajectory.Snapshot) (trajectory.Snapshot, error) {
		snapshot.Items[0].Content = "fabricated"
		return snapshot, nil
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "changed semantic item") || result.Committed {
		t.Fatalf("semantic projection mutation escaped: result=%#v err=%v", result, err)
	}
	if got := store.Snapshot(); got.Version != 1 || got.Items[0].Content != "original" {
		t.Fatalf("rejected projection mutated canonical state: %#v", got)
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

// An undeclared name must never execute, and must never cost the session.
//
// A reasoner that hallucinates a tool name has made the same kind of mistake
// as one that hallucinates an argument, and a tool error is what both are for.
// Failing the invocation would end the conversation over the model's spelling:
// observed in the suites as "google_calendar.list_events?" with the question
// mark attached, and as a sentence from the model's own instructions.
func TestAnUndeclaredToolIsRecordedRatherThanExecutedOrFatal(t *testing.T) {
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
	if err != nil {
		t.Fatalf("an undeclared name ended the invocation: %v", err)
	}
	if len(result.ToolCalls) != 0 {
		t.Fatalf("undeclared tool escaped as executable: %#v", result.ToolCalls)
	}
	if len(result.ToolProposals) != 1 || result.ToolProposals[0].Name != "unknown" {
		t.Fatalf("the attempt has to be recorded so the model can see it failed: %#v", result.ToolProposals)
	}
	var proposals int
	for _, item := range store.Snapshot().Items {
		if item.Kind == trajectory.KindToolCall {
			t.Fatalf("undeclared tool was committed as executable: %#v", item)
		}
		if item.Kind == trajectory.KindToolProposal {
			proposals++
		}
	}
	if proposals != 1 {
		t.Fatalf("the log records one non-executable proposal, got %d", proposals)
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

// A continuation commits against the version it started from, so anything
// appended while it was thinking discards everything it produced - including
// its tool calls.
//
// This is reachable from outside: a caller who says one more word while the
// reasoner is working appends an observation, and the action the agent had
// worked out is thrown away. ErrStalePrefix says the output "must be
// recomputed from the new prefix", and nothing anywhere recomputes it.
func TestAnAppendWhileReasoningDiscardsTheReasoning(t *testing.T) {
	store := trajectory.NewStore()
	if err := store.Append(trajectory.Item{
		ID: "user-1", Kind: trajectory.KindObservation, MonotonicNS: 1,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
		Content:  "what is my balance",
	}); err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(RunnerConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}

	appended := make(chan struct{})
	provider := &interposingProvider{
		descriptor: Descriptor{
			Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow,
			Effort: EffortHigh, Streaming: true, ToolAuthority: ToolAuthorityExecute,
			SpeechAuthority: SpeechAuthoritySilent,
		},
		events: []Event{{Kind: EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{}`),
		}}},
		// Something else reaches the log while the model is still producing.
		duringRun: func() {
			_ = store.Append(trajectory.Item{
				ID: "user-2", Kind: trajectory.KindObservation, MonotonicNS: 2,
				Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
				Content:  "actually, hold on",
			})
			close(appended)
		},
	}
	result, runErr := runner.Run(context.Background(), provider, Invocation{
		Instruction: "Continue.",
		Tools: []ToolDefinition{{
			Name: "lookup", Description: "Lookup.", Parameters: json.RawMessage(`{"type":"object"}`),
		}},
	}, nil)
	<-appended
	err = runErr

	if !errors.Is(err, ErrStalePrefix) {
		t.Fatalf("a commit against a moved prefix is stale, got %v", err)
	}
	if result.Committed {
		t.Fatal("nothing derived from a stale prefix may enter the log")
	}
	if len(result.ToolCalls) != 0 {
		t.Fatalf("the discarded output still names calls: %+v", result.ToolCalls)
	}
	// The cost: the agent worked out an action and the caller will never see
	// it happen, and nothing in the system will work it out again.
	for _, item := range store.Snapshot().Items {
		if item.Kind == trajectory.KindToolCall {
			t.Fatal("the call reached the log after all")
		}
	}
}

// interposingProvider lets a test append to the log at the moment a
// continuation is mid-flight, which is the race the version check exists for.
type interposingProvider struct {
	descriptor Descriptor
	events     []Event
	duringRun  func()
	once       sync.Once
}

func (provider *interposingProvider) Descriptor() Descriptor { return provider.descriptor }

func (provider *interposingProvider) Continue(
	_ context.Context, _ Request, emit Emit,
) (Completion, error) {
	for _, event := range provider.events {
		if err := emit(event); err != nil {
			return Completion{}, err
		}
	}
	provider.once.Do(func() {
		if provider.duringRun != nil {
			provider.duringRun()
		}
	})
	return Completion{}, nil
}

// The voice talking while the reasoner reasons is the whole arrangement, and
// it used to throw the reasoning away.
//
// A commit checked against a version number cannot tell "the person said
// something else" from "the agent filled a silence": both move the number.
// Only the first invalidates what the reasoner worked out, and refusing its
// tool call because the voice said "one moment" removes the concurrency the
// design exists for.
func TestTheVoiceSpeakingDoesNotDiscardTheReasoning(t *testing.T) {
	store := trajectory.NewStore()
	if err := store.Append(trajectory.Item{
		ID: "user-1", Kind: trajectory.KindObservation, MonotonicNS: 1,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
		Content:  "what is my balance",
	}); err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(RunnerConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}

	spoke := make(chan struct{})
	provider := &interposingProvider{
		descriptor: Descriptor{
			Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow,
			Effort: EffortHigh, Streaming: true, ToolAuthority: ToolAuthorityExecute,
			SpeechAuthority: SpeechAuthoritySilent,
		},
		events: []Event{{Kind: EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{}`),
		}}},
		duringRun: func() {
			_ = store.Append(trajectory.Item{
				ID: "voice-1", Kind: trajectory.KindAssistant, MonotonicNS: 2,
				Producer: trajectory.Producer{
					Phase: trajectory.PhaseFast, SpeechAuthority: "voice",
				},
				Content: "One moment.", Visibility: trajectory.VisibilityPrepared,
			})
			close(spoke)
		},
	}
	result, err := runner.Run(context.Background(), provider, Invocation{
		Instruction: "Continue.",
		Tools: []ToolDefinition{{
			Name: "lookup", Description: "Lookup.", Parameters: json.RawMessage(`{"type":"object"}`),
		}},
	}, nil)
	<-spoke

	if err != nil {
		t.Fatalf("the voice filling a silence is not new evidence: %v", err)
	}
	if !result.Committed {
		t.Fatal("the reasoning was discarded because the agent talked")
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("the action it worked out has to survive: %+v", result.ToolCalls)
	}
	var calls int
	for _, item := range store.Snapshot().Items {
		if item.Kind == trajectory.KindToolCall {
			calls++
		}
	}
	if calls != 1 {
		t.Fatalf("the call has to reach the log, got %d", calls)
	}
}
