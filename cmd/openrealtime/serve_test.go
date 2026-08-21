package main

import (
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/interaction"
)

// A flag that parses and then does nothing is the failure mode this file
// exists for. Each of these asserts that a configured level reaches the policy
// set as a different policy, by name - a level that produced the same name as
// another level would be measuring nothing.

func defaultOptions() serveOptions {
	return serveOptions{
		rollout: "fast+slow", cadence: interaction.DefaultCadence,
		preparation: "endpoint-only", preparationPace: time.Second,
		bargeIn: "immediate", bargeInHold: 300 * time.Millisecond,
		observation: "endpoint-only", observers: "audio", policies: "none",
		narrator: "session", fastProvider: "openai-compatible",
		visionTokenEnv: "OPENREALTIME_VISION_API_KEY",
	}
}

func TestEveryRolloutLevelIsADistinctPolicy(t *testing.T) {
	seen := make(map[string]string)
	for _, level := range []string{"fast-only", "fast+slow", "endpointed-slow-only"} {
		options := defaultOptions()
		options.rollout = level
		policies, err := buildPolicies(options, nil)
		if err != nil {
			t.Fatalf("%s: %v", level, err)
		}
		name := policies.Rollout.Name()
		if previous, duplicate := seen[name]; duplicate {
			t.Fatalf("levels %q and %q are the same policy %q", previous, level, name)
		}
		seen[name] = level
	}
	options := defaultOptions()
	options.rollout = "telepathy"
	if _, err := buildPolicies(options, nil); err == nil {
		t.Fatal("an unknown rollout must be refused rather than silently defaulted")
	}
}

func TestEveryBargeInLevelIsADistinctPolicy(t *testing.T) {
	seen := make(map[string]string)
	for _, level := range []string{"immediate", "sustained", "never"} {
		options := defaultOptions()
		options.bargeIn = level
		policies, err := buildPolicies(options, nil)
		if err != nil {
			t.Fatalf("%s: %v", level, err)
		}
		name := policies.BargeIn.Name()
		if previous, duplicate := seen[name]; duplicate {
			t.Fatalf("levels %q and %q are the same policy %q", previous, level, name)
		}
		seen[name] = level
	}
	options := defaultOptions()
	options.bargeIn = "eventually"
	if _, err := buildPolicies(options, nil); err == nil {
		t.Fatal("an unknown barge-in level must be refused")
	}
}

