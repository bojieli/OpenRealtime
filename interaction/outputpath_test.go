package interaction_test

import (
	"testing"

	"github.com/bojieli/OpenRealtime/interaction"
)

// The reasoner's answer has an output path, or the rollout produces nothing.
//
// This rollout's slow provider is silent by construction: its answer is
// committed to the trajectory and reaches the client only through the fast
// step that reads it back. With that step disabled the plan is empty once slow
// commits, so nothing is spoken, nothing is sent, and no response is ever
// opened - a client that asked for one waits forever with no error anywhere.
//
// It shipped that way. The serve path builds RolloutOptions with only
// ToolResultProgress set, so -rollout endpointed-slow-only left VoiceSlowOutput
// false and the level produced silence. It is also the third level of the
// measurement plan's cognition factor, so any comparison against it would have
// been measuring a configuration that could not answer.
func TestTheEndpointedRolloutAlwaysHasAnOutputPath(t *testing.T) {
	// Built the way the serve path builds it: nothing about voicing declared.
	rollout, err := interaction.ParseRollout("endpointed-slow-only", interaction.RolloutOptions{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	steps := rollout.Plan(interaction.RolloutInput{
		Cause: interaction.Cause{SlowCommitted: true},
	})
	if len(steps) != 1 || steps[0].Kind != interaction.StepVoice {
		t.Fatalf("a committed answer must have something to voice it, got %+v", steps)
	}
}

// And the reference rollout keeps the same property, which it always had.
func TestTheFastThenSlowRolloutVoicesWhatSlowCommitted(t *testing.T) {
	rollout, err := interaction.ParseRollout("fast+slow", interaction.RolloutOptions{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	steps := rollout.Plan(interaction.RolloutInput{
		Cause: interaction.Cause{SlowCommitted: true},
	})
	if len(steps) != 1 || steps[0].Kind != interaction.StepVoice {
		t.Fatalf("got %+v", steps)
	}
}
