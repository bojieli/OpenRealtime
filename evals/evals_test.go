package evals_test

import (
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/evals"
)

// A case declares what may happen, not what must. Asking the user and handing
// the turn on can both be right at one boundary, and an evaluation that admits
// only one of them measures conformity rather than correctness.
func TestAnyAcceptableActionPasses(t *testing.T) {
	item := evals.Case{
		Name: "ambiguous", Decision: evals.DecisionHandOff,
		Accept: []evals.Action{evals.ActionHandOn, evals.ActionDraft},
	}
	for _, did := range []evals.Action{evals.ActionHandOn, evals.ActionDraft} {
		outcome := evals.Score(item, evals.Observation{Actions: []evals.Action{did}})
		if !outcome.Passed {
			t.Errorf("%s is in the acceptable set and must pass: %s", did, outcome.Because)
		}
	}
	outcome := evals.Score(item, evals.Observation{Actions: []evals.Action{evals.ActionFinish}})
	if outcome.Passed {
		t.Error("an action outside the set must not pass")
	}
}

// A veto is not a low score. Stating a delivery status nobody supplied misleads
// the caller whatever else the turn did well, so it fails even when the turn
// also did an acceptable thing.
func TestAForbiddenActionVetoesAnOtherwisePassingTurn(t *testing.T) {
	item := evals.Case{
		Name: "fabricated", Decision: evals.DecisionHandOff,
		Accept: []evals.Action{evals.ActionHandOn},
		Forbid: []evals.Action{evals.ActionFabricate},
	}
	outcome := evals.Score(item, evals.Observation{
		Actions: []evals.Action{evals.ActionHandOn, evals.ActionFabricate},
	})
	if outcome.Passed {
		t.Fatal("a fabricated result must not pass on the strength of handing on")
	}
	if !outcome.Vetoed {
		t.Fatal("the failure must be recorded as a veto, not an ordinary miss")
	}
}

// A provider that errored has not made a decision, and counting it as a wrong
// one would blame the model for the network.
func TestAProviderErrorIsNeitherPassNorVeto(t *testing.T) {
	item := evals.Case{Name: "down", Decision: evals.DecisionHandOff, Accept: []evals.Action{evals.ActionHandOn}}
	outcome := evals.Score(item, evals.Observation{Err: errTest})
	if outcome.Passed || outcome.Vetoed {
		t.Fatalf("an error is its own outcome: %+v", outcome)
	}
	report := evals.Report{Outcomes: []evals.Outcome{outcome}}
	if summary := report.Summary(); summary.Errored != 1 || summary.Passed != 0 {
		t.Fatalf("errors are counted apart: %+v", summary)
	}
}

func TestSummaryCountsVetoesApartFromMisses(t *testing.T) {
	report := evals.Report{Outcomes: []evals.Outcome{
		{Passed: true}, {Vetoed: true}, {}, {Observation: evals.Observation{Elapsed: 5 * time.Second}},
	}}
	summary := report.Summary()
	if summary.Total != 4 || summary.Passed != 1 || summary.Vetoed != 1 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if summary.Slowest != 5*time.Second {
		t.Fatalf("the slowest case is what a latency budget is judged on, got %v", summary.Slowest)
	}
}

// The hand-off set has to contain both kinds of case or it measures a bias.
func TestHandOffCasesCoverBothAnswers(t *testing.T) {
	var handOn, finish int
	for _, item := range evals.HandOffCases() {
		if item.Decision != evals.DecisionHandOff {
			t.Fatalf("%s is filed under the wrong decision", item.Name)
		}
		if len(item.Accept) == 0 {
			t.Fatalf("%s accepts nothing, so it can only fail", item.Name)
		}
		for _, action := range item.Accept {
			if action == evals.ActionHandOn {
				handOn++
			}
			if action == evals.ActionFinish {
				finish++
			}
		}
	}
	if handOn == 0 || finish == 0 {
		t.Fatalf("a set that only tests one answer rewards always giving it: %d hand-on, %d finish", handOn, finish)
	}
}

var errTest = testError("provider unreachable")

type testError string

func (err testError) Error() string { return string(err) }
