package cascade_test

import (
	"testing"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/session"
)

// interruptingFloor takes the floor from somebody who is still talking, which
// is what the interrupt act means.
type interruptingFloor struct{}

func (interruptingFloor) Name() string      { return "interrupting" }
func (interruptingFloor) EngineOwned() bool { return true }

func (interruptingFloor) Endpoint(decision interaction.Context) interaction.EndpointDecision {
	if decision.Revision.Empty() || decision.Revision.Final {
		return interaction.EndpointDecision{}
	}
	return interaction.EndpointDecision{
		Ended: true, Projected: true, Act: interaction.ActInterrupt,
		Reason: "the date is wrong and waiting would make the correction useless",
	}
}

func (interruptingFloor) Holder(session.Snapshot) interaction.Holder {
	return interaction.HolderNobody
}

// An interruption is speech begun on purpose over somebody who has not
// finished, so their carrying on is the premise of the act rather than
// evidence against it. The shipped barge-in policy yields the floor the moment
// the user speaks, and it could not tell the two apart: measured end to end,
// every correction was decided, worded, and then cancelled by the sentence it
// was correcting, after a single hundred-millisecond frame had gone out.
func TestAnInterruptionIsNotCancelledByWhatItInterrupted(t *testing.T) {
	policies := interaction.Defaults()
	policies.Floor = interruptingFloor{}
	fast := newFast([]continuation.Event{
		{Kind: continuation.EventAssistantDelta, Text: "Actually, the deadline is the third."},
	})
	slow := newSlow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Noted."}})
	runtime, sink := startSession(t, cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) {
			return &revisingASR{
				partial: "ship it by the thirteenth",
				final:   "ship it by the thirteenth which gives us plenty of time",
			}, nil
		},
		Fast: fast, Slow: slow, Policies: policies,
		Speech: toneSpeech{chunks: 8},
	}, binding.Settings{})

	// The person never stops: every push is more of the same stretch of speech
	// the agent chose to talk over.
	for index := 0; index < 12; index++ {
		pushAudio(t, runtime, tone(2400, 8000), 1)
	}
	waitFor(t, func() bool {
		for _, outcome := range sink.speechOutcomes() {
			if outcome.utterance.SpokeOver {
				return true
			}
		}
		return false
	}, "the correction never went out over them at all")

	for _, outcome := range sink.speechOutcomes() {
		if !outcome.utterance.SpokeOver {
			continue
		}
		if !outcome.outcome.Completed {
			t.Fatalf("the correction was cancelled by the speech it was correcting: %s (played %dms)",
				outcome.outcome.Reason, outcome.outcome.PlayedMS)
		}
		return
	}
}

// The other half of the same rule, and the reason it is keyed on a stretch of
// speech rather than set as a flag: an ordinary turn is speech the agent took
// the floor for, so somebody talking over it is taking the floor back and
// still gets it. A guard that could not tell the two apart would not be a
// description of one act, it would be a licence to talk over people.
func TestAnOrdinaryTurnIsStillInterruptible(t *testing.T) {
	fast := newFast([]continuation.Event{
		{Kind: continuation.EventAssistantDelta, Text: "The balance is four hundred and twelve pounds."},
	})
	slow := newSlow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Done."}})
	runtime, sink := startSession(t, cascade.Config{
		Fast: fast, Slow: slow, Speech: toneSpeech{chunks: 40},
	}, binding.Settings{})

	speak(t, runtime, 3)
	waitFor(t, func() bool { return len(sink.speechBegan()) > 0 }, "the answer never started")
	if began := sink.speechBegan(); began[0].SpokeOver {
		t.Fatalf("an answer given in silence was recorded as spoken over somebody: %+v", began[0])
	}
	// Now they talk over it.
	for index := 0; index < 12; index++ {
		pushAudio(t, runtime, tone(2400, 8000), 1)
	}
	waitFor(t, func() bool {
		for _, ended := range sink.speechOutcomes() {
			if !ended.outcome.Completed {
				return true
			}
		}
		return false
	}, "the answer kept playing over somebody who had taken the floor back")
}
