package m5

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bojieli/OpenRealtime/adapters/reference"
	openaiwire "github.com/bojieli/OpenRealtime/protocol/openai"
	"github.com/bojieli/OpenRealtime/rapidgame"
	"github.com/bojieli/OpenRealtime/translation"
)

func TestDemonstrationsReportAllRequiredMetricsAndConformantTraces(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	demonstrationPath := filepath.Join(root, "tests", "fixtures", "m5-demonstrations.json")
	demonstrations, err := reference.LoadDemonstrations(demonstrationPath)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Config{
		FixturePath:       filepath.Join(root, "tests", "fixtures", "m0-tone.wav"),
		DemonstrationPath: demonstrationPath, Demonstrations: demonstrations,
		Trials: 4, Seed: 20260817,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Translation.Conditions) != 3 || len(report.Game.Conditions) != 2 {
		t.Fatalf("unexpected report shape: %+v", report)
	}
	if report.Translation.ProtocolProfile != openaiwire.ProfileTranslation || report.Translation.ProtocolValidTraces != 12 {
		t.Fatal("translation protocol coverage was not recorded")
	}
	if report.Game.ProtocolProfile != openaiwire.ProfileRealtime || report.Game.ProtocolValidTraces != 8 {
		t.Fatal("game protocol coverage was not recorded")
	}
	translationConditions := make(map[translation.PolicyKind]TranslationCondition)
	for _, condition := range report.Translation.Conditions {
		translationConditions[condition.Policy] = condition
		if condition.MeanLag.Count == 0 || condition.Quality.Count == 0 || condition.Compute.Count == 0 || condition.Failures.Count == 0 {
			t.Fatalf("translation condition lacks required metrics: %+v", condition)
		}
	}
	if translationConditions[translation.PolicyStableIncrement].MeanLag.P50NS >= translationConditions[translation.PolicyEndpointed].MeanLag.P50NS {
		t.Fatal("incremental translation did not improve lag")
	}
	if translationConditions[translation.PolicyAggressive].FailureCount == 0 {
		t.Fatal("authored aggressive translation failures were not surfaced")
	}
	gameConditions := make(map[rapidgame.ConditionKind]GameCondition)
	for _, condition := range report.Game.Conditions {
		gameConditions[condition.Condition] = condition
		if condition.ReactionLatency.Count == 0 || condition.Quality.Count == 0 || condition.Compute.Count == 0 || condition.Failures.Count == 0 {
			t.Fatalf("game condition lacks required metrics: %+v", condition)
		}
	}
	if gameConditions[rapidgame.ConditionMicroturn].FailureCount >= gameConditions[rapidgame.ConditionEndpointed].FailureCount {
		t.Fatal("microturn game policy did not reduce deadline failures")
	}
}
