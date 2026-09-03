package fdbv3

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func TestReleaseEvidenceScoresTimingSafetyAndCleanSpeech(t *testing.T) {
	task := releaseEvidenceTask()
	call, result := releaseEvidenceCall(t, `{"order_id":"BOB12"}`, 1200, 1237)
	transcript := bench.Transcript{
		PlaybackMS: 1000,
		Moments: []bench.Moment{
			{AtMS: 0, Kind: bench.MomentReady},
			{AtMS: 1100, Kind: bench.MomentAgentText, Text: "I'll check that."},
			{AtMS: 1150, Kind: bench.MomentResponseDone},
			call,
			result,
		},
	}

	outcome, observed, err := scoreTaskTranscript(task, transcript, releaseEvidenceInventory())
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Completed || !outcome.Passed || len(observed) != 1 {
		t.Fatalf("scored outcome = %+v, observed=%+v", outcome, observed)
	}
	for metric, want := range map[string]float64{
		"unintended_effect_count":              0,
		"control_markup_speech_count":          0,
		"playback_start_to_first_tool_call_ms": 1200,
		"max_tool_call_to_result_latency_ms":   37,
		"release_validity_observed_calls":      1,
		"release_validity_argument_matches":    1,
	} {
		if got := outcome.Metrics[metric]; got != want {
			t.Errorf("metric %s = %v, want %v; all=%+v", metric, got, want, outcome.Metrics)
		}
	}
	if outcome.Notes["release_evidence_scorer"] != releaseEvidenceScorerIdentity ||
		!strings.Contains(outcome.Notes["tool_latency_evidence"], "1 retained") ||
		!strings.Contains(outcome.Notes["control_markup_speech_evidence"], "1 retained") {
		t.Fatalf("release evidence notes = %+v", outcome.Notes)
	}
}

func TestReleaseEvidenceCountsEveryUnpairedAttemptedEffect(t *testing.T) {
	task := releaseEvidenceTask()
	firstCall, firstResult := releaseEvidenceCall(t, `{"order_id":"BOB12"}`, 10, 20)
	secondCall, secondResult := releaseEvidenceCall(t, `{"order_id":"BOB12"}`, 30, 50)
	secondCall.CallID = "call-2"
	secondResult.CallID = "call-2"
	transcript := bench.Transcript{
		PlaybackMS: 1000,
		Moments: []bench.Moment{
			{AtMS: 0, Kind: bench.MomentReady},
			firstCall, firstResult, secondCall, secondResult,
		},
	}

	outcome, _, err := scoreTaskTranscript(task, transcript, releaseEvidenceInventory())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Passed || outcome.Metrics["release_validity_observed_calls"] != 2 ||
		outcome.Metrics["unintended_effect_count"] != 1 ||
		outcome.Metrics["max_tool_call_to_result_latency_ms"] != 20 {
		t.Fatalf("extra effect evidence = %+v", outcome)
	}
}

func TestReleaseEvidenceCountsNarrowMarkupAcrossTextChunks(t *testing.T) {
	task := releaseEvidenceTask()
	transcript := bench.Transcript{
		PlaybackMS: 1000,
		Moments: []bench.Moment{
			{AtMS: 0, Kind: bench.MomentReady},
			{AtMS: 10, Kind: bench.MomentAgentText, Text: "Ordinary prose about a tool call. <tool_"},
			{AtMS: 11, Kind: bench.MomentAgentText, Text: `CALL>{"name":"track_order"}</tool_call>`},
			{AtMS: 12, Kind: bench.MomentResponseDone},
			{AtMS: 13, Kind: bench.MomentAgentText, Text: "<function_"},
			// A response boundary is a parser reset, so fragments on its two
			// sides cannot be joined into invented markup.
			{AtMS: 14, Kind: bench.MomentResponseDone},
			{AtMS: 15, Kind: bench.MomentAgentText, Text: "call>{}"},
			{AtMS: 16, Kind: bench.MomentAgentText, Text: `<function=lookup>{}</function>`},
			{AtMS: 17, Kind: bench.MomentResponseDone},
		},
	}

	outcome, _, err := scoreTaskTranscript(task, transcript, releaseEvidenceInventory())
	if err != nil {
		t.Fatal(err)
	}
	if got := outcome.Metrics["control_markup_speech_count"]; got != 2 {
		t.Fatalf("control markup speech count = %v, want 2; notes=%+v", got, outcome.Notes)
	}
	if _, present := outcome.Metrics["playback_start_to_first_tool_call_ms"]; present {
		t.Fatal("a task with no retained call acquired a zero tool-call timestamp")
	}
	if _, present := outcome.Metrics["max_tool_call_to_result_latency_ms"]; present {
		t.Fatal("a task with no retained call acquired a zero tool-result latency")
	}
	if !strings.Contains(outcome.Notes["tool_latency_evidence"], "unavailable") {
		t.Fatalf("missing-call latency reason = %q", outcome.Notes["tool_latency_evidence"])
	}
}

