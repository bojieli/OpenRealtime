package bench_test

import (
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func TestTaskOutcomeValidationRejectsImpossibleAndNoncanonicalStates(t *testing.T) {
	valid := []bench.TaskOutcome{
		{ID: "completed-pass", Completed: true, Passed: true,
			Metrics: map[string]float64{"latency_ms": 12.5},
			Notes:   map[string]string{"detail": "", "multiline": "first\n\tsecond"}},
		{ID: "completed-failure", Completed: true},
		// An attempt can fail before the suite has a useful provider error. It is
		// still an honest incomplete row and must remain representable.
		{ID: "early-setup-failure"},
		{ID: "provider-failure", Error: "provider returned 500\nrequest aborted"},
		{ID: "attestation-failure", Completed: true, ExecutionError: "inspector unavailable"},
	}
	for _, outcome := range valid {
		if err := outcome.Validate(); err != nil {
			t.Errorf("valid outcome %q: %v", outcome.ID, err)
		}
	}

	tests := []struct {
		name    string
		outcome bench.TaskOutcome
		want    string
	}{
		{name: "missing ID", outcome: bench.TaskOutcome{}, want: "ID"},
		{name: "padded ID", outcome: bench.TaskOutcome{ID: " case"}, want: "noncanonical"},
		{name: "control in ID", outcome: bench.TaskOutcome{ID: "case\nother"}, want: "control"},
		{name: "invalid ID UTF-8", outcome: bench.TaskOutcome{ID: string([]byte{0xff})}, want: "UTF-8"},
		{name: "oversized ID", outcome: bench.TaskOutcome{ID: strings.Repeat("i", 8<<10)}, want: "oversized"},
		{name: "completed with error", outcome: bench.TaskOutcome{
			ID: "case", Completed: true, Error: "failed anyway",
		}, want: "completed task outcome carries an error"},
		{name: "incomplete pass", outcome: bench.TaskOutcome{
			ID: "case", Passed: true,
		}, want: "incomplete task outcome is marked passed"},
		{name: "padded error", outcome: bench.TaskOutcome{
			ID: "case", Error: " provider failed ",
		}, want: "whitespace"},
		{name: "noncanonical error control", outcome: bench.TaskOutcome{
			ID: "case", Error: "provider\rfailed",
		}, want: "control"},
		{name: "oversized error", outcome: bench.TaskOutcome{
			ID: "case", Error: strings.Repeat("e", 2<<20),
		}, want: "oversized"},
		{name: "nonfinite NaN metric", outcome: bench.TaskOutcome{
			ID: "case", Metrics: map[string]float64{"latency_ms": math.NaN()},
		}, want: "not finite"},
		{name: "nonfinite infinite metric", outcome: bench.TaskOutcome{
			ID: "case", Metrics: map[string]float64{"latency_ms": math.Inf(1)},
		}, want: "not finite"},
		{name: "empty metric name", outcome: bench.TaskOutcome{
			ID: "case", Metrics: map[string]float64{"": 1},
		}, want: "metric name"},
		{name: "padded note name", outcome: bench.TaskOutcome{
			ID: "case", Notes: map[string]string{" detail": "value"},
		}, want: "note name"},
		{name: "oversized note name", outcome: bench.TaskOutcome{
			ID: "case", Notes: map[string]string{strings.Repeat("k", 2<<10): "value"},
		}, want: "oversized"},
		{name: "padded note value", outcome: bench.TaskOutcome{
			ID: "case", Notes: map[string]string{"detail": " value"},
		}, want: "whitespace"},
		{name: "noncanonical note control", outcome: bench.TaskOutcome{
			ID: "case", Notes: map[string]string{"detail": "one\rtwo"},
		}, want: "control"},
		{name: "oversized note value", outcome: bench.TaskOutcome{
			ID: "case", Notes: map[string]string{"detail": strings.Repeat("v", 2<<20)},
		}, want: "oversized"},
		{name: "too many metrics", outcome: bench.TaskOutcome{
			ID: "case", Metrics: outcomeMetrics(5_000),
		}, want: "maximum"},
		{name: "too many notes", outcome: bench.TaskOutcome{
			ID: "case", Notes: outcomeNotes(5_000),
		}, want: "maximum"},
		{name: "evidence and execution error", outcome: bench.TaskOutcome{
			ID: "case", Completed: true, Execution: &bench.ExecutionEvidence{},
			ExecutionError: "inspector unavailable",
		}, want: "both execution evidence"},
		{name: "invalid execution evidence", outcome: bench.TaskOutcome{
			ID: "case", Completed: true, Execution: &bench.ExecutionEvidence{},
		}, want: "execution evidence"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.outcome.Validate()
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Validate() error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func outcomeMetrics(count int) map[string]float64 {
	result := make(map[string]float64, count)
	for index := 0; index < count; index++ {
		result["metric-"+strconv.Itoa(index)] = float64(index)
	}
	return result
}

func outcomeNotes(count int) map[string]string {
	result := make(map[string]string, count)
	for index := 0; index < count; index++ {
		result["note-"+strconv.Itoa(index)] = "value"
	}
	return result
}

func TestReportableRejectsACompletedTaskWithAnError(t *testing.T) {
	result := complete("forged", 1, 1)
	result.Tasks[0].Error = "hidden failure"
	result.Finish()
	if err := result.Reportable(); err == nil || !strings.Contains(err.Error(), "outcome is invalid") {
		t.Fatalf("a contradictory task outcome became reportable: %v", err)
	}
}

func complete(name string, passed, total int) bench.Result {
	result := bench.Result{
		Suite: "test", Cell: bench.Reference(), Expected: total,
		Provenance: bench.Provenance{
			Revision: "abc123", ExecutableSHA256: "deadbeef",
		},
	}
	result.Cell.Name = name
	for index := 0; index < total; index++ {
		result.Tasks = append(result.Tasks, bench.TaskOutcome{
			ID: string(rune('a' + index)), Completed: true, Passed: index < passed,
			Metrics: map[string]float64{"first_audio_ms": float64(500 + index*100)},
		})
	}
	result.Finish()
	return result
}

// The first reporting rule, enforced rather than remembered: a partially
// executed cell is a different experiment, not a smaller one.
func TestAnIncompleteCellIsNotReportable(t *testing.T) {
	result := complete("partial", 3, 5)
	result.Tasks = result.Tasks[:3]
	result.Finish()
	if result.Summary.Complete {
		t.Fatal("a cell missing tasks is not complete")
	}
	err := result.Reportable()
	if err == nil || !strings.Contains(err.Error(), "not attempted") {
		t.Fatalf("the refusal must say what is missing: %v", err)
	}

	failed := complete("failing", 4, 5)
	failed.Tasks[4].Completed = false
	failed.Tasks[4].Error = "provider timeout"
	failed.Finish()
	if failed.Summary.Complete {
		t.Fatal("a cell with a failed task is not complete")
	}
	if !strings.Contains(failed.Reportable().Error(), "did not complete") {
		t.Fatalf("the refusal must name the failure: %v", failed.Reportable())
	}
}

// A number that cannot be traced to a build is a number that will eventually
// be wrong about which build it describes.
func TestAnUntraceableRunIsNotReportable(t *testing.T) {
	modified := complete("modified", 5, 5)
	modified.Provenance.Modified = true
	if err := modified.Reportable(); err == nil || !strings.Contains(err.Error(), "not reproducible") {
		t.Fatalf("a modified working tree must not publish: %v", err)
	}
	unhashed := complete("unhashed", 5, 5)
	unhashed.Provenance.ExecutableSHA256 = ""
	if err := unhashed.Reportable(); err == nil {
		t.Fatal("a run whose executable cannot be hashed must not publish")
	}
	clean := complete("clean", 5, 5)
	if err := clean.Reportable(); err != nil {
		t.Fatalf("a complete, traceable cell must be reportable: %v", err)
	}
}

func TestALegacyCompletedTaskWithASessionFailureIsNotReportable(t *testing.T) {
	legacy := complete("legacy", 5, 5)
	legacy.Tasks[2].Notes = map[string]string{
		"session_failure": "recogniser quota exhausted",
	}
	if err := legacy.Reportable(); err == nil || !strings.Contains(err.Error(), "legacy scorer") {
		t.Fatalf("a session outage recorded as a completed task became reportable: %v", err)
	}
}

func TestPairedCellsChangeExactlyOneFactor(t *testing.T) {
	variant, err := bench.Vary(bench.FactorCognition, "fast-only")
	if err != nil {
		t.Fatalf("vary: %v", err)
	}
	factor, paired := bench.Paired(bench.Reference(), variant)
	if !paired || factor != bench.FactorCognition {
		t.Fatalf("expected a pairing on F2, got %q paired=%v", factor, paired)
	}
	// A cell that already has the level is not a variation.
	if _, err := bench.Vary(bench.FactorCognition, "fast+slow"); err == nil {
		t.Fatal("varying a factor to the value it already has is not an experiment")
	}
	if _, err := bench.Vary("F99", "x"); err == nil {
		t.Fatal("an unknown factor must be refused")
	}
}

// Two cells that differ in three factors do not measure any one of them, and
// the harness refuses rather than leaving that to whoever reads the numbers.
func TestAMultiFactorComparisonIsRefused(t *testing.T) {
	baseline := complete("reference", 4, 5)
	variant := complete("variant", 5, 5)
	variant.Cell.Levels[bench.FactorCognition] = "fast-only"
	variant.Cell.Levels[bench.FactorCadence] = "50ms"
	variant.Cell.Levels[bench.FactorFloor] = "model"

	comparison := bench.Pair(baseline, variant)
	if comparison.Reportable {
		t.Fatal("a three-factor difference is not a measurement of one factor")
	}
	if !strings.Contains(comparison.Refusal, "3 factors") {
		t.Fatalf("the refusal must say how many differed: %q", comparison.Refusal)
	}
}

func TestCrossSuiteComparisonIsRefused(t *testing.T) {
	baseline := complete("reference", 4, 5)
	variant := complete("variant", 5, 5)
	variant.Suite = "another"
	variant.Cell.Levels[bench.FactorCognition] = "fast-only"

	comparison := bench.Pair(baseline, variant)
	if comparison.Reportable {
		t.Fatal("two suites measuring different things do not compare")
	}
	if !strings.Contains(comparison.Refusal, "different things") {
		t.Fatalf("unexpected refusal %q", comparison.Refusal)
	}
}

func TestAValidPairingIsReported(t *testing.T) {
	baseline := complete("reference", 3, 5)
	variant := complete("fast-only", 5, 5)
	variant.Cell.Levels[bench.FactorCognition] = "fast-only"

	comparison := bench.Pair(baseline, variant)
	if !comparison.Reportable {
		t.Fatalf("a clean single-factor pairing must report: %q", comparison.Refusal)
	}
	if comparison.Factor != bench.FactorCognition {
		t.Fatalf("unexpected factor %q", comparison.Factor)
	}
	if comparison.Difference <= 0 {
		t.Fatalf("expected a positive difference, got %f", comparison.Difference)
	}
}

func TestComparisonRefusesAnUnrecordedBuildOrMachineChange(t *testing.T) {
	for name, mutate := range map[string]func(*bench.Result){
		"revision": func(result *bench.Result) { result.Provenance.Revision = "different" },
		"executable": func(result *bench.Result) {
			result.Provenance.ExecutableSHA256 = "different"
		},
		"machine": func(result *bench.Result) { result.Provenance.Machine.CPU = "different" },
	} {
		t.Run(name, func(t *testing.T) {
			baseline := complete("reference", 4, 5)
			variant := complete("variant", 4, 5)
			variant.Cell.Levels[bench.FactorCognition] = "fast-only"
			mutate(&variant)
			comparison := bench.Pair(baseline, variant)
			if comparison.Reportable || !strings.Contains(comparison.Refusal, "factor") &&
				!strings.Contains(comparison.Refusal, "same build") {
				t.Fatalf("uncontrolled %s change was reportable: %+v", name, comparison)
			}
		})
	}
}

// A mean without its tail describes a system nobody is using.
func TestLatencyIsSummarisedAsADistribution(t *testing.T) {
	result := complete("reference", 5, 5)
	distribution, present := result.Summary.Distributions["first_audio_ms"]
	if !present {
		t.Fatal("a metric must become a distribution")
	}
	if distribution.Count != 5 || distribution.Min != 500 || distribution.Max != 900 {
		t.Fatalf("unexpected distribution %+v", distribution)
	}
	if distribution.P50 != 700 {
		t.Fatalf("expected a median of 700, got %f", distribution.P50)
	}
	if distribution.P99 <= distribution.P50 {
		t.Fatal("the tail must be reported separately from the middle")
	}
}

func TestSummariseHandlesOneSampleAndNone(t *testing.T) {
	if got := bench.Summarise(nil); got.Count != 0 {
		t.Fatalf("no samples is no distribution, got %+v", got)
	}
	single := bench.Summarise([]float64{42})
	if single.Count != 1 || single.P50 != 42 || single.P99 != 42 {
		t.Fatalf("unexpected single-sample distribution %+v", single)
	}
}

func TestCellIdentityIsStable(t *testing.T) {
	if bench.Reference().ID() != bench.Reference().ID() {
		t.Fatal("the same configuration must have the same identity")
	}
	variant, _ := bench.Vary(bench.FactorCadence, "50ms")
	if variant.ID() == bench.Reference().ID() {
		t.Fatal("a different configuration must have a different identity")
	}
	if !strings.Contains(bench.Reference().Describe(), "F1=cascade") {
		t.Fatalf("a cell must describe itself completely: %q", bench.Reference().Describe())
	}
}

func TestTranscriptReadsAConversation(t *testing.T) {
	transcript := bench.Transcript{Moments: []bench.Moment{
		{AtMS: 100, Kind: bench.MomentSpeechStarted},
		{AtMS: 900, Kind: bench.MomentSpeechStopped},
		{AtMS: 950, Kind: bench.MomentTranscript, Text: "what is my balance"},
		{AtMS: 1400, Kind: bench.MomentAgentText, Text: "Checking"},
		{AtMS: 1500, Kind: bench.MomentAgentAudio, AudioMS: 100},
		{AtMS: 1600, Kind: bench.MomentAgentAudio, AudioMS: 100},
		{AtMS: 1700, Kind: bench.MomentResponseDone},
		{AtMS: 1800, Kind: bench.MomentToolCall, Name: "get_balance"},
	}}
	if turns := transcript.UserTurns(); len(turns) != 1 || turns[0] != "what is my balance" {
		t.Fatalf("unexpected user turns %v", turns)
	}
	if turns := transcript.AgentTurns(); len(turns) != 1 || turns[0] != "Checking" {
		t.Fatalf("unexpected agent turns %v", turns)
	}
	if calls := transcript.ToolCalls(); len(calls) != 1 || calls[0] != "get_balance" {
		t.Fatalf("unexpected tool calls %v", calls)
	}
	if audio := transcript.AudioBetween(1450, 1650); audio != 200 {
		t.Fatalf("expected 200 ms of audio in the window, got %f", audio)
	}
	latency, found := transcript.FirstAudioAfter(900)
	if !found || latency != 600 {
		t.Fatalf("expected 600 ms to first audio, got %f found=%v", latency, found)
	}
	if _, found := transcript.FirstAudioAfter(5000); found {
		t.Fatal("there is no audio after the conversation ended")
	}
}

// A count rendered as a duration is not a smaller mistake than a wrong number:
// it is a number that means nothing and looks like it means something.
func TestDistributionsCarryTheirUnit(t *testing.T) {
	result := bench.Result{
		Suite: "units", Cell: bench.Reference(), Expected: 2,
		Provenance: bench.Capture().Complete(),
		Tasks: []bench.TaskOutcome{
			{ID: "a", Completed: true, Passed: true, Metrics: map[string]float64{
				"response_latency_ms": 120, "missed_turns": 2, "agent_cost_usd": 0.01,
			}},
			{ID: "b", Completed: true, Passed: true, Metrics: map[string]float64{
				"response_latency_ms": 240, "missed_turns": 4, "agent_cost_usd": 0.03,
			}},
		},
	}
	result.Finish()

	expected := map[string]string{
		"response_latency_ms": "ms", "missed_turns": "count", "agent_cost_usd": "usd",
	}
	for metric, unit := range expected {
		distribution, present := result.Summary.Distributions[metric]
		if !present {
			t.Fatalf("%s is missing from the summary", metric)
		}
		if distribution.Unit != unit {
			t.Errorf("%s must be %q, got %q", metric, unit, distribution.Unit)
		}
	}
	if formatted := result.Summary.Distributions["missed_turns"].Format(3); strings.Contains(formatted, "ms") {
		t.Fatalf("a count must not be rendered as a duration, got %q", formatted)
	}
	if formatted := result.Summary.Distributions["response_latency_ms"].Format(180); formatted != "180 ms" {
		t.Fatalf("a duration must carry its unit, got %q", formatted)
	}
}
