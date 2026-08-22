package cognition_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/cognition"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type scriptedProvider struct {
	descriptor continuation.Descriptor
	events     []continuation.Event
	seen       *continuation.Request
}

func (provider *scriptedProvider) Descriptor() continuation.Descriptor { return provider.descriptor }

func (provider *scriptedProvider) Continue(
	_ context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	captured := request
	provider.seen = &captured
	for _, event := range provider.events {
		if err := emit(event); err != nil {
			return continuation.Completion{}, err
		}
	}
	return continuation.Completion{StopReason: "stop"}, nil
}

func fastProvider(events ...continuation.Event) *scriptedProvider {
	return &scriptedProvider{
		descriptor: continuation.Descriptor{
			Provider: "test-fast", Model: "fast", Phase: trajectory.PhaseFast,
			Effort: continuation.EffortMinimal, ToolAuthority: continuation.ToolAuthorityPropose,
			SpeechAuthority: continuation.SpeechAuthorityVoice,
		},
		events: events,
	}
}

func slowProvider(events ...continuation.Event) *scriptedProvider {
	return &scriptedProvider{
		descriptor: continuation.Descriptor{
			Provider: "test-slow", Model: "slow", Phase: trajectory.PhaseSlow,
			Effort: continuation.EffortHigh, ToolAuthority: continuation.ToolAuthorityExecute,
			SpeechAuthority: continuation.SpeechAuthoritySilent,
		},
		events: events,
	}
}

type catalog struct{}

func (catalog) Capabilities() []continuation.Capability {
	return []continuation.Capability{{Name: "get_balance", Description: "read a balance", Available: true}}
}

func (catalog) Tools() []continuation.ToolDefinition {
	return []continuation.ToolDefinition{{
		Name: "get_balance", Description: "read a balance", Parameters: json.RawMessage(`{"type":"object"}`),
	}}
}

