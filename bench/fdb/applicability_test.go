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
