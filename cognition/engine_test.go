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

type listedCatalog struct {
	tools []continuation.ToolDefinition
}

func (catalog listedCatalog) Capabilities() []continuation.Capability {
	result := make([]continuation.Capability, 0, len(catalog.tools))
	for _, tool := range catalog.tools {
		result = append(result, continuation.Capability{
			Name: tool.Name, Description: tool.Description, Available: true,
		})
	}
	return result
}

func (catalog listedCatalog) Tools() []continuation.ToolDefinition { return catalog.tools }

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

func TestFastExecutionRequiresAnExplicitToolFilter(t *testing.T) {
	fast := fastProvider()
	fast.descriptor.ToolAuthority = continuation.ToolAuthorityExecute
	_, err := cognition.New(cognition.Config{Store: trajectory.NewStore(), Fast: fast, Slow: slowProvider()})
	if err == nil || !strings.Contains(err.Error(), "tool filter") {
		t.Fatalf("expected the first boundary to be enforced at construction, got %v", err)
	}
}

func TestFastAllowlistRequiresExecutionAuthority(t *testing.T) {
	_, err := cognition.New(cognition.Config{
		Store: trajectory.NewStore(), Fast: fastProvider(), Slow: slowProvider(), Catalog: catalog{},
		FastToolFilter: func(tool continuation.ToolDefinition) bool { return tool.Name == "get_balance" },
	})
	if err == nil || !strings.Contains(err.Error(), "requires fast execution authority") {
		t.Fatalf("an allowlist with proposal authority is a misleading no-op, got %v", err)
	}
}

