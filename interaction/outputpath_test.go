package interaction_test

import (
	"testing"

	"github.com/bojieli/OpenRealtime/interaction"
)

// The reasoner's answer has an output path, or the rollout produces nothing.
//
// The slow provider is silent by construction: its answer is committed to the
// trajectory and reaches the client only through a fast turn that reads it.
// If nothing plans that turn, the plan is empty once slow commits, so nothing
// is spoken, nothing is sent, and no response is ever opened - a client that
// asked for one waits forever with no error anywhere.
//
// It shipped that way once, because the step that read the answer back was
// gated on an option the serve path never set, and -rollout
// endpointed-slow-only produced silence. There is no option now: a finished
// background result plans a fast turn in every rollout that runs slow at all,
// so the level cannot be configured into silence. That is also why it is
// worth a test - it is the third level of the measurement plan's cognition
// factor, and a comparison against a configuration that cannot answer is not
// a comparison.
func TestEveryRolloutThatDeliberatesCanSpeakTheResult(t *testing.T) {
	for _, level := range []string{"endpointed-slow-only", "fast+slow"} {
		// Built the way the serve path builds it: nothing about voicing declared.
		rollout, err := interaction.ParseRollout(level, interaction.RolloutOptions{})
		if err != nil {
			t.Fatalf("%s: parse: %v", level, err)
		}
		steps := rollout.Plan(interaction.RolloutInput{
			Cause: interaction.Cause{BackgroundResult: true},
		})
		if len(steps) != 1 || steps[0].Kind != interaction.StepFast {
			t.Fatalf("%s: a committed answer must have a turn that speaks it, got %+v", level, steps)
		}
	}
}

// And the level that never deliberates never leaves an answer unspoken,
// because it never produces one.
func TestFastOnlyNeverStrandsAResult(t *testing.T) {
	rollout, err := interaction.ParseRollout("fast-only", interaction.RolloutOptions{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if steps := rollout.Plan(interaction.RolloutInput{
		Cause: interaction.Cause{Escalated: true},
	}); len(steps) != 0 {
		t.Fatalf("fast-only must not deliberate, got %+v", steps)
	}
}
