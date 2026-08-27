package continuation_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

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

func TestObservationRunContentPreservesOrderElapsedTimeAndSupersession(t *testing.T) {
	t.Parallel()
	items := []trajectory.Item{
		{
			ID: "partial", Kind: trajectory.KindObservation, SourceRevision: 1,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "book a flight",
		},
		{
			ID: "updated", Kind: trajectory.KindObservation, SourceRevision: 2,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "book a flight tomorrow",
			Event: &trajectory.EventMetadata{SupersedesRevision: 1},
		},
	}
	run := continuation.ProviderRuns(items)[0]
	content := continuation.ObservationRunContent(run, map[string]string{"updated": "[2.0s later]"})
	want := "book a flight\n[2.0s later] Updated user speech revision; replace the earlier partial observation with this text:\nbook a flight tomorrow"
	if content != want {
		t.Fatalf("projected content = %q, want %q", content, want)
	}
}

func TestProviderRunsLeaveCanonicalSnapshotAndProvenanceUntouched(t *testing.T) {
	t.Parallel()
	snapshot := trajectory.Snapshot{Version: 17, Items: []trajectory.Item{
		{
			ID: "one", Kind: trajectory.KindObservation, MonotonicNS: uint64(time.Second),
			SourceRevision: 4, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "one",
			Observation: &trajectory.ObservationMeta{
				Observer: "voice", Source: "microphone", Authority: trajectory.AuthorityUser,
				Media: []trajectory.MediaRef{{Handle: "audio-1", MIMEType: "audio/wav", Source: "microphone"}},
			},
		},
		{
			ID: "two", Kind: trajectory.KindObservation, MonotonicNS: uint64(2 * time.Second),
			SourceRevision: 5, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "two",
		},
	}}
	before, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}

	runs := continuation.ProviderRuns(snapshot.Items)
	if len(runs) != 1 || len(runs[0].Items) != 2 {
		t.Fatalf("unexpected projection: %#v", runs)
	}
	if runs[0].Items[0].Observation == nil ||
		runs[0].Items[0].Observation.Media[0].Handle != "audio-1" ||
		runs[0].Items[0].SourceRevision != 4 {
		t.Fatalf("projection lost observation provenance: %#v", runs[0].Items[0])
	}
	after, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || snapshot.Version != 17 {
		t.Fatalf("provider projection mutated the canonical snapshot\nbefore: %s\nafter:  %s", before, after)
	}
	if strings.Contains(continuation.ObservationRunContent(runs[0], nil), "audio-1") {
		t.Fatal("media provenance leaked into user-visible text")
	}
}