func TestFastExecutesOnlyExactAllowedToolsAtAnEligibleSafePoint(t *testing.T) {
	store := trajectory.NewStore()
	seed(t, store)
	fast := fastProvider(
		continuation.Event{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "click-1", Name: "computer.click", Arguments: json.RawMessage(`{"source":"screen","x":10,"y":10}`),
		}},
		continuation.Event{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "transfer-1", Name: "transfer_funds", Arguments: json.RawMessage(`{"amount":10000}`),
		}},
	)
	fast.descriptor.ToolAuthority = continuation.ToolAuthorityExecute
	catalog := listedCatalog{tools: []continuation.ToolDefinition{
		{Name: "computer.click", Description: "click the screen", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "transfer_funds", Description: "move money", Parameters: json.RawMessage(`{"type":"object"}`)},
	}}
	engine, err := cognition.New(cognition.Config{
		Store: store, Fast: fast, Slow: slowProvider(), Catalog: catalog,
		RequireSilentSlow: true,
		FastToolFilter:    func(tool continuation.ToolDefinition) bool { return tool.Name == "computer.click" },
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	result, err := engine.RunFast(context.Background(), cognition.Request{
		SourceRevision: 1, AllowFastTools: true,
	}, nil)
	if err != nil {
		t.Fatalf("run fast: %v", err)
	}
	if len(fast.seen.Invocation.Tools) != 1 || fast.seen.Invocation.Tools[0].Name != "computer.click" {
		t.Fatalf("fast saw something other than the exact allowlist: %+v", fast.seen.Invocation.Tools)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Name != "computer.click" {
		t.Fatalf("the allowed action did not become executable: %+v", result)
	}
	if len(result.ToolProposals) != 1 || result.ToolProposals[0].Name != "transfer_funds" {
		t.Fatalf("the non-allowed call did not stay a proposal: %+v", result)
	}
	if !strings.Contains(fast.seen.Invocation.Instruction, cognition.FastActionInstruction) {
		t.Fatal("the bounded action guidance did not reach the fast provider")
	}
}

func TestFastAllowlistIsClosedOutsideAnObservationSafePoint(t *testing.T) {
	store := trajectory.NewStore()
	seed(t, store)
	fast := fastProvider(continuation.Event{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
		CallID: "click-1", Name: "computer.click", Arguments: json.RawMessage(`{"source":"screen","x":10,"y":10}`),
	}})
	fast.descriptor.ToolAuthority = continuation.ToolAuthorityExecute
	engine, err := cognition.New(cognition.Config{
		Store: store, Fast: fast, Slow: slowProvider(),
		Catalog: listedCatalog{tools: []continuation.ToolDefinition{{
			Name: "computer.click", Description: "click", Parameters: json.RawMessage(`{"type":"object"}`),
		}}},
		FastToolFilter: func(tool continuation.ToolDefinition) bool { return tool.Name == "computer.click" },
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	result, err := engine.RunFast(context.Background(), cognition.Request{SourceRevision: 1}, nil)
	if err != nil {
		t.Fatalf("run fast: %v", err)
	}
	if len(fast.seen.Invocation.Tools) != 0 || len(result.ToolCalls) != 0 || len(result.ToolProposals) != 1 {
		t.Fatalf("a holding/background-style fast turn acquired action authority: %+v", result)
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
	// The voice is no longer asked about the completion marker. It used to
	// decide whether the reasoner ran by omitting one, which put every
	// capability behind a judgement by the phase that cannot act on it; an
	// observation deliberates now, so the marker decides nothing and the
	// instruction does not spend four paragraphs on it.
	if strings.Contains(fast.seen.Invocation.Instruction, continuation.CompletionMarker) {
		t.Fatalf("the marker is inert and must not be asked for: %q", fast.seen.Invocation.Instruction)
	}
}

func TestSlowReceivesToolPrerequisitesAfterTheDeploymentInstruction(t *testing.T) {
	store := trajectory.NewStore()
	seed(t, store)
	slow := slowProvider()
	const deployment = "Authenticate the account before reading private records."
	engine, err := cognition.New(cognition.Config{
		Store: store, Fast: fastProvider(), Slow: slow, AgentInstruction: deployment,
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if _, err := engine.RunSlow(context.Background(), cognition.Request{SourceRevision: 1}, nil); err != nil {
		t.Fatalf("run slow: %v", err)
	}
	instruction := slow.seen.Invocation.Instruction
	deploymentAt := strings.Index(instruction, deployment)
	prerequisiteAt := strings.Index(instruction, cognition.SlowToolPrerequisiteInstruction)
	if deploymentAt < 0 || prerequisiteAt < 0 || prerequisiteAt <= deploymentAt {
		t.Fatalf("slow tool guidance did not reinforce the deployment prerequisite:\n%s", instruction)
	}
	if !strings.Contains(instruction, cognition.SlowNoResultInstruction) ||
		!strings.Contains(instruction, continuation.CompletionMarker) {
		t.Fatalf("slow phase has no control-only way to report that nothing new remains:\n%s", instruction)
	}
}

func TestSlowNoResultMarkerCommitsNoConversationalText(t *testing.T) {
	store := trajectory.NewStore()
	seed(t, store)
	slow := slowProvider(continuation.Event{
		Kind: continuation.EventAssistantDelta, Text: continuation.CompletionMarker,
	})
	engine, err := cognition.New(cognition.Config{
		Store: store, Fast: fastProvider(), Slow: slow, RequireSilentSlow: true,
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	result, err := engine.RunSlow(context.Background(), cognition.Request{SourceRevision: 1}, nil)
	if err != nil {
		t.Fatalf("run slow: %v", err)
	}
	if result.AssistantText != "" || !result.Finished {
		t.Fatalf("no-result marker escaped as content: %#v", result)
	}
	for _, item := range store.Snapshot().Items {
		if item.Kind == trajectory.KindAssistant && item.Producer.Phase == trajectory.PhaseSlow {
			t.Fatalf("control-only result became conversational history: %#v", item)
		}
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

// liveCatalog is a catalogue whose contents change after the engine exists,
// which is what a client declaring its tools in session.update looks like.
type liveCatalog struct{ declared []continuation.Capability }

func (c *liveCatalog) Capabilities() []continuation.Capability { return c.declared }
func (c *liveCatalog) Tools() []continuation.ToolDefinition    { return nil }

// A client declares its tools after the session exists, so a manifest captured
// at construction is empty for exactly the sessions that have tools. Fast then
// cannot know the agent can do anything - and since fast is what decides
// whether the reasoner runs, the agent never acts at all.
func TestTheCapabilityManifestIsReadWhenItIsAsked(t *testing.T) {
	store := trajectory.NewStore()
	seed(t, store)
	catalog := &liveCatalog{}
	fast := fastProvider(continuation.Event{Kind: continuation.EventAssistantDelta, Text: "one moment"})
	engine, err := cognition.New(cognition.Config{
		Store: store, Fast: fast, Slow: slowProvider(), Catalog: catalog, RequireSilentSlow: true,
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	// The session declares its tools only now.
	catalog.declared = []continuation.Capability{
		{Name: "get_balance", Description: "read a balance", Available: true},
	}
	if _, err := engine.RunFast(context.Background(), cognition.Request{SourceRevision: 1}, nil); err != nil {
		t.Fatalf("run fast: %v", err)
	}
	if len(fast.seen.Invocation.Capabilities) != 1 {
		t.Fatalf("fast must see what the session declared, got %+v", fast.seen.Invocation.Capabilities)
	}
}
