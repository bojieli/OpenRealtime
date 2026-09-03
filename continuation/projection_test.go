package continuation_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestPendingToolProposalContentPreservesExactComposablePayload(t *testing.T) {
	t.Parallel()
	call := &trajectory.ToolCall{
		CallID: "call-1", Name: "computer.wait",
		Arguments: json.RawMessage(" {\n  \"duration_ms\": 1000\n}"),
	}
	content, ok := continuation.PendingToolProposalContent(call)
	if !ok {
		t.Fatal("valid pending proposal was rejected")
	}
	if !strings.Contains(content, "Pending proposal tool name: computer.wait") ||
		!strings.Contains(content, "Pending proposal arguments (exact JSON bytes):  {\n  \"duration_ms\": 1000\n}") {
		t.Fatalf("pending proposal lost exact content: %q", content)
	}
	if strings.Contains(content, "non_executable_tool_proposal") ||
		strings.Contains(content, continuation.TerminalToolProposalNotice) {
		t.Fatalf("pending proposal was serialized as assistant control or terminal state: %q", content)
	}
	if call.Name != "computer.wait" || string(call.Arguments) != " {\n  \"duration_ms\": 1000\n}" {
		t.Fatalf("canonical call was mutated: %+v", call)
	}
}

func TestPendingToolProposalContentRejectsIncompleteDefensiveItems(t *testing.T) {
	t.Parallel()
	for _, call := range []*trajectory.ToolCall{
		nil,
		{Name: "", Arguments: json.RawMessage(`{}`)},
		{Name: "computer.wait"},
	} {
		if content, ok := continuation.PendingToolProposalContent(call); ok || content != "" {
			t.Fatalf("incomplete proposal projected as %q", content)
		}
	}
}

func TestProviderRunsGroupOnlyAdjacentUserObservations(t *testing.T) {
	t.Parallel()
	user := func(id, content string) trajectory.Item {
		return trajectory.Item{
			ID: id, Kind: trajectory.KindObservation, Content: content,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
		}
	}
	observer := trajectory.Item{
		ID: "screen", Kind: trajectory.KindObservation, Content: "a dialog is open",
		Producer: trajectory.Producer{Phase: trajectory.PhaseObserver},
		Observation: &trajectory.ObservationMeta{
			Observer: "video", Source: "screen", Authority: trajectory.AuthorityObserver,
		},
	}
	items := []trajectory.Item{
		user("user-1", "first"),
		user("user-2", "second"),
		{ID: "assistant", Kind: trajectory.KindAssistant, Content: "reply"},
		user("user-3", "third"),
		{ID: "tool", Kind: trajectory.KindToolResult, ToolResult: &trajectory.ToolResult{CallID: "call", Name: "lookup"}},
		user("user-4", "fourth"),
		observer,
		user("user-5", "fifth"),
		user("user-6", "sixth"),
	}

	runs := continuation.ProviderRuns(items)
	wantLengths := []int{2, 1, 1, 1, 1, 1, 2}
	if len(runs) != len(wantLengths) {
		t.Fatalf("got %d projection runs, want %d: %#v", len(runs), len(wantLengths), runs)
	}
	for index, want := range wantLengths {
		if len(runs[index].Items) != want {
			t.Errorf("run %d has %d items, want %d", index, len(runs[index].Items), want)
		}
	}
	for _, index := range []int{0, 2, 4, 6} {
		if !runs[index].UserObservations {
			t.Errorf("run %d was not identified as user observations", index)
		}
	}
	for _, index := range []int{1, 3, 5} {
		if runs[index].UserObservations {
			t.Errorf("boundary run %d was identified as user observations", index)
		}
	}
}

