package main

import "testing"

func TestSampleAttemptStatePreservesLifetimeBudget(t *testing.T) {
	t.Parallel()
	remaining, succeeded, err := sampleAttemptState([]attempt{
		{Sample: "target", Number: 1},
		{Sample: "other", Number: 1, Succeeded: true},
		{Sample: "target", Number: 2},
	}, "target", 3)
	if err != nil || succeeded || len(remaining) != 1 || remaining[0] != 3 {
		t.Fatalf("remaining=%v succeeded=%v err=%v", remaining, succeeded, err)
	}
}

func TestSampleAttemptStateRejectsDuplicateNumber(t *testing.T) {
	t.Parallel()
	_, _, err := sampleAttemptState([]attempt{
		{Sample: "target", Number: 1},
		{Sample: "target", Number: 1},
	}, "target", 3)
	if err == nil {
		t.Fatal("expected duplicate attempt number to be rejected")
	}
}

func TestValidatePriorRejectsChangedAttemptBudget(t *testing.T) {
	t.Parallel()
	prior := runManifest{SchemaVersion: "1", Benchmark: "benchmark", Revision: "revision", TrialAttempts: 2}
	planned := prior
	planned.TrialAttempts = 3
	if err := validatePrior(prior, planned); err == nil {
		t.Fatal("expected changed attempt budget to be rejected")
	}
}
