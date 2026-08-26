package interaction_test

import (
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/interaction"
)

// A policy nobody can turn off is worse than one that was never set.
func TestPinboardRevokesWhatSomebodyLifted(t *testing.T) {
	board := &interaction.Pinboard{}
	board.Pin(interaction.StandingInstruction{
		Text: "tell them when the kettle has boiled", Scope: interaction.ScopeConversation})
	if !board.Revoke("tell them when the kettle has boiled") {
		t.Fatal("an exact revocation did not match")
	}
	if len(board.InForce()) != 0 {
		t.Fatalf("the policy survived its revocation: %+v", board.InForce())
	}
}

// The words that lift a policy are rarely the words that set it.
func TestPinboardRevokesOnAPartialMatch(t *testing.T) {
	board := &interaction.Pinboard{}
	board.Pin(interaction.StandingInstruction{
		Text: "tell them when the kettle has boiled", Scope: interaction.ScopeConversation})
	if !board.Revoke("when the kettle has boiled") {
		t.Fatal("a revocation quoting part of the policy did not match")
	}
	if len(board.InForce()) != 0 {
		t.Fatal("the policy survived a partial revocation")
	}
}

// "Wait, I have more to say" must not outlive the sentence it was about.
func TestPinboardExpiresTurnScopedPoliciesAndKeepsTheRest(t *testing.T) {
	board := &interaction.Pinboard{}
	board.Pin(interaction.StandingInstruction{Text: "do not reply until they finish", Scope: interaction.ScopeTurn})
	board.Pin(interaction.StandingInstruction{Text: "count the animals out loud", Scope: interaction.ScopeConversation})
	board.EndTurn()
	inForce := board.InForce()
	if len(inForce) != 1 || inForce[0].Text != "count the animals out loud" {
		t.Fatalf("EndTurn kept the wrong set: %+v", inForce)
	}
}

// Someone restating a rule is not asking for it twice, and every entry is
// prompt that every later decision pays for.
func TestPinboardDoesNotAccumulateDuplicates(t *testing.T) {
	board := &interaction.Pinboard{}
	for range 3 {
		board.Pin(interaction.StandingInstruction{Text: "do not cut them off", Scope: interaction.ScopeConversation})
	}
	if got := len(board.InForce()); got != 1 {
		t.Fatalf("the same policy was pinned %d times", got)
	}
}

// Age is part of the instruction.
func TestPinboardLinesCarryAge(t *testing.T) {
	board := &interaction.Pinboard{}
	board.Pin(interaction.StandingInstruction{
		Text: "count the animals out loud", Scope: interaction.ScopeConversation, SetNS: 1000})
	lines := board.Lines(1000 + uint64(2*time.Minute))
	if len(lines) != 1 || !strings.Contains(lines[0], "2m ago") {
		t.Fatalf("age missing or malformed: %v", lines)
	}
	recent := board.Lines(1000 + uint64(30*time.Second))
	if !strings.Contains(recent[0], "30s ago") {
		t.Fatalf("a sub-minute age rendered as %q", recent[0])
	}
}

// TestAPinnedDelayIsShownToWhoeverDecides is the regression for an agent that
// checked in at eight seconds when it had been asked to wait fifteen.
// Extraction lifts "after 15s" out of the text and into a number, which is
// what the runtime needs and leaves the model deciding whether to speak with
// no idea a number was ever named.
func TestAPinnedDelayIsShownToWhoeverDecides(t *testing.T) {
	board := &interaction.Pinboard{}
	board.Pin(interaction.StandingInstruction{
		Text: "ask whether they are still there", Scope: interaction.ScopeConversation,
		After: 15 * time.Second,
	})
	lines := board.Lines(0)
	if len(lines) != 1 {
		t.Fatalf("expected one pinned line, got %v", lines)
	}
	if !strings.Contains(lines[0], "15s") {
		t.Fatalf("the pinned line does not say how long to wait: %q", lines[0])
	}
}
