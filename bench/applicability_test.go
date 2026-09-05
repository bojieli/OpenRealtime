package bench_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func TestSummarySeparatesExecutionFromApplicableBehavior(t *testing.T) {
	result := complete("applicability", 1, 3)
	result.Tasks[0].Applicability = bench.Applicable
	result.Tasks[1].Applicability = bench.Applicable
	result.Tasks[2].Applicability = bench.NotApplicable
	result.Tasks[2].Passed = false
	result.Finish()
	if !result.Summary.Complete || result.Summary.Completed != 3 || result.Summary.Failed != 0 ||
		result.Summary.NotApplicable != 1 || result.Summary.Passed != 1 || result.Summary.PassRate != 0.5 {
		t.Fatalf("inapplicable attempt distorted quality or execution: %+v", result.Summary)
	}
	if result.Summary.Distributions["first_audio_ms"].Count != 3 {
		t.Fatal("inapplicability erased execution measurements")
	}
}

func TestApplicabilityRejectsImpossibleOutcomeStates(t *testing.T) {
	for _, test := range []struct {
		name string
		out  bench.TaskOutcome
	}{
		{"unknown", bench.TaskOutcome{ID: "case", Completed: true, Applicability: "maybe"}},
		{"not executed", bench.TaskOutcome{ID: "case", Applicability: bench.NotApplicable}},
		{"unearned pass", bench.TaskOutcome{ID: "case", Completed: true, Passed: true, Applicability: bench.NotApplicable}},
		{"contradictory note", bench.TaskOutcome{ID: "case", Completed: true, Applicability: bench.NotApplicable, Notes: map[string]string{"applicable": "true"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.out.Validate(); err == nil {
				t.Fatalf("impossible applicability accepted: %+v", test.out)
			}
		})
	}
}

func TestHistoricalApplicabilityIsNotRewrittenDuringVerification(t *testing.T) {
	result := complete("historical", 2, 2)
	result.Tasks[1].Notes = map[string]string{"applicable": "false"}
	result.Finish()
	if result.Summary.Passed != 2 || result.Summary.NotApplicable != 0 {
		t.Fatalf("historical outcome was silently reinterpreted: %+v", result.Summary)
	}
	payload, err := json.Marshal(result.Summary)
	if err != nil || strings.Contains(string(payload), "not_applicable") {
		t.Fatalf("historical summary bytes gained a field: %s, %v", payload, err)
	}
}

func TestPairRefusesDifferentApplicablePopulations(t *testing.T) {
	baseline := complete("reference", 1, 3)
	variant := complete("variant", 1, 3)
	variant.Cell.Levels[bench.FactorCognition] = "fast-only"
	baseline.Tasks[1].Applicability = bench.NotApplicable
	variant.Tasks[2].Applicability = bench.NotApplicable
	baseline.Finish()
	variant.Finish()
	if got := bench.Pair(baseline, variant); got.Reportable || !strings.Contains(got.Refusal, "applicable task populations") {
		t.Fatalf("equal denominators hid different eligible cases: %+v", got)
	}
	variant.Tasks[1].Applicability = bench.NotApplicable
	variant.Tasks[2].Applicability = bench.Applicable
	variant.Finish()
	if got := bench.Pair(baseline, variant); !got.Reportable {
		t.Fatalf("same applicable population refused: %+v", got)
	}
}

func TestPairRefusesACompleteRunWithNoApplicableTasks(t *testing.T) {
	baseline := complete("reference", 0, 2)
	variant := complete("variant", 0, 2)
	variant.Cell.Levels[bench.FactorCognition] = "fast-only"
	for _, result := range []*bench.Result{&baseline, &variant} {
		for index := range result.Tasks {
			result.Tasks[index].Applicability = bench.NotApplicable
		}
		result.Finish()
		if !result.Summary.Complete || result.Summary.Completed != 2 || result.Summary.NotApplicable != 2 {
			t.Fatalf("fully executed run lost completion: %+v", result.Summary)
		}
	}
	if got := bench.Pair(baseline, variant); got.Reportable || !strings.Contains(got.Refusal, "no applicable tasks") {
		t.Fatalf("undefined rates were compared: %+v", got)
	}
}

func TestPairRefusesUnclassifiedHistoricalFDBScores(t *testing.T) {
	baseline := complete("reference", 1, 2)
	variant := complete("variant", 1, 2)
	baseline.Suite, variant.Suite = "fdb-v1.5", "fdb-v1.5"
	variant.Cell.Levels[bench.FactorCognition] = "fast-only"
	if got := bench.Pair(baseline, variant); got.Reportable || !strings.Contains(got.Refusal, "explicit applicability") {
		t.Fatalf("legacy FDB nominal passes were compared as quality: %+v", got)
	}
}
