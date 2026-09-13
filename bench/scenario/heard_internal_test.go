package scenario

import (
	"errors"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func spans(bounds ...[2]int) Timeline {
	timeline := Timeline{}
	for _, bound := range bounds {
		timeline.Spans = append(timeline.Spans, Span{StartMS: bound[0], EndMS: bound[1]})
	}
	timeline.TotalMS = bounds[len(bounds)-1][1] + 10_000
	return timeline
}

// script is the shape of the resumed case: an instruction, the line that cut
// the agent off, and the line that told it to carry on.
var script = spans([2]int{0, 6000}, [2]int{15000, 17000}, [2]int{25000, 28000})

func resumedCheck() Check {
	return Check{Kind: CheckResumed, Line: 2, Interrupted: 1, AfterMS: 12000, BeforeMS: 2000, Note: "note",
		Count: &CountRequirement{From: 1, Through: 40, MinimumBefore: 3, MinimumAfter: 3, StopWithinMS: 2500}}
}

// scripted answers the two windows the check asks about, and nothing else, so
// a check that looked anywhere else would come back empty rather than right.
func scripted(before, after string) heard {
	return func(fromMS, toMS int) (string, error) {
		switch {
		case fromMS == 0 && toMS == script.Spans[1].EndMS+2500:
			return before, nil
		case fromMS == script.Spans[2].EndMS:
			return after, nil
		default:
			return "", nil
		}
	}
}

// The behaviour the case exists to reward: it carried on from the number the
// user actually heard.
func TestCarryingOnFromWhereTheVoiceGotToPasses(t *testing.T) {
	for name, after := range map[string]string{
		"the next number":             "fourteen, fifteen, sixteen, seventeen",
		"repeating the half-said one": "thirteen, fourteen, fifteen, sixteen",
		"written as digits":           "14, 15, 16, 17",
	} {
		t.Run(name, func(t *testing.T) {
			failure := resumed(resumedCheck(), script,
				scripted("one two three four five six seven eight nine ten eleven twelve thirteen", after))
			if failure != "" {
				t.Fatalf("%s", failure)
			}
		})
	}
}

// The split brain, in the direction that sounds like amnesia: it knew it was
// interrupted and not where, so it started the list again.
func TestStartingAgainFromTheBeginningIsReportedAsThat(t *testing.T) {
	failure := resumed(resumedCheck(), script,
		scripted("one two three four five six seven eight nine ten", "one, two, three, four"))
	if !strings.Contains(failure, "started again") {
		t.Fatalf("restarting was not reported as restarting: %q", failure)
	}
	if !strings.Contains(failure, "10") {
		t.Fatalf("the failure does not say where it had got to: %q", failure)
	}
}

// The split brain in the other direction, and the one this whole change exists
// to remove: it carried on from the end of the text it wrote rather than from
// the end of what its voice had said.
func TestCarryingOnPastWordsNobodyHeardIsReportedAsThat(t *testing.T) {
	failure := resumed(resumedCheck(), script,
		scripted("one two three four five six seven eight", "twenty one, twenty two, twenty three"))
	if !strings.Contains(failure, "past") || !strings.Contains(failure, "nobody heard") {
		t.Fatalf("skipping was not reported as skipping: %q", failure)
	}
	if !strings.Contains(failure, "21") || !strings.Contains(failure, "8") {
		t.Fatalf("the failure names neither end of the gap: %q", failure)
	}
}

// Going back over ground the user already covered is its own failure: it is
// not amnesia and it is not skipping, and reported as either it would be
// diagnosed as the wrong thing.
func TestRepeatingWhatWasAlreadyHeardIsItsOwnFailure(t *testing.T) {
	failure := resumed(resumedCheck(), script,
		scripted("one two three four five six seven eight nine ten", "six, seven, eight, nine"))
	if !strings.Contains(failure, "went back") {
		t.Fatalf("repetition was not reported as repetition: %q", failure)
	}
}

// Saying nothing at all after being asked to carry on is a third thing again,
// and one an agent does when it has lost the thread rather than the place.
func TestCountingNothingAfterwardsIsReportedSeparately(t *testing.T) {
	failure := resumed(resumedCheck(), script,
		scripted("one two three four five", "Sure, where were we?"))
	if !strings.Contains(failure, "counted nothing") {
		t.Fatalf("silence was not reported as silence: %q", failure)
	}
	if !strings.Contains(failure, "where were we") {
		t.Fatalf("the failure does not quote what it did say: %q", failure)
	}
}

// A jump inside the resumed run is a different fault from a wrong starting
// point, and it is worth catching: an agent can restart correctly and then
// skip.
func TestAJumpInsideTheResumedRunIsCaught(t *testing.T) {
	failure := resumed(resumedCheck(), script,
		scripted("one two three", "four, five, six, twenty, twenty one"))
	if !strings.Contains(failure, "jumped from") {
		t.Fatalf("a jump mid-run was not caught: %q", failure)
	}
}

// The claim is about what a loudspeaker produced. With nothing able to listen
// to the recording, the claim has not been checked - which is a different thing
// from having been met, and must never read as a pass.
func TestWithNothingToListenWithTheClaimIsNotVerifiedRatherThanMet(t *testing.T) {
	failure := resumed(resumedCheck(), script, nil)
	if !strings.HasPrefix(failure, "NOT VERIFIED") {
		t.Fatalf("an unscorable claim did not say so: %q", failure)
	}
	if !strings.Contains(failure, "-transcribe-url") {
		t.Fatalf("the message does not say what is missing: %q", failure)
	}
}

// A recogniser that refuses is infrastructure, not behaviour, and reporting it
// as a behaviour failure would blame the agent for a broken endpoint.
func TestATranscriptionFailureIsNotABehaviourFailure(t *testing.T) {
	failure := resumed(resumedCheck(), script, func(int, int) (string, error) {
		return "", errors.New("recogniser refused")
	})
	if !strings.HasPrefix(failure, "NOT VERIFIED") || !strings.Contains(failure, "recogniser refused") {
		t.Fatalf("a broken endpoint was reported as agent behaviour: %q", failure)
	}
}

// Which spelling comes back is the recogniser's decision rather than the
// agent's, so a check that understood only one of them would be measuring the
// recogniser.
func TestBothSpellingsOfANumberAreRead(t *testing.T) {
	for text, want := range map[string][]int{
		"one, two, three":              {1, 2, 3},
		"1, 2, 3":                      {1, 2, 3},
		"twenty one, twenty two":       {21, 22},
		"Twenty-three. Twenty-four.":   {23, 24},
		"nineteen, 20, twenty one":     {19, 20, 21},
		"Sure, where were we? Eleven.": {11},
		"One. To. Three. For. Five.":   {1, 2, 3, 4, 5},
	} {
		got := numbersIn(text)
		if len(got) != len(want) {
			t.Fatalf("%q -> %v, want %v", text, got, want)
		}
		for index := range want {
			if got[index] != want[index] {
				t.Fatalf("%q -> %v, want %v", text, got, want)
			}
		}
	}
}

// The window is rebuilt on the episode clock with its silences intact. A
// recogniser handed the agent's speech with every pause removed hears one
// run-on utterance and punctuates it wherever it likes.
func TestTheAgentWindowKeepsItsSilences(t *testing.T) {
	capture := bench.SessionAudioCapture{
		SampleRateHz: 24_000,
		Agent: []bench.TimedAudioChunk{
			{AtMS: 100, PCM16: []int16{1, 1, 1}},
			{AtMS: 900, PCM16: []int16{2, 2, 2}},
			// Outside the window on both sides.
			{AtMS: 1900, PCM16: []int16{3, 3, 3}},
		},
	}
	window := agentAudioBetween(capture, 0, 1000)
	if len(window) != 24_000 {
		t.Fatalf("a one-second window is %d samples", len(window))
	}
	if window[100*24] != 1 || window[900*24] != 2 {
		t.Fatalf("chunks did not land at their own positions")
	}
	if window[500*24] != 0 {
		t.Fatal("the gap between two chunks was not preserved as silence")
	}
	for _, sample := range window {
		if sample == 3 {
			t.Fatal("audio from outside the window leaked into it")
		}
	}
}
