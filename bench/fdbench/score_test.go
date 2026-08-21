package fdbench

import (
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func agentAudio(atMS, durationMS float64) bench.Moment {
	return bench.Moment{AtMS: atMS, Kind: bench.MomentAgentAudio, AudioMS: durationMS}
}

// Three things in tension, and no single number that could stand for them:
// how fast the agent answered, whether it began over the top of the person,
// and whether it answered at all.

func TestAConversationAnsweredInsideTheBudgetPasses(t *testing.T) {
	var outcome bench.TaskOutcome
	score(&outcome, bench.Transcript{Moments: []bench.Moment{
		agentAudio(1100, 400),
		agentAudio(3100, 400),
	}}, []Turn{
		{StartMS: 0, EndMS: 1000},
		{StartMS: 2000, EndMS: 3000},
	}, 800)

	if !outcome.Passed {
		t.Fatalf("both turns were answered in the budget: %+v", outcome.Metrics)
	}
	if outcome.Metrics["answered"] != 2 || outcome.Metrics["missed_turns"] != 0 {
		t.Fatalf("unexpected counts: %+v", outcome.Metrics)
	}
	if outcome.Metrics["response_latency_ms"] != 100 {
		t.Fatalf("latency must be measured from the end of the turn: %v",
			outcome.Metrics["response_latency_ms"])
	}
}

// An answer that begins while the person is still talking is an endpointing
// failure and fails the conversation.
func TestSpeakingOverAPersonMidTurnIsPremature(t *testing.T) {
	var outcome bench.TaskOutcome
	score(&outcome, bench.Transcript{Moments: []bench.Moment{
		agentAudio(500, 300),
		agentAudio(1100, 300),
	}}, []Turn{{StartMS: 0, EndMS: 1000}}, 800)

	if outcome.Passed {
		t.Fatal("beginning an answer mid-turn must fail the conversation")
	}
	if outcome.Metrics["premature_turns"] != 1 || outcome.Metrics["overrun_turns"] != 0 {
		t.Fatalf("the two overlap causes must not be confused: %+v", outcome.Metrics)
	}
	if outcome.Metrics["overlap_ms"] <= 0 {
		t.Fatal("overlap that happened must be reported")
	}
}

// An answer that was already running when the person started again is an
// overrun. It is what barge-in is for, is measured separately, and does not
// fail the conversation.
func TestAnAnswerRunningIntoTheNextTurnIsAnOverrunAndNotAFailure(t *testing.T) {
	var outcome bench.TaskOutcome
	score(&outcome, bench.Transcript{Moments: []bench.Moment{
		agentAudio(1100, 300),
		agentAudio(1900, 300), // still going as the next turn starts
		agentAudio(2100, 300),
		agentAudio(3100, 300),
	}}, []Turn{
		{StartMS: 0, EndMS: 1000},
		{StartMS: 2000, EndMS: 3000},
	}, 800)

	if outcome.Metrics["overrun_turns"] != 1 || outcome.Metrics["premature_turns"] != 0 {
		t.Fatalf("an overrun is not an endpointing failure: %+v", outcome.Metrics)
	}
	if !outcome.Passed {
		t.Fatalf("an overrun must not fail the conversation: %+v", outcome.Metrics)
	}
}

// The window for a reply closes when the next turn begins. After that the
// person has moved on, and a reply is an interruption rather than a late
// answer to the previous thing.
func TestAReplyAfterThePersonMovedOnIsMissedRatherThanLate(t *testing.T) {
	// The only reply lands after the second turn. It answers the second and is
	// not a late answer to the first, however generous the budget.
	var outcome bench.TaskOutcome
	score(&outcome, bench.Transcript{Moments: []bench.Moment{
		agentAudio(3100, 300),
	}}, []Turn{
		{StartMS: 0, EndMS: 1000},
		{StartMS: 2000, EndMS: 3000},
	}, 5000)

	if outcome.Metrics["missed_turns"] != 1 || outcome.Metrics["answered"] != 1 {
		t.Fatalf("the first turn's window closed when the second began: %+v", outcome.Metrics)
	}
	if outcome.Metrics["premature_turns"] != 0 {
		t.Fatalf("nothing was said over the person: %+v", outcome.Metrics)
	}
	if outcome.Passed {
		t.Fatal("a missed turn fails the conversation")
	}
}

func TestSilenceIsEveryTurnMissed(t *testing.T) {
	var outcome bench.TaskOutcome
	score(&outcome, bench.Transcript{}, []Turn{
		{StartMS: 0, EndMS: 1000},
		{StartMS: 2000, EndMS: 3000},
	}, 800)

	if outcome.Metrics["missed_turns"] != 2 || outcome.Metrics["answered"] != 0 {
		t.Fatalf("unexpected counts: %+v", outcome.Metrics)
	}
	if _, reported := outcome.Metrics["response_latency_ms"]; reported {
		t.Fatal("a latency with no answers behind it is not a latency")
	}
}
