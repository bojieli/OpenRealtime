package scenario

import (
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func eventCountFixture() (Check, Timeline, bench.SessionAudioCapture) {
	return Check{Kind: CheckEventCounts, Events: []CountEvent{{Line: 1, Number: 1, WithinMS: 5000}, {Line: 2, Number: 2, WithinMS: 5000}}},
		spans([2]int{0, 1000}, [2]int{2000, 3000}, [2]int{7000, 8000}),
		activityCapture([2]int{3500, 3900}, [2]int{8500, 8900})
}

func TestEventCountsRequireExactIndependentSpeech(t *testing.T) {
	check, timeline, capture := eventCountFixture()
	for _, test := range []struct{ name, first, second, want string }{
		{"words", "One.", "Two!", ""},
		{"digits", "1", "2.", ""},
		{"reference speech", "This is Pierce. Daekwon?", "2", "count 1 not established"},
		{"garbled second", "1", "Ever as mom.", "count 2 not established"},
		{"number with extra words", "Here is one.", "2", "no extra words"},
		{"repeated count", "one one", "2", "no extra words"},
		{"merged recognition", "1", "22", "count 2 not established"},
		{"homophone ambiguity", "1", "to", "count 2 not established"},
		{"no recognized speech", "", "2", "count 1 not established"},
		{"negative digit", "-1", "2", "count 1 not established"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var windows [][2]int
			listen := func(from, to int) (string, error) {
				windows = append(windows, [2]int{from, to})
				if from < 7000 {
					return test.first, nil
				}
				return test.second, nil
			}
			// The wire reports the correct counts even in every broken-audio case.
			transcript := bench.Transcript{Moments: []bench.Moment{{Kind: bench.MomentAgentText, AtMS: 3500, Text: "1"}, {Kind: bench.MomentAgentText, AtMS: 8500, Text: "2"}}}
			result := score(Scenario{Checks: []Check{check}}, timeline, transcript, nil, listen, &capture)
			if result.Passed != (test.want == "") || test.want != "" && !strings.Contains(strings.Join(result.Failures, " "), test.want) {
				t.Fatalf("result=%+v", result)
			}
			if !reflect.DeepEqual(windows, [][2]int{{3300, 4100}, {8300, 9100}}) {
				t.Fatalf("wrong hearing windows: %v", windows)
			}
			got := result.EventCounts[0]
			if got.Events[0].Heard != test.first || got.Events[1].Heard != test.second || got.Events[0].ToMS != 7000 {
				t.Fatalf("observations lost: %+v", got)
			}
		})
	}
}

func TestEventCountsRejectUnlicensedOrMissingAudio(t *testing.T) {
	check, timeline, normal := eventCountFixture()
	for _, test := range []struct {
		name    string
		capture *bench.SessionAudioCapture
		ears    bool
		want    string
	}{
		{"no waveform", nil, true, "NOT VERIFIED"},
		{"no ears", &normal, false, "NOT VERIFIED"},
		{"silent transport", pointerCapture(activityCapture()), true, "audible activity"},
		{"setup speech", pointerCapture(activityCapture([2]int{500, 900}, [2]int{3500, 3900}, [2]int{8500, 8900})), true, "outside the count windows"},
		{"late extra speech", pointerCapture(activityCapture([2]int{3500, 3900}, [2]int{8500, 8900}, [2]int{17000, 17500})), true, "outside the count windows"},
		{"speech after authored horizon", pointerCapture(activityCapture([2]int{3500, 3900}, [2]int{8500, 8900}, [2]int{18500, 19000})), true, "outside the count windows"},
		{"missing first count", pointerCapture(activityCapture([2]int{8500, 8900})), true, "count 1 has only"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var listen heard
			if test.ears {
				listen = func(from, to int) (string, error) {
					if from < 7000 {
						return "1", nil
					}
					return "2", nil
				}
			}
			_, failure := eventCounts(check, timeline, listen, test.capture)
			if !strings.Contains(failure, test.want) {
				t.Fatalf("failure=%q want %q", failure, test.want)
			}
		})
	}
	_, failure := eventCounts(check, timeline, func(int, int) (string, error) { return "", errors.New("recognizer unavailable") }, &normal)
	if !strings.Contains(failure, "NOT VERIFIED") {
		t.Fatal(failure)
	}
}

