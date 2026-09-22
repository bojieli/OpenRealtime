package interaction_test

import (
	"testing"

	"github.com/bojieli/OpenRealtime/interaction"
)

func TestAcousticProjectionEndsOrHoldsOnlyOnEvidence(t *testing.T) {
	t.Parallel()
	projection, err := interaction.NewAcousticProjection(0.6)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Project(interaction.Context{}); got.Ending || got.Continuing {
		t.Fatalf("no evidence must leave silence in charge, got %+v", got)
	}
	ending := projection.Project(interaction.Context{AcousticEndpoint: &interaction.AcousticEndpoint{Probability: 0.6, Model: "smart-turn"}})
	if !ending.Ending || ending.Continuing {
		t.Fatalf("P at the threshold must end the turn, got %+v", ending)
	}
	holding := projection.Project(interaction.Context{AcousticEndpoint: &interaction.AcousticEndpoint{Probability: 0.2}})
	if holding.Ending || !holding.Continuing {
		t.Fatalf("P below the threshold must hold the turn, got %+v", holding)
	}
	for _, threshold := range []float64{0, 1, -0.1, 1.5} {
		if _, err := interaction.NewAcousticProjection(threshold); err == nil {
			t.Errorf("threshold %v was accepted", threshold)
		}
	}
}

// Through the engine floor: evidence of a finished turn ends a pause before
// the silence threshold, evidence of an unfinished one holds past it but no
// further than the projection hold.
func TestTheEngineFloorActsOnAcousticEvidenceWithinItsBound(t *testing.T) {
	t.Parallel()
	projection, err := interaction.NewAcousticProjection(0.5)
	if err != nil {
		t.Fatal(err)
	}
	floor := interaction.NewEngineFloor(interaction.EngineFloorOptions{Projection: projection})
	const ms = uint64(1_000_000)
	pause := func(silence uint64, probability float64) interaction.EndpointDecision {
		context := interaction.Context{
			NowNS:            10_000 * ms,
			Revision:         interaction.Revision{ID: 1, StableText: "I would like to", SilenceNS: silence},
			AcousticEndpoint: &interaction.AcousticEndpoint{Probability: probability},
		}
		context.Duplex.UserSpeechStartedNS = 5_000 * ms
		return floor.Endpoint(context)
	}
	if decision := pause(200*ms, 0.9); !decision.Ended {
		t.Fatalf("a finished-sounding pause at 200 ms must end, got %+v", decision)
	}
	if decision := pause(700*ms, 0.1); decision.Ended || decision.ReconsiderAfter <= 0 {
		t.Fatalf("an unfinished-sounding pause past the silence threshold must be held with a retry, got %+v", decision)
	}
	if decision := pause(1_600*ms, 0.1); !decision.Ended {
		t.Fatalf("the hold is bounded by silence plus the projection hold, got %+v", decision)
	}
}
