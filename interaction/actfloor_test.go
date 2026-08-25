package interaction_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/interaction"
)

// answers a fixed act, or fails.
type answers struct {
	act  interaction.Act
	fail bool
}

func (decider answers) Name() string { return "fake" }
func (decider answers) Decide(context.Context, interaction.Decision) (interaction.Outcome, error) {
	if decider.fail {
		return interaction.Outcome{}, errors.New("endpoint unreachable")
	}
	return interaction.Outcome{Option: string(decider.act)}, nil
}

func floorFor(t *testing.T, act interaction.Act, fail bool) interaction.Floor {
	t.Helper()
	model, err := interaction.NewInteractionModel(answers{act: act, fail: fail})
	if err != nil {
		t.Fatal(err)
	}
	floor, err := interaction.NewActFloor(model, interaction.ActFloorOptions{
		SilenceDuration: 500 * time.Millisecond, Liveness: 20 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return floor
}

func waiting(silence time.Duration, revision uint64) interaction.Context {
	state := interaction.Situation{Heard: "so I was thinking that we could", Silence: silence.String()}
	return interaction.Context{
		Revision:  interaction.Revision{ID: revision, StableText: "so I was thinking that we could", SilenceNS: uint64(silence)},
		Situation: &state,
	}
}

// Only two acts end a turn. Every other one leaves it open, including the ones
// that produce speech: speaking through somebody is speech that does not end
// their turn.
func TestActFloorEndsOnlyOnAnsweringOrInterrupting(t *testing.T) {
	for _, act := range []interaction.Act{interaction.ActAnswer, interaction.ActInterrupt} {
		if verdict := floorFor(t, act, false).Endpoint(waiting(700*time.Millisecond, 1)); !verdict.Ended {
			t.Fatalf("%s did not end the turn", act)
		}
	}
	for _, act := range []interaction.Act{interaction.ActStaySilent, interaction.ActSpeakThrough} {
		if verdict := floorFor(t, act, false).Endpoint(waiting(3*time.Second, 1)); verdict.Ended {
			t.Fatalf("%s ended the turn after three seconds of silence, which is the point of this floor", act)
		}
	}
	// call-tool has to be reachable to be tested: an act the situation does
	// not offer is rejected before it can decide anything, which is itself the
	// behaviour the next case checks.
	armed := waiting(3*time.Second, 1)
	armed.Situation.Tools = []string{"press_key(digit)"}
	if verdict := floorFor(t, interaction.ActCallTool, false).Endpoint(armed); verdict.Ended {
		t.Fatal("acting silently ended the turn, though nothing was said into it")
	}
}

// An act the situation does not offer cannot be acted on, and falling back is
// what stops a model naming an impossible one from deciding anything.
func TestActFloorRejectsAnActThatIsNotAvailable(t *testing.T) {
	floor := floorFor(t, interaction.ActCallTool, false)
	if verdict := floor.Endpoint(waiting(200*time.Millisecond, 1)); verdict.Ended {
		t.Fatal("an unavailable act ended a turn 200ms into a pause")
	}
	if verdict := floor.Endpoint(waiting(900*time.Millisecond, 2)); !verdict.Ended {
		t.Fatal("an unavailable act left the turn open past the silence threshold")
	}
}

// A model that answers "listen" to everything produces an agent that never
// speaks again, and nothing in the conversation would report it.
func TestActFloorEndsAtTheLivenessBound(t *testing.T) {
	floor := floorFor(t, interaction.ActStaySilent, false)
	if verdict := floor.Endpoint(waiting(19*time.Second, 1)); verdict.Ended {
		t.Fatal("the floor ended before the liveness bound")
	}
	verdict := floor.Endpoint(waiting(30*time.Second, 2))
	if !verdict.Ended {
		t.Fatal("a model holding the floor forever was never overruled")
	}
}

// A dead policy model must neither mute the agent nor make it interrupt.
func TestActFloorFallsBackToTheSilenceRule(t *testing.T) {
	floor := floorFor(t, interaction.ActStaySilent, true)
	if verdict := floor.Endpoint(waiting(200*time.Millisecond, 1)); verdict.Ended {
		t.Fatal("an unreachable model ended a turn 200ms into a pause")
	}
	if verdict := floor.Endpoint(waiting(900*time.Millisecond, 2)); !verdict.Ended {
		t.Fatal("an unreachable model left the turn open past the silence threshold")
	}
}

// Without the conversation there is nothing this floor can do that the silence
// rule does not already do better.
func TestActFloorWithoutASituationIsTheSilenceRule(t *testing.T) {
	floor := floorFor(t, interaction.ActStaySilent, false)
	bare := interaction.Context{Revision: interaction.Revision{ID: 1, SilenceNS: uint64(900 * time.Millisecond)}}
	if verdict := floor.Endpoint(bare); !verdict.Ended {
		t.Fatal("with no conversation attached the floor did not fall back to silence")
	}
}

// Answering means the speaker has finished. While they are still audible that
// is a fact the model has contradicted rather than a judgement it may make,
// and acting on it cuts the utterance mid-word - the recogniser is handed a
// fragment and the agent answers something nobody said.
func TestActFloorRefusesToAnswerOverSomebodyStillSpeaking(t *testing.T) {
	speaking := waiting(0, 1)
	speaking.Duplex.UserSpeaking = true
	speaking.Situation.Speaking = true
	if verdict := floorFor(t, interaction.ActAnswer, false).Endpoint(speaking); verdict.Ended {
		t.Fatal("a turn was ended while its speaker was mid-word")
	}
	// Taking a floor somebody still holds is what interrupt is for, and the
	// model has to say so.
	if verdict := floorFor(t, interaction.ActInterrupt, false).Endpoint(speaking); !verdict.Ended {
		t.Fatal("interrupting did not take the floor from an active speaker")
	}
}

// The last call before a pause is made while the speaker is still audible,
// where answering is refused. A cache keyed on anything that does not change
// during the pause serves that refusal back for as long as the pause lasts,
// and the turn never ends at all.
func TestActFloorDoesNotServeARefusalBackThroughTheWholePause(t *testing.T) {
	floor := floorFor(t, interaction.ActAnswer, false)
	// Mid-word: answering is refused because they still hold the floor.
	speaking := waiting(0, 7)
	speaking.Duplex.UserSpeaking = true
	speaking.Situation.Speaking = true
	if verdict := floor.Endpoint(speaking); verdict.Ended {
		t.Fatal("a turn was ended mid-word")
	}
	// They stop. Same revision, same transcript, and the answer must change.
	stopped := waiting(700*time.Millisecond, 7)
	stopped.Situation.Speaking = false
	stopped.Situation.Silence = "700ms"
	if verdict := floor.Endpoint(stopped); !verdict.Ended {
		t.Fatalf("the refusal was served back after the speaker stopped: %s", verdict.Reason)
	}
}

// Interrupting stops being an interruption when it stops being scarce. The
// model is asked afresh on every partial and cannot remember having just cut
// in; consulted sixty-five times in one conversation it answered "interrupt"
// sixty-five times, each defensible alone and together an agent nobody could
// speak to.
func TestActFloorWillNotInterruptTwiceInQuickSuccession(t *testing.T) {
	model, err := interaction.NewInteractionModel(answers{act: interaction.ActInterrupt})
	if err != nil {
		t.Fatal(err)
	}
	floor, err := interaction.NewActFloor(model, interaction.ActFloorOptions{
		SilenceDuration: 500 * time.Millisecond, Liveness: 20 * time.Second,
		MinimumBetweenInterruptions: 4 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	at := func(ns uint64, revision uint64) interaction.Context {
		context := waiting(0, revision)
		context.NowNS = ns
		context.Duplex.UserSpeaking = true
		context.Situation.Speaking = true
		context.Situation.Heard = "they are still going on about it " + string(rune('a'+revision))
		return context
	}
	if verdict := floor.Endpoint(at(uint64(1*time.Second), 1)); !verdict.Ended {
		t.Fatal("the first interruption was refused")
	}
	if verdict := floor.Endpoint(at(uint64(2*time.Second), 2)); verdict.Ended {
		t.Fatalf("interrupted again one second later: %s", verdict.Reason)
	}
	if verdict := floor.Endpoint(at(uint64(7*time.Second), 3)); !verdict.Ended {
		t.Fatalf("still refusing six seconds later: %s", verdict.Reason)
	}
}

// Interrupting the same stretch of speech twice is talking over somebody;
// interrupting a later one is a fresh decision that may well be right. A flat
// timer cannot tell them apart.
func TestActFloorInterruptsOncePerStretchOfSpeech(t *testing.T) {
	model, err := interaction.NewInteractionModel(answers{act: interaction.ActInterrupt})
	if err != nil {
		t.Fatal(err)
	}
	floor, err := interaction.NewActFloor(model, interaction.ActFloorOptions{
		SilenceDuration: 500 * time.Millisecond, Liveness: 20 * time.Second,
		MinimumBetweenInterruptions: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	speaking := func(ns uint64, revision uint64, heard string) interaction.Context {
		context := waiting(0, revision)
		context.NowNS = ns
		context.Duplex.UserSpeaking = true
		context.Situation.Speaking = true
		context.Situation.Heard = heard
		return context
	}
	if v := floor.Endpoint(speaking(uint64(time.Second), 1, "so the first option would be")); !v.Ended {
		t.Fatal("the first interruption was refused")
	}
	// The same sentence, further along. Cutting in again is talking over them,
	// however long has passed.
	later := speaking(uint64(30*time.Second), 2, "so the first option would be to rewrite it entirely")
	if v := floor.Endpoint(later); v.Ended {
		t.Fatalf("cut into the same sentence twice: %s", v.Reason)
	}
	// A different stretch of speech is a new decision.
	fresh := speaking(uint64(40*time.Second), 3, "and the second option is to leave it alone")
	if v := floor.Endpoint(fresh); !v.Ended {
		t.Fatalf("refused to interrupt a different sentence entirely: %s", v.Reason)
	}
}