func TestProviderRunsCompactAdjacentTypedSupersessionToCurrentObservation(t *testing.T) {
	t.Parallel()
	items := []trajectory.Item{
		{
			ID: "partial", Kind: trajectory.KindObservation, SourceRevision: 1,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "book a flight",
			Event: revisionEvent("partial", "asr", "microphone", 0),
		},
		{
			ID: "updated", Kind: trajectory.KindObservation, SourceRevision: 2,
			CausalParentIDs: []string{"partial"},
			Producer:        trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "book a flight tomorrow",
			Event: revisionEvent("updated", "asr", "microphone", 1),
		},
	}
	run := continuation.ProviderRuns(items)[0]
	if len(run.Items) != 1 || run.Items[0].ID != "updated" {
		t.Fatalf("provider retained a superseded partial: %#v", run.Items)
	}
	if run.Items[0].Event == nil || run.Items[0].Event.SupersedesRevision != 0 {
		t.Fatalf("consumed in-run replacement remained model-visible: %#v", run.Items[0].Event)
	}
	content := continuation.ObservationRunContent(run, map[string]string{"updated": "[2.0s later]"})
	want := "[2.0s later] book a flight tomorrow"
	if content != want {
		t.Fatalf("projected content = %q, want %q", content, want)
	}
}

func TestProviderRunsCompactEachSupersessionChainAndKeepIndependentFragments(t *testing.T) {
	t.Parallel()
	items := []trajectory.Item{
		{
			ID: "voice-partial", Kind: trajectory.KindObservation, SourceRevision: 1,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "enter incident",
			Event: revisionEvent("voice-partial", "asr", "voice", 0),
		},
		{
			ID: "independent", Kind: trajectory.KindObservation, SourceRevision: 2,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "also retain this final fragment",
			Event: revisionEvent("independent", "keyboard", "text", 0),
		},
		{
			ID: "voice-middle", Kind: trajectory.KindObservation, SourceRevision: 3,
			CausalParentIDs: []string{"voice-partial"},
			Producer:        trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "enter incident code ALF",
			Event: revisionEvent("voice-middle", "asr", "voice", 1),
		},
		{
			ID: "voice-final", Kind: trajectory.KindObservation, SourceRevision: 4,
			CausalParentIDs: []string{"voice-middle"},
			Producer:        trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "enter incident code alpha dash 7",
			Event: revisionEvent("voice-final", "asr", "voice", 3),
		},
	}

	runs := continuation.ProviderRuns(items)
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1: %#v", len(runs), runs)
	}
	if got := []string{runs[0].Items[0].ID, runs[0].Items[1].ID}; !slices.Equal(got, []string{"independent", "voice-final"}) {
		t.Fatalf("surviving observations = %#v", got)
	}
	want := "also retain this final fragment\nenter incident code alpha dash 7"
	if got := continuation.ObservationRunContent(runs[0], nil); got != want {
		t.Fatalf("projected content = %q, want %q", got, want)
	}
}

func TestProviderRunsDoNotCompactAcrossObservationLanes(t *testing.T) {
	t.Parallel()
	items := []trajectory.Item{
		{
			ID: "voice", Kind: trajectory.KindObservation, SourceRevision: 1,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "spoken fragment",
			Event: revisionEvent("voice", "asr", "voice", 0),
		},
		{
			ID: "text", Kind: trajectory.KindObservation, SourceRevision: 2,
			CausalParentIDs: []string{"voice"},
			Producer:        trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "typed fragment",
			Event: revisionEvent("text", "keyboard", "text", 1),
		},
	}

	run := continuation.ProviderRuns(items)[0]
	if len(run.Items) != 2 {
		t.Fatalf("a cross-lane replacement hid user input: %#v", run.Items)
	}
	if got := continuation.ObservationRunContent(run, nil); !strings.Contains(got, "spoken fragment") ||
		!strings.Contains(got, "Updated user speech revision") {
		t.Fatalf("cross-lane evidence was rewritten: %q", got)
	}
}

