package preparation

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type latestProvider struct {
	mu        sync.Mutex
	active    int
	maxActive int
	started   chan string
}

func (provider *latestProvider) Descriptor() continuation.Descriptor {
	return continuation.Descriptor{
		Provider: "test", Model: "fast", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, Streaming: true,
		ToolAuthority: continuation.ToolAuthorityPropose, RetainsToolCalls: true,
	}
}

func (provider *latestProvider) Continue(ctx context.Context, request continuation.Request, emit continuation.Emit) (continuation.Completion, error) {
	text := request.Trajectory.Items[0].Content
	provider.mu.Lock()
	provider.active++
	provider.maxActive = max(provider.maxActive, provider.active)
	provider.mu.Unlock()
	defer func() {
		provider.mu.Lock()
		provider.active--
		provider.mu.Unlock()
	}()
	provider.started <- text
	if text == "first" {
		<-ctx.Done()
		return continuation.Completion{}, ctx.Err()
	}
	if err := emit(continuation.Event{Kind: continuation.EventAssistantDelta, Text: "I'll check."}); err != nil {
		return continuation.Completion{}, err
	}
	if err := emit(continuation.Event{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
		CallID: "proposal-1", Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`),
	}}); err != nil {
		return continuation.Completion{}, err
	}
	return continuation.Completion{StopReason: "tool_calls"}, nil
}

func TestFingerprintIgnoresOperationalRevisionButNotSemanticInput(t *testing.T) {
	t.Parallel()
	first := preparedInput("same", 1)
	second := preparedInput("same", 99)
	second.Request.Trajectory.Items[0].ID = "different-operational-id"
	second.Request.Trajectory.Items[0].MonotonicNS = 12345
	left, err := Fingerprint(first)
	if err != nil {
		t.Fatal(err)
	}
	right, err := Fingerprint(second)
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatalf("operational metadata changed semantic fingerprint: %s != %s", left, right)
	}
	changed, err := Fingerprint(preparedInput("changed", 99))
	if err != nil {
		t.Fatal(err)
	}
	if changed == left {
		t.Fatal("changed model-visible text retained the same fingerprint")
	}
}

func TestManagerCancelsAndCoalescesToLatestExactInput(t *testing.T) {
	t.Parallel()
	provider := &latestProvider{started: make(chan string, 4)}
	manager, err := NewManager(Config{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Observe(ctx, preparedInput("first", 1)); err != nil {
		t.Fatal(err)
	}
	if got := <-provider.started; got != "first" {
		t.Fatalf("first start = %q", got)
	}
	latest := preparedInput("latest", 2)
	if err := manager.Observe(ctx, latest); err != nil {
		t.Fatal(err)
	}
	if err := manager.Observe(ctx, preparedInput("latest", 3)); err != nil {
		t.Fatal(err)
	}
	if got := <-provider.started; got != "latest" {
		t.Fatalf("latest start = %q", got)
	}
	candidate, report, err := manager.Finalize(ctx, preparedInput("latest", 4))
	if err != nil {
		t.Fatal(err)
	}
	if candidate == nil || !report.Accepted || report.Superseded != 1 || report.Coalesced != 1 {
		t.Fatalf("unexpected preparation result: candidate=%v report=%+v", candidate != nil, report)
	}
	provider.mu.Lock()
	maxActive := provider.maxActive
	provider.mu.Unlock()
	if maxActive != 1 {
		t.Fatalf("latest-wins manager ran %d provider calls concurrently", maxActive)
	}

	replay := candidate.ReplayProvider()
	var assistant string
	var proposals int
	_, err = replay.Continue(ctx, preparedInput("latest", 4).Request, func(event continuation.Event) error {
		if event.Kind == continuation.EventAssistantDelta {
			assistant += event.Text
		}
		if event.Kind == continuation.EventToolCall {
			proposals++
		}
		return nil
	})
	if err != nil || assistant != "I'll check." || proposals != 1 {
		t.Fatalf("replay assistant=%q proposals=%d err=%v", assistant, proposals, err)
	}
	if _, err := replay.Continue(ctx, preparedInput("latest", 4).Request, func(continuation.Event) error { return nil }); err == nil {
		t.Fatal("one-shot candidate replayed twice")
	}
}

func TestCandidateReplayRejectsDifferentSemanticInput(t *testing.T) {
	t.Parallel()
	provider := &latestProvider{started: make(chan string, 1)}
	manager, err := NewManager(Config{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	input := preparedInput("ready", 1)
	if err := manager.Observe(ctx, input); err != nil {
		t.Fatal(err)
	}
	<-provider.started
	candidate, _, err := manager.Finalize(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if candidate == nil {
		t.Fatal("expected candidate")
	}
	changed := preparedInput("different", 2)
	if _, err := candidate.ReplayProvider().Continue(ctx, changed.Request, func(continuation.Event) error { return nil }); err == nil {
		t.Fatal("candidate replay accepted different model-visible input")
	}
}

func TestFinalizeRejectsStaleCompletedCandidate(t *testing.T) {
	t.Parallel()
	provider := &latestProvider{started: make(chan string, 2)}
	manager, err := NewManager(Config{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Observe(ctx, preparedInput("ready", 1)); err != nil {
		t.Fatal(err)
	}
	<-provider.started
	for manager.Report().Completed == 0 {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	candidate, report, err := manager.Finalize(ctx, preparedInput("different", 2))
	if err != nil {
		t.Fatal(err)
	}
	if candidate != nil || report.Accepted {
		t.Fatalf("stale candidate was accepted: %+v", report)
	}
}

func TestManagerCountsNonSupersessionFailure(t *testing.T) {
	t.Parallel()
	provider := &latestProvider{started: make(chan string, 1)}
	manager, err := NewManager(Config{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Observe(ctx, preparedInput("first", 1)); err != nil {
		t.Fatal(err)
	}
	<-provider.started
	report, err := manager.Close(ctx, errors.New("admission preempted"))
	if err != nil {
		t.Fatal(err)
	}
	if report.Failed != 1 || report.Superseded != 0 || len(report.Attempts) != 1 || report.Attempts[0].Outcome != "failed" {
		t.Fatalf("failure telemetry = %+v", report)
	}
}

func preparedInput(text string, revision uint64) Input {
	descriptor := (&latestProvider{}).Descriptor()
	invocation := continuation.Invocation{
		Instruction: "Respond.", SourceRevision: revision,
		Capabilities:    []continuation.Capability{{Name: "lookup", Description: "Lookup.", Available: true, ExecutionPhase: "slow"}},
		Tools:           []continuation.ToolDefinition{{Name: "lookup", Description: "Lookup.", Parameters: json.RawMessage(`{"type":"object"}`)}},
		MaxOutputTokens: 32,
	}
	return Input{Request: continuation.Request{
		InvocationID: "operational", Descriptor: descriptor, Invocation: invocation,
		Trajectory: trajectory.Snapshot{Version: 2, Items: []trajectory.Item{
			{ID: "observation", Kind: trajectory.KindObservation, SourceRevision: revision, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: text},
			{ID: "instruction", Kind: trajectory.KindInstruction, SourceRevision: revision, Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, Content: invocation.Instruction},
		}},
	}}
}

func TestNewManagerRejectsExecutableProvider(t *testing.T) {
	t.Parallel()
	provider := &latestProvider{}
	descriptorProvider := executableProvider{DescriptorValue: provider.Descriptor()}
	descriptorProvider.DescriptorValue.ToolAuthority = continuation.ToolAuthorityExecute
	descriptorProvider.DescriptorValue.ExecutableTools = true
	if _, err := NewManager(Config{Provider: descriptorProvider}); err == nil {
		t.Fatal("expected executable preparation provider to fail")
	}
}

type executableProvider struct{ DescriptorValue continuation.Descriptor }

func (provider executableProvider) Descriptor() continuation.Descriptor {
	return provider.DescriptorValue
}
func (provider executableProvider) Continue(context.Context, continuation.Request, continuation.Emit) (continuation.Completion, error) {
	return continuation.Completion{}, errors.New("not called")
}
