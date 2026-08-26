package realtimecu

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
)

func TestScoreSeparatesCorrectnessFromDeadline(t *testing.T) {
	item := Case{Task: Task{CueAt: time.Second, Deadline: 400 * time.Millisecond}, Grounding: GroundingPixel}
	base := bench.TaskOutcome{Metrics: map[string]float64{}, Notes: map[string]string{}}
	transcript := bench.Transcript{Moments: []bench.Moment{
		{AtMS: 100, Kind: bench.MomentReady},
		{AtMS: 1700, Kind: bench.MomentToolCall},
	}}
	result := score(base, item, time.Now(), PageResult{
		Complete: true, Success: true, CompletedAtMS: 1600, Reason: "right but late",
	}, nil, transcript)
	if result.Passed || result.Metrics["correct_action_rate"] != 1 || result.Metrics["deadline_miss_count"] != 1 {
		t.Fatalf("late correctness must remain visible without passing: %+v", result)
	}
}

func TestScoreRetainsRecognizedUserTurnsForFailureDiagnosis(t *testing.T) {
	base := bench.TaskOutcome{Metrics: map[string]float64{}, Notes: map[string]string{}}
	transcript := bench.Transcript{Moments: []bench.Moment{
		{AtMS: 10, Kind: bench.MomentTranscript, Text: "enter incident alpha seven"},
		{AtMS: 20, Kind: bench.MomentTranscript, Text: "submit it"},
	}}
	result := score(base, Case{}, time.Now(), PageResult{}, nil, transcript)
	var turns []string
	if err := json.Unmarshal([]byte(result.Notes["recognized_user_turns"]), &turns); err != nil {
		t.Fatalf("recognized user turns are not retained as JSON: %v", err)
	}
	if len(turns) != 2 || turns[0] != "enter incident alpha seven" || turns[1] != "submit it" {
		t.Fatalf("unexpected recognized user turns: %v", turns)
	}
}

func TestObservationLatencyPairsEachObservationWithItsSource(t *testing.T) {
	metrics := map[string]float64{}
	item := Case{Task: Task{CueAt: time.Second, Camera: true}}
	transcript := bench.Transcript{Moments: []bench.Moment{
		{AtMS: 0, Kind: bench.MomentReady},
		{AtMS: 900, Kind: bench.MomentVideoFrame, Source: "screen"},
		{AtMS: 1000, Kind: bench.MomentVideoFrame, Source: "camera"},
		{AtMS: 1250, Kind: bench.MomentObservation, Source: "camera"},
	}}
	observationMetrics(metrics, item, transcript, transcript.Moments[0], true)
	if metrics["frame_to_observation_latency_ms"] != 250 || metrics["cue_to_observation_latency_ms"] != 250 {
		t.Fatalf("unexpected metrics: %v", metrics)
	}
}
