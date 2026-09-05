package fdb

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func trial(id string, completed, passed bool, applicability bench.Applicability) bench.TaskOutcome {
	return bench.TaskOutcome{
		ID: id, Completed: completed, Passed: passed, Applicability: applicability,
		Notes: map[string]string{"category": string(BackgroundSpeech)},
	}
}

// A recording that agrees with itself and one that does not are different
// facts, and the pass count cannot tell them apart.
func TestStabilitySeparatesASettledVerdictFromANoisyOne(t *testing.T) {
	result := bench.Result{Tasks: []bench.TaskOutcome{
		trial("background_speech/1#1", true, true, bench.Applicable),
		trial("background_speech/1#2", true, true, bench.Applicable),
		trial("background_speech/2#1", true, true, bench.Applicable),
		trial("background_speech/2#2", true, false, bench.Applicable),
		trial("background_speech/3#1", true, false, bench.Applicable),
		trial("background_speech/3#2", true, false, bench.Applicable),
		trial("background_speech/4#1", true, false, bench.NotApplicable),
		trial("background_speech/4#2", true, false, bench.NotApplicable),
		trial("background_speech/5#1", true, false, bench.NotApplicable),
		trial("background_speech/5#2", true, true, bench.Applicable),
	}}
	got := Measure(result)
	want := Stability{
		Recordings: 5, Trials: 2, AlwaysPassed: 1, AlwaysFailed: 1, AlwaysNotApplicable: 1,
		Mixed: []string{"background_speech/2", "background_speech/5"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stability = %+v, want %+v", got, want)
	}
	// The two runs that disagree still sum to a steady-looking total, which is
	// the whole reason this exists.
	result.Expected = 10
	result.Finish()
	if result.Summary.Passed != 4 {
		t.Fatalf("summary passed = %d, want 4", result.Summary.Passed)
	}
}

// A single run has no trial suffix and must read exactly as it always did,
// with every recording settled because none had the chance to disagree.
func TestStabilityOfASingleRunClaimsNothing(t *testing.T) {
	result := bench.Result{Tasks: []bench.TaskOutcome{
		trial("background_speech/1", true, true, bench.Applicable),
		trial("background_speech/2", true, false, bench.Applicable),
	}}
	got := Measure(result)
	if got.Trials != 1 || len(got.Mixed) != 0 || got.AlwaysPassed != 1 || got.AlwaysFailed != 1 {
		t.Fatalf("single-run stability = %+v", got)
	}
	if RecordingID("background_speech/1") != "background_speech/1" ||
		RecordingID("background_speech/1#3") != "background_speech/1" {
		t.Fatal("recording identity does not survive the trial suffix")
	}
}

// An incomplete attempt is its own verdict: a recording that reached no
// evaluation once and passed once has not settled, and calling it settled
// would hide exactly the infrastructure trouble the suite refuses to score.
func TestStabilityTreatsAnIncompleteAttemptAsADisagreement(t *testing.T) {
	result := bench.Result{Tasks: []bench.TaskOutcome{
		trial("user_interruption/1#1", false, false, ""),
		trial("user_interruption/1#2", true, true, bench.Applicable),
	}}
	got := Measure(result)
	if len(got.Mixed) != 1 || got.Mixed[0] != "user_interruption/1" || got.AlwaysPassed != 0 {
		t.Fatalf("incomplete attempt was absorbed: %+v", got)
	}
}

// Every attempt's execution evidence names that attempt.
//
// A repeated run has several attempts at one recording, and reportability
// compares each task's identity against the scope its evidence carries. The
// scope was the recording's own identity, so the first stability run refused
// itself: fifty attempts after the first, each looking like evidence for a
// different task. This is that refusal, driven from the same check.
func TestARepeatedAttemptMustCarryItsOwnExecutionScope(t *testing.T) {
	attempt := func(id, scope string) bench.TaskOutcome {
		return bench.TaskOutcome{
			ID: id, Completed: true, Passed: true, Applicability: bench.Applicable,
			Notes:     map[string]string{"category": string(BackgroundSpeech)},
			Execution: &bench.ExecutionEvidence{FormatVersion: 1, Kind: "graph-native", Scope: scope},
		}
	}
	drifted := bench.Result{Suite: "fdb-v1.5", Expected: 2, Tasks: []bench.TaskOutcome{
		attempt("background_speech/1#1", "background_speech/1"),
		attempt("background_speech/1#2", "background_speech/1"),
	}}
	drifted.Finish()
	err := drifted.Reportable()
	if err == nil || !strings.Contains(err.Error(), "execution evidence for scope") {
		t.Fatalf("attempts carrying the recording's scope were accepted: %v", err)
	}

	owned := bench.Result{Suite: "fdb-v1.5", Expected: 2, Tasks: []bench.TaskOutcome{
		attempt("background_speech/1#1", "background_speech/1#1"),
		attempt("background_speech/1#2", "background_speech/1#2"),
	}}
	owned.Finish()
	if err := owned.Reportable(); err != nil &&
		strings.Contains(err.Error(), "execution evidence for scope") {
		t.Fatalf("an attempt naming itself was still refused: %v", err)
	}
}
