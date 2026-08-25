package scenario_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/scenario"
)

func timeline(spans ...scenario.Span) scenario.Timeline {
	total := 0
	for _, span := range spans {
		if span.EndMS > total {
			total = span.EndMS
		}
	}
	return scenario.Timeline{Spans: spans, TotalMS: total}
}

func spoke(atMS, audioMS float64, text string) []bench.Moment {
	return []bench.Moment{
		{AtMS: atMS, Kind: bench.MomentAgentAudio, AudioMS: audioMS},
		{AtMS: atMS, Kind: bench.MomentAgentText, Text: text},
	}
}

// The failure this whole suite exists to catch: talking over somebody who
// asked to be left alone.
func TestSilenceCheckCatchesSpeechInTheWindow(t *testing.T) {
	line := timeline(scenario.Span{StartMS: 1000, EndMS: 5000})
	item := scenario.Scenario{
		Name:   "quiet",
		Checks: []scenario.Check{{Kind: scenario.CheckSilent, Line: 0, AfterMS: 2000}},
	}
	quiet := scenario.Score(item, line, bench.Transcript{})
	if !quiet.Passed {
		t.Fatalf("silence failed a silence check: %v", quiet.Failures)
	}
	noisy := scenario.Score(item, line, bench.Transcript{Moments: spoke(5500, 900, "sorry to butt in")})
	if noisy.Passed {
		t.Fatal("speech inside the window passed a silence check")
	}
}

// A cancelled turn can leave a few milliseconds of a syllable in the record,
// and reporting that as speech reports a failure nobody in the room heard.
func TestSilenceCheckToleratesAClippedSyllable(t *testing.T) {
	line := timeline(scenario.Span{StartMS: 0, EndMS: 4000})
	result := scenario.Score(
		scenario.Scenario{Checks: []scenario.Check{{Kind: scenario.CheckSilent, Line: 0}}},
		line, bench.Transcript{Moments: spoke(2000, 40, "")})
	if !result.Passed {
		t.Fatalf("forty milliseconds of a clipped word failed a silence check: %v", result.Failures)
	}
}

// The false pass that nearly shipped: a check asking only whether the agent
// spoke during a long monologue is satisfied by an agent that waited politely
// until the end, which is the behaviour interrupting replaces.
func TestSpokeCheckCanRequireCuttingInEarly(t *testing.T) {
	line := timeline(scenario.Span{StartMS: 0, EndMS: 20000})
	item := scenario.Scenario{
		Checks: []scenario.Check{{Kind: scenario.CheckSpoke, Line: 0, AfterMS: -5000}},
	}
	late := scenario.Score(item, line, bench.Transcript{Moments: spoke(19000, 2000, "as you were saying")})
	if late.Passed {
		t.Fatal("speaking at the very end passed a check that asked for an interruption")
	}
	early := scenario.Score(item, line, bench.Transcript{Moments: spoke(8000, 900, "hold on, the third")})
	if !early.Passed {
		t.Fatalf("cutting in halfway failed: %v", early.Failures)
	}
}

// The other false pass: the word turning up somewhere in half a minute of
// unrelated talk is not the agent counting.
func TestSaidCheckIsBoundedByItsWindow(t *testing.T) {
	line := timeline(scenario.Span{StartMS: 0, EndMS: 3000}, scenario.Span{StartMS: 20000, EndMS: 23000})
	item := scenario.Scenario{
		Checks: []scenario.Check{{Kind: scenario.CheckSaid, Line: 0, AfterMS: 1000, Any: []string{"two"}}},
	}
	elsewhere := scenario.Score(item, line, bench.Transcript{Moments: spoke(21000, 800, "you mentioned two animals")})
	if elsewhere.Passed {
		t.Fatal("a phrase said long outside the window passed a windowed check")
	}
	inside := scenario.Score(item, line, bench.Transcript{Moments: spoke(2500, 400, "two")})
	if !inside.Passed {
		t.Fatalf("a phrase said inside the window failed: %v", inside.Failures)
	}
}

// Speaking at the right moment about the wrong thing looks like success.
func TestNotSaidCatchesTheRightMomentAndWrongContent(t *testing.T) {
	line := timeline(scenario.Span{StartMS: 0, EndMS: 4000})
	item := scenario.Scenario{
		Checks: []scenario.Check{{Kind: scenario.CheckNotSaid, Line: 0, AfterMS: 1000, Any: []string{"?"}}},
	}
	interviewing := scenario.Score(item, line, bench.Transcript{Moments: spoke(2000, 700, "How many animals did you see?")})
	if interviewing.Passed {
		t.Fatal("a question passed a check that forbade questions")
	}
	counting := scenario.Score(item, line, bench.Transcript{Moments: spoke(2000, 300, "one")})
	if !counting.Passed {
		t.Fatalf("counting failed a check that only forbade questions: %v", counting.Failures)
	}
}

func TestToolCheckReportsWhatWasMissing(t *testing.T) {
	item := scenario.Scenario{
		Checks: []scenario.Check{{Kind: scenario.CheckToolCalled, Tool: "press_key", Line: -1}},
	}
	missing := scenario.Score(item, timeline(), bench.Transcript{})
	if missing.Passed || !strings.Contains(strings.Join(missing.Failures, " "), "press_key") {
		t.Fatalf("a missing tool call was not reported clearly: %v", missing.Failures)
	}
	called := scenario.Score(item, timeline(), bench.Transcript{
		Moments: []bench.Moment{{AtMS: 900, Kind: bench.MomentToolCall, Name: "press_key"}}})
	if !called.Passed {
		t.Fatalf("a tool that was called failed its check: %v", called.Failures)
	}
}

// The harness bug that made every scenario measure the wrong thing: the
// backend returns 44.1 kHz whatever it is asked for, and 44.1 kHz samples fed
// to a 24 kHz pipeline stretch by 1.84 and drop an octave.
func TestResampleMatchesTheSessionRate(t *testing.T) {
	// One second of 44.1 kHz becomes one second of 24 kHz.
	source := make([]int16, 44100)
	for index := range source {
		source[index] = int16(index % 1000)
	}
	got := scenario.ResampleForTest(source, 44100, 24000)
	if len(got) != 24000 {
		t.Fatalf("a second of audio became %d samples at 24 kHz, want 24000", len(got))
	}
	if same := scenario.ResampleForTest(source, 24000, 24000); len(same) != len(source) {
		t.Fatalf("resampling to the same rate changed the length: %d then %d", len(source), len(same))
	}
}