func seed(t *testing.T, store *trajectory.Store) {
	t.Helper()
	if err := store.Append(trajectory.Item{
		ID: "obs-1", Kind: trajectory.KindObservation, MonotonicNS: 1, SourceRevision: 1,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "what is my balance",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestSlowMustBeSilentWhenTheArrangementPairsThem(t *testing.T) {
	store := trajectory.NewStore()
	loud := slowProvider()
	loud.descriptor.SpeechAuthority = continuation.SpeechAuthorityVoice
	_, err := cognition.New(cognition.Config{
		Store: store, Fast: fastProvider(), Slow: loud, RequireSilentSlow: true,
	})
	if err == nil || !strings.Contains(err.Error(), "silent") {
		t.Fatalf("expected the second boundary to be enforced at construction, got %v", err)
	}
	// A single-provider arrangement is legitimate and must still start.
	if _, err := cognition.New(cognition.Config{Store: store, Fast: fastProvider(), Slow: loud}); err != nil {
		t.Fatalf("an unpaired arrangement must be allowed: %v", err)
	}
}

func TestFastMustNotHoldExecutionAuthority(t *testing.T) {
	fast := fastProvider()
	fast.descriptor.ToolAuthority = continuation.ToolAuthorityExecute
	_, err := cognition.New(cognition.Config{Store: trajectory.NewStore(), Fast: fast, Slow: slowProvider()})
	if err == nil || !strings.Contains(err.Error(), "executable-tool") {
		t.Fatalf("expected the first boundary to be enforced at construction, got %v", err)
	}
}

func TestFastEmittedCallsCommitAsNonExecutableProposals(t *testing.T) {
	store := trajectory.NewStore()
	seed(t, store)
	fast := fastProvider(
		continuation.Event{Kind: continuation.EventAssistantDelta, Text: "Checking that now."},
		continuation.Event{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "c1", Name: "get_balance", Arguments: json.RawMessage(`{}`),
		}},
	)
	engine, err := cognition.New(cognition.Config{
		Store: store, Fast: fast, Slow: slowProvider(), Catalog: catalog{}, RequireSilentSlow: true,
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	result, err := engine.RunFast(context.Background(), cognition.Request{SourceRevision: 1}, nil)
	if err != nil {
		t.Fatalf("run fast: %v", err)
	}
	if len(result.ToolProposals) != 1 || len(result.ToolCalls) != 0 {
		t.Fatalf("fast output must be a proposal: %+v", result)
	}
	for _, item := range store.Snapshot().Items {
		if item.Kind == trajectory.KindToolCall {
			t.Fatal("fast must never append an executable call")
		}
	}
}

func TestSlowIsSilentAndFastKnowsCapabilitiesWithoutTools(t *testing.T) {
	store := trajectory.NewStore()
	seed(t, store)
	fast := fastProvider(continuation.Event{Kind: continuation.EventAssistantDelta, Text: "You have forty dollars."})
	slow := slowProvider(continuation.Event{
		Kind: continuation.EventAssistantDelta,
		Text: "The account balance is $40.00 as of the most recent statement.",
	})
	engine, err := cognition.New(cognition.Config{
		Store: store, Fast: fast, Slow: slow, Catalog: catalog{}, RequireSilentSlow: true,
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	slowResult, err := engine.RunSlow(context.Background(), cognition.Request{SourceRevision: 1}, nil)
	if err != nil {
		t.Fatalf("run slow: %v", err)
	}
	if !slowResult.Committed {
		t.Fatal("slow output must commit")
	}
	var slowAssistant *trajectory.Item
	for index, item := range store.Snapshot().Items {
		if item.Kind == trajectory.KindAssistant && item.Producer.Phase == trajectory.PhaseSlow {
			slowAssistant = &store.Snapshot().Items[index]
		}
	}
	if slowAssistant == nil {
		t.Fatal("expected a slow assistant item")
	}
	if slowAssistant.Producer.SpeechAuthority != string(continuation.SpeechAuthoritySilent) {
		t.Fatalf("the log must record that slow could not speak, got %q", slowAssistant.Producer.SpeechAuthority)
	}

	// Fast is told what the agent can do, and given no tools to call: its
	// authority is to hand the turn on, not to act.
	if _, err := engine.RunFast(context.Background(), cognition.Request{SourceRevision: 1}, nil); err != nil {
		t.Fatalf("run fast: %v", err)
	}
	if len(fast.seen.Invocation.Tools) != 0 {
		t.Fatal("the fast provider cannot execute, so it is offered nothing to execute")
	}
	if len(fast.seen.Invocation.Capabilities) == 0 {
		t.Fatal("fast must know what the agent can do, or it will deny a capability the agent has")
	}
	if !strings.Contains(fast.seen.Invocation.Instruction, continuation.EscalationMarker) {
		t.Fatalf("fast must be told how to hand the turn on: %q", fast.seen.Invocation.Instruction)
	}
}

func TestRepairObligationIsInjectedFromTypedStateOnly(t *testing.T) {
	store := trajectory.NewStore()
	seed(t, store)
	fast := fastProvider(continuation.Event{Kind: continuation.EventAssistantDelta, Text: "Correcting that."})
	engine, err := cognition.New(cognition.Config{Store: store, Fast: fast, Slow: slowProvider()})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if _, err := engine.RunFast(context.Background(), cognition.Request{SourceRevision: 1}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(fast.seen.Invocation.Instruction, "invalidated") {
		t.Fatal("no obligation, no injection")
	}
	if _, err := engine.RunFast(context.Background(), cognition.Request{SourceRevision: 1, PendingRepair: true}, nil); err != nil {
		t.Fatalf("run with obligation: %v", err)
	}
	if !strings.Contains(fast.seen.Invocation.Instruction, "invalidated") {
		t.Fatal("an outstanding obligation must reach the provider")
	}
}

func TestPlaceholdersCloseOutInterruptedCalls(t *testing.T) {
	store := trajectory.NewStore()
	seed(t, store)
	if err := store.Append(trajectory.Item{
		ID: "call-1", Kind: trajectory.KindToolCall, MonotonicNS: 2, CausalParentIDs: []string{"obs-1"},
		SourceRevision: 1, InvocationID: "inv-1", Producer: trajectory.Producer{Phase: trajectory.PhaseSlow},
		ToolCall: &trajectory.ToolCall{CallID: "c1", Name: "get_balance", Arguments: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatalf("seed call: %v", err)
	}
	engine, err := cognition.New(cognition.Config{
		Store: store, Fast: fastProvider(), Slow: slowProvider(), Catalog: catalog{}, RequireSilentSlow: true,
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	placeholders, err := engine.PlaceholderForInterrupted("user interrupted")
	if err != nil {
		t.Fatalf("placeholders: %v", err)
	}
	if len(placeholders) != 1 || placeholders[0].CallID != "c1" {
		t.Fatalf("unexpected placeholders %+v", placeholders)
	}
	if len(trajectory.UnresolvedToolCalls(store.Snapshot())) != 0 {
		t.Fatal("the interrupted prefix must be well-formed")
	}
}

func TestSlowInvocationsAreCountedFromCanonicalState(t *testing.T) {
	store := trajectory.NewStore()
	seed(t, store)
	slow := slowProvider(continuation.Event{Kind: continuation.EventAssistantDelta, Text: "thinking"})
	engine, err := cognition.New(cognition.Config{
		Store: store, Fast: fastProvider(), Slow: slow, RequireSilentSlow: true,
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if engine.SlowInvocations(1) != 0 {
		t.Fatal("a fresh turn has no slow invocations")
	}
	for index := 0; index < 2; index++ {
		if _, err := engine.RunSlow(context.Background(), cognition.Request{SourceRevision: 1}, nil); err != nil {
			t.Fatalf("run slow %d: %v", index, err)
		}
	}
	if got := engine.SlowInvocations(1); got != 2 {
		t.Fatalf("expected two invocations counted from the log, got %d", got)
	}
	// A new engine over the same store must see the same count.
	rebuilt, err := cognition.New(cognition.Config{
		Store: store, Fast: fastProvider(), Slow: slowProvider(), RequireSilentSlow: true,
	})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if got := rebuilt.SlowInvocations(1); got != 2 {
		t.Fatalf("the bound must survive a restart, got %d", got)
	}
}
