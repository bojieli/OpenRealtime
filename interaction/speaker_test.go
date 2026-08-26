package interaction_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// Everything that reached a microphone was labelled "user", which is a false
// statement rather than a simplification. Measured: two people in a room
// discussing the milk were reported to the decision layer as the user asking
// the agent about milk, and it answered them - inventing having added it to a
// list. Read the situation back and no model would do otherwise.
func TestAnObservationIsLabelledByWhoProducedIt(t *testing.T) {
	mic := trajectory.Item{
		Kind: trajectory.KindObservation, Content: "what is my balance",
		Producer:    trajectory.Producer{Phase: trajectory.PhaseUser},
		Observation: &trajectory.ObservationMeta{Observer: "voice", Source: "microphone", Authority: trajectory.AuthorityUser},
	}
	if got := interaction.SpeakerOf(mic); got != "user" {
		t.Fatalf("the person at the microphone is the user, got %q", got)
	}
	// A channel the runtime can tell apart is described as itself. This is
	// what a phone line's far end, or a second microphone, or a recogniser
	// that reports who spoke, all look like from here.
	far := mic
	far.Content = "press one for billing"
	far.Observation = &trajectory.ObservationMeta{Observer: "voice", Source: "far-end", Authority: trajectory.AuthorityUser}
	if got := interaction.SpeakerOf(far); got != "far-end" {
		t.Fatalf("a separate channel must be named, got %q", got)
	}
	lines := interaction.RecentLines([]trajectory.Item{mic, far}, 6)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "user: ") || !strings.HasPrefix(lines[1], "far-end: ") {
		t.Fatalf("the conversation window must say who said what: %q", lines)
	}
}
