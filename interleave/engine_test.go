package interleave

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/agenttool"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type providerScript struct {
	events     []continuation.Event
	completion continuation.Completion
	err        error
}

type sequenceProvider struct {
	mu         sync.Mutex
	descriptor continuation.Descriptor
	scripts    []providerScript
	requests   []continuation.Request
}

type staticCatalog struct {
	capabilities []continuation.Capability
	tools        []continuation.ToolDefinition
}

func (catalog staticCatalog) Capabilities() []continuation.Capability {
	return append([]continuation.Capability(nil), catalog.capabilities...)
}

func (catalog staticCatalog) Tools() []continuation.ToolDefinition {
	result := append([]continuation.ToolDefinition(nil), catalog.tools...)
	for index := range result {
		result[index].Parameters = append(json.RawMessage(nil), result[index].Parameters...)
	}
	return result
}

func (provider *sequenceProvider) Descriptor() continuation.Descriptor { return provider.descriptor }

func (provider *sequenceProvider) Continue(_ context.Context, request continuation.Request, emit continuation.Emit) (continuation.Completion, error) {
	provider.mu.Lock()
	index := len(provider.requests)
	provider.requests = append(provider.requests, request)
	if index >= len(provider.scripts) {
		provider.mu.Unlock()
		return continuation.Completion{}, errors.New("unexpected provider invocation")
	}
	script := provider.scripts[index]
	provider.mu.Unlock()
	for _, event := range script.events {
		if err := emit(event); err != nil {
			return continuation.Completion{}, err
		}
	}
	return script.completion, script.err
}

func (provider *sequenceProvider) Requests() []continuation.Request {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]continuation.Request(nil), provider.requests...)
}

func descriptor(model string, phase trajectory.Phase, tools bool) continuation.Descriptor {
	effort := continuation.EffortMinimal
	authority := continuation.ToolAuthorityPropose
	if phase == trajectory.PhaseSlow {
		effort = continuation.EffortHigh
		authority = continuation.ToolAuthorityNone
		if tools {
			authority = continuation.ToolAuthorityExecute
		}
	}
	return continuation.Descriptor{
		Provider: "test", Model: model, Phase: phase, Effort: effort,
		Streaming: true, RetainsToolCalls: true, ToolAuthority: authority,
		ExecutableTools: authority == continuation.ToolAuthorityExecute,
	}
}

