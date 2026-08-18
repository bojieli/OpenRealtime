package interleave

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestProjectSlowContextUsesTypedProvenanceOnly(t *testing.T) {
	t.Parallel()
	snapshot := trajectory.Snapshot{Version: 8, Items: []trajectory.Item{
		{ID: "user", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "same text"},
		{ID: "fast-instruction", Kind: trajectory.KindInstruction, InvocationID: "fast-run", Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, Content: "same text"},
		{ID: "fast-reasoning", Kind: trajectory.KindReasoning, InvocationID: "fast-run", Producer: trajectory.Producer{Phase: trajectory.PhaseFast}, Content: "same text"},
		{ID: "fast-assistant", Kind: trajectory.KindAssistant, InvocationID: "fast-run", CausalParentIDs: []string{"fast-reasoning"}, Producer: trajectory.Producer{Phase: trajectory.PhaseFast}, Content: "same text", ProviderStateType: "private", ProviderState: json.RawMessage(`{"state":true}`)},
		{ID: "fast-proposal", Kind: trajectory.KindToolProposal, InvocationID: "fast-run", CausalParentIDs: []string{"fast-assistant"}, Producer: trajectory.Producer{Phase: trajectory.PhaseFast}, ToolCall: &trajectory.ToolCall{CallID: "proposal", Name: "lookup", Arguments: json.RawMessage(`{}`)}},
		{ID: "fast-state", Kind: trajectory.KindAssistantState, Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, AssistantState: &trajectory.AssistantState{AssistantItemID: "fast-assistant", Visibility: trajectory.VisibilityPlayed}},
		{ID: "slow-call", Kind: trajectory.KindToolCall, Producer: trajectory.Producer{Phase: trajectory.PhaseSlow}, ToolCall: &trajectory.ToolCall{CallID: "call", Name: "lookup", Arguments: json.RawMessage(`{}`)}},
		{ID: "result", Kind: trajectory.KindToolResult, CausalParentIDs: []string{"slow-call", "fast-proposal"}, Producer: trajectory.Producer{Phase: trajectory.PhaseTool}, ToolResult: &trajectory.ToolResult{CallID: "call", Name: "lookup", Output: json.RawMessage(`{"ok":true}`)}},
	}}

	content, err := ProjectSlowContext(snapshot, SlowContextContentOnly)
	if err != nil {
		t.Fatal(err)
	}
	if content.Version != snapshot.Version || len(content.Items) != 5 {
		t.Fatalf("unexpected content-only projection: %#v", content)
	}
	assistant := findItem(content, "fast-assistant")
	if assistant == nil || assistant.Content != "same text" || assistant.ProviderStateType != "" || len(assistant.ProviderState) != 0 {
		t.Fatalf("content-only projection did not retain only portable assistant content: %#v", assistant)
	}
	if findItem(content, "fast-reasoning") != nil || findItem(content, "fast-proposal") != nil || findItem(content, "fast-instruction") != nil {
		t.Fatalf("content-only projection retained private fast work: %#v", content)
	}
	result := findItem(content, "result")
	if result == nil || len(result.CausalParentIDs) != 1 || result.CausalParentIDs[0] != "slow-call" {
		t.Fatalf("projection retained a dangling causal parent: %#v", result)
	}

	independent, err := ProjectSlowContext(snapshot, SlowContextIndependent)
	if err != nil {
		t.Fatal(err)
	}
	if len(independent.Items) != 3 || findItem(independent, "fast-assistant") != nil || findItem(independent, "fast-state") != nil {
		t.Fatalf("independent projection retained fast state: %#v", independent)
	}
	if findItem(independent, "user") == nil || findItem(independent, "slow-call") == nil || findItem(independent, "result") == nil {
		t.Fatalf("independent projection discarded authoritative context: %#v", independent)
	}
}

func TestEngineAppliesSlowProjectionWithoutForkingCanonicalStore(t *testing.T) {
	t.Parallel()
	for _, policy := range []SlowContextPolicy{SlowContextContentOnly, SlowContextIndependent} {
		policy := policy
		t.Run(string(policy), func(t *testing.T) {
			t.Parallel()
			store := trajectory.NewStore()
			if err := store.Append(trajectory.Item{ID: "user", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "lookup"}); err != nil {
				t.Fatal(err)
			}
			fast := &sequenceProvider{
				descriptor: descriptor("fast", trajectory.PhaseFast, false),
				scripts: []providerScript{{events: []continuation.Event{
					{Kind: continuation.EventReasoningDelta, Text: "private"},
					{Kind: continuation.EventAssistantDelta, Text: "I'll check."},
				}}},
			}
			slow := &sequenceProvider{
				descriptor: descriptor("slow", trajectory.PhaseSlow, false),
				scripts:    []providerScript{{events: []continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Done."}}}},
			}
			engine, err := New(Config{
				Store: store, FastProvider: fast, SlowProvider: slow,
				RetainReasoning: true, SlowContextPolicy: policy,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := engine.Run(context.Background(), Request{SourceRevision: 1}, nil); err != nil {
				t.Fatal(err)
			}
			request := slow.Requests()[0].Trajectory
			if snapshotContains(request, trajectory.KindReasoning, "private") {
				t.Fatalf("slow control received fast reasoning: %#v", request)
			}
			hasFastContent := snapshotContains(request, trajectory.KindAssistant, "I'll check.")
			if hasFastContent != (policy == SlowContextContentOnly) {
				t.Fatalf("policy %s fast content=%t: %#v", policy, hasFastContent, request)
			}
			canonical := store.Snapshot()
			if !snapshotContains(canonical, trajectory.KindReasoning, "private") || !snapshotContains(canonical, trajectory.KindAssistant, "I'll check.") {
				t.Fatalf("experimental projection mutated canonical state: %#v", canonical)
			}
		})
	}
}

func findItem(snapshot trajectory.Snapshot, id string) *trajectory.Item {
	for index := range snapshot.Items {
		if snapshot.Items[index].ID == id {
			return &snapshot.Items[index]
		}
	}
	return nil
}
