package interaction_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/interaction"
)

// A recogniser cuts where somebody breathes, so the tail of one request
// arrives capitalised and punctuated like a request of its own. Read that way
// answering it is the obvious move, and it produces a second acknowledgement
// of something already agreed to - which the voice is told not to give and
// cannot avoid, because nothing in front of it says this is the same sentence.
//
// Measured on the moment this was found: told only the gap in milliseconds the
// model answered seven times out of seven; told this, it listened seven times
// out of seven.
func TestTheRestOfAnAnsweredSentenceSaysSo(t *testing.T) {
	tail := interaction.Situation{
		Recent: []string{
			"user: I'm going to read for a bit. Tell me the moment.",
			"agent: I'll let you know.",
		},
		Heard:   "The build finishes and don't say anything else.",
		Silence: "500ms", SincePrevious: "110ms",
		ContinuesAnswered: true,
	}.Render()
	if !strings.Contains(tail, "already replied to") {
		t.Fatalf("the tail of an answered sentence was rendered as a new request:\n%s", tail)
	}
	// And an ordinary utterance is not described that way, or every request
	// would look like one that had been dealt with.
	fresh := interaction.Situation{Heard: "what is my balance", Silence: "500ms"}.Render()
	if strings.Contains(fresh, "already replied to") {
		t.Fatalf("an ordinary request was rendered as a continuation:\n%s", fresh)
	}
}
