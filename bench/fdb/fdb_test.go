package fdb_test

import (
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/fdb"
)

func outcome(category string, completed, passed bool, applicable string) bench.TaskOutcome {
	return bench.TaskOutcome{
		ID: category, Completed: completed, Passed: passed,
		Notes: map[string]string{"category": category, "applicable": applicable},
	}
}

// Two of the four categories want the opposite of the other two, so the report
// breaks them out rather than averaging them. A single number would say a
// system that always yields handles overlap well.
func TestBreakdownKeepsTheFourConditionsApart(t *testing.T) {
	result := bench.Result{Tasks: []bench.TaskOutcome{
		outcome(string(fdb.Interruption), true, true, "true"),
		outcome(string(fdb.Interruption), true, false, "true"),
		outcome(string(fdb.Backchannel), true, true, "true"),
		outcome(string(fdb.Backchannel), true, true, "true"),
	}}
	summaries := fdb.Breakdown(result)
	if got := summaries[fdb.Interruption]; got.Applicable != 2 || got.Passed != 1 || got.Rate != 0.5 {
		t.Fatalf("interruption: %+v", got)
	}
	if got := summaries[fdb.Backchannel]; got.Applicable != 2 || got.Passed != 2 || got.Rate != 1 {
		t.Fatalf("backchannel: %+v", got)
	}
	if len(summaries) != 2 {
		t.Fatalf("a category with no recordings must not appear: %v", summaries)
	}
}

// A recording where the agent was not speaking when the event arrived says
// something about latency and nothing about overlap. Folding it in either
// direction would corrupt both readings.
func TestNotApplicableIsNeitherAPassNorAFailure(t *testing.T) {
	result := bench.Result{Tasks: []bench.TaskOutcome{
		outcome(string(fdb.Interruption), true, true, "true"),
		outcome(string(fdb.Interruption), true, false, "false"),
		outcome(string(fdb.Interruption), true, false, "false"),
	}}
	summary := fdb.Breakdown(result)[fdb.Interruption]
	if summary.Total != 3 || summary.NotApplicable != 2 || summary.Applicable != 1 {
		t.Fatalf("unexpected split: %+v", summary)
	}
	if summary.Rate != 1 {
		t.Fatalf("the rate is over applicable recordings only, got %v", summary.Rate)
	}
}

// A recording that never ran is a failure of the run, not a score of zero.
func TestAnIncompleteRecordingIsCountedAsAFailureOfTheRun(t *testing.T) {
	result := bench.Result{Tasks: []bench.TaskOutcome{
		outcome(string(fdb.Interruption), false, false, "true"),
	}}
	summary := fdb.Breakdown(result)[fdb.Interruption]
	if summary.Failed != 1 || summary.Applicable != 0 || summary.Rate != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
}

// Two categories want the agent to stop and two want it to keep going. The
// scorer has to know which is which.
func TestOnlyInterruptionsWantTheAgentToYield(t *testing.T) {
	yielding := 0
	for _, category := range fdb.Categories() {
		if category.ShouldYield() {
			yielding++
			if category != fdb.Interruption {
				t.Fatalf("%q must not want the agent to yield", category)
			}
		}
	}
	if yielding != 1 {
		t.Fatalf("exactly one of the four conditions wants a yield, got %d", yielding)
	}
	if len(fdb.Categories()) != 4 {
		t.Fatalf("the suite has four conditions, got %d", len(fdb.Categories()))
	}
}
