package preparation

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type chainTestProvider struct {
	descriptor continuation.Descriptor
	events     []continuation.Event
	started    chan struct{}
	release    chan struct{}
	requestsMu sync.Mutex
	requests   []continuation.Request
}

func (provider *chainTestProvider) Descriptor() continuation.Descriptor { return provider.descriptor }

func (provider *chainTestProvider) Continue(ctx context.Context, request continuation.Request, emit continuation.Emit) (continuation.Completion, error) {
	provider.requestsMu.Lock()
	provider.requests = append(provider.requests, cloneInput(Input{Request: request}).Request)
	provider.requestsMu.Unlock()
	if provider.started != nil {
		select {
		case <-provider.started:
		default:
			close(provider.started)
		}
	}
	if provider.release != nil {
		select {
		case <-provider.release:
		case <-ctx.Done():
			return continuation.Completion{}, ctx.Err()
		}
	}
	for _, event := range provider.events {
		if err := emit(event); err != nil {
			return continuation.Completion{}, err
		}
	}
	return continuation.Completion{StopReason: "done"}, nil
}

func (provider *chainTestProvider) Requests() []continuation.Request {
	provider.requestsMu.Lock()
	defer provider.requestsMu.Unlock()
	return append([]continuation.Request(nil), provider.requests...)
}

type fallbackProvider struct {
	descriptor continuation.Descriptor
	calls      atomic.Int64
}

func (provider *fallbackProvider) Descriptor() continuation.Descriptor { return provider.descriptor }
func (provider *fallbackProvider) Continue(_ context.Context, _ continuation.Request, emit continuation.Emit) (continuation.Completion, error) {
	provider.calls.Add(1)
	if err := emit(continuation.Event{Kind: continuation.EventAssistantDelta, Text: "fallback"}); err != nil {
		return continuation.Completion{}, err
	}
	return continuation.Completion{}, nil
}

