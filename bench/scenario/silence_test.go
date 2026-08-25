package scenario

import (
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

// A silence check asks whether something in the window made the agent speak.
// An agent asked to report a build finishing says briefly that it will, and
// the tail of that sentence was being counted as a reaction to the first
// screen it saw three seconds later - while the word it actually said at that
// screen, in another run, was the thing the check existed to catch.
func TestSilenceIsAboutWhatStartedInTheWindow(t *testing.T) {
	spilling := bench.Transcript{Moments: []bench.Moment{
		{AtMS: 4200, Kind: bench.MomentAgentAudio, AudioMS: 100},
		{AtMS: 4300, Kind: bench.MomentAgentAudio, AudioMS: 100},
		{AtMS: 7100, Kind: bench.MomentAgentAudio, AudioMS: 100},
		{AtMS: 7200, Kind: bench.MomentAgentAudio, AudioMS: 100},
		{AtMS: 9700, Kind: bench.MomentResponseDone},
	}}
	if audio := spilling.AudioStartedBetween(7000, 14000); audio != 0 {
		t.Fatalf("a sentence already under way was counted as a reaction to the window: %.0fms", audio)
	}

	reacting := bench.Transcript{Moments: []bench.Moment{
		{AtMS: 4200, Kind: bench.MomentAgentAudio, AudioMS: 100},
		{AtMS: 4700, Kind: bench.MomentResponseDone},
		{AtMS: 7600, Kind: bench.MomentAgentAudio, AudioMS: 300},
		{AtMS: 7900, Kind: bench.MomentAgentAudio, AudioMS: 300},
		{AtMS: 9500, Kind: bench.MomentResponseDone},
	}}
	if audio := reacting.AudioStartedBetween(7000, 14000); audio != 600 {
		t.Fatalf("a turn that began at the frame was not counted: %.0fms", audio)
	}
}
