package fdb

import (
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func TestScorerDoesNotAwardAPassWhenThereWasNothingToInterrupt(t *testing.T) {
	outcome := bench.TaskOutcome{ID: "user_interruption/1", Completed: true, Notes: map[string]string{"category": string(Interruption)}}
	scoreOutcome(&outcome, bench.Transcript{}, attemptContext{
		Category: Interruption, EventStartMS: 1000, EventEndMS: 2000,
		ShouldYield: true, YieldWindowMS: 1000, HoldWindowMS: 1000,
	})
	if outcome.Passed || outcome.Applicability != bench.NotApplicable || outcome.Notes["applicable"] != "false" {
		t.Fatalf("silence received overlap credit: %+v", outcome)
	}
	if err := outcome.Validate(); err != nil {
		t.Fatal(err)
	}
	result := bench.Result{Suite: "fdb-v1.5", Expected: 1, Tasks: []bench.TaskOutcome{outcome}}
	result.Finish()
	if result.Summary.Passed != 0 || result.Summary.NotApplicable != 1 || !result.Summary.Complete {
		t.Fatalf("generic summary mislabeled FDB silence: %+v", result.Summary)
	}
	if summary := Breakdown(result)[Interruption]; summary.Passed != 0 || summary.NotApplicable != 1 || summary.Applicable != 0 {
		t.Fatalf("category and generic summary disagree: %+v", summary)
	}
}
