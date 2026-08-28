package cascade

import (
	"testing"

	"github.com/bojieli/OpenRealtime/interaction"
)

// An act chosen on the understanding that somebody else is talking cannot be
// vetoed for somebody else talking. Answering is the opposite case: it
// presumes they finished, so their carrying on is real evidence against it.
func TestOnlyTheActsThatCrossTheFloorSurviveBeingSpokenOver(t *testing.T) {
	across := []interaction.Act{
		interaction.ActSpeakThrough, interaction.ActInterrupt, interaction.ActActSilently,
	}
	for _, act := range across {
		if !acrossTheFloor(string(act)) {
			t.Fatalf("%q is chosen while they are talking, so it must survive them talking", act)
		}
	}
	waits := []interaction.Act{
		interaction.ActAnswer, interaction.ActStaySilent,
		interaction.ActKeepSpeaking, interaction.ActStopSpeaking,
	}
	for _, act := range waits {
		if acrossTheFloor(string(act)) {
			t.Fatalf("%q does not presume they are talking, so being overtaken still counts", act)
		}
	}
	// An unauthorized turn is not an interjection by default.
	if acrossTheFloor("") || acrossTheFloor("something else") {
		t.Fatal("an unrecognised reason was treated as crossing the floor")
	}
}
