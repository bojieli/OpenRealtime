package trajectory

import (
	"encoding/json"
	"errors"
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

func TestStoreAppendBatchAtRejectsStaleWriterAtomically(t *testing.T) {
	t.Parallel()
	store := NewStore()
	if err := store.Append(Item{ID: "one", Kind: KindObservation, Producer: Producer{Phase: PhaseUser}, Content: "one"}); err != nil {
		t.Fatal(err)
	}
	err := store.AppendBatchAt(0, []Item{{
		ID: "stale", Kind: KindObservation, Producer: Producer{Phase: PhaseUser}, Content: "stale",
	}})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("got %v, want version conflict", err)
	}
	snapshot := store.Snapshot()
	if snapshot.Version != 1 || len(snapshot.Items) != 1 || snapshot.Items[0].ID != "one" {
		t.Fatalf("stale append changed trajectory: %#v", snapshot)
	}
	if err := store.AppendBatchAt(1, []Item{{
		ID: "two", Kind: KindObservation, Producer: Producer{Phase: PhaseUser}, Content: "two",
	}}); err != nil {
		t.Fatal(err)
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

func TestStorePreservesStructuredEventMetadata(t *testing.T) {
	t.Parallel()
	store := NewStore()
	metadata := &EventMetadata{
		EventID: "event-1", Type: "asr.revision", Source: "qwen3-asr",
		Channel: "voice", OccurredNS: 10, CorrelationID: "session-1",
	}
	if err := store.Append(Item{
		ID: "observation", Kind: KindObservation, MonotonicNS: 20,
		Producer: Producer{Phase: PhaseUser}, Content: "hello", Event: metadata,
	}); err != nil {
		t.Fatal(err)
	}
	metadata.Type = "mutated"
	snapshot := store.Snapshot()
	if snapshot.Items[0].Event.Type != "asr.revision" || snapshot.Items[0].Event.OccurredNS != 10 || snapshot.Items[0].MonotonicNS != 20 {
		t.Fatalf("event occurrence and commit metadata changed: %#v", snapshot.Items[0])
	}
	snapshot.Items[0].Event.Source = "mutated"
	if got := store.Snapshot().Items[0].Event.Source; got != "qwen3-asr" {
		t.Fatalf("event metadata aliases snapshot: %q", got)
	}
	if err := store.Append(Item{
		ID: "bad", Kind: KindAssistant, MonotonicNS: 21,
		Producer: Producer{Phase: PhaseFast}, Content: "bad", Event: &EventMetadata{
			EventID: "event-2", Type: "bad", Source: "bad", Channel: "bad",
		},
	}); err == nil {
		t.Fatal("model output accepted external event metadata")
	}
}

func TestAssistantVisibilityResolvesAppendOnlyPlaybackState(t *testing.T) {
	t.Parallel()
	snapshot := Snapshot{Items: []Item{
		{ID: "prepared", Kind: KindAssistant, InvocationID: "inv-prepared", Producer: Producer{Phase: PhaseFast}, Content: "one"},
		{ID: "queued", Kind: KindAssistant, InvocationID: "inv-queued", Producer: Producer{Phase: PhaseFast}, Content: "two"},
		{ID: "queued-state", Kind: KindAssistantState, Producer: Producer{Phase: PhaseRuntime}, AssistantState: &AssistantState{AssistantItemID: "queued", Visibility: VisibilityQueued}},
		{ID: "cancelled", Kind: KindAssistant, InvocationID: "inv-cancelled", Producer: Producer{Phase: PhaseFast}, Content: "three"},
		{ID: "cancelled-state", Kind: KindAssistantState, Producer: Producer{Phase: PhaseRuntime}, AssistantState: &AssistantState{AssistantItemID: "cancelled", Visibility: VisibilityCancelled}},
	}}

	visibility := AssistantVisibility(snapshot)
	if visibility["prepared"] != VisibilityPrepared || visibility["queued"] != VisibilityQueued || visibility["cancelled"] != VisibilityCancelled {
		t.Fatalf("unexpected resolved visibility: %#v", visibility)
	}
	cancelled := CancelledAssistantInvocations(snapshot)
	if _, ok := cancelled["inv-cancelled"]; !ok || len(cancelled) != 1 {
		t.Fatalf("unexpected cancelled invocations: %#v", cancelled)
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

func TestPlayedInvalidationCreatesAndResolvesTypedRepair(t *testing.T) {
	t.Parallel()
	store := NewStore()
	if err := store.AppendBatch([]Item{
		{ID: "user", Kind: KindObservation, SourceRevision: 1, Producer: Producer{Phase: PhaseUser}, Content: "request"},
		{ID: "fast", Kind: KindAssistant, SourceRevision: 1, Producer: Producer{Phase: PhaseFast}, Content: "provisional answer"},
		{ID: "queued", Kind: KindAssistantState, Producer: Producer{Phase: PhaseRuntime}, AssistantState: &AssistantState{AssistantItemID: "fast", Visibility: VisibilityQueued}},
		{ID: "played", Kind: KindAssistantState, Producer: Producer{Phase: PhaseRuntime}, AssistantState: &AssistantState{AssistantItemID: "fast", Visibility: VisibilityPlayed, PlayedAudioMS: 120}},
		{ID: "updated", Kind: KindObservation, SourceRevision: 2, Producer: Producer{Phase: PhaseUser}, Content: "updated request"},
		{ID: "required", Kind: KindRepair, SourceRevision: 2, CausalParentIDs: []string{"fast"}, Producer: Producer{Phase: PhaseRuntime}, Repair: &RepairState{TargetAssistantItemID: "fast", Status: RepairRequired, PlayedAudioMS: 120}},
	}); err != nil {
		t.Fatal(err)
	}
	pending := PendingRepairs(store.Snapshot())
	if len(pending) != 1 || pending[0].TargetAssistantItemID != "fast" || pending[0].PlayedAudioMS != 120 {
		t.Fatalf("pending repairs = %#v", pending)
	}
	if err := store.Append(Item{ID: "bad-cancel", Kind: KindAssistantState, Producer: Producer{Phase: PhaseRuntime}, AssistantState: &AssistantState{AssistantItemID: "fast", Visibility: VisibilityCancelled}}); err == nil {
		t.Fatal("played content was erased while its repair was pending")
	}
	if err := store.AppendBatch([]Item{
		{ID: "correction", Kind: KindAssistant, SourceRevision: 2, Producer: Producer{Phase: PhaseSlow}, Content: "Correction: the updated answer is different."},
		{ID: "resolved", Kind: KindRepair, SourceRevision: 2, CausalParentIDs: []string{"fast", "correction"}, Producer: Producer{Phase: PhaseRuntime}, Repair: &RepairState{TargetAssistantItemID: "fast", Status: RepairResolved, RepairAssistantItemID: "correction"}},
	}); err != nil {
		t.Fatal(err)
	}
	if pending := PendingRepairs(store.Snapshot()); len(pending) != 0 {
		t.Fatalf("resolved repair remained pending: %#v", pending)
	}
}

func TestRepairLifecycleRejectsUnplayedAndUnmatchedTransitions(t *testing.T) {
	t.Parallel()
	store := NewStore()
	if err := store.Append(Item{ID: "assistant", Kind: KindAssistant, Producer: Producer{Phase: PhaseFast}, Content: "answer"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(Item{ID: "required", Kind: KindRepair, Producer: Producer{Phase: PhaseRuntime}, Repair: &RepairState{TargetAssistantItemID: "assistant", Status: RepairRequired, PlayedAudioMS: 10}}); err == nil {
		t.Fatal("unplayed assistant content created a repair obligation")
	}
	if err := store.Append(Item{ID: "correction", Kind: KindAssistant, Producer: Producer{Phase: PhaseSlow}, Content: "correction"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(Item{ID: "resolved", Kind: KindRepair, Producer: Producer{Phase: PhaseRuntime}, Repair: &RepairState{TargetAssistantItemID: "assistant", Status: RepairResolved, RepairAssistantItemID: "correction"}}); err == nil {
		t.Fatal("repair resolved without a required transition")
	}
}

func TestStoreEnforcesTypedSupersessionProvenance(t *testing.T) {
	t.Parallel()
	store := NewStore()
	if err := store.Append(Item{
		ID: "first", Kind: KindObservation, SourceRevision: 1,
		Producer: Producer{Phase: PhaseUser}, Content: "partial",
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &EventMetadata{
		EventID: "event-2", Type: "asr.stable_partial", Source: "asr", Channel: "voice",
		SupersedesRevision: 1,
	}
	if err := store.Append(Item{
		ID: "missing-cause", Kind: KindObservation, SourceRevision: 2,
		Producer: Producer{Phase: PhaseUser}, Content: "extended", Event: metadata,
	}); err == nil {
		t.Fatal("supersession without its causal target was accepted")
	}
	if err := store.Append(Item{
		ID: "second", Kind: KindObservation, SourceRevision: 2,
		CausalParentIDs: []string{"first"}, Producer: Producer{Phase: PhaseUser},
		Content: "extended", Event: metadata,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(Item{
		ID: "stale-branch", Kind: KindObservation, SourceRevision: 3,
		CausalParentIDs: []string{"first"}, Producer: Producer{Phase: PhaseUser},
		Content: "stale branch", Event: &EventMetadata{
			EventID: "event-stale", Type: "asr.stable_partial", Source: "asr", Channel: "voice",
			SupersedesRevision: 1,
		},
	}); err == nil {
		t.Fatal("supersession skipped the latest canonical observation")
	}
	if err := store.Append(Item{
		ID: "assistant", Kind: KindAssistant, SourceRevision: 2,
		Producer: Producer{Phase: PhaseSlow}, Content: "answer",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(Item{
		ID: "fake-supersession", Kind: KindAssistantState,
		Producer:       Producer{Phase: PhaseRuntime},
		AssistantState: &AssistantState{AssistantItemID: "assistant", Visibility: VisibilityQueued},
		Event: &EventMetadata{
			EventID: "event-3", Type: "speech.queued", Source: "tts", Channel: "voice",
			SupersedesRevision: 1,
		},
	}); err == nil {
		t.Fatal("non-observation event claimed observation supersession")
	}
}
