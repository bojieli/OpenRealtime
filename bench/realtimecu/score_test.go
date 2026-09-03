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
	page, actions, call := successfulScoreEvidence(time.Unix(100, 0), 1600, "right but late")
	call.AtMS = 1700
	transcript := bench.Transcript{Moments: []bench.Moment{
		{AtMS: 100, Kind: bench.MomentReady},
		call,
	}}
	result := score(base, item, page, actions, transcript, false)
	if result.Passed || result.Metrics["correct_action_rate"] != 1 || result.Metrics["deadline_miss_count"] != 1 {
		t.Fatalf("late correctness must remain visible without passing: %+v", result)
	}
}

func TestScoreTreatsAuthoritativeBeforeConditionAsPrematureInsideTimestampTolerance(t *testing.T) {
	item := Case{Task: Task{CueAt: time.Second, Deadline: 400 * time.Millisecond}, Grounding: GroundingPixel}
	base := bench.TaskOutcome{Metrics: map[string]float64{}, Notes: map[string]string{}}
	transcript := bench.Transcript{Moments: []bench.Moment{
		{AtMS: 100, Kind: bench.MomentReady},
		// The action is only 25 ms before the transcript-derived cue, inside the
		// scorer's clock-alignment tolerance. The browser nevertheless observed
		// that the authored condition was false and is authoritative about it.
		{AtMS: 1075, Kind: bench.MomentToolCall},
	}}
	result := score(base, item, PageResult{
		Complete: true, Success: false, Code: PageResultCodeBeforeCondition,
		CompletedAtMS: 975, Reason: "human-readable wording may change", Actions: 1,
	}, nil, transcript, false)
	if result.Passed || result.Metrics["premature_action_count"] != 1 ||
		result.Metrics["deadline_miss_count"] != 1 {
		t.Fatalf("authoritative before-condition result must fail timing safety: %+v", result)
	}
	if result.Notes["page_result_code"] != string(PageResultCodeBeforeCondition) {
		t.Fatalf("structured page result code was not retained: %+v", result.Notes)
	}
}

func TestScoreRetainsTimestampToleranceWithoutBeforeConditionCode(t *testing.T) {
	item := Case{Task: Task{CueAt: time.Second, Deadline: 400 * time.Millisecond}, Grounding: GroundingPixel}
	base := bench.TaskOutcome{Metrics: map[string]float64{}, Notes: map[string]string{}}
	page, actions, call := successfulScoreEvidence(time.Unix(200, 0), 975, "fixture success")
	call.AtMS = 975
	transcript := bench.Transcript{Moments: []bench.Moment{
		{AtMS: 0, Kind: bench.MomentReady},
		call,
	}}
	result := score(base, item, page, actions, transcript, false)
	if !result.Passed || result.Metrics["premature_action_count"] != 0 ||
		result.Metrics["deadline_miss_count"] != 0 {
		t.Fatalf("small clock skew without authoritative early evidence must retain tolerance: %+v", result)
	}
}

func TestScoreRetainsRecognizedUserTurnsForFailureDiagnosis(t *testing.T) {
	base := bench.TaskOutcome{Metrics: map[string]float64{}, Notes: map[string]string{}}
	transcript := bench.Transcript{Moments: []bench.Moment{
		{AtMS: 10, Kind: bench.MomentTranscript, Text: "enter incident alpha seven"},
		{AtMS: 20, Kind: bench.MomentTranscript, Text: "submit it"},
	}}
	result := score(base, Case{}, PageResult{}, nil, transcript, false)
	var turns []string
	if err := json.Unmarshal([]byte(result.Notes["recognized_user_turns"]), &turns); err != nil {
		t.Fatalf("recognized user turns are not retained as JSON: %v", err)
	}
	if len(turns) != 2 || turns[0] != "enter incident alpha seven" || turns[1] != "submit it" {
		t.Fatalf("unexpected recognized user turns: %v", turns)
	}
}

