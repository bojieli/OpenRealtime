package scenario

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func TestResumedRejectsInsufficientOrInvalidCountingSequences(t *testing.T) {
	for _, test := range []struct{ name, before, after, want string }{
		{"retained one then two", "1.", "2.", "insufficient sustained counting"},
		{"one before", "one", "two three four", "insufficient sustained counting"},
		{"one after", "one two three", "four", "insufficient sustained counting"},
		{"two before", "one two", "three four five", "insufficient sustained counting"},
		{"two after", "one two three", "four five", "insufficient sustained counting"},
		{"repeated before", "one one one", "two three four", "before interruption the count jumped"},
		{"skipped prefix", "one two ten", "eleven twelve thirteen", "before interruption the count jumped"},
		{"wrong beginning", "two three four", "five six seven", "initial count began"},
		{"outside task", "one two three", "four five forty one", "outside the requested range"},
		{"negative-equivalent zero", "zero one two", "three four five", "outside the requested range"},
		{"three on each side", "one two three", "four five six", ""},
		{"one recognizer omission", "one three four", "five seven eight", ""},
		{"half-spoken boundary repeated", "one two three", "three four five", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := resumed(resumedCheck(), script, scripted(test.before, test.after))
			if test.want == "" && failure != "" || test.want != "" && !strings.Contains(failure, test.want) {
				t.Fatalf("failure %q, want %q", failure, test.want)
			}
		})
	}
	check := resumedCheck()
	// Use a larger task to retain the minimum-length configuration while
	// exercising exhaustion before interruption.
	check.Count.Through = 4
	if failure := resumed(check, script, scripted("one two three four", "two three four")); !strings.Contains(failure, "already reached") {
		t.Fatalf("finished task earned interruption credit: %s", failure)
	}
}

func TestResumedRequiresAudibleSpeechNearTheInterruption(t *testing.T) {
	timeline := script
	timeline.TotalMS = 42000
	for _, test := range []struct {
		name    string
		capture *bench.SessionAudioCapture
		want    string
	}{
		{"absent", nil, "NOT VERIFIED"},
		{"long finished", pointerCapture(activityCapture([2]int{8000, 11000})), "no audible interruption opportunity"},
		{"silent packet", &bench.SessionAudioCapture{SampleRateHz: 24000, Agent: []bench.TimedAudioChunk{{AtMS: 13000, PCM16: make([]int16, 2000*24)}}}, "no audible interruption opportunity"},
		{"clipped syllable", pointerCapture(activityCapture([2]int{14880, 15000})), "no audible interruption opportunity"},
		{"recent counted speech", pointerCapture(activityCapture([2]int{13900, 14300})), ""},
		{"continued after stop deadline", pointerCapture(activityCapture([2]int{13900, 14300}, [2]int{22000, 23000})), "did not stay stopped"},
	} {
		t.Run(test.name, func(t *testing.T) {
			measurement, failure := resumedAcross(resumedCheck(), timeline, scripted("one two three", "four five six"), test.capture)
			if test.want == "" && failure != "" || test.want != "" && !strings.Contains(failure, test.want) {
				t.Fatalf("failure %q, want %q; %+v", failure, test.want, measurement)
			}
			if !reflect.DeepEqual(measurement.BeforeNumbers, []int{1, 2, 3}) || !reflect.DeepEqual(measurement.AfterNumbers, []int{4, 5, 6}) || measurement.RecentFromMS != 13000 || measurement.RecentToMS != 15000 {
				t.Fatalf("lost scored count/window observations: %+v", measurement)
			}
		})
	}
}