func TestProviderRunsUseTheCanonicalObservationLaneRules(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		firstEvent *trajectory.EventMetadata
		lastEvent  *trajectory.EventMetadata
		firstRev   uint64
		lastRev    uint64
		compact    bool
	}{
		{
			name: "same channel and source", compact: true, firstRev: 2, lastRev: 9,
			firstEvent: revisionEvent("first", "asr", "microphone", 0),
			lastEvent:  revisionEvent("last", "asr", "microphone", 2),
		},
		{
			name: "same source on different channels", firstRev: 2, lastRev: 9,
			firstEvent: revisionEvent("first", "asr", "microphone", 0),
			lastEvent:  revisionEvent("last", "asr", "voice", 2),
		},
		{
			name: "different sources on a non-voice channel", firstRev: 2, lastRev: 9,
			firstEvent: revisionEvent("first", "asr-a", "microphone", 0),
			lastEvent:  revisionEvent("last", "asr-b", "microphone", 2),
		},
		{
			name: "user voice handoff between sources", compact: true, firstRev: 2, lastRev: 9,
			firstEvent: revisionEvent("first", "audio", "voice", 0),
			lastEvent:  revisionEvent("last", "client", "voice", 2),
		},
		{
			name: "legacy target has the historical global lane", compact: true, firstRev: 2, lastRev: 9,
			firstEvent: nil,
			lastEvent:  revisionEvent("last", "client", "text", 2),
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			items := []trajectory.Item{
				{
					ID: "first", Kind: trajectory.KindObservation, SourceRevision: test.firstRev,
					Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "first version",
					Event: test.firstEvent,
				},
				{
					ID: "last", Kind: trajectory.KindObservation, SourceRevision: test.lastRev,
					CausalParentIDs: []string{"first"},
					Producer:        trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "last version",
					Event: test.lastEvent,
				},
			}
			run := continuation.ProviderRuns(items)[0]
			if test.compact {
				if got := projectedIDs(run.Items); !slices.Equal(got, []string{"last"}) {
					t.Fatalf("same-lane projection retained stale input: %#v", got)
				}
				if run.Items[0].Event == nil || run.Items[0].Event.SupersedesRevision != 0 {
					t.Fatalf("consumed replacement marker remained visible: %#v", run.Items[0].Event)
				}
				return
			}
			if got := projectedIDs(run.Items); !slices.Equal(got, []string{"first", "last"}) {
				t.Fatalf("cross-lane projection hid input: %#v", got)
			}
			if run.Items[1].Event == nil || run.Items[1].Event.SupersedesRevision == 0 {
				t.Fatal("unconsumed cross-lane replacement marker was cleared")
			}
		})
	}
}