func TestReleaseEvidenceDistinguishesSilentToolOnlySpeechEvidence(t *testing.T) {
	task := releaseEvidenceTask()
	call, result := releaseEvidenceCall(t, `{"order_id":"BOB12"}`, 10, 20)
	transcript := bench.Transcript{
		PlaybackMS: 1000,
		Moments: []bench.Moment{
			{AtMS: 0, Kind: bench.MomentReady}, call, result,
		},
	}
	outcome, _, err := scoreTaskTranscript(task, transcript, releaseEvidenceInventory())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Metrics["control_markup_speech_count"] != 0 ||
		!strings.Contains(outcome.Notes["control_markup_speech_evidence"], "silent response") {
		t.Fatalf("silent response evidence = %+v", outcome)
	}
}

func TestReleaseEvidenceRejectsMissingOrInvalidTranscriptTiming(t *testing.T) {
	valid := bench.Transcript{
		PlaybackMS: 1000,
		Moments:    []bench.Moment{{AtMS: 0, Kind: bench.MomentReady}},
	}
	for _, testCase := range []struct {
		name   string
		mutate func(*bench.Transcript)
	}{
		{name: "missing moments", mutate: func(value *bench.Transcript) { value.Moments = nil }},
		{name: "missing ready", mutate: func(value *bench.Transcript) {
			value.Moments[0].Kind = bench.MomentAgentText
		}},
		{name: "duplicate ready", mutate: func(value *bench.Transcript) {
			value.Moments = append(value.Moments, bench.Moment{AtMS: 1, Kind: bench.MomentReady})
		}},
		{name: "invalid playback", mutate: func(value *bench.Transcript) { value.PlaybackMS = math.NaN() }},
		{name: "invalid moment timestamp", mutate: func(value *bench.Transcript) {
			value.Moments = append(value.Moments, bench.Moment{AtMS: math.Inf(1), Kind: bench.MomentAgentText})
		}},
		{name: "regressing timestamp", mutate: func(value *bench.Transcript) {
			value.Moments = append(value.Moments,
				bench.Moment{AtMS: 2, Kind: bench.MomentAgentText, Text: "later"},
				bench.Moment{AtMS: 1, Kind: bench.MomentResponseDone})
		}},
		{name: "invalid assistant UTF-8", mutate: func(value *bench.Transcript) {
			value.Moments = append(value.Moments,
				bench.Moment{AtMS: 1, Kind: bench.MomentAgentText, Text: string([]byte{0xff})})
		}},
		{name: "outstanding work", mutate: func(value *bench.Transcript) {
			value.OutstandingTools = 1
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			transcript := valid
			transcript.Moments = append([]bench.Moment(nil), valid.Moments...)
			testCase.mutate(&transcript)
			outcome, observed, err := scoreTaskTranscript(
				releaseEvidenceTask(), transcript, releaseEvidenceInventory(),
			)
			if err == nil || len(outcome.Metrics) != 0 || len(observed) != 0 {
				t.Fatalf("invalid evidence scored cleanly: err=%v outcome=%+v observed=%+v",
					err, outcome, observed)
			}
		})
	}
}

func TestRecoveredReleaseEvidenceExactlyMatchesLiveScoring(t *testing.T) {
	task := releaseEvidenceTask()
	call, result := releaseEvidenceCall(t, `{"order_id":"BOB12"}`, 1200, 1225)
	transcript := bench.Transcript{
		PlaybackMS: 1000,
		Moments: []bench.Moment{
			{AtMS: 0, Kind: bench.MomentReady},
			{AtMS: 1100, Kind: bench.MomentAgentText, Text: "<tool_"},
			{AtMS: 1101, Kind: bench.MomentAgentText, Text: `call>{"name":"track_order"}</tool_call>`},
			{AtMS: 1102, Kind: bench.MomentResponseDone},
			call, result,
		},
	}
	inventory := releaseEvidenceInventory()
	live, _, err := scoreTaskTranscript(task, transcript, inventory)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := reconstructRecoveredOutcome(task, transcript, inventory)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(live, recovered) {
		t.Fatalf("recovered score differs\nlive:      %+v\nrecovered: %+v", live, recovered)
	}
}

func releaseEvidenceTask() Task {
	return Task{
		ID: "release-evidence-fixture", Domain: "test", Difficulty: "easy",
		Expected: []ExpectedCall{{
			Function: "track_order", Args: json.RawMessage(`{"order_id":"BOB12"}`),
		}},
	}
}

func releaseEvidenceInventory() releasedDatasetInventory {
	return releasedDatasetInventory{
		Revision: "release-evidence-test", ArtifactDigest: strings.Repeat("a", 64),
		TaskNames: []string{"release-evidence-fixture"},
	}
}

func releaseEvidenceCall(
	t *testing.T, arguments string, callAtMS, resultAtMS float64,
) (bench.Moment, bench.Moment) {
	t.Helper()
	result, _ := visibleSimulatorResult("track_order", json.RawMessage(arguments))
	if len(result) == 0 {
		t.Fatal("fixture simulator returned no result")
	}
	return bench.Moment{
			AtMS: callAtMS, Kind: bench.MomentToolCall, CallID: "call-1",
			Name: "track_order", Arguments: arguments,
		}, bench.Moment{
			AtMS: resultAtMS, Kind: bench.MomentToolResult, CallID: "call-1",
			Name: "track_order", Text: string(result),
		}
}
