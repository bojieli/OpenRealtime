package cognition

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/spoken"
)

// The trajectory can only tell a model about turns that have ended. A turn
// decided while the previous one is still coming out of the loudspeaker is
// exactly where that matters, and it is the case this field exists for.
func TestTheVoiceIsToldWhatIsAudibleAndWhatIsOnlyQueued(t *testing.T) {
	prompt := Instruct("base", Request{Speaking: spoken.Mark{
		Spoken: "one two three", Cut: "four", Pending: "four five six", Measured: true,
	}})
	if !strings.Contains(prompt, SpeakingNowInstruction) {
		t.Fatalf("the voice was not told it is mid-sentence: %q", prompt)
	}
	heard := strings.Index(prompt, "one two three")
	queued := strings.Index(prompt, "four five six")
	if heard < 0 || queued < 0 {
		t.Fatalf("both halves have to be visible: %q", prompt)
	}
	// They license opposite things, so they must be separable. Presented as one
	// blob a model has no way to tell what it may not repeat from what it has
	// not said yet.
	if heard >= queued {
		t.Fatalf("what was heard must precede what is only queued: %q", prompt)
	}
	if !strings.Contains(prompt, "Already audible") || !strings.Contains(prompt, "not yet audible") {
		t.Fatalf("the two halves are not labelled: %q", prompt)
	}
}

// Nothing to say about the agent's own voice means nothing said about it. A
// line stating that no audio is in flight would appear on every ordinary turn
// and cost a paragraph of attention for a fact that never changes.
func TestSilenceAddsNoLineAtAll(t *testing.T) {
	if prompt := Instruct("base", Request{}); prompt != "base" {
		t.Fatalf("an ordinary turn gained a line about speech: %q", prompt)
	}
}

// A turn that has been fully spoken is a different situation from one that was
// cut off, and saying so is what stops the voice repeating it.
func TestAFullySpokenTurnSaysItHasAllBeenSaid(t *testing.T) {
	prompt := Instruct("base", Request{Speaking: spoken.Mark{Spoken: "all of it"}})
	if !strings.Contains(prompt, "All of it has been said") {
		t.Fatalf("a completed turn is not described as one: %q", prompt)
	}
}
