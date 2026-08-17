package simtime

import "testing"

func TestSampleIsDeterministicAndRejectsInvalidJitter(t *testing.T) {
	t.Parallel()
	model := DefaultModel()
	first, err := Sample(model, 42)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Sample(model, 42)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first.Stages.Sum() == 0 {
		t.Fatalf("unexpected deterministic samples: %+v %+v", first, second)
	}
	model.Cognition = Delay{BaseNS: 1, JitterNS: 2}
	if _, err := Sample(model, 42); err == nil {
		t.Fatal("expected jitter larger than base to fail")
	}
}
