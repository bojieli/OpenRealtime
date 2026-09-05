package trajectory

import (
	"reflect"
	"strings"
	"testing"
)

func TestHistoryAppendVerifiesOriginalPrefixAndKeepsCanonicalCommitTime(t *testing.T) {
	store := NewStore()
	first := Item{ID: "partial", Kind: KindObservation, MonotonicNS: 10, Producer: Producer{Phase: PhaseUser}, Content: "A capybara"}
	if err := store.Append(first); err != nil {
		t.Fatal(err)
	}
	prefix, err := IdentifyPrefix(store.Snapshot(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Append(Item{ID: "final", Kind: KindObservation, MonotonicNS: 30, Producer: Producer{Phase: PhaseUser}, Content: "A capybara wandered over."}); err != nil {
		t.Fatal(err)
	}
	input := []Item{{ID: "speech", Kind: KindAssistant, MonotonicNS: 20, CausalParentIDs: []string{"partial"}, InvocationID: "count", Producer: Producer{Phase: PhaseFast, SpeechAuthority: "voice"}, Content: "One.", Visibility: VisibilityPrepared}}
	before := store.Snapshot()
	for _, bad := range []PrefixIdentity{{Version: 1, Digest: "sha256:" + strings.Repeat("0", 64)}, {Version: 3, Digest: prefix.Digest}, {Version: 0, Digest: prefix.Digest}, {Version: 1}} {
		if err = store.AppendBatchOnPrefix(bad, input); err == nil {
			t.Fatalf("accepted bad prefix: %+v", bad)
		}
		if !reflect.DeepEqual(before, store.Snapshot()) {
			t.Fatal("invalid prefix mutated history")
		}
	}
	invalid := append(cloneItems(input), Item{ID: "bad", Kind: KindAssistant, CausalParentIDs: []string{"missing"}, Content: "bad", Producer: Producer{Phase: PhaseFast}})
	if err = store.AppendBatchOnPrefix(prefix, invalid); err == nil {
		t.Fatal("accepted invalid batch")
	}
	if !reflect.DeepEqual(before, store.Snapshot()) {
		t.Fatal("invalid item partially appended history")
	}
	if err = store.AppendBatchOnPrefix(prefix, input); err != nil {
		t.Fatal(err)
	}
	after := store.Snapshot()
	got := after.Items[2]
	if got.MonotonicNS != 30 || got.Content != "One." || got.CausalParentIDs[0] != "partial" || got.Visibility != VisibilityPrepared || input[0].MonotonicNS != 20 {
		t.Fatalf("history was relabeled or source mutated: %+v", got)
	}
	if err = VerifyPrefix(after, prefix); err != nil {
		t.Fatal(err)
	}
	if err = store.AppendBatchOnPrefix(prefix, input); err == nil {
		t.Fatal("duplicate history appended")
	}
	if !reflect.DeepEqual(after, store.Snapshot()) {
		t.Fatal("duplicate changed history")
	}
}
