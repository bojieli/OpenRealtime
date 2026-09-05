package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func TestFDBSummaryReportsApplicableDenominatorAndUndefinedRate(t *testing.T) {
	result := bench.Result{Expected: 2, Tasks: []bench.TaskOutcome{
		{ID: "user_interruption/1", Completed: true, Passed: true, Applicability: bench.Applicable, Notes: map[string]string{"category": "user_interruption"}},
		{ID: "user_interruption/2", Completed: true, Applicability: bench.NotApplicable, Notes: map[string]string{"category": "user_interruption"}},
	}}
	result.Finish()
	var output bytes.Buffer
	writeFDBScores(&output, result)
	if !strings.Contains(output.String(), "1/1 applicable, 100.0% (1 not applicable)") {
		t.Fatalf("wrong applicable summary: %s", &output)
	}
	result.Tasks[0].Passed = false
	result.Tasks[0].Applicability = bench.NotApplicable
	result.Finish()
	output.Reset()
	writeFDBScores(&output, result)
	if !strings.Contains(output.String(), "0/0 applicable, unavailable (no applicable recordings) (2 not applicable)") || strings.Contains(output.String(), "%") {
		t.Fatalf("undefined rate printed as a score: %s", &output)
	}
}
