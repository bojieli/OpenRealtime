package m4

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bojieli/OpenRealtime/adapters/reference"
	"github.com/bojieli/OpenRealtime/cognition"
)

func TestFastSlowStudyReportsTruthQualityCostAndLifecycle(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	workloadPath := filepath.Join(root, "tests", "fixtures", "m4-difficult-workload.json")
	workload, err := reference.LoadDifficultWorkload(workloadPath)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Config{WorkloadPath: workloadPath, Workload: workload, Trials: 5, Seed: 20260817})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Conditions) != 3 || len(report.SafetyScenarios) != 4 || !report.FabricatedProgressRejected {
		t.Fatalf("unexpected report shape: %+v", report)
	}
	conditions := make(map[ConditionKind]Condition)
	for _, condition := range report.Conditions {
		conditions[condition.Kind] = condition
		if condition.TruthViolationCount != 0 {
			t.Fatalf("condition %s has truth violations", condition.Kind)
		}
	}
	fastOnly := conditions[ConditionFastOnly]
	fastSlow := conditions[ConditionFastSlow]
	blocking := conditions[ConditionSingleBlocking]
	if fastSlow.FirstTruthfulProgress.P50NS >= blocking.FirstTruthfulProgress.P50NS {
		t.Fatal("fast/slow acknowledgement did not improve truthful progress timing")
	}
	if fastSlow.Quality.P50 != blocking.Quality.P50 || fastSlow.Quality.P50 <= fastOnly.Quality.P50 {
		t.Fatal("fast/slow quality comparison is inconsistent")
	}
	if fastSlow.Compute.P50 <= blocking.Compute.P50 {
		t.Fatal("fast/slow compute accounting omitted foreground cost")
	}
	for _, scenario := range report.SafetyScenarios {
		if !scenario.Closed || scenario.StaleAccepted {
			t.Fatalf("unsafe lifecycle scenario: %+v", scenario)
		}
		if scenario.Name == "stale_revision_rejected" && (scenario.FinalState != cognition.SlowCompleted || scenario.StaleRejectCount != 1) {
			t.Fatalf("stale scenario failed: %+v", scenario)
		}
	}
}
