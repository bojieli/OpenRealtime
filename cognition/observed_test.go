package cognition_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/cognition"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// A turn caused by a stretch of silence has no utterance behind it. Handed a
// conversation that ends with its own last words and nothing new addressed to
// it, a provider says nothing at all - measured three runs out of three, which
// is the same shape as an interjection whose sentence was left in the
// instruction. What happened is real; it simply was not spoken.
func TestSomethingNobodySaidStillReachesTheProvider(t *testing.T) {
	store := trajectory.NewStore()
	fast := fastProvider(continuation.Event{
		Kind: continuation.EventAssistantDelta, Text: "Are you still there?",
	})
	engine, err := cognition.New(cognition.Config{Store: store, Fast: fast, Slow: slowProvider()})
	if err != nil {
		t.Fatal(err)
	}
	for index, item := range []trajectory.Item{
		{
			ID: "obs-1", Kind: trajectory.KindObservation, MonotonicNS: 1, SourceRevision: 1,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
			Content:  "if I go quiet for fifteen seconds, ask whether I'm still there",
		},
		{
			ID: "say-1", Kind: trajectory.KindAssistant, MonotonicNS: 2, SourceRevision: 1,
			Producer: trajectory.Producer{
				Phase: trajectory.PhaseFast, SpeechAuthority: string(continuation.SpeechAuthorityVoice),
			},
			Content: "Understood, I'll check in.",
		},
	} {
		if err := store.Append(item); err != nil {
			t.Fatalf("seed %d: %v", index, err)
		}
	}

	request := cognition.Request{
		Standing: []string{"check whether they are still there (20s ago)"},
		Because:  "answer", Observed: "nobody has said anything for 15s",
	}
	if _, err := engine.RunFast(t.Context(), request, nil); err != nil {
		t.Fatal(err)
	}
	if fast.seen == nil {
		t.Fatal("the provider was never invoked")
	}
	if !strings.Contains(fast.seen.Invocation.Instruction, "Nobody said anything") {
		t.Fatalf("the voice was not told what happened:\n%s", fast.seen.Invocation.Instruction)
	}
	// And it reaches the conversation, not only the instruction - which is the
	// whole of the lesson from the interjection that returned an empty string.
	var found bool
	for _, item := range fast.seen.Trajectory.Items {
		if item.Kind == trajectory.KindObservation &&
			strings.Contains(item.Content, "nobody has said anything") {
			found = true
		}
	}
	if !found {
		t.Fatalf("nothing new was addressed to the provider: %+v", fast.seen.Trajectory.Items)
	}
}