func TestProviderRunsResolveDuplicateAndInterleavedRevisionOrderingExactly(t *testing.T) {
	t.Parallel()
	observation := func(id, source, channel string, revision, supersedes uint64, parent string) trajectory.Item {
		item := trajectory.Item{
			ID: id, Kind: trajectory.KindObservation, SourceRevision: revision,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: id,
			Event: revisionEvent(id, source, channel, supersedes),
		}
		if parent != "" {
			item.CausalParentIDs = []string{parent}
		}
		return item
	}
	for _, test := range []struct {
		name  string
		items []trajectory.Item
		want  []string
	}{
		{
			name: "interleaved independent lane does not make target stale",
			items: []trajectory.Item{
				observation("voice-1", "asr", "voice", 1, 0, ""),
				observation("text-2", "keyboard", "text", 2, 0, ""),
				observation("voice-3", "asr", "voice", 3, 1, "voice-1"),
			},
			want: []string{"text-2", "voice-3"},
		},
		{
			name: "newer same-lane observation makes old target stale",
			items: []trajectory.Item{
				observation("voice-1", "asr", "voice", 1, 0, ""),
				observation("voice-2", "asr", "voice", 2, 0, ""),
				observation("voice-3", "asr", "voice", 3, 1, "voice-1"),
			},
			want: []string{"voice-1", "voice-2", "voice-3"},
		},
		{
			name: "globally newer duplicate revision on another lane makes claim ambiguous",
			items: []trajectory.Item{
				observation("voice-old", "asr", "voice", 1, 0, ""),
				observation("text-duplicate", "keyboard", "text", 1, 0, ""),
				observation("voice-final", "asr", "voice", 2, 1, "voice-old"),
			},
			want: []string{"voice-old", "text-duplicate", "voice-final"},
		},
		{
			name: "causal edge to older same-lane duplicate fails open",
			items: []trajectory.Item{
				observation("voice-old", "asr", "voice", 1, 0, ""),
				observation("voice-newer", "asr", "voice", 1, 0, ""),
				observation("voice-final", "asr", "voice", 2, 1, "voice-old"),
			},
			want: []string{"voice-old", "voice-newer", "voice-final"},
		},
		{
			name: "causal edge to globally latest duplicate removes only that target",
			items: []trajectory.Item{
				observation("voice-old", "asr", "voice", 1, 0, ""),
				observation("voice-newer", "asr", "voice", 1, 0, ""),
				observation("voice-final", "asr", "voice", 2, 1, "voice-newer"),
			},
			want: []string{"voice-old", "voice-final"},
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			runs := continuation.ProviderRuns(test.items)
			if len(runs) != 1 {
				t.Fatalf("projection split adjacent user input: %#v", runs)
			}
			if got := projectedIDs(runs[0].Items); !slices.Equal(got, test.want) {
				t.Fatalf("projected IDs = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestProviderRunsDoNotCompactAcrossModelVisibleBoundaries(t *testing.T) {
	t.Parallel()
	boundaries := map[string]trajectory.Item{
		"instruction omitted by provider adapters": {
			ID: "instruction", Kind: trajectory.KindInstruction,
			Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, Content: "historical policy",
		},
		"reasoning": {
			ID: "reasoning", Kind: trajectory.KindReasoning,
			Producer: trajectory.Producer{Phase: trajectory.PhaseSlow}, Content: "internal state",
		},
		"assistant": {
			ID: "assistant", Kind: trajectory.KindAssistant,
			Producer: trajectory.Producer{Phase: trajectory.PhaseFast}, Content: "acted",
		},
		"assistant state omitted by provider adapters": {
			ID: "state", Kind: trajectory.KindAssistantState,
			Producer:       trajectory.Producer{Phase: trajectory.PhaseRuntime},
			AssistantState: &trajectory.AssistantState{AssistantItemID: "assistant", Visibility: trajectory.VisibilityQueued},
		},
		"repair state omitted by provider adapters": {
			ID: "repair", Kind: trajectory.KindRepair,
			Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime},
			Repair:   &trajectory.RepairState{TargetAssistantItemID: "assistant", Status: trajectory.RepairRequired},
		},
		"tool proposal": {
			ID: "proposal", Kind: trajectory.KindToolProposal,
			Producer: trajectory.Producer{Phase: trajectory.PhaseSlow},
			ToolCall: &trajectory.ToolCall{CallID: "call", Name: "lookup", Arguments: json.RawMessage(`{}`)},
		},
		"tool call": {
			ID: "call", Kind: trajectory.KindToolCall,
			Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime},
			ToolCall: &trajectory.ToolCall{CallID: "call", Name: "lookup", Arguments: json.RawMessage(`{}`)},
		},
		"tool result": {
			ID: "result", Kind: trajectory.KindToolResult,
			Producer:   trajectory.Producer{Phase: trajectory.PhaseTool},
			ToolResult: &trajectory.ToolResult{CallID: "call", Name: "lookup", Output: json.RawMessage(`{"ok":true}`)},
		},
		"tool placeholder": {
			ID: "placeholder", Kind: trajectory.KindToolPlaceholder,
			Producer:        trajectory.Producer{Phase: trajectory.PhaseRuntime},
			ToolPlaceholder: &trajectory.ToolPlaceholder{CallID: "call", Name: "lookup", Reason: "interrupted"},
		},
		"observer": {
			ID: "observer", Kind: trajectory.KindObservation,
			Producer: trajectory.Producer{Phase: trajectory.PhaseObserver}, Content: "screen changed",
			Observation: &trajectory.ObservationMeta{Observer: "vision", Source: "screen", Authority: trajectory.AuthorityObserver},
		},
	}
	for name, boundary := range boundaries {
		boundary := boundary
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			items := []trajectory.Item{
				{
					ID: "partial", Kind: trajectory.KindObservation, SourceRevision: 1,
					Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "partial",
					Event: revisionEvent("partial", "asr", "voice", 0),
				},
				boundary,
				{
					ID: "updated", Kind: trajectory.KindObservation, SourceRevision: 2,
					CausalParentIDs: []string{"partial"},
					Producer:        trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "complete",
					Event: revisionEvent("updated", "asr", "voice", 1),
				},
			}
			runs := continuation.ProviderRuns(items)
			if len(runs) != 3 || len(runs[0].Items) != 1 || len(runs[2].Items) != 1 {
				t.Fatalf("projection compacted across %s boundary: %#v", name, runs)
			}
			if got := continuation.ObservationRunContent(runs[2], nil); !strings.Contains(got, "Updated user speech revision") {
				t.Fatalf("post-%s replacement lost its explicit marker: %q", name, got)
			}
		})
	}
}

