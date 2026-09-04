package interaction

import (
	"strings"
	"testing"
)

// Keep-speaking and stop-speaking are different acts depending on whether the
// thing worth saying has already been said. Stopping after the sentence landed
// costs nothing; stopping two words in throws away the reason the agent spoke.
// The decision could not see which of those it was in.
func TestAPolicySeesHowMuchOfItsOwnSentenceLanded(t *testing.T) {
	block := Situation{
		AgentSpeaking: true,
		AgentSaying:   "the deadline is the third, not the thirteenth",
		AgentSpoken:   "the deadline is the third,",
		AgentPending:  "not the thirteenth",
	}.Render()
	if !strings.Contains(block, `the user has already heard: "the deadline is the third,"`) {
		t.Fatalf("what landed is not in the block:\n%s", block)
	}
	if !strings.Contains(block, `not audible yet: "not the thirteenth"`) {
		t.Fatalf("what has not landed is not in the block:\n%s", block)
	}
}

// Absence of a boundary is not a boundary at the start. A deployment with no
// way to measure one must not have the runtime tell its policy that the user
// has heard nothing - that is the one fact these lines exist to supply, and
// inventing it is worse than leaving it out.
func TestNoMeasuredBoundaryAddsNoLine(t *testing.T) {
	block := Situation{AgentSpeaking: true, AgentSaying: "something"}.Render()
	for _, phrase := range []string{"already heard", "not audible yet", "none of it has reached"} {
		if strings.Contains(block, phrase) {
			t.Fatalf("an unmeasured turn was described as %q:\n%s", phrase, block)
		}
	}
}

// The two edges have to be sayable, or a policy reads their absence as the
// absence of a measurement.
func TestBothEdgesOfTheBoundaryAreStated(t *testing.T) {
	started := Situation{
		AgentSpeaking: true, AgentSaying: "hello there", AgentPending: "hello there",
	}.Render()
	if !strings.Contains(started, "none of it has reached the user yet") {
		t.Fatalf("a turn that is queued and not yet audible is not described:\n%s", started)
	}
	finished := Situation{
		AgentSpeaking: true, AgentSaying: "hello there", AgentSpoken: "hello there",
	}.Render()
	if !strings.Contains(finished, "all of it has reached the user") {
		t.Fatalf("a turn that has all been heard is not described:\n%s", finished)
	}
}
