package trajectory

import (
	"encoding/json"
	"testing"
)

func TestStoreAppendLifecycle(t *testing.T) {
	t.Parallel()
	store := NewStore()
	items := []Item{
		{ID: "user-1", Kind: KindObservation, MonotonicNS: 1, Producer: Producer{Phase: PhaseUser}, Content: "Schedule lunch."},
		{ID: "fast-1", Kind: KindAssistant, MonotonicNS: 2, CausalParentIDs: []string{"user-1"}, InvocationID: "inv-fast", Producer: Producer{Phase: PhaseFast, Provider: "local", Model: "qwen"}, Content: "I'll check.", Visibility: VisibilityPrepared},
		{ID: "fast-played", Kind: KindAssistantState, MonotonicNS: 3, CausalParentIDs: []string{"fast-1"}, Producer: Producer{Phase: PhaseRuntime}, AssistantState: &AssistantState{AssistantItemID: "fast-1", Visibility: VisibilityPlayed}},
		{ID: "tool-1", Kind: KindToolCall, MonotonicNS: 4, CausalParentIDs: []string{"fast-1"}, InvocationID: "inv-slow", Producer: Producer{Phase: PhaseSlow, Provider: "google", Model: "gemini"}, ToolCall: &ToolCall{CallID: "call-1", Name: "calendar.read", Arguments: json.RawMessage(`{"day":"Tuesday"}`)}},
		{ID: "result-1", Kind: KindToolResult, MonotonicNS: 5, CausalParentIDs: []string{"tool-1"}, Producer: Producer{Phase: PhaseTool}, ToolResult: &ToolResult{CallID: "call-1", Name: "calendar.read", Output: json.RawMessage(`{"free":true}`)}},
	}
	if err := store.AppendBatch(items); err != nil {
		t.Fatal(err)
	}
	snapshot := store.Snapshot()
	if snapshot.Version != uint64(len(items)) || len(snapshot.Items) != len(items) {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
	snapshot.Items[0].Content = "mutated"
	if got := store.Snapshot().Items[0].Content; got != "Schedule lunch." {
		t.Fatalf("snapshot aliases store: %q", got)
	}
}

func TestStoreAppendBatchIsAtomic(t *testing.T) {
	t.Parallel()
	store := NewStore()
	err := store.AppendBatch([]Item{
		{ID: "one", Kind: KindObservation, Producer: Producer{Phase: PhaseUser}, Content: "one"},
		{ID: "two", Kind: KindObservation, CausalParentIDs: []string{"missing"}, Producer: Producer{Phase: PhaseUser}, Content: "two"},
	})
	if err == nil {
		t.Fatal("expected invalid parent error")
	}
	if got := store.Snapshot().Version; got != 0 {
		t.Fatalf("partial append changed store version to %d", got)
	}
}

func TestStoreRejectsInvalidToolAndVisibilityTransitions(t *testing.T) {
	t.Parallel()
	store := NewStore()
	if err := store.Append(Item{ID: "assistant", Kind: KindAssistant, Producer: Producer{Phase: PhaseFast}, Content: "hello", Visibility: VisibilityPlayed}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(Item{ID: "cancel", Kind: KindAssistantState, MonotonicNS: 1, Producer: Producer{Phase: PhaseRuntime}, AssistantState: &AssistantState{AssistantItemID: "assistant", Visibility: VisibilityCancelled}}); err == nil {
		t.Fatal("expected played content cancellation to fail")
	}
	if err := store.Append(Item{ID: "result", Kind: KindToolResult, MonotonicNS: 1, Producer: Producer{Phase: PhaseTool}, ToolResult: &ToolResult{CallID: "unknown", Name: "tool", Output: json.RawMessage(`true`)}}); err == nil {
		t.Fatal("expected unknown tool result to fail")
	}
}

func TestStoreValidatesOpaqueProviderState(t *testing.T) {
	t.Parallel()
	store := NewStore()
	if err := store.Append(Item{ID: "bad", Kind: KindReasoning, Producer: Producer{Phase: PhaseSlow}, ProviderStateType: "gemini-content-v1", ProviderState: json.RawMessage(`{`)}); err == nil {
		t.Fatal("expected malformed provider state to fail")
	}
	if err := store.Append(Item{ID: "opaque", Kind: KindReasoning, Producer: Producer{Phase: PhaseSlow}, ProviderStateType: "gemini-content-v1", ProviderState: json.RawMessage(`{"role":"model","parts":[]}`)}); err != nil {
		t.Fatal(err)
	}
}

func TestToolProposalCannotReceiveResultOrBecomeCallByIDReuse(t *testing.T) {
	t.Parallel()
	store := NewStore()
	proposal := &ToolCall{CallID: "proposal-1", Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`)}
	if err := store.Append(Item{ID: "proposal", Kind: KindToolProposal, Producer: Producer{Phase: PhaseFast}, ToolCall: proposal}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(Item{ID: "result", Kind: KindToolResult, Producer: Producer{Phase: PhaseTool}, ToolResult: &ToolResult{CallID: "proposal-1", Name: "lookup", Output: json.RawMessage(`true`)}}); err == nil {
		t.Fatal("proposal accepted a tool result")
	}
	if err := store.Append(Item{ID: "call", Kind: KindToolCall, Producer: Producer{Phase: PhaseSlow}, ToolCall: proposal}); err == nil {
		t.Fatal("executable call reused a proposal ID")
	}
}
