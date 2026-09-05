package scenario_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/scenario"
)

type unavailableContentVoice struct{ calls int }

func (voice *unavailableContentVoice) Speak(context.Context, string, string) ([]int16, error) {
	voice.calls++
	return nil, errors.New("synthesis must not run for an invalid scorer")
}

func TestPlayRejectsUndefinedAssertionsBeforeSynthesis(t *testing.T) {
	for _, checks := range [][]scenario.Check{
		nil,
		{{Kind: "silnet", Line: 0}},
		{{Kind: scenario.CheckNotSaid, Line: 0}},
		{{Kind: scenario.CheckSilent, Line: 5}},
		{{Kind: scenario.CheckReachedMenu}},
	} {
		voice := &unavailableContentVoice{}
		item := scenario.Scenario{Script: []scenario.Line{{Text: "Hello"}}, Checks: checks}
		_, err := scenario.Play(context.Background(), voice, bench.SessionConfig{}, item)
		if voice.calls != 0 || err == nil || !strings.Contains(err.Error(), "invalid scenario checks") {
			t.Fatalf("invalid scorer reached synthesis: calls=%d, err=%v", voice.calls, err)
		}
	}
}

func TestContentChecksMatchWordsRatherThanFragments(t *testing.T) {
	for _, test := range []struct {
		name, phrase, text string
		want               bool
	}{
		{"count is not none", "one", "None of those count.", false},
		{"count is not a larger number", "1", "There are 10.", false},
		{"date is not a later date", "the 3", "The deadline is the 30th.", false},
		{"unfinished is not finished", "finished", "The build is unfinished.", false},
		{"undone is not done", "done", "It remains undone.", false},
		{"punctuation around a word", "one", "One!", true},
		{"later whole match", "one", "Someone saw one.", true},
		{"Unicode boundary", "one", "éone", false},
		{"combining mark boundary", "one", "one\u0301", false},
		{"whitespace and case", "sea bass", "The SEA\n\tBASS, please.", true},
		{"punctuation assertion", "?", "How many?", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, kind := range []scenario.CheckKind{scenario.CheckSaid, scenario.CheckNotSaid} {
				item := scenario.Scenario{Checks: []scenario.Check{{Kind: kind, Line: -1, Any: []string{test.phrase}}}}
				got := scenario.Score(item, timeline(), bench.Transcript{Moments: spoke(100, 200, test.text)})
				want := test.want
				if kind == scenario.CheckNotSaid {
					want = !want
				}
				if got.Passed != want {
					t.Fatalf("%s %q in %q: passed=%v, want %v (%v)", kind, test.phrase, test.text, got.Passed, want, got.Failures)
				}
			}
		})
	}
}

func TestScorerRefusesUndefinedAssertions(t *testing.T) {
	for _, test := range []struct {
		name   string
		checks []scenario.Check
	}{
		{"no checks", nil},
		{"unknown check", []scenario.Check{{Kind: "silnet", Line: 0}}},
		{"missing alternatives", []scenario.Check{{Kind: scenario.CheckNotSaid, Line: 0}}},
		{"empty alternative", []scenario.Check{{Kind: scenario.CheckSaid, Line: 0, Any: []string{" "}}}},
		{"missing menu", []scenario.Check{{Kind: scenario.CheckReachedMenu}}},
		{"missing line", []scenario.Check{{Kind: scenario.CheckSilent, Line: 5}}},
		{"missing sight", []scenario.Check{{Kind: scenario.CheckSilent, Sight: 2}}},
		{"negative sight", []scenario.Check{{Kind: scenario.CheckSilent, Sight: -1}}},
		{"overflowed window", []scenario.Check{{Kind: scenario.CheckSilent, Line: 0, AfterMS: math.MaxInt}}},
		{"empty window", []scenario.Check{{Kind: scenario.CheckSilent, Line: 0, FromMS: 1000, AfterMS: 500}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := scenario.Score(scenario.Scenario{Checks: test.checks}, timeline(scenario.Span{EndMS: 2000}), bench.Transcript{})
			if result.Passed || len(result.Failures) == 0 {
				t.Fatalf("undefined assertion passed: %+v", result)
			}
		})
	}
}

