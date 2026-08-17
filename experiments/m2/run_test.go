package m2

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bojieli/OpenRealtime/adapters/reference"
	"github.com/bojieli/OpenRealtime/internal/simtime"
)

func TestCadenceAblationReconcilesAndEventPolicyWinsReferenceFixture(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	manifest, err := reference.LoadManifest(filepath.Join(root, "tests", "fixtures", "m1-reference-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Config{
		FixturePath: filepath.Join(root, "tests", "fixtures", "m0-tone.wav"),
		Manifest:    manifest, Trials: 5, Seed: 20260817, FrameMS: 20,
		Timing: simtime.DefaultModel(), Policies: DefaultPolicies(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Conditions) != len(DefaultPolicies()) {
		t.Fatalf("unexpected condition count: %d", len(report.Conditions))
	}
	var eventP50, fixed100P50 uint64
	for _, condition := range report.Conditions {
		if condition.Distributions["reconciliation_error_ns"].MaxNS != 0 {
			t.Fatalf("condition %s failed reconciliation", condition.Policy.Name)
		}
		for _, trial := range condition.Trials {
			if trial.ObservedMinusBaselineNS != -trial.PlanningOverlapNS {
				t.Fatalf("trial does not reconcile: %+v", trial)
			}
			if trial.Counters.PlanningJobs == 0 || len(trial.Actions) == 0 {
				t.Fatalf("trial is missing planning evidence: %+v", trial)
			}
		}
		switch condition.Policy.Name {
		case "revision_event":
			eventP50 = condition.Distributions["observed_latency_ns"].P50NS
		case "fixed_100ms":
			fixed100P50 = condition.Distributions["observed_latency_ns"].P50NS
		}
	}
	if eventP50 == 0 || fixed100P50 == 0 || eventP50 >= fixed100P50 {
		t.Fatalf("reference event policy P50 %d did not beat fixed 100 ms P50 %d", eventP50, fixed100P50)
	}
}

func TestPolicyValidation(t *testing.T) {
	t.Parallel()
	if err := validatePolicies([]Policy{{Name: "bad", Kind: PolicyFixed}}); err == nil {
		t.Fatal("zero fixed cadence must fail")
	}
	if err := validatePolicies([]Policy{{Name: "same", Kind: PolicyEventDriven}, {Name: "same", Kind: PolicyEventDriven}}); err == nil {
		t.Fatal("duplicate policy names must fail")
	}
}

func TestSlowPlanningRejectsStaleRevisionResults(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	manifest, err := reference.LoadManifest(filepath.Join(root, "tests", "fixtures", "m1-reference-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	timing := simtime.DefaultModel()
	timing.Cognition = simtime.Delay{BaseNS: 400_000_000}
	report, err := Run(context.Background(), Config{
		FixturePath: filepath.Join(root, "tests", "fixtures", "m0-tone.wav"),
		Manifest:    manifest, Trials: 1, Seed: 5, FrameMS: 20, Timing: timing,
		Policies: []Policy{{Name: "revision_event", Kind: PolicyEventDriven}},
	})
	if err != nil {
		t.Fatal(err)
	}
	trial := report.Conditions[0].Trials[0]
	if trial.Counters.StaleResults < 2 || trial.Counters.Cancelled < 2 {
		t.Fatalf("stale results were not rejected: %+v", trial.Counters)
	}
	if trial.ReconciliationErrorNS != 0 {
		t.Fatalf("slow-planning trial failed reconciliation: %+v", trial)
	}
}
