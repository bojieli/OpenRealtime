package cognition

import (
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
)

// Cutting into somebody's sentence has no time to think in. Measured at
// fifteen repeats, giving the voice a reasoning budget gains three or four
// runs on counting, the visual case and the waiter, and loses seven on this
// one - where waiting until they finish makes the correction useless, and a
// voice that thinks for a second and a half has already waited.
func TestInterruptingLeavesNoTimeToThink(t *testing.T) {
	if got := effortFor("interrupt"); got != continuation.EffortMinimal {
		t.Fatalf("an interrupt was given room to think: %q", got)
	}
}

// Everything else keeps whatever the deployment configured, and empty is how
// this says "leave it alone" - a deployment that asked for no budget must not
// be given one here.
func TestEveryOtherActKeepsWhatTheDeploymentConfigured(t *testing.T) {
	for _, act := range []string{"", "answer", "speak-through", "act-silently"} {
		if got := effortFor(act); got != "" {
			t.Fatalf("%q had its budget overridden with %q", act, got)
		}
	}
}
