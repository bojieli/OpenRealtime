package demonstrations

import (
	"bytes"
	"testing"

	"github.com/bojieli/OpenRealtime/analysis"
	m5experiment "github.com/bojieli/OpenRealtime/experiments/m5"
	"github.com/bojieli/OpenRealtime/rapidgame"
	"github.com/bojieli/OpenRealtime/translation"
)

func TestRender(t *testing.T) {
	t.Parallel()
	distribution := analysis.Distribution{Count: 1, P50NS: 50_000_000}
	scalar := analysis.ScalarDistribution{Count: 1, P50: 100}
	report := m5experiment.Report{
		Translation: m5experiment.TranslationStudy{Conditions: []m5experiment.TranslationCondition{{
			Policy: translation.PolicyStableIncrement, MeanLag: distribution,
			CompletionLag: distribution, Quality: scalar, Compute: scalar,
		}}},
		Game: m5experiment.GameStudy{Name: "signal_match", Conditions: []m5experiment.GameCondition{{
			Condition: rapidgame.ConditionMicroturn, ReactionLatency: distribution,
			Quality: scalar, Compute: scalar, Failures: scalar,
		}}},
	}
	var output bytes.Buffer
	if err := Render(&output, report); err != nil {
		t.Fatal(err)
	}
	if output.Len() == 0 {
		t.Fatal("empty visualization")
	}
}
