package main

import (
	"slices"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func TestResolveTauVoiceCellRecordsLevelsHeldFixedAcrossAPair(t *testing.T) {
	t.Parallel()
	baseline, err := resolveCellFrom(
		bench.Reference(), "sensevoice-baseline", "F12=sensevoice-small", "", "",
	)
	if err != nil {
		t.Fatal(err)
	}
	variant, err := resolveCellFrom(
		bench.Reference(), "interaction", "F12=sensevoice-small", "F8", "interaction",
	)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Levels[bench.FactorRecognizer] != "sensevoice-small" ||
		variant.Levels[bench.FactorRecognizer] != "sensevoice-small" {
		t.Fatalf("the recogniser must be fixed across the pair: %#v %#v", baseline, variant)
	}
	if !slices.Equal(bench.Compare(baseline, variant), []bench.Factor{bench.FactorPolicy}) ||
		!slices.Equal(variant.Varies, []bench.Factor{bench.FactorPolicy}) {
		t.Fatalf("the pair must vary only F8: %#v %#v", baseline, variant)
	}
}
