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

func TestPinboardLinesExceptRetainsOnlyOtherTurns(t *testing.T) {
	board := &interaction.Pinboard{}
	board.SetForTurn(1, []interaction.StandingInstruction{{
		Text: "count each animal", Scope: interaction.ScopeConversation, SetNS: uint64(time.Second),
	}})
	board.SetForTurn(2, []interaction.StandingInstruction{{
		Text: "acknowledge an alert if it appears", Scope: interaction.ScopeConversation, SetNS: uint64(2 * time.Second),
	}})
	lines := board.LinesExcept(uint64(3*time.Second), 2)
	if len(lines) != 1 || !strings.Contains(lines[0], "count each animal") ||
		strings.Contains(lines[0], "acknowledge") {
		t.Fatalf("LinesExcept current turn = %v", lines)
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

// A turn is read many times as the words arrive, and every reading answers for
// the whole of it. Measured, "I'm going to tell you about my afternoon. Count
// the animals out loud as I mention them, and say nothing else" pinned three
// policies one at a time, the first of which told the agent not to reply until
// the story was over.
func TestATurnsPoliciesAreReplacedByItsLaterReading(t *testing.T) {
	board := &interaction.Pinboard{}
	board.SetForTurn(7, []interaction.StandingInstruction{
		{Text: "do not reply until they have finished telling you about their afternoon",
			Scope: interaction.ScopeConversation, SetNS: 1_000},
	})
	board.SetForTurn(7, []interaction.StandingInstruction{
		{Text: "count the animals out loud as they mention them, and say nothing else",
			Scope: interaction.ScopeConversation, SetNS: 9_000, Counting: true, Restricting: true},
	})
	inForce := board.InForce()
	if len(inForce) != 1 {
		t.Fatalf("one turn should leave the policies it last named, got %d: %v", len(inForce), inForce)
	}
	if !inForce[0].Counting || !inForce[0].Restricting {
		t.Fatal("what the last reading found should carry over")
	}
}

// A policy may be stated before the exclusion attached to it. The cheap
// classification pass once read the short policy as counting and the expanded
// one as not counting, so replacing all metadata made final-event recovery
// repeat the first animal and turn the second count into three.
func TestAnExpandedReadingKeepsAPositiveCountingClassification(t *testing.T) {
	board := &interaction.Pinboard{}
	if added := board.SetForTurn(7, []interaction.StandingInstruction{{
		Text:  "count the animals out loud as they mention them",
		Scope: interaction.ScopeConversation, SetNS: 1_000, Counting: true,
	}}); added != 1 {
		t.Fatalf("the first reading added %d policies, want 1", added)
	}
	if added := board.SetForTurn(7, []interaction.StandingInstruction{{
		Text:  "count the animals out loud as they mention them and say nothing else",
		Scope: interaction.ScopeConversation, SetNS: 9_000, Restricting: true,
	}}); added != 0 {
		t.Fatalf("an expansion of the same policy added %d policies, want 0", added)
	}
	inForce := board.InForce()
	if len(inForce) != 1 {
		t.Fatalf("the expanded reading should leave one policy: %+v", inForce)
	}
	if !inForce[0].Counting || !inForce[0].Restricting {
		t.Fatalf("the expanded policy lost a positive classification: %+v", inForce[0])
	}
	if inForce[0].SetNS != 1_000 {
		t.Fatalf("the expanded policy moved from %d to %d", 1_000, inForce[0].SetNS)
	}
}

// Positive metadata belongs to the policy that established it, not to every
// later policy the same stretch of speech happens to establish.
func TestADifferentLaterPolicyDoesNotInheritCounting(t *testing.T) {
	board := &interaction.Pinboard{}
	board.SetForTurn(7, []interaction.StandingInstruction{{
		Text: "count the animals out loud", Scope: interaction.ScopeConversation,
		Counting: true,
	}})
	board.SetForTurn(7, []interaction.StandingInstruction{{
		Text: "translate each phrase into English", Scope: interaction.ScopeConversation,
	}})
	inForce := board.InForce()
	if len(inForce) != 1 || inForce[0].Counting {
		t.Fatalf("a different policy inherited counting: %+v", inForce)
	}
}

// Two policies from one turn both stand. Replacing one with the next was what
// a one-at-a-time reading forced, and it destroyed a live counting policy the
// moment the same turn also asked not to be interrupted.
func TestATurnMaySetMoreThanOnePolicy(t *testing.T) {
	board := &interaction.Pinboard{}
	board.SetForTurn(3, []interaction.StandingInstruction{
		{Text: "do not interrupt them", Scope: interaction.ScopeConversation, SetNS: 10},
		{Text: "count the animals out loud as they mention them", Scope: interaction.ScopeConversation, SetNS: 10},
	})
	if len(board.InForce()) != 2 {
		t.Fatalf("both policies should stand: %v", board.InForce())
	}
}

// An earlier turn's policies are separate requests and nobody lifted them. The
// age of one that is still there carries over, since re-reading somebody's
// sentence as more of it arrives is not them asking again.
func TestAnotherTurnLeavesEarlierPoliciesAloneAndKeepsTheirAge(t *testing.T) {
	board := &interaction.Pinboard{}
	board.SetForTurn(1, []interaction.StandingInstruction{
		{Text: "tell them when the kettle boils", Scope: interaction.ScopeConversation, SetNS: 500},
	})
	board.SetForTurn(2, []interaction.StandingInstruction{
		{Text: "count the animals out loud", Scope: interaction.ScopeConversation, SetNS: 4_000},
	})
	board.SetForTurn(2, []interaction.StandingInstruction{
		{Text: "count the animals out loud", Scope: interaction.ScopeConversation, SetNS: 9_000},
	})
	inForce := board.InForce()
	if len(inForce) != 2 {
		t.Fatalf("the earlier turn's policy should survive: %v", inForce)
	}
	for _, existing := range inForce {
		if existing.Text == "count the animals out loud" && existing.SetNS != 4_000 {
			t.Fatalf("age should not reset on re-reading, got %d", existing.SetNS)
		}
	}
}

// A policy set earlier and listed again by a later turn stays where it was
// set. The runtime declines to act on the utterance that set a policy, so an
// origin that drifts onto whatever the speaker is saying makes that refusal
// land on an occurrence: measured, the second animal in a story went uncounted
// because the policy had been re-listed from the sentence that mentioned it.
func TestAPolicysOriginDoesNotMoveWhenALaterTurnListsItAgain(t *testing.T) {
	board := &interaction.Pinboard{}
	board.SetForTurn(1, []interaction.StandingInstruction{
		{Text: "count the animals out loud", Scope: interaction.ScopeConversation, SetNS: 100},
	})
	board.SetForTurn(2, []interaction.StandingInstruction{
		{Text: "count the animals out loud", Scope: interaction.ScopeConversation, SetNS: 9_000},
	})
	inForce := board.InForce()
	if len(inForce) != 1 {
		t.Fatalf("one policy, not one per turn that mentions it: %v", inForce)
	}
	if inForce[0].Turn != 1 || inForce[0].SetNS != 100 {
		t.Fatalf("origin moved: turn=%d setNS=%d", inForce[0].Turn, inForce[0].SetNS)
	}
}

// The extraction pass must not be shown its own answer for the turn it is
// re-reading. Told both to list everything that stretch of speech sets and not
// to repeat what is already in force, it sees its own pin on the list and
// declines to repeat it - and since the answer replaces that turn's policies,
// declining deletes them.
func TestATurnsOwnPoliciesAreHiddenFromTheReadingOfIt(t *testing.T) {
	board := &interaction.Pinboard{}
	board.SetForTurn(1, []interaction.StandingInstruction{
		{Text: "tell them when the kettle boils", Scope: interaction.ScopeConversation},
	})
	board.SetForTurn(2, []interaction.StandingInstruction{
		{Text: "count the animals out loud", Scope: interaction.ScopeConversation},
	})
	shown := board.InForceExcept(2)
	if len(shown) != 1 || shown[0].Text != "tell them when the kettle boils" {
		t.Fatalf("a turn should see every policy but its own: %v", shown)
	}
	if len(board.InForceExcept(9)) != 2 {
		t.Fatal("an unrelated turn should see both")
	}
}
