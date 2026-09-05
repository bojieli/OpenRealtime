package fdbench

import (
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

// An agent that finishes one answer and begins the next without pausing
// produces no gap at all. Judged on gaps alone, that next answer is invisible
// and its turn is recorded as unanswered: measured on 2026-09-05, one turn of
// thirty-six was counted missed while twenty-two deltas of a genuinely new
// response arrived 148 ms after it ended.
func TestAnAnswerThatFollowsTheLastOneWithoutAPauseStillCounts(t *testing.T) {
	var moments []bench.Moment
	for at := 4_000.0; at < 5_000.0; at += 20 {
		moments = append(moments, bench.Moment{
			AtMS: at, Kind: bench.MomentAgentAudio, AudioMS: 20, ResponseID: "resp_first",
		})
	}
	for at := 5_000.0; at < 6_000.0; at += 20 {
		moments = append(moments, bench.Moment{
			AtMS: at, Kind: bench.MomentAgentAudio, AudioMS: 20, ResponseID: "resp_second",
		})
	}
	transcript := bench.Transcript{Moments: moments}

	latency, found := firstAudioOnsetAfter(transcript, 4_900)
	if !found {
		t.Fatal("the second answer was not seen at all")
	}
	if latency < 90 || latency > 110 {
		t.Fatalf("the second answer began %.0f ms after the turn, want about 100", latency)
	}
}

// A response that was already speaking when the turn ended is an overrun, not
// a fresh answer, and must not be counted as one.
func TestAnAnswerAlreadyRunningWhenTheTurnEndedIsNotAFreshOne(t *testing.T) {
	var moments []bench.Moment
	for at := 4_000.0; at < 6_000.0; at += 20 {
		moments = append(moments, bench.Moment{
			AtMS: at, Kind: bench.MomentAgentAudio, AudioMS: 20, ResponseID: "resp_only",
		})
	}
	if _, found := firstAudioOnsetAfter(bench.Transcript{Moments: moments}, 4_900); found {
		t.Fatal("an answer that was already playing was counted as a reply to the turn it ran into")
	}
}