func TestVisualContentCannotBorrowAnEarlierAnswer(t *testing.T) {
	item := scenario.Scenario{Checks: []scenario.Check{{Kind: scenario.CheckSaid, Sight: 1, Line: -1, AfterMS: 1000, Any: []string{"finished"}}}}
	line := scenario.Timeline{Sights: []int{2000}, TotalMS: 4000}
	for _, at := range []float64{1000, 2500, 3500} {
		result := scenario.Score(item, line, bench.Transcript{Moments: spoke(at, 500, "The build has finished.")})
		if result.Passed != (at == 2500) {
			t.Fatalf("visual check at %.0fms: %+v", at, result)
		}
	}
}

func TestResultsIdentifyTheScorerWithoutRelabelingHistoricalEvidence(t *testing.T) {
	result := scenario.Score(scenario.Scenario{Checks: []scenario.Check{{Kind: scenario.CheckSaid, Line: -1, Any: []string{"one"}}}}, timeline(), bench.Transcript{Moments: spoke(100, 200, "One.")})
	if result.ScorerVersion != scenario.ScorerVersion || !result.Passed {
		t.Fatalf("current scorer is unidentified: %+v", result)
	}
	payload, err := json.Marshal(result)
	if err != nil || !strings.Contains(string(payload), `"scorer_version":4`) {
		t.Fatalf("retained result lacks scorer identity: %s, %v", payload, err)
	}
	var historical scenario.Result
	if err := json.Unmarshal([]byte(`{"scenario":"old","passed":true,"transcript":{}}`), &historical); err != nil {
		t.Fatal(err)
	}
	payload, err = json.Marshal(historical)
	if err != nil || historical.ScorerVersion != 0 || strings.Contains(string(payload), "scorer_version") {
		t.Fatalf("historical result was relabeled: %s, %v", payload, err)
	}
}

func TestTranslationRequiresEveryAppointmentDetail(t *testing.T) {
	var item scenario.Scenario
	for _, candidate := range scenario.Suite() {
		if candidate.Name == "translating as they speak" {
			item = candidate
			break
		}
	}
	if item.Name == "" {
		t.Fatal("translation scenario is missing")
	}
	line := timeline(
		scenario.Span{EndMS: 5000},
		scenario.Span{StartMS: 13000, EndMS: 16000},
		scenario.Span{StartMS: 20000, EndMS: 23000},
	)
	for _, test := range []struct {
		name, first, second string
		want                bool
	}{
		{"complete", "Hello, nice to meet you.", "We'll meet tomorrow afternoon at three in the office.", true},
		{"alternative wording", "Hi, pleased to meet you!", "See you at the office tomorrow at 3 p.m.", true},
		{"greeting alone", "Hello.", "We'll meet tomorrow afternoon at three in the office.", false},
		{"single keyword", "Hello, nice to meet you.", "Tomorrow.", false},
		{"missing date", "Hello, nice to meet you.", "We'll meet at three in the afternoon at the office.", false},
		{"missing time", "Hello, nice to meet you.", "We'll meet tomorrow afternoon at the office.", false},
		{"missing afternoon", "Hello, nice to meet you.", "We'll meet tomorrow at three at the office.", false},
		{"missing place", "Hello, nice to meet you.", "We'll meet tomorrow afternoon at three.", false},
		{"wrong time", "Hello, nice to meet you.", "We'll meet tomorrow afternoon at thirteen in the office.", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			moments := append(spoke(15000, 1500, test.first), spoke(22500, 1500, test.second)...)
			result := scenario.Score(item, line, bench.Transcript{Moments: moments})
			if result.Passed != test.want {
				t.Fatalf("passed=%v, want %v: %s", result.Passed, test.want, strings.Join(result.Failures, "; "))
			}
		})
	}
}
