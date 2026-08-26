package realtimecu

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/computeruse"
)

func score(
	outcome bench.TaskOutcome, item Case, started time.Time, page PageResult,
	actions []ActionRecord, transcript bench.Transcript,
) bench.TaskOutcome {
	outcome.Completed = true
	outcome.Notes["page_result"] = page.Reason
	if transcript.Failure != "" {
		outcome.Notes["session_failure"] = transcript.Failure
	}
	if turns := transcript.UserTurns(); len(turns) > 0 {
		if encoded, err := json.Marshal(turns); err == nil {
			// Keep the recognizer's evidence beside the action trace. A bad
			// literal may be an ASR failure or a reasoning failure; without the
			// heard input those two causes are indistinguishable after a run.
			outcome.Notes["recognized_user_turns"] = string(encoded)
		}
	}
	if encoded, err := json.Marshal(actions); err == nil {
		outcome.Notes["actions"] = string(encoded)
	}
	outcome.Metrics["task_success_rate"] = truth(page.Complete && page.Success)
	outcome.Metrics["correct_action_rate"] = truth(page.Success)
	outcome.Metrics["action_count"] = float64(len(actions))
	outcome.Metrics["invalid_action_count"] = float64(countInvalid(actions))
	outcome.Metrics["grounding_error_count"] = float64(countGroundingErrors(actions))
	outcome.Metrics["observation_count"] = float64(countMoments(transcript, bench.MomentObservation))
	outcome.Metrics["video_frame_count"] = float64(countMoments(transcript, bench.MomentVideoFrame))
	if page.Complete {
		outcome.Metrics["task_completion_ms"] = page.CompletedAtMS
		outcome.Metrics["cue_to_completion_ms"] = page.CompletedAtMS - milliseconds(item.Task.CueAt)
	}

	readyAt, ready := firstMoment(transcript, bench.MomentReady, "")
	firstCall, called := firstEffect(transcript)
	deadlineMissed := true
	if ready && called {
		reaction := firstCall.AtMS - (readyAt.AtMS + milliseconds(item.Task.CueAt))
		outcome.Metrics["cue_to_action_latency_ms"] = reaction
		outcome.Metrics["premature_action_count"] = truth(reaction < -250)
		deadlineMissed = reaction < -250 || reaction > milliseconds(item.Task.Deadline)
		if stopped, ok := lastMomentBefore(transcript, bench.MomentSpeechStopped, firstCall.AtMS); ok {
			outcome.Metrics["speech_end_to_action_latency_ms"] = firstCall.AtMS - stopped.AtMS
		}
	} else {
		outcome.Metrics["premature_action_count"] = 0
	}
	if anyCall, ok := firstMoment(transcript, bench.MomentToolCall, ""); ready && ok {
		outcome.Metrics["cue_to_first_tool_latency_ms"] =
			anyCall.AtMS - (readyAt.AtMS + milliseconds(item.Task.CueAt))
	}
	if page.Complete && page.Success {
		completionReaction := page.CompletedAtMS - milliseconds(item.Task.CueAt)
		deadlineMissed = completionReaction < -250 || completionReaction > milliseconds(item.Task.Deadline)
	}
	outcome.Metrics["deadline_miss_count"] = truth(deadlineMissed)
	outcome.Passed = page.Complete && page.Success && !deadlineMissed

	if len(actions) > 0 {
		var execution []float64
		for _, action := range actions {
			if !action.CompletedAt.IsZero() && !action.ReceivedAt.IsZero() {
				execution = append(execution, float64(action.CompletedAt.Sub(action.ReceivedAt).Microseconds())/1000)
			}
		}
		if len(execution) > 0 {
			outcome.Metrics["action_execution_ms"] = bench.Summarise(execution).P50
		}
	}
	observationMetrics(outcome.Metrics, item, transcript, readyAt, ready)
	return outcome
}