func TestResumedRequirementsAndWindowsFailClosed(t *testing.T) {
	for _, change := range []func(*Check){
		func(c *Check) { c.Count = nil },
		func(c *Check) { c.Count.MinimumBefore = 1 },
		func(c *Check) { c.Count.MinimumAfter = 1 },
		func(c *Check) { c.Count.From = 0 },
		func(c *Check) { c.Count.Through = 1001 },
		func(c *Check) { c.BeforeMS = 0 },
		func(c *Check) { c.Line = -1 },
		func(c *Check) { c.AfterMS = -1 },
		func(c *Check) { c.Count.StopWithinMS = -1 },
		func(c *Check) { c.Count.StopWithinMS = 10001 },
	} {
		check := resumedCheck()
		change(&check)
		if err := validateCheck(check, script); err == nil {
			t.Fatal("malformed counting requirement accepted")
		}
	}
	for _, timeline := range []Timeline{
		spans([2]int{0, 6000}, [2]int{15000, 17000}, [2]int{10000, 12000}),
		spans([2]int{0, 6000}, [2]int{1000, 17000}, [2]int{25000, 28000}),
		{Spans: script.Spans, TotalMS: 28000},
	} {
		_, failure := resumedAcross(resumedCheck(), timeline, func(int, int) (string, error) { t.Fatal("invalid window invoked recognition"); return "", nil }, nil)
		if !strings.Contains(failure, "invalid scenario check") {
			t.Fatalf("invalid timing accepted: %s", failure)
		}
	}
}

func TestResumedIncludesNumbersHeardWhileStopping(t *testing.T) {
	timeline := script
	timeline.TotalMS = 42000
	capture := activityCapture([2]int{13900, 14300}, [2]int{15000, 16600})
	for _, after := range []string{"seven eight nine", "eight nine ten"} {
		var windows [][2]int
		listen := func(from, to int) (string, error) {
			windows = append(windows, [2]int{from, to})
			if from == 0 {
				if to == 15000 {
					return "one two three four five", nil
				}
				return "one two three four five six seven", nil
			}
			return after, nil
		}
		measurement, failure := resumedAcross(resumedCheck(), timeline, listen, &capture)
		if failure != "" || !reflect.DeepEqual(windows, [][2]int{{0, 19500}, {28000, 40000}}) || measurement.BeforeToMS != 19500 || measurement.QuietActiveMS != 0 {
			t.Fatalf("audible stopping prefix lost: failure=%s windows=%v measurement=%+v", failure, windows, measurement)
		}
	}
}

func TestResumedMeasurementsSurviveReviewAndReplay(t *testing.T) {
	item := SubturnSuite()[0]
	capture := activityCapture([2]int{1000, 2000}, [2]int{14000, 14700}, [2]int{25300, 26000})
	result, wav := replayFixture(t, item, &capture, func(from, to int) (string, error) {
		if from == 0 {
			return "one two three", nil
		}
		return "four five six", nil
	})
	if !result.Passed || len(result.Resumptions) != 1 {
		t.Fatalf("count fixture failed: %+v", result)
	}
	var rendered strings.Builder
	renderReviewTranscript(&rendered, result)
	if !strings.Contains(rendered.String(), "[1 2 3] before and [4 5 6] after") {
		t.Fatalf("review lost count: %s", rendered.String())
	}
	copy := sanitizedReviewResult(result, func(s string) string { return s })
	copy.Resumptions[0].BeforeNumbers[0] = 99
	if result.Resumptions[0].BeforeNumbers[0] != 1 {
		t.Fatal("review aliased caller count")
	}
	for _, change := range []func(*ResumeMeasurement){
		func(m *ResumeMeasurement) { m.BeforeNumbers[0] = 99 },
		func(m *ResumeMeasurement) { m.RecentActiveMS++ },
		func(m *ResumeMeasurement) { m.RecentFromMS++ },
	} {
		changed := cloneReplayResult(t, result)
		change(&changed.Resumptions[0])
		if _, err := ReplayRecorded(t.Context(), item, changed, wav); err == nil {
			t.Fatal("changed count measurement replayed")
		}
	}
}

func pointerCapture(capture bench.SessionAudioCapture) *bench.SessionAudioCapture { return &capture }
