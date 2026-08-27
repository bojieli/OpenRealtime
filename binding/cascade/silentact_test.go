package cascade_test

import (
	"context"
	"sync"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/session"
)

// callingFloor asks for a tool call on every revision, which is what a model
// that has just seen a phone menu does.
type callingFloor struct{}

func (callingFloor) Name() string      { return "calling" }
func (callingFloor) EngineOwned() bool { return true }

func (callingFloor) Endpoint(decision interaction.Context) interaction.EndpointDecision {
	if decision.Revision.Empty() {
		return interaction.EndpointDecision{}
	}
	return interaction.EndpointDecision{Act: interaction.ActActSilently, Reason: "the menu named the option"}
}

func (callingFloor) Holder(session.Snapshot) interaction.Holder { return interaction.HolderNobody }

// Acting twice on one stretch of speech is acting twice for one reason, and a
// revision boundary is not a new reason. A recorded menu is one utterance that
// produces a revision every few hundred milliseconds, and one act per revision
// was nine key presses in a single call - a person doing that lands three
// menus deep.
func TestOneSilentActPerStretchOfSpeech(t *testing.T) {
	var mu sync.Mutex
	var acts []string
	model, err := interaction.NewInteractionModel(silentDecider{})
	if err != nil {
		t.Fatal(err)
	}
	policies := interaction.Defaults()
	policies.Floor = callingFloor{}
	// actSilently runs only where the interaction model owns the decision,
	// which is the arrangement this rule belongs to.
	policies.Interaction = model
	policies.ShadowInteraction = func(record interaction.ShadowDecision) {
		if record.Predicates["where"] != "interject" && record.Predicates["where"] != "act-silently" {
			return
		}
		mu.Lock()
		acts = append(acts, record.Act)
		mu.Unlock()
	}
	runtime, _ := startSession(t, cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) {
			return &scriptedASR{
				partials: []string{
					"press one for billing",
					"press one for billing press two for order status",
					"press one for billing press two for order status press three",
				},
				final: "press one for billing press two for order status press three",
			}, nil
		},
		Fast: newFast(), Slow: newSlow(), Policies: policies,
	}, binding.Settings{})

	for index := 0; index < 8; index++ {
		pushAudio(t, runtime, tone(2400, 8000), 1)
	}
	// A second utterance, which is what the recogniser produces every few
	// seconds out of one recorded menu - and what the prefix test alone
	// cannot tell from a genuinely new prompt.
	for index := 0; index < 8; index++ {
		pushAudio(t, runtime, tone(2400, 8000), 1)
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(acts) > 0
	}, "the act was never taken at all")
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	taken := 0
	for _, act := range acts {
		if act == "acted" || act == "refused" {
			taken++
		}
	}
	if taken > 1 {
		t.Fatalf("acted %d times in three seconds: %q", taken, acts)
	}
}

// silentDecider always stays silent; the floor above is what asks for the act.
type silentDecider struct{}

func (silentDecider) Name() string { return "silent" }

func (silentDecider) Decide(
	_ context.Context, request interaction.Decision,
) (interaction.Outcome, error) {
	for index, option := range request.Options {
		if option == string(interaction.ActStaySilent) {
			return interaction.Outcome{Index: index}, nil
		}
	}
	return interaction.Outcome{Index: 0}, nil
}
