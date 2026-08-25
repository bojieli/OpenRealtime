package evals_test

import (
	"context"
	"testing"

	"github.com/bojieli/OpenRealtime/evals"
)

// scripted answers a fixed sequence, so the scoring can be tested without a
// model. What is under test here is the clock, not a provider.
type scripted struct{ acts []evals.Action }

func (runner *scripted) Name() string             { return "scripted" }
func (runner *scripted) Decision() evals.Decision { return evals.DecisionInteraction }
func (runner *scripted) Observe(context.Context, evals.Case) evals.Observation {
	act := runner.acts[0]
	runner.acts = runner.acts[1:]
	return evals.Observation{Actions: []evals.Action{act}}
}

func moments(times ...int) []evals.Moment {
	built := make([]evals.Moment, len(times))
	for index, at := range times {
		built[index] = evals.Moment{AtMS: at}
	}
	return built
}

func TestTimelineFailsADecisionTakenTooLate(t *testing.T) {
	outcome := evals.RunTimeline(context.Background(),
		&scripted{acts: []evals.Action{evals.ActStaySilent, evals.ActStaySilent, evals.ActAnswer}},
		evals.TimelineCase{Moments: moments(300, 900, 2500), Expect: evals.ActAnswer, NotAfterMS: 1500})
	if outcome.Passed {
		t.Fatalf("an answer 1000ms past the deadline passed: %s", outcome.Because)
	}
}

func TestTimelineFailsADecisionTakenTooEarly(t *testing.T) {
	outcome := evals.RunTimeline(context.Background(),
		&scripted{acts: []evals.Action{evals.ActAnswer, evals.ActAnswer}},
		evals.TimelineCase{Moments: moments(100, 900), Expect: evals.ActAnswer, NotBeforeMS: 400})
	if outcome.Passed {
		t.Fatalf("an answer before anyone had stopped talking passed: %s", outcome.Because)
	}
}

// Evidence only accumulates as a pause lengthens, so a decision that reverts
// was not reading the clock.
func TestTimelineCatchesADecisionTakenBack(t *testing.T) {
	item := evals.TimelineCase{
		Moments: moments(300, 900, 1800), Expect: evals.ActAnswer,
		Forbid: []evals.Action{evals.ActStaySilent}, Monotone: true,
	}
	acts := []evals.Action{evals.ActStaySilent, evals.ActAnswer, evals.ActStaySilent}
	if outcome := evals.RunTimeline(context.Background(), &scripted{acts: acts}, item); outcome.Passed {
		t.Fatalf("a reverted decision passed: %s", outcome.Because)
	}
	// Staying silent while the pause is still short is exactly right, and a
	// monotone case must not score it as a revert before the decision exists.
	early := []evals.Action{evals.ActStaySilent, evals.ActStaySilent, evals.ActAnswer}
	if outcome := evals.RunTimeline(context.Background(), &scripted{acts: early}, item); !outcome.Passed {
		t.Fatalf("waiting through a short pause was scored as taking a decision back: %s", outcome.Because)
	}
}

// A case that forbids answering during a pause says nothing about the moment
// after the speaker resumes and finishes their sentence.
func TestTimelineForbidsOnlyInsideItsWindow(t *testing.T) {
	item := evals.TimelineCase{
		Moments: moments(300, 900, 1400),
		Forbid:  []evals.Action{evals.ActAnswer}, ForbidUntilMS: 1100,
	}
	acts := []evals.Action{evals.ActStaySilent, evals.ActStaySilent, evals.ActAnswer}
	if outcome := evals.RunTimeline(context.Background(), &scripted{acts: acts}, item); !outcome.Passed {
		t.Fatalf("an act after the forbidden window failed the case: %s", outcome.Because)
	}
	inside := []evals.Action{evals.ActStaySilent, evals.ActAnswer, evals.ActStaySilent}
	if outcome := evals.RunTimeline(context.Background(), &scripted{acts: inside}, item); outcome.Passed {
		t.Fatalf("an act inside the forbidden window passed: %s", outcome.Because)
	}
}