func TestScoreRejectsPostSuccessContinuationWithoutErasingCorrectEffect(t *testing.T) {
	started := time.Unix(100, 0)
	item := Case{Task: Task{CueAt: 500 * time.Millisecond, Deadline: 2 * time.Second}, Grounding: GroundingPixel}
	base := bench.TaskOutcome{Metrics: map[string]float64{}, Notes: map[string]string{}}
	arguments := json.RawMessage(`{"source":"screen","x":500,"y":500}`)
	success := PageResult{
		Complete: true, Success: true, CompletedAtMS: 1000, Reason: "target hit", Actions: 1,
	}
	transcript := bench.Transcript{Moments: []bench.Moment{
		{AtMS: 0, Kind: bench.MomentReady},
		{AtMS: 900, Kind: bench.MomentToolCall, CallID: "successful", Name: "computer.click", Arguments: string(arguments)},
		{AtMS: 1200, Kind: bench.MomentToolCall, CallID: "continued", Name: "computer.click", Arguments: string(arguments)},
	}}
	actions := []ActionRecord{
		{Ordinal: 1, CallID: "successful", Name: "computer.click", Arguments: arguments,
			ReceivedAt: started.Add(900 * time.Millisecond), CompletedAt: started.Add(910 * time.Millisecond),
			PageBefore: clonePageResult(PageResult{}), PageAfter: clonePageResult(success)},
		{Ordinal: 2, CallID: "continued", Name: "computer.click", Arguments: arguments,
			ReceivedAt: started.Add(1200 * time.Millisecond), CompletedAt: started.Add(1210 * time.Millisecond),
			PageBefore: clonePageResult(success), PageAfter: clonePageResult(success),
			Error: "task is already complete; refusing post-completion action"},
	}
	result := score(base, item, PageResult{
		Complete: true, Success: true, CompletedAtMS: 1000, Reason: "target hit",
		Actions: 3, ActionsAfterCompletion: 2,
	}, actions, transcript, false)
	if result.Passed || result.Metrics["task_success_rate"] != 0 ||
		result.Metrics["correct_action_rate"] != 1 ||
		result.Metrics["post_success_action_count"] != 2 ||
		result.Metrics["session_timeout_count"] != 0 {
		t.Fatalf("post-success continuation must fail settled task success: %+v", result)
	}
}

func TestScoreRejectsPostSuccessSessionTimeout(t *testing.T) {
	started := time.Unix(200, 0)
	item := Case{Task: Task{CueAt: 500 * time.Millisecond, Deadline: 2 * time.Second}, Grounding: GroundingPixel}
	base := bench.TaskOutcome{Metrics: map[string]float64{}, Notes: map[string]string{}}
	page, actions, call := successfulScoreEvidence(started, 1000, "target hit")
	call.AtMS = 900
	transcript := bench.Transcript{Moments: []bench.Moment{
		{AtMS: 0, Kind: bench.MomentReady},
		call,
	}}
	result := score(base, item, page, actions, transcript, true)
	if result.Passed || result.Metrics["task_success_rate"] != 0 ||
		result.Metrics["correct_action_rate"] != 1 ||
		result.Metrics["session_timeout_count"] != 1 ||
		result.Metrics["post_success_action_count"] != 0 || result.Notes["session_timeout"] == "" {
		t.Fatalf("post-success timeout must fail settled task success: %+v", result)
	}
}

func TestScoreFailsClosedWhenSuccessActionTimingIsMissing(t *testing.T) {
	started := time.Unix(300, 0)
	item := Case{Task: Task{CueAt: 500 * time.Millisecond, Deadline: 2 * time.Second}, Grounding: GroundingPixel}
	base := bench.TaskOutcome{Metrics: map[string]float64{}, Notes: map[string]string{}}
	page, actions, call := successfulScoreEvidence(started, 1000, "target hit")
	actions[0].ReceivedAt = time.Time{}
	transcript := bench.Transcript{Moments: []bench.Moment{
		{AtMS: 0, Kind: bench.MomentReady},
		call,
	}}
	result := score(base, item, page, actions, transcript, false)
	if result.Passed || result.Metrics["task_success_rate"] != 0 ||
		result.Metrics["settlement_evidence_missing_count"] != 1 ||
		result.Notes["settlement_evidence"] == "" {
		t.Fatalf("missing settlement timing must fail closed: %+v", result)
	}
}

