package realtimecu

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/computeruse"
)

// RealtimeCUScorerIdentity is retained with every current review context. The
// version belongs in the identity so a source bundle can never acquire newer
// pass semantics merely because it is reopened by newer code.
const RealtimeCUScorerIdentity = "openrealtime.realtime-cu.settled-success.v1"

func score(
	outcome bench.TaskOutcome, item Case, page PageResult,
	actions []ActionRecord, transcript bench.Transcript, timedOut bool,
) bench.TaskOutcome {
	outcome.Completed = true
	outcome.Notes["page_result"] = page.Reason
	if page.Code != "" {
		outcome.Notes["page_result_code"] = string(page.Code)
	}
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
	settlement := assessSettlement(page, actions, transcript)
	outcome.Metrics["post_success_action_count"] = float64(settlement.postSuccessActions)
	outcome.Metrics["settlement_evidence_missing_count"] = truth(settlement.evidenceMissing)
	outcome.Metrics["session_timeout_count"] = truth(timedOut)
	outcome.Metrics["session_failure_count"] = truth(transcript.Failure != "")
	outcome.Metrics["outstanding_response_count"] = float64(transcript.OutstandingResponses)
	outcome.Metrics["outstanding_tool_count"] = float64(transcript.OutstandingTools)
	if settlement.evidenceMissing {
		outcome.Notes["settlement_evidence"] = settlement.reason
	}
	if timedOut {
		outcome.Notes["session_timeout"] = "the connected agent continued beyond the evaluation horizon"
	}
	if transcript.OutstandingResponses != 0 || transcript.OutstandingTools != 0 {
		outcome.Notes["outstanding_work"] = fmt.Sprintf(
			"session ended with %d response(s) and %d tool call(s) outstanding",
			transcript.OutstandingResponses, transcript.OutstandingTools,
		)
	}
	sessionSettled := !timedOut && transcript.Failure == "" &&
		transcript.OutstandingResponses == 0 && transcript.OutstandingTools == 0
	settledSuccess := page.Complete && page.Success && sessionSettled &&
		settlement.postSuccessActions == 0 && !settlement.evidenceMissing
	outcome.Metrics["task_success_rate"] = truth(settledSuccess)
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
	premature := page.Code == PageResultCodeBeforeCondition
	deadlineMissed := true
	if ready && called {
		reaction := firstCall.AtMS - (readyAt.AtMS + milliseconds(item.Task.CueAt))
		outcome.Metrics["cue_to_action_latency_ms"] = reaction
		premature = premature || reaction < -250
		deadlineMissed = premature || reaction > milliseconds(item.Task.Deadline)
		if stopped, ok := lastMomentBefore(transcript, bench.MomentSpeechStopped, firstCall.AtMS); ok {
			outcome.Metrics["speech_end_to_action_latency_ms"] = firstCall.AtMS - stopped.AtMS
		}
	}
	outcome.Metrics["premature_action_count"] = truth(premature)
	if anyCall, ok := firstMoment(transcript, bench.MomentToolCall, ""); ready && ok {
		outcome.Metrics["cue_to_first_tool_latency_ms"] =
			anyCall.AtMS - (readyAt.AtMS + milliseconds(item.Task.CueAt))
	}
	if page.Complete && page.Success {
		completionReaction := page.CompletedAtMS - milliseconds(item.Task.CueAt)
		deadlineMissed = premature || completionReaction < -250 || completionReaction > milliseconds(item.Task.Deadline)
	}
	outcome.Metrics["deadline_miss_count"] = truth(deadlineMissed)
	outcome.Passed = settledSuccess && !deadlineMissed

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

type settlementAssessment struct {
	postSuccessActions int
	evidenceMissing    bool
	reason             string
}

// assessSettlement proves successful completion from a serialized
// browser-state/action/browser-state witness. It intentionally never compares
// browser elapsed time with the host clock: Ready records those clocks on
// opposite sides of a CDP round trip, which leaves an unprovable interval.
func assessSettlement(page PageResult, actions []ActionRecord, transcript bench.Transcript) settlementAssessment {
	assessment := settlementAssessment{}
	if !page.Complete || !page.Success {
		return assessment
	}
	var problems []string
	if err := validatePageResult(page); err != nil {
		problems = append(problems, "final page result: "+err.Error())
	}
	toolCalls := make([]bench.Moment, 0, len(actions))
	for _, moment := range transcript.Moments {
		if moment.Kind == bench.MomentToolCall {
			toolCalls = append(toolCalls, moment)
		}
	}
	if len(actions) == 0 {
		problems = append(problems, "no action record established browser success")
	}
	if len(toolCalls) != len(actions) {
		problems = append(problems, fmt.Sprintf(
			"transcript has %d tool call(s), action witness has %d", len(toolCalls), len(actions),
		))
	}
	if transcript.OutstandingResponses < 0 || transcript.OutstandingTools < 0 {
		problems = append(problems, "transcript carries a negative outstanding-work count")
	}
	seenCallIDs := make(map[string]struct{}, len(actions))
	successTransitions := 0
	var successPage *PageResult
	var previousAfter *PageResult
	var previousCompletedAt time.Time
	for index, action := range actions {
		if action.Ordinal != index+1 {
			problems = append(problems, fmt.Sprintf("action %d has invalid ordinal %d", index+1, action.Ordinal))
		}
		if strings.TrimSpace(action.CallID) == "" || strings.TrimSpace(action.Name) == "" {
			problems = append(problems, fmt.Sprintf("action %d has an empty call identity", index+1))
		} else if _, duplicate := seenCallIDs[action.CallID]; duplicate {
			problems = append(problems, fmt.Sprintf("action %d repeats call id %q", index+1, action.CallID))
		} else {
			seenCallIDs[action.CallID] = struct{}{}
		}
		if len(action.Arguments) == 0 || !json.Valid(action.Arguments) {
			problems = append(problems, fmt.Sprintf("action %d arguments are not valid JSON", index+1))
		}
		if action.ReceivedAt.IsZero() || action.CompletedAt.IsZero() ||
			action.CompletedAt.Before(action.ReceivedAt) {
			problems = append(problems, fmt.Sprintf("action %d timestamps are missing or reversed", index+1))
		}
		if !previousCompletedAt.IsZero() && action.ReceivedAt.Before(previousCompletedAt) {
			problems = append(problems, fmt.Sprintf("action %d overlaps the preceding serialized action", index+1))
		}
		previousCompletedAt = action.CompletedAt
		if action.PageBefore == nil || action.PageAfter == nil {
			problems = append(problems, fmt.Sprintf("action %d is missing page-state evidence", index+1))
		} else {
			if previousAfter != nil && !samePageResult(*previousAfter, *action.PageBefore) {
				problems = append(problems, fmt.Sprintf("action %d before state does not continue action %d", index+1, index))
			}
			if err := validatePageResult(*action.PageBefore); err != nil {
				problems = append(problems, fmt.Sprintf("action %d before state: %v", index+1, err))
			}
			if err := validatePageResult(*action.PageAfter); err != nil {
				problems = append(problems, fmt.Sprintf("action %d after state: %v", index+1, err))
			}
			if action.PageBefore.Complete {
				assessment.postSuccessActions++
				if !samePageResult(*action.PageBefore, *action.PageAfter) {
					problems = append(problems, fmt.Sprintf("refused action %d changed a completed page", index+1))
				}
			}
			if !action.PageBefore.Complete && action.PageAfter.Complete && action.PageAfter.Success {
				successTransitions++
				if action.Error != "" {
					problems = append(problems, fmt.Sprintf("successful action %d carries an execution error", index+1))
				}
				state := *action.PageAfter
				successPage = &state
			}
			state := *action.PageAfter
			previousAfter = &state
		}
		if index < len(toolCalls) {
			call := toolCalls[index]
			if call.CallID != action.CallID || call.Name != action.Name ||
				call.Arguments != string(action.Arguments) {
				problems = append(problems, fmt.Sprintf(
					"action %d does not match its transcript tool call", index+1,
				))
			}
		}
	}
	if successTransitions != 1 {
		problems = append(problems, fmt.Sprintf(
			"action witness has %d successful page transition(s), want exactly 1", successTransitions,
		))
	} else if !samePageCompletion(*successPage, page) {
		problems = append(problems, "successful action transition differs from the final page result")
	}
	if previousAfter != nil && !samePageResult(*previousAfter, page) {
		problems = append(problems, "final page result does not match the last serialized action state")
	}
	if page.ActionsAfterCompletion > assessment.postSuccessActions {
		// The fixture may observe multiple DOM effects within one dispatched
		// action. Its sticky counter is authoritative for that case; explicit
		// refused requests are counted from their PageBefore state above.
		assessment.postSuccessActions = page.ActionsAfterCompletion
	}
	assessment.evidenceMissing = len(problems) > 0
	if assessment.evidenceMissing {
		assessment.reason = "post-success quiescence could not be established: " + strings.Join(problems, "; ")
	}
	return assessment
}

func validatePageResult(result PageResult) error {
	if result.Actions < 0 {
		return errors.New("action count is negative")
	}
	if result.ActionsAfterCompletion < 0 ||
		(result.Actions == 0 && result.ActionsAfterCompletion != 0) ||
		(result.Actions > 0 && result.ActionsAfterCompletion > result.Actions-1) {
		return errors.New("post-completion action count is outside the page action history")
	}
	if result.Success && !result.Complete {
		return errors.New("success is true while completion is false")
	}
	if result.Complete {
		if result.Actions < 1 {
			return errors.New("completed page has no action")
		}
		if math.IsNaN(result.CompletedAtMS) || math.IsInf(result.CompletedAtMS, 0) ||
			result.CompletedAtMS < 0 {
			return errors.New("completion time is invalid")
		}
	}
	return nil
}

func samePageCompletion(left, right PageResult) bool {
	return left.Complete == right.Complete && left.Success == right.Success &&
		left.Code == right.Code && left.Reason == right.Reason &&
		left.CompletedAtMS == right.CompletedAtMS && left.Actions <= right.Actions
}

func samePageResult(left, right PageResult) bool {
	return left.Complete == right.Complete && left.Success == right.Success &&
		left.Code == right.Code && left.Reason == right.Reason &&
		left.CompletedAtMS == right.CompletedAtMS && left.Actions == right.Actions &&
		left.ActionsAfterCompletion == right.ActionsAfterCompletion
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

// Breakdown is one condition's raw correctness and fully accepted pass rate.
type Breakdown struct {
	Cases        int
	Correct      int
	Accepted     int
	CorrectRate  float64
	AcceptedRate float64
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
			entry.Accepted++
		}
		breakdown[grounding] = entry
	}
	for grounding, entry := range breakdown {
		if entry.Cases > 0 {
			entry.CorrectRate = float64(entry.Correct) / float64(entry.Cases)
			entry.AcceptedRate = float64(entry.Accepted) / float64(entry.Cases)
			breakdown[grounding] = entry
		}
	}
	return breakdown
}