func TestProviderRunsLeaveCanonicalSnapshotAndProvenanceUntouched(t *testing.T) {
	t.Parallel()
	snapshot := trajectory.Snapshot{Version: 17, Items: []trajectory.Item{
		{
			ID: "one", Kind: trajectory.KindObservation, MonotonicNS: uint64(time.Second),
			SourceRevision: 4, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "one",
			Event: revisionEvent("one", "asr", "voice", 0),
			Observation: &trajectory.ObservationMeta{
				Observer: "voice", Source: "microphone", Authority: trajectory.AuthorityUser,
				Media: []trajectory.MediaRef{{Handle: "audio-1", MIMEType: "audio/wav", Source: "microphone"}},
			},
		},
		{
			ID: "two", Kind: trajectory.KindObservation, MonotonicNS: uint64(2 * time.Second),
			SourceRevision: 5, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "two",
			CausalParentIDs: []string{"one"},
			Event:           revisionEvent("two", "asr", "voice", 4),
			Observation: &trajectory.ObservationMeta{
				Observer: "voice", Source: "microphone", Authority: trajectory.AuthorityUser,
				Media: []trajectory.MediaRef{{Handle: "audio-2", MIMEType: "audio/wav", Source: "microphone"}},
			},
		},
	}}
	before, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}

	runs := continuation.ProviderRuns(snapshot.Items)
	if len(runs) != 1 || len(runs[0].Items) != 1 {
		t.Fatalf("unexpected projection: %#v", runs)
	}
	if runs[0].Items[0].ID != "two" || runs[0].Items[0].SourceRevision != 5 ||
		runs[0].Items[0].Observation == nil || runs[0].Items[0].Observation.Media[0].Handle != "audio-2" ||
		runs[0].Items[0].Event == nil ||
		runs[0].Items[0].Event == snapshot.Items[1].Event ||
		runs[0].Items[0].Event.SupersedesRevision != 0 {
		t.Fatalf("projection did not isolate its replacement metadata: %#v", runs[0].Items[0])
	}
	after, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || snapshot.Version != 17 {
		t.Fatalf("provider projection mutated the canonical snapshot\nbefore: %s\nafter:  %s", before, after)
	}
	if snapshot.Items[1].Event.SupersedesRevision != 4 {
		t.Fatal("provider projection mutated canonical event metadata through a shared pointer")
	}
	if strings.Contains(continuation.ObservationRunContent(runs[0], nil), "audio-1") {
		t.Fatal("media provenance leaked into user-visible text")
	}
}

func TestProviderRunsReturnItemStructCopiesForSingleObservationRuns(t *testing.T) {
	t.Parallel()
	items := []trajectory.Item{{
		ID: "only", Kind: trajectory.KindObservation,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "canonical",
	}}

	runs := continuation.ProviderRuns(items)
	if len(runs) != 1 || len(runs[0].Items) != 1 {
		t.Fatalf("unexpected projection: %#v", runs)
	}
	runs[0].Items[0].Content = "projection mutation"
	if items[0].Content != "canonical" {
		t.Fatalf("single-item projection aliases the canonical slice: %#v", items[0])
	}
}

func projectedIDs(items []trajectory.Item) []string {
	ids := make([]string, len(items))
	for index, item := range items {
		ids[index] = item.ID
	}
	return ids
}

func flattenProviderRuns(runs []continuation.ProviderRun) []trajectory.Item {
	var items []trajectory.Item
	for _, run := range runs {
		items = append(items, run.Items...)
	}
	return items
}

func supersessionChain(length int) []trajectory.Item {
	items := make([]trajectory.Item, length)
	for index := range items {
		revision := uint64(index + 1)
		id := fmt.Sprintf("revision-%04d", revision)
		items[index] = trajectory.Item{
			ID: id, Kind: trajectory.KindObservation, SourceRevision: revision,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: id,
			Event: revisionEvent(id, "asr", "voice", revision-1),
		}
		if index > 0 {
			items[index].CausalParentIDs = []string{items[index-1].ID}
		}
	}
	return items
}

func revisionEvent(id, source, channel string, supersedes uint64) *trajectory.EventMetadata {
	return &trajectory.EventMetadata{
		EventID: id + "-event", Type: "input.revision", Source: source, Channel: channel,
		SupersedesRevision: supersedes,
	}
}
