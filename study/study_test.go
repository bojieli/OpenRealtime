package study

import (
	"reflect"
	"testing"
)

func TestBuildIsDeterministicAndKeepsComparabilityBoundaries(t *testing.T) {
	t.Parallel()
	first, err := Build("..")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build("..")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("study build is not deterministic")
	}
	if len(first.Conditions) < 10 || len(first.PairedEffects) != 2 || len(first.Claims) != 5 {
		t.Fatalf("unexpected study shape: %+v", first)
	}
	if first.Conditions[0].Status != "not_run" || first.Conditions[0].Family != "native" {
		t.Fatal("unavailable native comparison was not explicit")
	}
	for _, effect := range first.PairedEffects {
		if effect.Wins != effect.Samples || effect.MedianBootstrap95.UpperNS >= 0 {
			t.Fatalf("paired effect inconsistent with reference trials: %+v", effect)
		}
	}
}
