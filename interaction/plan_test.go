package interaction_test

import (
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/interaction"
)

func TestEveryInteractionActHasExplicitFloorSemantics(t *testing.T) {
	want := map[interaction.Act]interaction.FloorSemantics{
		interaction.ActStaySilent:   interaction.FloorUnchanged,
		interaction.ActSpeakThrough: interaction.FloorPreserve,
		interaction.ActAnswer:       interaction.FloorTake,
		interaction.ActInterrupt:    interaction.FloorTake,
		interaction.ActActSilently:  interaction.FloorUnchanged,
		interaction.ActKeepSpeaking: interaction.FloorUnchanged,
		interaction.ActStopSpeaking: interaction.FloorYield,
	}
	for act, floor := range want {
		plan, err := interaction.NewPlan(act, "test-policy", "revision:1", time.Now(), interaction.Outcome{Option: string(act), Confidence: .8})
		if err != nil {
			t.Fatalf("%s: %v", act, err)
		}
		if plan.Floor != floor {
			t.Fatalf("%s: got floor %s, want %s", act, plan.Floor, floor)
		}
	}
}

func TestAPlanCannotLieAboutItsFloorMeaning(t *testing.T) {
	plan := interaction.Plan{Act: interaction.ActSpeakThrough, Floor: interaction.FloorTake}
	if err := plan.Validate(); err == nil {
		t.Fatal("speak-through taking the floor must be rejected")
	}
}