func TestEventCountsPreserveInteriorSpeechAndRejectInvalidWindows(t *testing.T) {
	check, timeline, capture := eventCountFixture()
	capture = activityCapture([2]int{3500, 3900}, [2]int{5000, 5500}, [2]int{8500, 8900})
	var windows [][2]int
	_, failure := eventCounts(check, timeline, func(from, to int) (string, error) {
		windows = append(windows, [2]int{from, to})
		if from < 7000 {
			return "one something else", nil
		}
		return "two", nil
	}, &capture)
	if !strings.Contains(failure, "no extra words") || windows[0] != [2]int{3300, 5700} {
		t.Fatalf("dropped interior speech: %v %s", windows, failure)
	}
	for _, change := range []func(*Check){
		func(c *Check) { c.Events = nil }, func(c *Check) { c.Events[0].Number = 0 }, func(c *Check) { c.Events[0].Number = 100 },
		func(c *Check) { c.Events[0].WithinMS = 0 }, func(c *Check) { c.Events[0].WithinMS = math.MaxInt },
		func(c *Check) { c.Events[1].Line = c.Events[0].Line }, func(c *Check) { c.Events[1].Line = 100 },
		func(c *Check) { c.Sight = 1 }, func(c *Check) { c.AfterMS = 1 },
		func(c *Check) { c.Tool = "unexpected" }, func(c *Check) { c.BeforeMS = 1 },
	} {
		c := check
		c.Events = slices.Clone(check.Events)
		change(&c)
		_, failure := eventCounts(c, timeline, func(int, int) (string, error) { t.Fatal("invalid check invoked recognizer"); return "", nil }, &capture)
		if !strings.Contains(failure, "invalid scenario check") {
			t.Fatalf("invalid contract accepted: %+v %s", c, failure)
		}
	}
	for _, change := range []func(*Timeline){
		func(s *Timeline) { s.TotalMS = 0 },
		func(s *Timeline) { s.TotalMS = maximumReplayMS + 1 },
		func(s *Timeline) { s.Spans[1].StartMS = -1 },
		func(s *Timeline) { s.Spans[1].EndMS = s.TotalMS + 1 },
		func(s *Timeline) { s.Spans[2].StartMS = s.Spans[1].EndMS - 1 },
		func(s *Timeline) { s.Spans[2].EndMS = s.Spans[2].StartMS },
	} {
		changed := timeline
		changed.Spans = slices.Clone(timeline.Spans)
		change(&changed)
		_, failure := eventCounts(check, changed, func(int, int) (string, error) { t.Fatal("invalid timeline invoked recognizer"); return "", nil }, &capture)
		if !strings.Contains(failure, "invalid scenario check") {
			t.Fatalf("invalid timeline accepted: %+v %s", changed, failure)
		}
	}
}

func TestEventCountReplayAndReviewBindObservations(t *testing.T) {
	check, _, capture := eventCountFixture()
	item := Scenario{Name: "event-count-replay", Script: []Line{{Speaker: "user", Text: "Count.", AtMS: 0}, {Speaker: "user", Text: "A cat.", AtMS: 2000}, {Speaker: "user", Text: "A dog.", AtMS: 7000}}, TrailingMS: 10000, Checks: []Check{check}}
	result, wav := replayFixture(t, item, &capture, func(from, to int) (string, error) {
		if from < 7000 {
			return "One.", nil
		}
		return "Two.", nil
	})
	if !result.Passed || len(result.Replay.Hearings) != 2 {
		t.Fatalf("fixture did not pass with both independent hearings: %+v", result)
	}
	if _, err := ReplayRecorded(t.Context(), item, result, wav); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Result){
		func(r *Result) { r.EventCounts[0].Events[0].Heard = "This is Pierce." },
		func(r *Result) { r.EventCounts[0].OutsideActiveMS++ },
		func(r *Result) { r.EventCounts[0].Events[0].HeardFromMS++ },
		func(r *Result) { r.Replay.Hearings = nil },
		func(r *Result) { r.Replay.Hearings[0].Text = "one plus extra words" },
	} {
		r := cloneReplayResult(t, result)
		change(&r)
		if _, err := ReplayRecorded(t.Context(), item, r, wav); err == nil {
			t.Fatal("changed hearing/measurement passed replay")
		}
	}
	copy := sanitizedReviewResult(result, func(s string) string { return strings.ReplaceAll(s, "One.", "[redacted]") })
	copy.EventCounts[0].Events[0].Number = 99
	if result.EventCounts[0].Events[0].Number != 1 || copy.EventCounts[0].Events[0].Heard != "[redacted]" {
		t.Fatal("review aliases observations or loses redaction")
	}
	var rendered strings.Builder
	renderReviewTranscript(&rendered, result)
	if !strings.Contains(rendered.String(), "independent hearing") || !strings.Contains(rendered.String(), "One.") {
		t.Fatal("review omits hearing evidence")
	}
}
