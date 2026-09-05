package releasevalidation

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func newApplicableBehavioralFixture(t *testing.T) behavioralFixture {
	t.Helper()
	fixture := newBehavioralFixture(t)
	fixture.result.Suite = "fdb-v1.5"
	target := &fixture.targets.Suites[0]
	target.Suite = fixture.result.Suite
	target.Aggregate.MinimumApplicable = intPointer(2)
	for index := range target.Cases.Targets {
		target.Cases.Targets[index].MinimumApplicable = intPointer(1)
		fixture.result.Tasks[index].Applicability = bench.Applicable
	}
	fixture.result.Finish()
	return fixture
}

func TestBehavioralApplicabilityFloorsPreserveFailedCaseExposure(t *testing.T) {
	for _, domain := range []string{"aggregate", "case"} {
		t.Run(domain, func(t *testing.T) {
			fixture := newApplicableBehavioralFixture(t)
			// Exercise each floor independently. The failed case keeps its zero
			// pass floor: only the loss of exposure must make acceptance fail.
			if domain == "aggregate" {
				fixture.targets.Suites[0].Cases.Targets[1].MinimumApplicable = intPointer(0)
			} else {
				fixture.targets.Suites[0].Aggregate.MinimumApplicable = intPointer(1)
			}
			if report := fixture.evaluate(t); !report.Accepted {
				t.Fatalf("applicable reference rejected: %+v", report)
			}
			fixture.result.Tasks[1].Applicability = bench.NotApplicable
			fixture.result.Finish()
			report := fixture.evaluate(t)
			if report.Accepted || !strings.Contains(strings.Join(report.Failures, "\n"), "applicable attempts, below preregistered minimum") {
				t.Fatalf("silencing a failure improved acceptance: %+v", report)
			}
			if report.Suites[0].Passed != 1 || report.Suites[0].NotApplicable != 1 || report.Suites[0].Completed != 2 {
				t.Fatalf("execution and behavioral counts conflated: %+v", report.Suites[0])
			}
			found := false
			for _, check := range report.Suites[0].Checks {
				if check.Domain == domain && check.Metric == "applicable" && !check.Passed {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing failed %s applicability check: %+v", domain, report)
			}
		})
	}
}

func TestBehavioralAcceptanceRetainsPermittedNotApplicableRecording(t *testing.T) {
	fixture := newApplicableBehavioralFixture(t)
	fixture.targets.Suites[0].Aggregate.MinimumApplicable = intPointer(1)
	fixture.targets.Suites[0].Cases.Targets[1].MinimumApplicable = intPointer(0)
	fixture.result.Tasks[1].Applicability = bench.NotApplicable
	fixture.result.Finish()
	if report := fixture.evaluate(t); !report.Accepted || report.Suites[0].NotApplicable != 1 {
		t.Fatalf("permitted not-applicable recording was discarded: %+v", report)
	}
}

func TestBehavioralAcceptanceRefusesMissingOrForgedApplicability(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*behavioralFixture)
		want   string
	}{
		{"legacy FDB", func(fixture *behavioralFixture) {
			fixture.result.Tasks[1].Applicability = ""
		}, "lacks explicit applicability"},
		{"forged summary", func(fixture *behavioralFixture) {
			fixture.result.Summary.NotApplicable++
		}, "stored summary differs"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newApplicableBehavioralFixture(t)
			test.mutate(&fixture)
			report := fixture.evaluate(t)
			if report.Accepted || !strings.Contains(strings.Join(report.Failures, "\n"), test.want) {
				t.Fatalf("missing failure %q: %+v", test.want, report)
			}
		})
	}
}

func TestFDBTargetsRequireBoundedExplicitApplicability(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*BehavioralSuiteTarget)
	}{
		{"missing aggregate", func(target *BehavioralSuiteTarget) { target.Aggregate.MinimumApplicable = nil }},
		{"empty aggregate", func(target *BehavioralSuiteTarget) { target.Aggregate.MinimumApplicable = intPointer(0) }},
		{"negative aggregate", func(target *BehavioralSuiteTarget) { target.Aggregate.MinimumApplicable = intPointer(-1) }},
		{"oversized aggregate", func(target *BehavioralSuiteTarget) { target.Aggregate.MinimumApplicable = intPointer(3) }},
		{"missing case", func(target *BehavioralSuiteTarget) { target.Cases.Targets[0].MinimumApplicable = nil }},
		{"negative case", func(target *BehavioralSuiteTarget) { target.Cases.Targets[0].MinimumApplicable = intPointer(-1) }},
		{"oversized case", func(target *BehavioralSuiteTarget) { target.Cases.Targets[0].MinimumApplicable = intPointer(2) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			targets := newApplicableBehavioralFixture(t).targets
			test.mutate(&targets.Suites[0])
			if err := targets.Validate(); err == nil || !strings.Contains(err.Error(), "applicable") {
				t.Fatalf("invalid applicability target accepted: %v", err)
			}
		})
	}
}

func TestCheckedFDBFloorsTranslateHistoricalEvidenceWithoutLosingExposure(t *testing.T) {
	targets, _, err := LoadBehavioralTargets(filepath.Join("..", "..", "scripts", "behavioral-acceptance-targets.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets.Suites {
		if target.Suite != "fdb-v1.5" {
			continue
		}
		if target.Aggregate.MinimumPassed == nil || *target.Aggregate.MinimumPassed != 287 ||
			target.Aggregate.MinimumApplicable == nil || *target.Aggregate.MinimumApplicable != 430 || len(target.Cases.Targets) != 498 {
			t.Fatalf("FDB aggregate does not preserve corrected historical floor: %+v", target.Aggregate)
		}
		passes, applicable, exposedFailures, interruptionPasses, interruptionApplicable := 0, 0, 0, 0, 0
		for _, row := range target.Cases.Targets {
			if row.ExpectedAttempts != 1 || row.MinimumApplicable == nil || row.MinimumPassed > *row.MinimumApplicable {
				t.Fatalf("FDB case has an unearned pass or lost applicability: %+v", row)
			}
			passes += row.MinimumPassed
			applicable += *row.MinimumApplicable
			if row.MinimumPassed == 0 && *row.MinimumApplicable == 1 {
				exposedFailures++
			}
			if strings.HasPrefix(row.Case, "user_interruption/") {
				interruptionPasses += row.MinimumPassed
				interruptionApplicable += *row.MinimumApplicable
			}
		}
		if passes != 287 || applicable != 430 || exposedFailures != 143 || interruptionPasses != 15 || interruptionApplicable != 156 {
			t.Fatalf("FDB pass/exposure floors drifted: %d/%d, %d failures, interruption %d/%d", passes, applicable, exposedFailures, interruptionPasses, interruptionApplicable)
		}
		return
	}
	t.Fatal("FDB target missing")
}
