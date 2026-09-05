package policy

import (
	"reflect"
	"testing"

	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestSemanticExtractionHistoryUsesDisjointCanonicalSpeech(t *testing.T) {
	endpoint := func(id, stream, speaker, text string, revision uint64) trajectory.Item {
		return trajectory.Item{
			ID: id, Kind: trajectory.KindObservation, Content: text, SourceRevision: revision,
			Producer:    trajectory.Producer{Phase: trajectory.PhaseUser},
			Observation: &trajectory.ObservationMeta{Observer: "asr", Source: speaker, Authority: trajectory.AuthorityUser},
			Event:       &trajectory.EventMetadata{EventID: "event-" + id, Type: "asr.endpoint", Source: "asr", Channel: "microphone", CorrelationID: stream},
		}
	}
	partial := func(id, stream, speaker, text string, revision uint64) trajectory.Item {
		item := endpoint(id, stream, speaker, text, revision)
		item.Event.Type = "asr.revision"
		return item
	}
	const firstWords = "Count the animals as I mention them."
	const lastWords = "And say nothing else."
	old := endpoint("old", "old-stream", "microphone", firstWords, 1)
	answer := trajectory.Item{ID: "answer", Kind: trajectory.KindAssistant, Content: "Understood.", Visibility: trajectory.VisibilityPlayed}
	firstPartial := partial("first-partial", "first-stream", "incorrect-speaker", "Count the trains as I mention them.", 2)
	first := endpoint("first", "first-stream", "microphone", firstWords, 3)
	lastPartial := partial("last-partial", "last-stream", "microphone", "And say more.", 4)
	last := endpoint("last", "last-stream", "microphone", lastWords, 5)
	seen := trajectory.Item{ID: "seen", Kind: trajectory.KindObservation, Content: "The train is on screen.",
		Producer:    trajectory.Producer{Phase: trajectory.PhaseObserver},
		Observation: &trajectory.ObservationMeta{Observer: "video", Authority: trajectory.AuthorityObserver}}
	draft := trajectory.Item{ID: "draft", Kind: trajectory.KindAssistant, Content: "Ready.", Visibility: trajectory.VisibilityPrepared}
	canceled := trajectory.Item{ID: "canceled", Kind: trajectory.KindAssistantState,
		AssistantState: &trajectory.AssistantState{AssistantItemID: draft.ID, Visibility: trajectory.VisibilityCancelled}}

	t.Run("split endpoints and their superseded partials", func(t *testing.T) {
		items := []trajectory.Item{old, answer, firstPartial, first, seen, draft, canceled, lastPartial, last}
		want := []string{"user: " + firstWords, "agent: Understood.", "seen: The train is on screen.", "canceled (not said): Ready."}
		if got := semanticRecentBefore(items, last.ID, 12); !reflect.DeepEqual(got, want) {
			t.Fatalf("extraction history repeated the current utterance or removed unrelated context: %q", got)
		}
		if got := semanticHeardSince(items, last.ID, "user", 12); got != firstWords+" "+lastWords {
			t.Fatalf("reconstructed utterance changed: %q", got)
		}
	})
	t.Run("another speaker ends the reconstructed utterance", func(t *testing.T) {
		other := endpoint("other", "other-stream", "recorded-menu", "Press one.", 4)
		items := []trajectory.Item{first, other, last}
		want := []string{"user: " + firstWords, "recorded-menu: Press one."}
		if got := semanticRecentBefore(items, last.ID, 12); !reflect.DeepEqual(got, want) {
			t.Fatalf("other-speaker boundary lost prior history: %q", got)
		}
		if got := semanticHeardSince(items, last.ID, "user", 12); got != lastWords {
			t.Fatalf("another speaker entered the current utterance: %q", got)
		}
	})
	t.Run("bounded utterance leaves older endpoints in history", func(t *testing.T) {
		items := []trajectory.Item{first, last}
		want := []string{"user: " + firstWords}
		if got := semanticRecentBefore(items, last.ID, 1); !reflect.DeepEqual(got, want) {
			t.Fatalf("endpoint outside the bounded utterance disappeared: %q", got)
		}
		if got := semanticHeardSince(items, last.ID, "user", 1); got != lastWords {
			t.Fatalf("utterance exceeded its bound: %q", got)
		}
	})
	t.Run("uncorrelated and other-source partials remain context", func(t *testing.T) {
		unbound := partial("unbound", "", "microphone", "Unbound hypothesis.", 2)
		otherSource := partial("other-source", "last-stream", "microphone", "Another recognizer.", 3)
		otherSource.Event.Source = "other-asr"
		items := []trajectory.Item{unbound, otherSource, lastPartial, last}
		want := []string{"user: Unbound hypothesis.", "user: Another recognizer."}
		if got := semanticRecentBefore(items, last.ID, 12); !reflect.DeepEqual(got, want) {
			t.Fatalf("partial removal guessed a stream identity: %q", got)
		}
	})
}
