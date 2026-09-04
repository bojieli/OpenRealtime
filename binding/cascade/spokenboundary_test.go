package cascade_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// The end-to-end claim: when somebody takes the floor back mid-sentence, the
// runtime records which of the agent's words they actually heard, and it
// records the rest as prepared and never spoken.
//
// A duration alone cannot support that. "The agent was audible for 1.4
// seconds" leaves every later model with two options and both are wrong: treat
// the whole turn as said, and it carries on past words nobody heard; treat none
// of it as said, and it says the heard half again.
func TestAnInterruptedTurnRecordsWhichWordsWereHeard(t *testing.T) {
	const answer = "one two three four five six seven eight nine ten eleven twelve"
	fast := newFast([]continuation.Event{
		{Kind: continuation.EventAssistantDelta, Text: answer},
	})
	slow := newSlow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Done."}})
	runtime, sink := startSession(t, cascade.Config{
		Fast: fast, Slow: slow, Speech: toneSpeech{chunks: 60},
	}, binding.Settings{})

	speak(t, runtime, 3)
	// Let a few seconds of counting actually go out before taking the floor
	// back. A cut inside the first word is a real case and a poor test of
	// where a boundary lands, because every boundary looks the same there.
	waitFor(t, func() bool { return framesOut(sink) >= 20 }, "the answer never got going")
	// They take the floor back while it is still counting.
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

	for _, ended := range sink.speechOutcomes() {
		if ended.outcome.Completed || ended.utterance.Text != answer {
			continue
		}
		mark := ended.outcome.Mark
		if !mark.Started() {
			t.Fatalf("audio went out for %dms and nothing was recorded as heard: %+v",
				ended.outcome.PlayedMS, mark)
		}
		if mark.Complete() {
			t.Fatalf("a cancelled turn was recorded as fully spoken: %+v", mark)
		}
		if !strings.HasPrefix(answer, mark.Spoken) {
			t.Fatalf("what was recorded as heard is not a prefix of what was said: %q", mark.Spoken)
		}
		// The two halves have to reconstruct the turn, or something has been
		// lost between deciding it and recording it.
		rejoined := strings.Join(strings.Fields(mark.Spoken+" "+mark.Pending), " ")
		if rejoined != answer {
			t.Fatalf("heard %q plus prepared %q is not the turn that was said", mark.Spoken, mark.Pending)
		}
		if mark.PlayedMS != ended.outcome.PlayedMS {
			t.Fatalf("the boundary was taken at %dms and playback stopped at %dms",
				mark.PlayedMS, ended.outcome.PlayedMS)
		}
		return
	}
	t.Fatalf("no cancelled utterance carried the answer: %+v", sink.speechOutcomes())
}

// The same fact, one layer out: the boundary has to reach the models. A record
// only the action plane can see would leave every provider projection exactly
// as wrong as it was before.
func TestTheNextTurnSeesWhatTheUserActuallyHeard(t *testing.T) {
	const answer = "one two three four five six seven eight nine ten eleven twelve"
	fast := newFast(
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: answer}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Carrying on."}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Carrying on."}},
	)
	slow := newSlow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Done."}})
	runtime, sink := startSession(t, cascade.Config{
		Fast: fast, Slow: slow, Speech: toneSpeech{chunks: 60},
	}, binding.Settings{})

	speak(t, runtime, 3)
	waitFor(t, func() bool { return framesOut(sink) >= 20 }, "the answer never got going")
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
	}, "the answer was never cut off")

	// Say something else. The boundary is recorded when playback ends, and the
	// turn that was already in flight when it did was assembled before it -
	// what has to be shown is that the turn after it can see it.
	speak(t, runtime, 3)

	var heard, prepared string
	waitFor(t, func() bool {
		fast.mu.Lock()
		defer fast.mu.Unlock()
		for _, request := range fast.requests {
			for _, run := range continuation.ProviderRuns(request.Trajectory.Items) {
				for _, item := range run.Items {
					if item.Kind != trajectory.KindAssistant {
						continue
					}
					before, after, split := strings.Cut(item.Content, continuation.HeardPreamble)
					if split {
						heard, prepared = before, after
						return true
					}
				}
			}
		}
		return false
	}, "no later request ever carried the boundary")

	if strings.TrimSpace(heard) == "" {
		t.Fatal("the projection says the user heard nothing of a turn that was audible")
	}
	if !strings.HasPrefix(answer, strings.TrimSpace(heard)) {
		t.Fatalf("what the projection presents as heard is not a prefix of the turn: %q", heard)
	}
	if strings.TrimSpace(heard) == answer {
		t.Fatalf("the projection presents the whole cancelled turn as heard: %q", heard)
	}
	// The unheard words must still be visible, and visible as something other
	// than speech: a model that cannot see them has nothing to carry on from.
	if !strings.Contains(prepared, "twelve") {
		t.Fatalf("the prepared remainder is missing from the note: %q", prepared)
	}
}

// framesOut is how many paced audio frames have reached the sink, which is the
// only honest way for a test to wait until the agent has actually said
// something rather than merely started.
func framesOut(sink *recordingSink) int {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.frames
}
