package main

import (
	"strings"
	"testing"
)

// An acoustic end-of-turn classifier is consulted in either mode, but only
// control mode lets it end or hold a turn, and it cannot share that job with
// a second projection.
func TestAcousticEndOfTurnControlsTheFloorOnlyWhenSelected(t *testing.T) {
	options := defaultOptions()
	options.turnEndURL = "http://127.0.0.1:9130/v1/endpoint/smart-turn"
	observed, err := buildPolicies(options, nil)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if strings.Contains(observed.Floor.Name(), "acoustic-endpoint") {
		t.Fatalf("observe mode installed the classifier on the floor: %q", observed.Floor.Name())
	}

	options.turnEndMode = "control"
	options.turnEndThreshold = 0.6
	controlled, err := buildPolicies(options, nil)
	if err != nil {
		t.Fatalf("control: %v", err)
	}
	if controlled.TurnProjection.Name() != "acoustic-endpoint@0.60" ||
		!strings.Contains(controlled.Floor.Name(), "acoustic-endpoint@0.60") {
		t.Fatalf("control mode must install and consult the acoustic projection, floor=%q", controlled.Floor.Name())
	}

	options.policies, options.policyModel = "turn-projection", "qwen-3b"
	if _, err := buildPolicies(options, nil); err == nil || !strings.Contains(err.Error(), "parallel endpoint controllers") {
		t.Fatalf("two projections were accepted: %v", err)
	}

	options = defaultOptions()
	options.turnEndMode = "control"
	if _, err := buildPolicies(options, nil); err == nil {
		t.Fatal("control mode without a classifier was accepted")
	}
	options.turnEndMode = "decide"
	if _, err := buildPolicies(options, nil); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
}
