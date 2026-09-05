package fdb

import (
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func TestScorerDoesNotAwardAPassWhenThereWasNothingToInterrupt(t *testing.T) {
	outcome := bench.TaskOutcome{ID: "user_interruption/1", Completed: true, Notes: map[string]string{"category": string(Interruption)}}
	scoreOutcome(&outcome, bench.Transcript{}, attemptContext{
		Category: Interruption, EventStartMS: 1000, EventEndMS: 2000,
		ShouldYield: true, YieldWindowMS: 1000, HoldWindowMS: 1000,
	})
	if outcome.Passed || outcome.Applicability != bench.NotApplicable || outcome.Notes["applicable"] != "false" {
		t.Fatalf("silence received overlap credit: %+v", outcome)
	}
	if err := outcome.Validate(); err != nil {
		t.Fatal(err)
	}
	result := bench.Result{Suite: "fdb-v1.5", Expected: 1, Tasks: []bench.TaskOutcome{outcome}}
	result.Finish()
	if result.Summary.Passed != 0 || result.Summary.NotApplicable != 1 || !result.Summary.Complete {
		t.Fatalf("generic summary mislabeled FDB silence: %+v", result.Summary)
	}
	if summary := Breakdown(result)[Interruption]; summary.Passed != 0 || summary.NotApplicable != 1 || summary.Applicable != 0 {
		t.Fatalf("category and generic summary disagree: %+v", summary)
	}
}

// One sample of audio is not the agent speaking.
//
// The scorer's two audio tests asked for more than zero, and at 24 kHz a
// single sample is 0.0417 ms. A recording whose answer ended a sample inside
// the lookback window was judged applicable, asked to hold through an overlap
// it had already finished, and failed for it. The 2026-09-05 FDB v1.5 campaign
// contained two such recordings, both counted against the hold categories.
func TestScorerDoesNotCallOneSampleOfAudioSpeaking(t *testing.T) {
	sliver := bench.Transcript{Moments: []bench.Moment{
		{AtMS: 900, Kind: bench.MomentAgentAudio, AudioMS: 0.0416},
	}}
	outcome := bench.TaskOutcome{
		ID: "background_speech/6", Completed: true,
		Notes: map[string]string{"category": string(BackgroundSpeech)},
	}
	scoreOutcome(&outcome, sliver, attemptContext{
		Category: BackgroundSpeech, EventStartMS: 1000, EventEndMS: 2000,
		YieldWindowMS: 1000, HoldWindowMS: 1000,
	})
	if outcome.Applicability != bench.NotApplicable || outcome.Passed ||
		outcome.Notes["agent_was_speaking"] != "false" {
		t.Fatalf("a single sample was scored as the agent speaking: %+v", outcome)
	}

	// A packet of audio is, and then holding is judged on its own evidence.
	audible := bench.Transcript{Moments: []bench.Moment{
		{AtMS: 900, Kind: bench.MomentAgentAudio, AudioMS: 60},
		{AtMS: 1000, Kind: bench.MomentAgentAudio, AudioMS: 300},
	}}
	held := bench.TaskOutcome{
		ID: "background_speech/7", Completed: true,
		Notes: map[string]string{"category": string(BackgroundSpeech)},
	}
	scoreOutcome(&held, audible, attemptContext{
		Category: BackgroundSpeech, EventStartMS: 1000, EventEndMS: 2000,
		YieldWindowMS: 1000, HoldWindowMS: 1000,
	})
	if held.Applicability != bench.Applicable || !held.Passed {
		t.Fatalf("an audible answer that kept speaking was not credited: %+v", held)
	}
}

// An answer that finished before the overlap arrived has nothing to hold.
//
// The half-second lookback answers "did the agent speak recently", not "was it
// speaking when the event began", and for a one-line command whose answer is
// over in a second those are different questions. Four of the first forty
// background-speech recordings on 2026-09-05 were judged applicable on audio
// that had stopped hundreds of milliseconds before the event, then failed the
// hold they had nothing left to hold. Being quick is not a hold failure.
func TestScorerDoesNotAskAFinishedAnswerToHold(t *testing.T) {
	finished := bench.Transcript{Moments: []bench.Moment{
		{AtMS: 500, Kind: bench.MomentAgentAudio, AudioMS: 200},
		{AtMS: 700, Kind: bench.MomentAgentAudio, AudioMS: 120},
	}}
	outcome := bench.TaskOutcome{
		ID: "background_speech/20", Completed: true,
		Notes: map[string]string{"category": string(BackgroundSpeech)},
	}
	scoreOutcome(&outcome, finished, attemptContext{
		Category: BackgroundSpeech, EventStartMS: 1000, EventEndMS: 2000,
		YieldWindowMS: 1000, HoldWindowMS: 1000,
	})
	if outcome.Applicability != bench.NotApplicable || outcome.Passed {
		t.Fatalf("an answer that ended 300ms before the event was asked to hold: %+v", outcome)
	}
	if outcome.Metrics["agent_audio_before_event_ms"] < 300 {
		t.Fatalf("the half-second total must still be reported as evidence: %+v", outcome.Metrics)
	}
	if outcome.Metrics["agent_audio_at_event_ms"] != 0 {
		t.Fatalf("audio that never reached the event was counted at it: %+v", outcome.Metrics)
	}

	// Audio still flowing into the event is still applicable, and a gap of a
	// few packets inside the tolerance does not end an utterance.
	flowing := bench.Transcript{Moments: []bench.Moment{
		{AtMS: 500, Kind: bench.MomentAgentAudio, AudioMS: 200},
		{AtMS: 950, Kind: bench.MomentAgentAudio, AudioMS: 60},
	}}
	held := bench.TaskOutcome{
		ID: "background_speech/21", Completed: true,
		Notes: map[string]string{"category": string(BackgroundSpeech)},
	}
	scoreOutcome(&held, flowing, attemptContext{
		Category: BackgroundSpeech, EventStartMS: 1000, EventEndMS: 2000,
		YieldWindowMS: 1000, HoldWindowMS: 1000,
	})
	if held.Applicability != bench.Applicable {
		t.Fatalf("audio reaching the event was not counted as speaking: %+v", held)
	}
}
