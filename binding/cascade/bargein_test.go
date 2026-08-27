package cascade_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/session"
)

// waitingSpeech claims an utterance and then waits before producing its first
// frame. That interval is the race this test exercises: the response exists in
// the action plane, but duplex state must still report that no agent audio has
// reached the user.
type waitingSpeech struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (speech *waitingSpeech) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "waiting-speech", Version: "1", Capabilities: v1.Capabilities{}}
}

func (speech *waitingSpeech) Synthesize(context.Context, v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	return nil, errors.New("unused")
}

func (speech *waitingSpeech) Stream(
	ctx context.Context, plan v1.SpeechPlan, emit func(v1.SpeechChunk) error,
) error {
	speech.once.Do(func() { close(speech.started) })
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-speech.release:
	}
	return emit(v1.SpeechChunk{
		ChunkID: "waiting", CandidateID: plan.CandidateID, SampleRateHz: 24_000,
		PCM16LE: make([]byte, 4800), Final: true,
	})
}

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
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, outcome := range sink.speechOutcomes() {
			if outcome.utterance.SpokeOver {
				goto correctionSpoken
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the correction never went out over them at all; speech=%+v turns=%+v activities=%+v",
		sink.speechOutcomes(), sink.turnOutcomes(), sink.activityEvents())

correctionSpoken:

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

// If the user resumes after a response has entered synthesis but before its
// first frame, there is no audible overlap for duplex state to report. The
// pending response is still stale and fully reversible; allowing it to start
// produces the characteristic failure where an agent begins answering a
// fragment in the middle of the sentence that continued.
func TestRenewedSpeechCancelsAResponseBeforeItsFirstFrame(t *testing.T) {
	speech := &waitingSpeech{started: make(chan struct{}), release: make(chan struct{})}
	fast := newFast([]continuation.Event{{
		Kind: continuation.EventAssistantDelta, Text: "Please repeat that fragment.",
	}})
	runtime, sink := startSession(t, cascade.Config{
		Fast: fast, Slow: newSlow(), Speech: speech,
	}, binding.Settings{})

	// Finish one short turn and wait until its answer has claimed the speech
	// planner but has emitted no audio.
	speak(t, runtime, 3)
	select {
	case <-speech.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the response never entered synthesis")
	}

	// Start speaking again. Four 100ms voiced blocks leave ample margin over
	// the onset hysteresis; require the second gate-open event so this test
	// cannot pass or fail by exercising only the synthesiser release race.
	pushAudio(t, runtime, tone(2400, 8000), 4)
	started := 0
	for _, event := range sink.activityEvents() {
		if event.Started {
			started++
		}
	}
	if started < 2 {
		t.Fatalf("renewed speech did not reopen the acoustic gate: %+v", sink.activityEvents())
	}
	close(speech.release)
	waitFor(t, func() bool { return len(sink.speechOutcomes()) > 0 }, "the pending response never terminated")

	outcome := sink.speechOutcomes()[0].outcome
	if outcome.Completed || outcome.PlayedMS != 0 {
		t.Fatalf("a response to the old fragment became audible after speech resumed: %+v", outcome)
	}
	sink.mu.Lock()
	frames := sink.frames
	sink.mu.Unlock()
	if frames != 0 {
		t.Fatalf("%d stale audio frames crossed after the user had resumed", frames)
	}
}

// Pending synthesis is evidence for the barge-in policy, not a cancellation
// rule of its own. A deployment that deliberately disables barge-in must keep
// that choice even in the pre-playback interval.
func TestPendingResponseStillRespectsNeverBargeIn(t *testing.T) {
	speech := &waitingSpeech{started: make(chan struct{}), release: make(chan struct{})}
	policies := interaction.Defaults()
	policies.BargeIn = interaction.NewNeverBargeIn()
	runtime, sink := startSession(t, cascade.Config{
		Fast: newFast([]continuation.Event{{
			Kind: continuation.EventAssistantDelta, Text: "This response must finish.",
		}}),
		Slow: newSlow(), Speech: speech, Policies: policies,
	}, binding.Settings{})

	speak(t, runtime, 3)
	select {
	case <-speech.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the response never entered synthesis")
	}
	pushAudio(t, runtime, tone(2400, 8000), 4)
	close(speech.release)
	waitFor(t, func() bool { return len(sink.speechOutcomes()) > 0 }, "the response never terminated")

	outcome := sink.speechOutcomes()[0].outcome
	if !outcome.Completed || outcome.PlayedMS != 100 {
		t.Fatalf("never-barge-in policy did not preserve pending speech: %+v", outcome)
	}
}