func lookupRegistry(t *testing.T) *agenttool.Registry {
	t.Helper()
	registry := agenttool.NewRegistry(nil)
	err := registry.Register(agenttool.Definition{
		Tool: continuation.ToolDefinition{
			Name: "lookup", Description: "Look up a test value.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"key":{"type":"string"}},"required":["key"],"additionalProperties":false}`),
		},
		Capability: continuation.Capability{
			Name: "lookup", Description: "Look up a test value.", Available: true, ExecutionPhase: "slow",
		},
		ReadOnly: true,
	}, func(_ context.Context, call trajectory.ToolCall) (any, error) {
		if call.CallID != "call-1" {
			t.Errorf("handler did not receive stable call ID: %#v", call)
		}
		return map[string]any{"value": 7}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestEngineRunsFastThenSlowToolContinuation(t *testing.T) {
	t.Parallel()
	store := trajectory.NewStore()
	if err := store.Append(trajectory.Item{
		ID: "user", Kind: trajectory.KindObservation, MonotonicNS: 1,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "What is x?",
	}); err != nil {
		t.Fatal(err)
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
			{events: []continuation.Event{
				{Kind: continuation.EventReasoningDelta, Text: "Use the lookup capability."},
				{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{CallID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`)}},
			}},
			{events: []continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "The value is 7."}}},
		},
	}
	var now uint64 = 1
	var id uint64
	engine, err := New(Config{
		Store: store, FastProvider: fast, SlowProvider: slow, Tools: lookupRegistry(t),
		RetainReasoning: true,
		Now:             func() uint64 { now++; return now },
		NextID:          func(prefix string) string { id++; return fmt.Sprintf("%s-%d", prefix, id) },
	})
	if err != nil {
		t.Fatal(err)
	}
	var phases []trajectory.Phase
	result, err := engine.Run(context.Background(), Request{SourceRevision: 3}, func(event StreamEvent) error {
		phases = append(phases, event.Phase)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Fast.AssistantText != "I'll check." || len(result.Fast.ToolProposals) != 1 || len(result.Slow) != 2 ||
		len(result.ToolResults) != 1 || result.Slow[1].AssistantText != "The value is 7." {
		t.Fatalf("unexpected rollout: %#v", result)
	}
	if len(phases) != 5 || phases[0] != trajectory.PhaseFast || phases[1] != trajectory.PhaseFast ||
		phases[2] != trajectory.PhaseSlow || phases[3] != trajectory.PhaseSlow || phases[4] != trajectory.PhaseSlow {
		t.Fatalf("unexpected stream phases: %#v", phases)
	}
	fastRequests := fast.Requests()
	slowRequests := slow.Requests()
	if len(fastRequests) != 1 || len(fastRequests[0].Invocation.Tools) != 1 || len(fastRequests[0].Invocation.Capabilities) != 1 {
		t.Fatalf("fast phase did not receive proposal schemas and capabilities: %#v", fastRequests)
	}
	if len(slowRequests) != 2 || len(slowRequests[0].Invocation.Tools) != 1 ||
		len(slowRequests[0].Invocation.Capabilities) != 1 {
		t.Fatalf("slow phase did not receive tools and capabilities: %#v", slowRequests)
	}
	if !snapshotContains(slowRequests[0].Trajectory, trajectory.KindAssistant, "I'll check.") ||
		!snapshotContains(slowRequests[0].Trajectory, trajectory.KindToolProposal, "") ||
		!snapshotContains(slowRequests[1].Trajectory, trajectory.KindToolResult, "") {
		t.Fatalf("slow continuations did not inherit the canonical prefix: %#v", slowRequests)
	}
}

func TestEngineExposesSimpleFastThenSlowPhaseBoundary(t *testing.T) {
	t.Parallel()
	store := trajectory.NewStore()
	if err := store.Append(trajectory.Item{
		ID: "user", Kind: trajectory.KindObservation,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "What is x?",
	}); err != nil {
		t.Fatal(err)
	}
	fast := &sequenceProvider{
		descriptor: descriptor("fast", trajectory.PhaseFast, false),
		scripts: []providerScript{{events: []continuation.Event{{
			Kind: continuation.EventAssistantDelta, Text: "I'll check.",
		}}}},
	}
	slow := &sequenceProvider{
		descriptor: descriptor("slow", trajectory.PhaseSlow, false),
		scripts: []providerScript{{events: []continuation.Event{{
			Kind: continuation.EventAssistantDelta, Text: "It is 7.",
		}}}},
	}
	engine, err := New(Config{Store: store, FastProvider: fast, SlowProvider: slow})
	if err != nil {
		t.Fatal(err)
	}
	fastResult, err := engine.RunFast(context.Background(), Request{SourceRevision: 1}, nil)
	if err != nil || fastResult.AssistantText != "I'll check." || len(slow.Requests()) != 0 {
		t.Fatalf("fast result=%#v err=%v slow_requests=%d", fastResult, err, len(slow.Requests()))
	}
	slowResult, err := engine.RunSlow(context.Background(), Request{SourceRevision: 1}, nil)
	if err != nil || len(slowResult.Runs) != 1 || slowResult.Runs[0].AssistantText != "It is 7." {
		t.Fatalf("slow result=%#v err=%v", slowResult, err)
	}
	requests := slow.Requests()
	if len(requests) != 1 || !snapshotContains(requests[0].Trajectory, trajectory.KindAssistant, "I'll check.") {
		t.Fatalf("slow did not inherit fast trajectory: %#v", requests)
	}
}

func TestEngineSharesOneAgentPolicyAcrossFastAndSlowProfiles(t *testing.T) {
	t.Parallel()
	store := trajectory.NewStore()
	if err := store.Append(trajectory.Item{
		ID: "user", Kind: trajectory.KindObservation,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "Change my booking.",
	}); err != nil {
		t.Fatal(err)
	}
	fast := &sequenceProvider{
		descriptor: descriptor("fast", trajectory.PhaseFast, false),
		scripts:    []providerScript{{events: []continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "I'll help."}}}},
	}
	slow := &sequenceProvider{
		descriptor: descriptor("slow", trajectory.PhaseSlow, false),
		scripts:    []providerScript{{events: []continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Please provide the booking ID."}}}},
	}
	engine, err := New(Config{
		Store: store, FastProvider: fast, SlowProvider: slow,
		AgentInstruction: "Follow the airline policy and preserve the caller's identity.",
		FastInstruction:  "Produce the smallest truthful first segment.",
		SlowInstruction:  "Continue carefully from the same prefix.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Run(context.Background(), Request{}, nil); err != nil {
		t.Fatal(err)
	}
	fastInstruction := fast.Requests()[0].Invocation.Instruction
	slowInstruction := slow.Requests()[0].Invocation.Instruction
	if fastInstruction != "Follow the airline policy and preserve the caller's identity.\n\nProduce the smallest truthful first segment." {
		t.Fatalf("unexpected fast instruction: %q", fastInstruction)
	}
	if slowInstruction != "Follow the airline policy and preserve the caller's identity.\n\nContinue carefully from the same prefix." {
		t.Fatalf("unexpected slow instruction: %q", slowInstruction)
	}
}

func TestEngineResumesExternalToolBatchOnCanonicalTrajectory(t *testing.T) {
	t.Parallel()
	store := trajectory.NewStore()
	if err := store.Append(trajectory.Item{
		ID: "user", Kind: trajectory.KindObservation,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "Compare both records.",
	}); err != nil {
		t.Fatal(err)
	}
	catalog := staticCatalog{
		capabilities: []continuation.Capability{
			{Name: "read_a", Description: "Read record A.", Available: true, ExecutionPhase: "slow"},
			{Name: "read_b", Description: "Read record B.", Available: true, ExecutionPhase: "slow"},
		},
		tools: []continuation.ToolDefinition{
			{Name: "read_a", Description: "Read record A.", Parameters: json.RawMessage(`{"type":"object"}`)},
			{Name: "read_b", Description: "Read record B.", Parameters: json.RawMessage(`{"type":"object"}`)},
		},
	}
	fast := &sequenceProvider{
		descriptor: descriptor("fast", trajectory.PhaseFast, false),
		scripts:    []providerScript{{events: []continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "I'll compare them."}}}},
	}
	slow := &sequenceProvider{
		descriptor: descriptor("slow", trajectory.PhaseSlow, true),
		scripts: []providerScript{
			{events: []continuation.Event{
				{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{CallID: "call-a", Name: "read_a", Arguments: json.RawMessage(`{}`)}},
				{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{CallID: "call-b", Name: "read_b", Arguments: json.RawMessage(`{}`)}},
			}},
			{events: []continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "A is newer than B."}}},
		},
	}
	engine, err := New(Config{Store: store, FastProvider: fast, SlowProvider: slow, ToolCatalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.RunFast(context.Background(), Request{SourceRevision: 4}, nil); err != nil {
		t.Fatal(err)
	}
	first, err := engine.RunSlowStep(context.Background(), Request{SourceRevision: 4}, nil)
	if err != nil || len(first.ToolCalls) != 2 {
		t.Fatalf("unexpected first slow step: result=%#v err=%v", first, err)
	}
	versionBeforeResults := store.Snapshot().Version
	if err := engine.AppendToolResults(first.InvocationID, []trajectory.ToolResult{{
		CallID: "call-a", Name: "read_a", Output: json.RawMessage(`{"value":2}`),
	}}); err == nil {
		t.Fatal("partial external result batch was accepted")
	}
	if got := store.Snapshot().Version; got != versionBeforeResults {
		t.Fatalf("partial result batch changed version: got %d want %d", got, versionBeforeResults)
	}
	if err := engine.AppendToolResults(first.InvocationID, []trajectory.ToolResult{
		{CallID: "call-b", Name: "read_b", Output: json.RawMessage(`{"value":1}`)},
		{CallID: "call-a", Name: "read_a", Output: json.RawMessage(`{"value":2}`)},
	}); err != nil {
		t.Fatal(err)
	}
	second, err := engine.RunSlowStep(context.Background(), Request{SourceRevision: 4}, nil)
	if err != nil || second.AssistantText != "A is newer than B." {
		t.Fatalf("unexpected resumed slow step: result=%#v err=%v", second, err)
	}
	items := store.Snapshot().Items
	var resultNames []string
	for _, item := range items {
		if item.Kind == trajectory.KindToolResult {
			resultNames = append(resultNames, item.ToolResult.Name)
		}
	}
	if fmt.Sprint(resultNames) != "[read_a read_b]" {
		t.Fatalf("tool results did not commit in call order: %v", resultNames)
	}
	requests := slow.Requests()
	if len(requests) != 2 || !snapshotContains(requests[1].Trajectory, trajectory.KindToolResult, "") {
		t.Fatalf("resumed slow continuation did not inherit external results: %#v", requests)
	}
}

func TestEngineAlwaysInvokesSlowAfterSilentFast(t *testing.T) {
	t.Parallel()
	store := trajectory.NewStore()
	if err := store.Append(trajectory.Item{ID: "user", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	fast := &sequenceProvider{descriptor: descriptor("fast", trajectory.PhaseFast, false), scripts: []providerScript{{}}}
	slow := &sequenceProvider{
		descriptor: descriptor("slow", trajectory.PhaseSlow, false),
		scripts:    []providerScript{{events: []continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Hello."}}}},
	}
	engine, err := New(Config{Store: store, FastProvider: fast, SlowProvider: slow})
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.Run(context.Background(), Request{}, nil)
	if err != nil || len(result.Slow) != 1 || len(slow.Requests()) != 1 {
		t.Fatalf("slow phase was not unconditional: result=%#v err=%v", result, err)
	}
}

func TestEngineFallsBackToSlowAfterFastProviderFailure(t *testing.T) {
	t.Parallel()
	store := trajectory.NewStore()
	if err := store.Append(trajectory.Item{ID: "user", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "question"}); err != nil {
		t.Fatal(err)
	}
	fast := &sequenceProvider{
		descriptor: descriptor("fast", trajectory.PhaseFast, false),
		scripts: []providerScript{{
			events: []continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "partial"}},
			err:    errors.New("fast transport failed"),
		}},
	}
	slow := &sequenceProvider{
		descriptor: descriptor("slow", trajectory.PhaseSlow, false),
		scripts:    []providerScript{{events: []continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Recovered answer."}}}},
	}
	engine, err := New(Config{Store: store, FastProvider: fast, SlowProvider: slow})
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.Run(context.Background(), Request{}, nil)
	if err == nil || !result.FastFailed || len(result.Slow) != 1 || result.Slow[0].AssistantText != "Recovered answer." {
		t.Fatalf("slow fallback failed: result=%#v err=%v", result, err)
	}
}

func TestEngineObserverCancellationStopsBeforeSlow(t *testing.T) {
	t.Parallel()
	store := trajectory.NewStore()
	if err := store.Append(trajectory.Item{ID: "user", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "question"}); err != nil {
		t.Fatal(err)
	}
	fast := &sequenceProvider{
		descriptor: descriptor("fast", trajectory.PhaseFast, false),
		scripts:    []providerScript{{events: []continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "answer"}}}},
	}
	slow := &sequenceProvider{descriptor: descriptor("slow", trajectory.PhaseSlow, false), scripts: []providerScript{{}}}
	engine, err := New(Config{Store: store, FastProvider: fast, SlowProvider: slow})
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("observer stopped")
	_, err = engine.Run(context.Background(), Request{}, func(StreamEvent) error { return want })
	if !errors.Is(err, want) || len(slow.Requests()) != 0 {
		t.Fatalf("observer cancellation did not stop rollout: err=%v slow=%d", err, len(slow.Requests()))
	}
}

func snapshotContains(snapshot trajectory.Snapshot, kind trajectory.Kind, content string) bool {
	for _, item := range snapshot.Items {
		if item.Kind == kind && (content == "" || item.Content == content) {
			return true
		}
	}
	return false
}
