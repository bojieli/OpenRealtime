package frontier

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/analysis"
	m4experiment "github.com/bojieli/OpenRealtime/experiments/m4"
)

func TestRenderFrontier(t *testing.T) {
	t.Parallel()
	report := m4experiment.Report{Conditions: []m4experiment.Condition{{
		Kind:                  m4experiment.ConditionFastSlow,
		FirstTruthfulProgress: analysis.Distribution{Count: 2, P50NS: 35_000_000},
		FinalAnswer:           analysis.Distribution{Count: 2, P50NS: 400_000_000},
		Quality:               analysis.ScalarDistribution{Count: 2, P50: 90},
		Compute:               analysis.ScalarDistribution{Count: 2, P50: 100}, TaskSuccessCount: 2,
	}}}
	var output bytes.Buffer
	if err := Render(&output, report); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"<!doctype html>", "fast_slow", "35.000", "400.000", "2 / 2"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("output does not contain %q", expected)
		}
	}
}