func firstEffect(transcript bench.Transcript) (bench.Moment, bool) {
	for _, moment := range transcript.Moments {
		if moment.Kind != bench.MomentToolCall {
			continue
		}
		switch moment.Name {
		case computeruse.Screenshot, computeruse.Wait, computeruse.Move:
			continue
		default:
			return moment, true
		}
	}
	return bench.Moment{}, false
}

func observationMetrics(
	metrics map[string]float64, item Case, transcript bench.Transcript,
	ready bench.Moment, hasReady bool,
) {
	lastFrame := map[string]float64{}
	var pipeline []float64
	wantedSource := "screen"
	if item.Task.Camera {
		wantedSource = "camera"
	}
	cueAt := 0.0
	if hasReady {
		cueAt = ready.AtMS + milliseconds(item.Task.CueAt)
	}
	seenCue := false
	for _, moment := range transcript.Moments {
		switch moment.Kind {
		case bench.MomentVideoFrame:
			lastFrame[moment.Source] = moment.AtMS
		case bench.MomentObservation:
			if sent, exists := lastFrame[moment.Source]; exists && moment.AtMS >= sent {
				pipeline = append(pipeline, moment.AtMS-sent)
			}
			if !seenCue && hasReady && moment.Source == wantedSource && moment.AtMS >= cueAt {
				metrics["cue_to_observation_latency_ms"] = moment.AtMS - cueAt
				seenCue = true
			}
		}
	}
	if len(pipeline) > 0 {
		metrics["frame_to_observation_latency_ms"] = bench.Summarise(pipeline).P50
	}
}

func countInvalid(actions []ActionRecord) int {
	count := 0
	for _, action := range actions {
		if action.Error != "" {
			count++
		}
	}
	return count
}

func countGroundingErrors(actions []ActionRecord) int {
	count := 0
	for _, action := range actions {
		message := strings.ToLower(action.Error)
		if strings.Contains(message, "outside") || strings.Contains(message, "source") ||
			strings.Contains(message, "set-of-mark") || strings.Contains(message, "element") {
			count++
		}
	}
	return count
}

func countMoments(transcript bench.Transcript, kind string) int {
	count := 0
	for _, moment := range transcript.Moments {
		if moment.Kind == kind {
			count++
		}
	}
	return count
}

func firstMoment(transcript bench.Transcript, kind, source string) (bench.Moment, bool) {
	for _, moment := range transcript.Moments {
		if moment.Kind == kind && (source == "" || moment.Source == source) {
			return moment, true
		}
	}
	return bench.Moment{}, false
}

func lastMomentBefore(transcript bench.Transcript, kind string, before float64) (bench.Moment, bool) {
	var found bench.Moment
	present := false
	for _, moment := range transcript.Moments {
		if moment.AtMS > before {
			break
		}
		if moment.Kind == kind {
			found, present = moment, true
		}
	}
	return found, present
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration.Microseconds()) / 1000
}

func truth(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

// Breakdown is one condition's correctness and deadline-sensitive pass rate.
type Breakdown struct {
	Cases       int
	Correct     int
	Timely      int
	CorrectRate float64
	TimelyRate  float64
}

// ByGrounding exposes the paired pixel/set-of-mark reading in one cell.
func ByGrounding(result bench.Result) map[Grounding]Breakdown {
	breakdown := map[Grounding]Breakdown{}
	for _, task := range result.Tasks {
		grounding := Grounding(task.Notes["grounding"])
		entry := breakdown[grounding]
		entry.Cases++
		if task.Metrics["correct_action_rate"] == 1 {
			entry.Correct++
		}
		if task.Passed {
			entry.Timely++
		}
		breakdown[grounding] = entry
	}
	for grounding, entry := range breakdown {
		if entry.Cases > 0 {
			entry.CorrectRate = float64(entry.Correct) / float64(entry.Cases)
			entry.TimelyRate = float64(entry.Timely) / float64(entry.Cases)
			breakdown[grounding] = entry
		}
	}
	return breakdown
}
