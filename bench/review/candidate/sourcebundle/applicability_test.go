package sourcebundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func TestSourceReviewPreservesApplicableLabelsThroughSealing(t *testing.T) {
	fixture := newSourceFixture(t)
	var outcomes []bench.TaskOutcome
	for _, state := range []string{"pass", "fail", "not_applicable"} {
		specification := fixture.attempt(t, state, 1, false)
		attempt := beginAttempt(t, fixture, specification)
		if err := attempt.CaptureAudio(fixtureCapture()); err != nil {
			t.Fatal(err)
		}
		outcome := fixtureOutcome(state, state == "pass")
		outcome.Applicability = bench.Applicable
		if state == "not_applicable" {
			outcome.Applicability = bench.NotApplicable
		}
		completeAttempt(t, attempt, specification, outcome)
		outcomes = append(outcomes, outcome)
	}
	if err := fixture.bundle.FinishSuite(t.Context(), fixtureResult(fixture, outcomes...)); err != nil {
		t.Fatal(err)
	}
	manifest, receipt, err := Verify(t.Context(), fixture.directory, fixture.receipt)
	if err != nil || receipt.AttemptCount != 3 || manifest.AttemptCount != 3 {
		t.Fatalf("reopen source: %+v, %v", receipt, err)
	}
	payload, err := os.ReadFile(filepath.Join(fixture.directory, reviewName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"| not_applicable | complete |", "1/2 applicable deterministic passes; 1 not applicable; 3 completed"} {
		if !strings.Contains(string(payload), want) {
			t.Fatalf("review missing %q:\n%s", want, payload)
		}
	}
}

func TestLegacySourceReviewKeepsItsOriginalNominalScore(t *testing.T) {
	fixture := newSourceFixture(t)
	specification := fixture.attempt(t, "legacy-not-applicable", 1, false)
	attempt := beginAttempt(t, fixture, specification)
	if err := attempt.CaptureAudio(fixtureCapture()); err != nil {
		t.Fatal(err)
	}
	outcome := fixtureOutcome(specification.Case, true)
	outcome.Notes["applicable"] = "false"
	completeAttempt(t, attempt, specification, outcome)
	if err := fixture.bundle.FinishSuite(t.Context(), fixtureResult(fixture, outcome)); err != nil {
		t.Fatal(err)
	}
	manifest, _, err := Verify(t.Context(), fixture.directory, fixture.receipt)
	if err != nil || !manifest.Attempts[0].Deterministic.Passed || manifest.Attempts[0].Deterministic.Applicability != "" {
		t.Fatalf("legacy outcome reinterpreted during verification: %+v, %v", manifest, err)
	}
	payload, err := os.ReadFile(filepath.Join(fixture.directory, reviewName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), "1/1 deterministic passes; 1 completed; bundle population 1.") ||
		!strings.Contains(string(payload), "| pass | complete |") {
		t.Fatalf("legacy review was rewritten: %s", payload)
	}
}
