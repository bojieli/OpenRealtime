package cognition_test

import (
	"testing"

	"github.com/bojieli/OpenRealtime/cognition"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// An act that speaks into somebody else's turn runs on a sentence that is not
// in the log yet. Putting it in the instruction alone leaves the conversation
// ending with whatever the agent last said, and a provider asked to continue
// from its own last turn with nothing new addressed to it has nothing to
// continue.
//
// Measured against Gemini 3.5 Flash: "count them as I mention them", the agent
// having already said "1", and the next animal in the instruction returned an
// empty string three times out of three. The same call with that animal as a
// user turn returned "2" three times out of three. Every instruction variant
// tried, down to a six-hundred-character one, returned empty - so this is not
// a wording problem.
func TestTheLiveUtteranceIsShownToTheProviderAsConversation(t *testing.T) {
	store := trajectory.NewStore()
	fast := fastProvider(continuation.Event{Kind: continuation.EventAssistantDelta, Text: "2"})
	engine, err := cognition.New(cognition.Config{
		Store: store, Fast: fast, Slow: slowProvider(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for index, item := range []trajectory.Item{
		{
			ID: "obs-1", Kind: trajectory.KindObservation, MonotonicNS: 1, SourceRevision: 1,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
			Content:  "a capybara wandered over and sat down next to me",
		},
		{
			ID: "say-1", Kind: trajectory.KindAssistant, MonotonicNS: 2, SourceRevision: 1,
			Producer: trajectory.Producer{
				Phase: trajectory.PhaseFast, SpeechAuthority: string(continuation.SpeechAuthorityVoice),
			},
			Content: "1",
		},
	} {
		if err := store.Append(item); err != nil {
			t.Fatalf("seed %d: %v", index, err)
		}
	}

	if _, err := engine.RunFast(t.Context(), cognition.Request{
		Interjecting: true, Because: "speak-through",
		Standing: []string{"count the animals out loud as they are mentioned (20s ago)"},
		Heard:    "then a heron landed on the far bank",
	}, nil); err != nil {
		t.Fatal(err)
	}
	if fast.seen == nil {
		t.Fatal("the provider was never invoked")
	}

	items := fast.seen.Trajectory.Items
	var heron trajectory.Item
	for _, item := range items {
		if item.Kind == trajectory.KindObservation && item.Content == "then a heron landed on the far bank" {
			heron = item
		}
	}
	if heron.ID == "" {
		t.Fatalf("the sentence the turn exists for never reached the conversation: %+v", items)
	}
	// After the agent's own last word, or the provider is still being asked to
	// continue from itself.
	said, spoken := -1, -1
	for index, item := range items {
		if item.Kind == trajectory.KindAssistant {
			said = index
		}
		if item.ID == heron.ID {
			spoken = index
		}
	}
	if spoken < said {
		t.Fatalf("the live utterance was placed before the agent's own last turn: %+v", items)
	}
	// And it is shown, not appended: the recogniser commits it when the
	// speaker finishes, and committing it here would put it in twice.
	for _, item := range store.Snapshot().Items {
		if item.Content == "then a heron landed on the far bank" {
			t.Fatal("an utterance still being spoken was committed to the trajectory")
		}
	}
}