func TestScoreRejectsSuccessfulPageWithOutstandingOrFailedSessionWork(t *testing.T) {
	item := Case{Task: Task{Deadline: 2 * time.Second}, Grounding: GroundingPixel}
	base := bench.TaskOutcome{Metrics: map[string]float64{}, Notes: map[string]string{}}
	page, actions, call := successfulScoreEvidence(time.Unix(400, 0), 100, "target hit")
	for _, transcript := range []bench.Transcript{
		{Moments: []bench.Moment{{Kind: bench.MomentReady}, call}, OutstandingResponses: 1},
		{Moments: []bench.Moment{{Kind: bench.MomentReady}, call}, OutstandingTools: 1},
		{Moments: []bench.Moment{{Kind: bench.MomentReady}, call}, Failure: "protocol failed"},
	} {
		result := score(base, item, page, actions, transcript, false)
		if result.Passed || result.Metrics["task_success_rate"] != 0 ||
			result.Metrics["correct_action_rate"] != 1 {
			t.Fatalf("unfinished or failed session must not pass: %+v", result)
		}
	}
}

func TestScoreRejectsSuccessWithoutMatchingOrdinalTransitionWitness(t *testing.T) {
	item := Case{Task: Task{Deadline: 2 * time.Second}, Grounding: GroundingPixel}
	base := bench.TaskOutcome{Metrics: map[string]float64{}, Notes: map[string]string{}}
	page, actions, call := successfulScoreEvidence(time.Unix(500, 0), 100, "target hit")
	actions[0].Ordinal = 2
	call.CallID = "different-call"
	result := score(base, item, page, actions, bench.Transcript{
		Moments: []bench.Moment{{Kind: bench.MomentReady}, call},
	}, false)
	if result.Passed || result.Metrics["settlement_evidence_missing_count"] != 1 ||
		result.Notes["settlement_evidence"] == "" {
		t.Fatalf("mismatched ordinal witness must fail closed: %+v", result)
	}
}

func TestValidatePageResultRejectsImpossiblePostCompletionCounts(t *testing.T) {
	for _, page := range []PageResult{
		{Actions: -1},
		{ActionsAfterCompletion: 1},
		{Complete: true, Success: true, Actions: 1, ActionsAfterCompletion: 1},
		{Complete: true, Success: true, Actions: 0},
	} {
		if err := validatePageResult(page); err == nil {
			t.Fatalf("validatePageResult(%+v) accepted impossible state", page)
		}
	}
}

func successfulScoreEvidence(
	started time.Time, completedAtMS float64, reason string,
) (PageResult, []ActionRecord, bench.Moment) {
	arguments := json.RawMessage(`{"source":"screen","x":500,"y":500}`)
	page := PageResult{
		Complete: true, Success: true, CompletedAtMS: completedAtMS, Reason: reason, Actions: 1,
	}
	action := ActionRecord{
		Ordinal: 1, CallID: "successful", Name: "computer.click", Arguments: arguments,
		ReceivedAt: started.Add(time.Second), CompletedAt: started.Add(time.Second + 10*time.Millisecond),
		PageBefore: clonePageResult(PageResult{}), PageAfter: clonePageResult(page),
	}
	call := bench.Moment{
		AtMS: completedAtMS, Kind: bench.MomentToolCall, CallID: action.CallID,
		Name: action.Name, Arguments: string(arguments),
	}
	return page, []ActionRecord{action}, call
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
