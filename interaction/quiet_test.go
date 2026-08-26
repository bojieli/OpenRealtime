package interaction_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/interaction"
)

// Every other route to this model is caused by an arrival - words, a frame, a
// result - and that default has one hole in it. Somebody can ask to be told
// about something that happens on its own, and the moment they mean is exactly
// the one where nothing arrives. Measured, the agent acknowledged "ask whether
// I'm still there if I go quiet for fifteen seconds" and then never asked.
func TestQuietDecidesOnlyWhereSomebodyAskedAboutIt(t *testing.T) {
	nothing := interaction.Situation{Silence: "20s", Quiet: true}
	if nothing.Decidable() {
		t.Fatal("quiet with nothing standing must decide nothing; that is the inertia everything rests on")
	}
	asked := interaction.Situation{
		Pins:    []string{"ask whether I'm still there if I go quiet for fifteen seconds (30s ago)"},
		Silence: "20s", Quiet: true,
	}
	if !asked.Decidable() {
		t.Fatal("a policy about time makes the passing of time evidence")
	}
	// And an ordinary quiet moment under an ordinary policy is still a
	// question, because which policies are about time is the model's to read
	// and not something to pattern-match here.
	ordinary := interaction.Situation{
		Pins: []string{"count the animals out loud (30s ago)"}, Silence: "20s", Quiet: true,
	}
	if !ordinary.Decidable() {
		t.Fatal("the runtime does not get to decide which policies are about time")
	}
	if !strings.Contains(asked.Render(), "silence: 20s") {
		t.Fatalf("the quiet was not described to the model:\n%s", asked.Render())
	}
}
