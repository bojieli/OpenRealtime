package ablation

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/analysis"
	"github.com/bojieli/OpenRealtime/experiments/m2"
)

func TestRenderSelfContainedAblation(t *testing.T) {
	t.Parallel()
	report := m2.Report{Conditions: []m2.Condition{{
		Policy: m2.Policy{Name: "fixed_100ms", Kind: m2.PolicyFixed, CadenceNS: 100_000_000},
		Distributions: map[string]analysis.Distribution{
			"observed_latency_ns": {Count: 2, P50NS: 120_000_000, P95NS: 140_000_000},
			"baseline_latency_ns": {Count: 2, P50NS: 200_000_000},
		},
		SignedDistributions: map[string]analysis.SignedDistribution{
			"observed_minus_baseline_ns": {Count: 2, P50NS: -80_000_000},
		},
		PreparedPreEndpointCount: 1,
	}}}
	var output bytes.Buffer
	if err := Render(&output, report); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"<!doctype html>", "fixed_100ms", "120.000", "-80.000", "1 / 2"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("output does not contain %q", expected)
		}
	}
}
