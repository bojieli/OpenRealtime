package main

import (
	"slices"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func TestResolveRealtimeCUCellOverridesPairedReference(t *testing.T) {
	t.Parallel()
	baseline, err := resolveRealtimeCUCell("gemini-bounded-fast", "F10=bounded-fast", "", "")
	if err != nil {
		t.Fatal(err)
	}
	variant, err := resolveRealtimeCUCell(
		"local-vlm-bounded-fast", "F10=bounded-fast", "F9", "local-vlm-qwen2.5-vl-3b-bnb4",
	)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Levels[bench.FactorFastAction] != "bounded-fast" ||
		variant.Levels[bench.FactorFastAction] != "bounded-fast" {
		t.Fatalf("fast-action level was not fixed across the pair: %#v %#v", baseline, variant)
	}
	if baseline.Levels[bench.FactorFastModel] != "hosted-vision" ||
		variant.Levels[bench.FactorFastModel] != "local-vlm-qwen2.5-vl-3b-bnb4" {
		t.Fatalf("fast-model levels do not describe the pair: %#v %#v", baseline, variant)
	}
	if !slices.Equal(bench.Compare(baseline, variant), []bench.Factor{bench.FactorFastModel}) ||
		!slices.Equal(variant.Varies, []bench.Factor{bench.FactorFastModel}) {
		t.Fatalf("cells do not form an F9 pair: %#v %#v", baseline, variant)
	}
}

func TestResolveRealtimeCUCellRejectsInvalidReferenceLevels(t *testing.T) {
	t.Parallel()
	for _, levels := range []string{"F10", "F10=", "F99=value"} {
		if _, err := resolveRealtimeCUCell("", levels, "", ""); err == nil {
			t.Errorf("reference levels %q were accepted", levels)
		}
	}
}