func TestChainPreparesSlowFromFastTrajectoryAndCommitsWithoutWaiting(t *testing.T) {
	t.Parallel()
	fastDescriptor := continuation.Descriptor{
		Provider: "test", Model: "fast", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, Streaming: true,
		ToolAuthority: continuation.ToolAuthorityPropose, RetainsToolCalls: true,
	}
	slowDescriptor := continuation.Descriptor{
		Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh, Streaming: true,
		ToolAuthority: continuation.ToolAuthorityExecute, ExecutableTools: true,
		RetainsToolCalls: true,
	}
	fast := &chainTestProvider{descriptor: fastDescriptor, events: []continuation.Event{
		{Kind: continuation.EventReasoningDelta, Text: "need the lookup"},
		{Kind: continuation.EventAssistantDelta, Text: "I'll check."},
		{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "proposal-1", Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`),
		}},
	}}
	slowStarted, slowRelease := make(chan struct{}), make(chan struct{})
	slow := &chainTestProvider{
		descriptor: slowDescriptor, started: slowStarted, release: slowRelease,
		events: []continuation.Event{{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`),
		}}},
	}
	fastInvocation, slowInvocation := chainInvocations()
	manager, err := NewChainManager(ChainConfig{
		Stages: []ChainStage{
			{Provider: fast, Invocation: fastInvocation},
			{Provider: slow, Invocation: slowInvocation},
		},
		RetainReasoning: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	input := chainInput("lookup key", 7)
	if err := manager.Observe(ctx, input); err != nil {
		t.Fatal(err)
	}
	select {
	case <-slowStarted:
	case <-ctx.Done():
		t.Fatal("slow preparation did not start after fast completion")
	}

	committed, report, err := manager.Commit(chainInput("lookup key", 99))
	if err != nil {
		t.Fatal(err)
	}
	if committed == nil || !report.Committed {
		t.Fatalf("exact semantic root was not committed: %+v", report)
	}
	fastFallback := &fallbackProvider{descriptor: fastDescriptor}
	slowFallback := &fallbackProvider{descriptor: slowDescriptor}
	preparedFast, err := committed.StageProvider(0, fastFallback)
	if err != nil {
		t.Fatal(err)
	}
	preparedSlow, err := committed.StageProvider(1, slowFallback)
	if err != nil {
		t.Fatal(err)
	}

	store := trajectory.NewStore()
	if err := store.AppendBatch(cloneSnapshot(input.Trajectory).Items); err != nil {
		t.Fatal(err)
	}
	runner, err := continuation.NewRunner(continuation.RunnerConfig{
		Store: store, RetainReasoning: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	fastInvocation.SourceRevision = 99
	fastResult, err := runner.Run(ctx, preparedFast, fastInvocation, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fastResult.AssistantText != "I'll check." || len(fastResult.ToolProposals) != 1 {
		t.Fatalf("fast replay = %+v", fastResult)
	}

	type slowOutcome struct {
		result continuation.RunResult
		err    error
	}
	slowDone := make(chan slowOutcome, 1)
	go func() {
		slowInvocation.SourceRevision = 99
		result, runErr := runner.Run(ctx, preparedSlow, slowInvocation, nil)
		slowDone <- slowOutcome{result: result, err: runErr}
	}()
	select {
	case <-slowDone:
		t.Fatal("canonical slow replay did not wait for in-flight preparation")
	case <-time.After(10 * time.Millisecond):
	}
	close(slowRelease)
	outcome := <-slowDone
	if outcome.err != nil || len(outcome.result.ToolCalls) != 1 {
		t.Fatalf("slow replay = %+v err=%v", outcome.result, outcome.err)
	}
	if fastFallback.calls.Load() != 0 || slowFallback.calls.Load() != 0 {
		t.Fatalf("unexpected live fallback calls: fast=%d slow=%d", fastFallback.calls.Load(), slowFallback.calls.Load())
	}
	if preparedFast.ReplayedInvocationID() == "" || preparedSlow.ReplayedInvocationID() == "" {
		t.Fatal("prepared invocation provenance was not retained")
	}

	requests := slow.Requests()
	if len(requests) != 1 || !containsKind(requests[0].Trajectory, trajectory.KindReasoning) ||
		!containsKind(requests[0].Trajectory, trajectory.KindAssistant) ||
		!containsKind(requests[0].Trajectory, trajectory.KindToolProposal) {
		t.Fatalf("slow preparation did not inherit complete fast trajectory: %+v", requests)
	}
	finalReport := manager.Report()
	if finalReport.ReplayedStages != 2 || finalReport.FallbackStages != 0 || finalReport.Completed != 1 {
		t.Fatalf("chain report = %+v", finalReport)
	}
}

func TestChainRejectsStaleRootAndPreparedStageFallsBackOnSemanticMismatch(t *testing.T) {
	t.Parallel()
	fastDescriptor := continuation.Descriptor{
		Provider: "test", Model: "fast", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, Streaming: true,
		ToolAuthority: continuation.ToolAuthorityPropose,
	}
	fast := &chainTestProvider{descriptor: fastDescriptor, events: []continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "prepared"}}}
	fastInvocation, _ := chainInvocations()
	manager, err := NewChainManager(ChainConfig{Stages: []ChainStage{{Provider: fast, Invocation: fastInvocation}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	original := chainInput("original", 1)
	if err := manager.Observe(ctx, original); err != nil {
		t.Fatal(err)
	}
	for manager.Report().Completed == 0 {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	committed, _, err := manager.Commit(chainInput("changed", 2))
	if err != nil {
		t.Fatal(err)
	}
	if committed != nil {
		t.Fatal("stale preparation root was committed")
	}

	second, err := NewChainManager(ChainConfig{Stages: []ChainStage{{Provider: fast, Invocation: fastInvocation}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Observe(ctx, original); err != nil {
		t.Fatal(err)
	}
	for second.Report().Completed == 0 {
		time.Sleep(time.Millisecond)
	}
	chain, _, err := second.Commit(original)
	if err != nil || chain == nil {
		t.Fatalf("commit: chain=%v err=%v", chain != nil, err)
	}
	fallback := &fallbackProvider{descriptor: fastDescriptor}
	prepared, err := chain.StageProvider(0, fallback)
	if err != nil {
		t.Fatal(err)
	}
	store := trajectory.NewStore()
	changed := chainInput("changed", 2)
	if err := store.AppendBatch(changed.Trajectory.Items); err != nil {
		t.Fatal(err)
	}
	runner, err := continuation.NewRunner(continuation.RunnerConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(ctx, prepared, fastInvocation, nil)
	if err != nil || result.AssistantText != "fallback" || fallback.calls.Load() != 1 {
		t.Fatalf("mismatch fallback result=%+v calls=%d err=%v", result, fallback.calls.Load(), err)
	}
}

func TestChainRejectsExecutableAuthorityBeforeFinalStage(t *testing.T) {
	t.Parallel()
	fastInvocation, slowInvocation := chainInvocations()
	executable := &fallbackProvider{descriptor: continuation.Descriptor{
		Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh, Streaming: true,
		ToolAuthority: continuation.ToolAuthorityExecute, ExecutableTools: true,
	}}
	proposal := &fallbackProvider{descriptor: continuation.Descriptor{
		Provider: "test", Model: "fast", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, Streaming: true,
		ToolAuthority: continuation.ToolAuthorityPropose,
	}}
	if _, err := NewChainManager(ChainConfig{Stages: []ChainStage{
		{Provider: executable, Invocation: slowInvocation},
		{Provider: proposal, Invocation: fastInvocation},
	}}); err == nil {
		t.Fatal("executable non-final preparation stage was accepted")
	}
}

func TestChainRejectsNegativeStageStartInterval(t *testing.T) {
	t.Parallel()
	fastInvocation, _ := chainInvocations()
	provider := &fallbackProvider{descriptor: continuation.Descriptor{
		Provider: "test", Model: "fast", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, Streaming: true,
		ToolAuthority: continuation.ToolAuthorityPropose,
	}}
	if _, err := NewChainManager(ChainConfig{Stages: []ChainStage{{
		Provider: provider, Invocation: fastInvocation, MinimumStartInterval: -time.Millisecond,
	}}}); err == nil {
		t.Fatal("negative stage start interval was accepted")
	}
}

func TestChainPacesStageLaunchAndExactCommitBypassesWait(t *testing.T) {
	t.Parallel()
	fastDescriptor := continuation.Descriptor{
		Provider: "test", Model: "fast", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, Streaming: true,
		ToolAuthority: continuation.ToolAuthorityPropose,
	}
	slowDescriptor := continuation.Descriptor{
		Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh, Streaming: true,
		ToolAuthority: continuation.ToolAuthorityExecute, ExecutableTools: true,
	}
	fast := &chainTestProvider{descriptor: fastDescriptor, events: []continuation.Event{{
		Kind: continuation.EventAssistantDelta, Text: "fast",
	}}}
	slow := &chainTestProvider{descriptor: slowDescriptor, events: []continuation.Event{{
		Kind: continuation.EventAssistantDelta, Text: "slow",
	}}}
	fastInvocation, slowInvocation := chainInvocations()
	manager, err := NewChainManager(ChainConfig{Stages: []ChainStage{
		{Provider: fast, Invocation: fastInvocation},
		{Provider: slow, Invocation: slowInvocation, MinimumStartInterval: time.Hour},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Observe(ctx, chainInput("first", 1)); err != nil {
		t.Fatal(err)
	}
	waitForChainReport(t, ctx, manager, func(report ChainReport) bool { return report.Completed == 1 })
	if err := manager.Observe(ctx, chainInput("second", 2)); err != nil {
		t.Fatal(err)
	}
	waitForChainReport(t, ctx, manager, func(report ChainReport) bool { return report.PacedStageWaits == 1 })
	if requests := slow.Requests(); len(requests) != 1 {
		t.Fatalf("paced slow requests = %d, want 1 before commit", len(requests))
	}
	committed, _, err := manager.Commit(chainInput("second", 200))
	if err != nil || committed == nil {
		t.Fatalf("exact commit: committed=%v err=%v", committed != nil, err)
	}
	report := waitForChainReport(t, ctx, manager, func(report ChainReport) bool { return report.Completed == 2 })
	if requests := slow.Requests(); len(requests) != 2 {
		t.Fatalf("slow requests after commit = %d, want 2", len(requests))
	}
	if report.CommitBypasses != 1 || report.PacedStageCancels != 0 {
		t.Fatalf("pacing report = %+v", report)
	}
	last := report.Attempts[len(report.Attempts)-1].Stages[1]
	if !last.CommitBypassedWait || last.StartedOffsetMS == nil || last.PacingWaitMS < 0 || last.Outcome != "completed" {
		t.Fatalf("committed paced stage = %+v", last)
	}
}

func TestChainReportsSupersessionWhileStageIsPacedBeforeStart(t *testing.T) {
	t.Parallel()
	fastDescriptor := continuation.Descriptor{
		Provider: "test", Model: "fast", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, Streaming: true,
		ToolAuthority: continuation.ToolAuthorityPropose,
	}
	slowDescriptor := continuation.Descriptor{
		Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh, Streaming: true,
		ToolAuthority: continuation.ToolAuthorityExecute, ExecutableTools: true,
	}
	fast := &chainTestProvider{descriptor: fastDescriptor, events: []continuation.Event{{
		Kind: continuation.EventAssistantDelta, Text: "fast",
	}}}
	slow := &chainTestProvider{descriptor: slowDescriptor, events: []continuation.Event{{
		Kind: continuation.EventAssistantDelta, Text: "slow",
	}}}
	fastInvocation, slowInvocation := chainInvocations()
	manager, err := NewChainManager(ChainConfig{Stages: []ChainStage{
		{Provider: fast, Invocation: fastInvocation},
		{Provider: slow, Invocation: slowInvocation, MinimumStartInterval: time.Hour},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Observe(ctx, chainInput("first", 1)); err != nil {
		t.Fatal(err)
	}
	waitForChainReport(t, ctx, manager, func(report ChainReport) bool { return report.Completed == 1 })
	if err := manager.Observe(ctx, chainInput("second", 2)); err != nil {
		t.Fatal(err)
	}
	waitForChainReport(t, ctx, manager, func(report ChainReport) bool { return report.PacedStageWaits == 1 })
	if err := manager.Observe(ctx, chainInput("third", 3)); err != nil {
		t.Fatal(err)
	}
	report := waitForChainReport(t, ctx, manager, func(report ChainReport) bool {
		return report.Superseded == 1 && report.PacedStageCancels == 1
	})
	var cancelled *ChainStageAttempt
	for index := range report.Attempts {
		attempt := &report.Attempts[index]
		if attempt.SourceRevision == 2 && len(attempt.Stages) == 2 {
			cancelled = &attempt.Stages[1]
			break
		}
	}
	if cancelled == nil || cancelled.Outcome != "superseded_before_start" || cancelled.StartedOffsetMS != nil || cancelled.InvocationID != "" {
		t.Fatalf("paced cancellation telemetry = %+v", cancelled)
	}
	if committed, _, commitErr := manager.Commit(chainInput("third", 3)); commitErr != nil || committed == nil {
		t.Fatalf("cleanup commit: committed=%v err=%v", committed != nil, commitErr)
	}
	waitForChainReport(t, ctx, manager, func(report ChainReport) bool { return report.Completed == 2 })
}

func TestChainReportsSupersededStageSeparatelyFromFailure(t *testing.T) {
	t.Parallel()
	descriptor := continuation.Descriptor{
		Provider: "test", Model: "fast", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, Streaming: true,
		ToolAuthority: continuation.ToolAuthorityPropose,
	}
	started, release := make(chan struct{}), make(chan struct{})
	provider := &chainTestProvider{
		descriptor: descriptor, started: started, release: release,
		events: []continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "ready"}},
	}
	fastInvocation, _ := chainInvocations()
	manager, err := NewChainManager(ChainConfig{
		Stages: []ChainStage{{Provider: provider, Invocation: fastInvocation}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Observe(ctx, chainInput("first", 1)); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := manager.Observe(ctx, chainInput("second", 2)); err != nil {
		t.Fatal(err)
	}
	close(release)
	for {
		report := manager.Report()
		if report.Completed == 1 && report.Superseded == 1 {
			if report.Failed != 0 || len(report.Attempts) != 2 || report.Attempts[0].Outcome != "superseded" ||
				len(report.Attempts[0].Stages) != 1 || report.Attempts[0].Stages[0].Outcome != "superseded" {
				t.Fatalf("supersession telemetry = %+v", report)
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

func chainInput(text string, revision uint64) ChainInput {
	return ChainInput{
		SourceRevision: revision,
		Trajectory: trajectory.Snapshot{Version: 1, Items: []trajectory.Item{{
			ID: "observation", Kind: trajectory.KindObservation,
			SourceRevision: revision, Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
			Content: text,
		}}},
	}
}

func chainInvocations() (continuation.Invocation, continuation.Invocation) {
	capabilities := []continuation.Capability{{Name: "lookup", Description: "Lookup.", Available: true, ExecutionPhase: "slow"}}
	tools := []continuation.ToolDefinition{{Name: "lookup", Description: "Lookup.", Parameters: json.RawMessage(`{"type":"object"}`)}}
	return continuation.Invocation{
			Instruction: "Respond quickly.", Capabilities: capabilities, Tools: tools, MaxOutputTokens: 32,
		}, continuation.Invocation{
			Instruction: "Continue carefully.", Capabilities: capabilities, Tools: tools, MaxOutputTokens: 64,
		}
}

func containsKind(snapshot trajectory.Snapshot, kind trajectory.Kind) bool {
	for _, item := range snapshot.Items {
		if item.Kind == kind {
			return true
		}
	}
	return false
}

func waitForChainReport(t *testing.T, ctx context.Context, manager *ChainManager, ready func(ChainReport) bool) ChainReport {
	t.Helper()
	for {
		report := manager.Report()
		if ready(report) {
			return report
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

var _ continuation.Provider = (*chainTestProvider)(nil)
var _ continuation.Provider = (*fallbackProvider)(nil)