// Preparation now really starts continuations, so the two levels have to be
// two things and the default has to be the one that spends nothing.
func TestPreparationDefaultsToOffAndIsSelectable(t *testing.T) {
	policies, err := buildPolicies(defaultOptions(), nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if policies.Preparation.Name() != "endpoint-only" {
		t.Fatalf("the default must speculate nothing, got %q", policies.Preparation.Name())
	}
	options := defaultOptions()
	options.preparation = "continuous"
	policies, err = buildPolicies(options, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.HasPrefix(policies.Preparation.Name(), "continuous") {
		t.Fatalf("continuous preparation must be selectable, got %q", policies.Preparation.Name())
	}
	options.preparation = "sometimes"
	if _, err := buildPolicies(options, nil); err == nil {
		t.Fatal("an unknown preparation level must be refused")
	}
}

// The trigger cadence is factor F4, so every level must reach the policy.
func TestTriggerCadenceReachesThePolicy(t *testing.T) {
	for _, cadence := range []time.Duration{
		50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond,
		400 * time.Millisecond, 800 * time.Millisecond,
	} {
		options := defaultOptions()
		options.cadence = cadence
		policies, err := buildPolicies(options, nil)
		if err != nil {
			t.Fatalf("%s: %v", cadence, err)
		}
		if !strings.Contains(policies.Trigger.Name(), cadence.String()[:len(cadence.String())-2]) {
			t.Fatalf("cadence %s did not reach the trigger, which reports %q", cadence, policies.Trigger.Name())
		}
	}
}

// The stable-partial observation policy answers before the endpoint, so the
// deferral policy has to be willing to act before the endpoint. Composing the
// pair here is what keeps the flag usable; the binding refuses the incoherent
// one outright.
func TestStablePartialComposesADeferralThatCanActOnAPartial(t *testing.T) {
	options := defaultOptions()
	policies, err := buildPolicies(options, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if policies.Deferral.Name() != "duplex" {
		t.Fatalf("endpoint-only observation keeps the shipped deferral, got %q", policies.Deferral.Name())
	}

	options.observation = "stable-partial"
	policies, err = buildPolicies(options, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(policies.Deferral.Name(), "allow-over-user") {
		t.Fatalf("stable-partial needs a deferral that admits a partial, got %q", policies.Deferral.Name())
	}

	options.observation = "eventually"
	if _, err := buildPolicies(options, nil); err == nil {
		t.Fatal("an unknown observation policy must be refused")
	}
}

// Enabling a policy model without naming one is a configuration that cannot
// work, and it has to say so at startup rather than at the first decision.
func TestPolicyModelsNeedAModel(t *testing.T) {
	options := defaultOptions()
	options.policies = "backchannel"
	if _, err := buildPolicies(options, nil); err == nil {
		t.Fatal("enabling a policy model with no model must be refused")
	}

	options.policyModel = "qwen-3b"
	policies, err := buildPolicies(options, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if policies.Backchannel.Name() == "off" {
		t.Fatal("a configured backchannel policy model must replace the rule fallback")
	}

	options.policies = "everything"
	if _, err := buildPolicies(options, nil); err == nil {
		t.Fatal("an unknown policy model name must be refused")
	}
}

// Turn projection reaches the conversation through the floor, so installing
// one without rebuilding the floor would leave it unused - the silent no-op a
// measured factor must never be.
func TestTurnProjectionRebuildsTheFloor(t *testing.T) {
	options := defaultOptions()
	options.policies = "turn-projection"
	options.policyModel = "qwen-3b"
	policies, err := buildPolicies(options, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if policies.TurnProjection.Name() == "vad-only" {
		t.Fatal("the projection policy was not installed")
	}
	if !strings.Contains(policies.Floor.Name(), policies.TurnProjection.Name()) {
		t.Fatalf("the floor must consult the projection it was given, floor=%q projection=%q",
			policies.Floor.Name(), policies.TurnProjection.Name())
	}
}

// Factor F3's video-only level has to remove the recogniser, not just add a
// video observer alongside it.
func TestVideoOnlySelectsAnObserverSetWithoutAudio(t *testing.T) {
	for _, test := range []struct {
		configured string
		expected   []string
	}{
		{configured: "audio"},
		{configured: "audio+video"},
		{configured: "video", expected: []string{"video"}},
	} {
		selection, err := defaultObserverSet(test.configured)
		if err != nil {
			t.Fatalf("%s: %v", test.configured, err)
		}
		if strings.Join(selection, ",") != strings.Join(test.expected, ",") {
			t.Fatalf("%s selected %v, expected %v", test.configured, selection, test.expected)
		}
	}
	if _, err := defaultObserverSet("lidar"); err == nil {
		t.Fatal("an unknown observer set must be refused")
	}
}

// The governor is off unless a deployment states a capacity, because the unit
// is abstract and a made-up number would throttle a healthy deployment.
func TestTheGovernorIsOffUntilACapacityIsStated(t *testing.T) {
	governor, err := buildGovernor(defaultOptions())
	if err != nil || governor != nil {
		t.Fatalf("expected no governor, got %v err=%v", governor, err)
	}
	options := defaultOptions()
	options.gpuCapacity = 8
	governor, err = buildGovernor(options)
	if err != nil || governor == nil {
		t.Fatalf("expected a governor, got %v err=%v", governor, err)
	}
	snapshot := governor.Snapshot()
	if snapshot.Capacity != 8 || snapshot.ReservedInteractive <= 0 {
		t.Fatalf("the governor must reserve capacity for interactive work: %+v", snapshot)
	}
}

func TestServeRejectsFlagsItCannotHonour(t *testing.T) {
	var output strings.Builder
	for _, arguments := range [][]string{
		{"-binding", "telepathy"},
		{"-rollout", "sideways"},
		{"positional-argument"},
	} {
		if err := runServe(arguments, &output); err == nil {
			t.Fatalf("expected %v to be refused", arguments)
		}
	}
}

// Factor F7's narrator composition was a label: both levels asked for
// -vision-model and both used whatever it named, so a report could say
// "session" about a separate model nobody in the session was using.
func TestNarratorCompositionNamesTheModelThatActuallyNarrates(t *testing.T) {
	options := defaultOptions()
	options.narrator = "session"
	options.fastURL = "http://127.0.0.1:8000/v1"
	options.fastModel = "qwen-vl"
	options.fastTokenEnv = "FAST_KEY"
	options.fastProvider = "openai-compatible"
	options.visionModel = "some-other-model"

	// A session narrator is the session's own model, so it must not quietly
	// use the dedicated one that happens to be configured beside it.
	options.fastVision = true
	label, url, model, tokenEnv, err := narratorComposition(options)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if label != "session" || model != "qwen-vl" || url != options.fastURL || tokenEnv != "FAST_KEY" {
		t.Fatalf("a session narrator is the session's own model: %q %q %q %q", label, url, model, tokenEnv)
	}

	// And a model that cannot see is not narrating anything, so asking for it
	// is refused rather than silently producing nothing.
	options.fastVision = false
	if _, _, _, _, err := narratorComposition(options); err == nil {
		t.Fatal("a text-only fast model cannot be the session narrator")
	}

	options.narrator = "dedicated"
	label, url, model, tokenEnv, err = narratorComposition(options)
	if err != nil {
		t.Fatalf("dedicated: %v", err)
	}
	if label != "dedicated" || model != "some-other-model" || tokenEnv != options.visionTokenEnv {
		t.Fatalf("a dedicated narrator is the one named: %q %q %q %q", label, url, model, tokenEnv)
	}

	options.visionModel = ""
	if _, _, _, _, err := narratorComposition(options); err == nil {
		t.Fatal("a dedicated narrator with no model must be refused")
	}
	options.narrator = "telepathy"
	if _, _, _, _, err := narratorComposition(options); err == nil {
		t.Fatal("an unknown narrator composition must be refused")
	}
}
