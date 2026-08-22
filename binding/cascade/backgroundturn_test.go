package cascade_test

import (
	"testing"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
)

// The turn the background reasoner starts reports running out of room, exactly
// as the turn a person started does.
//
// A fast+slow rollout speaks twice: once to answer now, and once when the
// reasoner has finished and there is something to say about it. Both are
// ordinary spoken turns and both fold into the turn's report - but only the
// first had coverage, so the second could stop being reported with every test
// still green, and the diagnosis would go quiet for exactly the turns a
// deployment cares most about.
//
// Written against the rollout as it now is rather than replayed from the shape
// it had: the voicing step is gone, the fast phase decides whether a turn needs
// deliberation and hands it on with the escalation marker, and what the
// reasoner leaves behind starts a further spoken turn of its own.
func TestTheTurnTheReasonerStartsReportsRunningOutOfRoom(t *testing.T) {
	// First turn answers and hands on; second turn is the one the background
	// result starts, and it spends its whole budget deliberating.
	fast := newFast(
		[]continuation.Event{{
			Kind: continuation.EventAssistantDelta,
			Text: "Let me check that." + continuation.EscalationMarker,
		}},
		[]continuation.Event{},
	).stopping(continuation.Completion{StopReason: "length", ReasoningInContent: true})
	slow := newSlow([]continuation.Event{{
		Kind: continuation.EventAssistantDelta,
		Text: "The account balance is $40.00 as of the latest statement.",
	}})
	runtime, sink := startSession(t, cascade.Config{Fast: fast, Slow: slow}, binding.Settings{})

	speak(t, runtime, 3)

	waitFor(t, func() bool {
		for _, outcome := range sink.turnOutcomes() {
			if outcome.Incomplete {
				return true
			}
		}
		return false
	}, "a spoken turn that produced nothing must say why, whoever started it")

	for _, outcome := range sink.turnOutcomes() {
		if !outcome.Incomplete {
			continue
		}
		if outcome.Reason != binding.TurnIncompleteTokens {
			t.Fatalf("reason = %q, want the protocol's own vocabulary", outcome.Reason)
		}
		if outcome.Detail == "" {
			t.Fatal("the operator must be told which knob to turn")
		}
	}
}
