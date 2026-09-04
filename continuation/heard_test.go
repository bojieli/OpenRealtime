package continuation_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/spoken"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// An interrupted turn is two facts and a projection carrying only one of them
// is wrong in a specific way. This asserts both survive, and that only one of
// them looks like something the agent said.
func TestAnInterruptedTurnIsProjectedAsWhatWasHeardPlusWhatWasNot(t *testing.T) {
	projected := continuation.AssistantHeardContent(
		"1, 2, 3, 4, 5, 6, 7, 8, 9, 10.",
		spoken.Mark{Spoken: "1, 2, 3, 4, 5, 6,", Cut: "7,", Pending: "7, 8, 9, 10.", Measured: true},
	)
	if !strings.HasPrefix(projected, "1, 2, 3, 4, 5, 6,") {
		t.Fatalf("the heard part does not open the turn: %q", projected)
	}
	// The words nobody heard must not read as words the agent said. The line
	// between them is the whole projection.
	spokenPart, note, split := strings.Cut(projected, continuation.HeardPreamble)
	if !split {
		t.Fatalf("no runtime note separates heard from unheard: %q", projected)
	}
	for _, unheard := range []string{"8", "9", "10"} {
		if strings.Contains(spokenPart, unheard) {
			t.Fatalf("%q appears as something the user was told: %q", unheard, spokenPart)
		}
		if !strings.Contains(note, unheard) {
			t.Fatalf("%q was prepared and is missing from the note: %q", unheard, note)
		}
	}
	if !strings.Contains(note, "7,") {
		t.Fatalf("the word playback stopped inside is not named: %q", note)
	}
}

// A turn that finished is left exactly alone. Annotating one would put a
// runtime note on every ordinary sentence the agent ever says.
func TestATurnThatFinishedIsNotAnnotated(t *testing.T) {
	const said = "The capital of France is Paris."
	if got := continuation.AssistantHeardContent(said, spoken.Mark{Spoken: said}); got != said {
		t.Fatalf("a completed turn was rewritten to %q", got)
	}
}

// The projection belongs to ProviderRuns, which is the one place all three
// provider adapters go through. Putting it in each adapter would mean the one
// that forgot is the one whose agent talks past words nobody heard.
func TestEveryProviderSeesTheProjectionBecauseTheRunsCarryIt(t *testing.T) {
	items := []trajectory.Item{
		{ID: "user-1", Kind: trajectory.KindObservation, Content: "count to ten for me",
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}},
		{ID: "assistant-1", Kind: trajectory.KindAssistant, Content: "one two three four five",
			Visibility: trajectory.VisibilityPrepared,
			Producer:   trajectory.Producer{Phase: trajectory.PhaseFast}},
		{ID: "state-1", Kind: trajectory.KindAssistantState,
			Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime},
			AssistantState: &trajectory.AssistantState{
				AssistantItemID: "assistant-1", Visibility: trajectory.VisibilityPlayed,
				PlayedAudioMS: 1200,
				Heard: &spoken.Mark{
					Spoken: "one two", Cut: "three", Pending: "three four five", Measured: true,
				},
			}},
	}
	runs := continuation.ProviderRuns(items)
	var assistant string
	for _, run := range runs {
		for _, item := range run.Items {
			if item.Kind == trajectory.KindAssistant {
				assistant = item.Content
			}
		}
	}
	if assistant == "" {
		t.Fatal("the assistant turn vanished from the projection")
	}
	heard, note, split := strings.Cut(assistant, continuation.HeardPreamble)
	if !split {
		t.Fatalf("the run carried the raw turn: %q", assistant)
	}
	if strings.Contains(heard, "four") || strings.Contains(heard, "five") {
		t.Fatalf("unheard words were projected as speech: %q", heard)
	}
	if !strings.Contains(note, "three four five") {
		t.Fatalf("the prepared remainder is missing: %q", note)
	}
	// The canonical log is a record of what happened and is never rewritten by
	// a projection over it.
	if items[1].Content != "one two three four five" {
		t.Fatalf("the projection mutated the canonical item: %q", items[1].Content)
	}
}

// Absence of a boundary means nobody measured, which is not the same as
// nothing being heard. A text-only binding, an upstream provider, and every
// trajectory written before boundaries existed all land here, and annotating
// them would report every one of their turns as never spoken.
func TestATurnWithNoRecordedBoundaryIsLeftAlone(t *testing.T) {
	items := []trajectory.Item{
		{ID: "assistant-1", Kind: trajectory.KindAssistant, Content: "delivered as text",
			Visibility: trajectory.VisibilityPlayed,
			Producer:   trajectory.Producer{Phase: trajectory.PhaseFast}},
		{ID: "state-1", Kind: trajectory.KindAssistantState,
			Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime},
			AssistantState: &trajectory.AssistantState{
				AssistantItemID: "assistant-1", Visibility: trajectory.VisibilityPlayed,
			}},
	}
	for _, run := range continuation.ProviderRuns(items) {
		for _, item := range run.Items {
			if item.Kind == trajectory.KindAssistant && item.Content != "delivered as text" {
				t.Fatalf("an unmeasured turn was annotated: %q", item.Content)
			}
		}
	}
}
