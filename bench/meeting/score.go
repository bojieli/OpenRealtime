package meeting

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
)

// PageEvent is one deterministic transition in the shared-screen fixture.
type PageEvent struct {
	Name string  `json:"name"`
	AtMS float64 `json:"at_ms"`
}

// PageResult is hidden evaluator state; only rendered pixels and declared
// computer tools reach the model.
type PageResult struct {
	Events []PageEvent `json:"events"`
	State  string      `json:"state"`
}

// ActionRecord retains every computer action and its execution interval.
type ActionRecord struct {
	CallID        string          `json:"call_id"`
	Name          string          `json:"name"`
	Arguments     json.RawMessage `json:"arguments"`
	ReceivedAtMS  float64         `json:"received_at_ms"`
	CompletedAtMS float64         `json:"completed_at_ms"`
	Error         string          `json:"error,omitempty"`
}

// ToolRecord retains long-running knowledge-tool intervals on the environment
// clock. The overlap interval is part of the score, not a diagnostic note.
type ToolRecord struct {
	CallID        string  `json:"call_id,omitempty"`
	Name          string  `json:"name"`
	ReceivedAtMS  float64 `json:"received_at_ms"`
	CompletedAtMS float64 `json:"completed_at_ms"`
	Error         string  `json:"error,omitempty"`
}

func score(
	task Task, page PageResult, actions []ActionRecord, tools []ToolRecord, transcript bench.Transcript,
) bench.TaskOutcome {
	outcome := bench.TaskOutcome{
		ID: task.ID, Completed: true, Metrics: map[string]float64{},
		Notes: map[string]string{"category": task.Category, "difficulty": task.Difficulty},
	}
	if encoded, err := json.Marshal(page.Events); err == nil {
		outcome.Notes["page_events"] = string(encoded)
	}
	if encoded, err := json.Marshal(actions); err == nil {
		outcome.Notes["actions"] = string(encoded)
	}
	if encoded, err := json.Marshal(tools); err == nil {
		outcome.Notes["tools"] = string(encoded)
	}
	if turns := transcript.UserTurns(); len(turns) > 0 {
		if encoded, err := json.Marshal(turns); err == nil {
			outcome.Notes["recognized_user_turns"] = string(encoded)
		}
	}
	if turns := transcript.AgentTurns(); len(turns) > 0 {
		if encoded, err := json.Marshal(turns); err == nil {
			outcome.Notes["agent_turns"] = string(encoded)
		}
	}
	if transcript.Runtime != nil {
		if encoded, err := json.Marshal(transcript.Runtime); err == nil {
			outcome.Notes["runtime"] = string(encoded)
		}
	}
	var observations []bench.Moment
	for _, moment := range transcript.Moments {
		if moment.Kind == bench.MomentVideoFrame {
			outcome.Metrics["video_frame_count"]++
		}
		if moment.Kind == bench.MomentObservation {
			observations = append(observations, moment)
		}
	}
	outcome.Metrics["observation_count"] = float64(len(observations))
	if encoded, err := json.Marshal(observations); err == nil {
		outcome.Notes["observation_trace"] = string(encoded)
	}
	outcome.Metrics["action_count"] = float64(len(actions))
	outcome.Metrics["invalid_action_count"] = float64(invalidActions(actions))
	outcome.Metrics["deadline_miss_count"] = 1

	switch task.ID {
	case "open-share-present":
		cue := task.Primary()
		// This request is deliberately compositional: each explicit control may
		// be acted on as soon as its words are understood, before the acoustic
		// endpoint for the complete sentence. Starting the search at cue.End
		// makes a lower-latency controller fail by erasing its valid actions.
		opened, hasOpened := firstPageEvent(page, "document-opened", milliseconds(cue.Start))
		shared, hasShared := firstPageEvent(page, "screen-shared", milliseconds(cue.Start))
		toolUsed := hasTool(tools, ToolReadLaunchReview)
		grounded := transcriptContains(transcript, "18.4", "eighteen point four")
		outcome.Metrics["document_open_rate"] = truth(hasOpened)
		outcome.Metrics["screen_share_rate"] = truth(hasShared)
		outcome.Metrics["grounded_answer_rate"] = truth(toolUsed && grounded)
		if hasOpened {
			outcome.Metrics["cue_to_document_open_ms"] = opened.AtMS - milliseconds(cue.End)
		}
		if hasShared {
			outcome.Metrics["cue_to_screen_share_ms"] = shared.AtMS - milliseconds(cue.End)
		}
		timely := hasOpened && hasShared &&
			opened.AtMS-milliseconds(cue.End) <= milliseconds(cue.Deadline) &&
			shared.AtMS-milliseconds(cue.End) <= milliseconds(cue.Deadline)
		outcome.Passed = timely && toolUsed && grounded
	case "follow-up-during-analysis":
		cue := task.Cue("follow-up")
		// The controller is explicitly expected to engage from partial speech as
		// soon as the destination is clear. Starting at the acoustic endpoint
		// erases a valid low-latency action made while the cue is still being
		// spoken; latency remains measured against the endpoint below, so such an
		// action is correctly represented by a negative value.
		risks, hasRisks := firstPageEvent(page, "risks-slide", milliseconds(cue.Start))
		concurrent := false
		if hasRisks {
			for _, tool := range tools {
				if tool.Name == ToolAnalyzeLaunchReview && tool.Error == "" &&
					tool.ReceivedAtMS <= milliseconds(cue.Start) && tool.CompletedAtMS >= risks.AtMS {
					concurrent = true
					break
				}
			}
			outcome.Metrics["followup_action_latency_ms"] = risks.AtMS - milliseconds(cue.End)
		}
		timely := hasRisks && risks.AtMS-milliseconds(cue.End) <= milliseconds(cue.Deadline)
		outcome.Metrics["followup_during_reasoning_rate"] = truth(concurrent)
		outcome.Passed = timely && concurrent
	case "visual-alert-during-presentation":
		cue := task.Cue("deployment-alert")
		for _, moment := range transcript.Moments {
			if moment.Kind == bench.MomentVideoFrame && moment.AtMS >= milliseconds(cue.Start) &&
				moment.AtMS <= milliseconds(cue.Start+cue.Deadline) {
				outcome.Metrics["video_frames_in_alert_window"]++
			}
		}
		ack, hasAck := firstPageEvent(page, "alert-acknowledged", milliseconds(cue.Start))
		continuous := false
		if hasAck {
			beforeAudio := transcript.AudioBetween(milliseconds(cue.Start)-2_000, milliseconds(cue.Start))
			afterAudio := transcript.AudioBetween(ack.AtMS, ack.AtMS+2_000)
			outcome.Metrics["presentation_audio_before_cue_ms"] = beforeAudio
			outcome.Metrics["presentation_audio_after_action_ms"] = afterAudio
			before := beforeAudio > 0
			after := afterAudio > 0
			continuous = before && after
			outcome.Metrics["visual_cue_to_action_ms"] = ack.AtMS - milliseconds(cue.Start)
		}
		timely := hasAck && ack.AtMS-milliseconds(cue.Start) <= milliseconds(cue.Deadline)
		outcome.Metrics["presentation_continuity_rate"] = truth(continuous)
		outcome.Passed = timely && continuous
	case "spoken-navigation-correction":
		cue := task.Cue("correction")
		_, hasSummary := lastPageEventBefore(page, "summary-slide", milliseconds(cue.Start))
		overview, hasOverview := firstPageEvent(page, "overview-slide", milliseconds(cue.End))
		timely := hasOverview && overview.AtMS-milliseconds(cue.End) <= milliseconds(cue.Deadline)
		outcome.Metrics["initial_navigation_rate"] = truth(hasSummary)
		outcome.Metrics["correction_navigation_rate"] = truth(hasOverview)
		if hasOverview {
			outcome.Metrics["correction_to_action_ms"] = overview.AtMS - milliseconds(cue.End)
		}
		outcome.Passed = hasSummary && timely
	}
	if outcome.Passed {
		outcome.Metrics["deadline_miss_count"] = 0
	}
	outcome.Metrics["task_success_rate"] = truth(outcome.Passed)
	return outcome
}

func firstPageEvent(page PageResult, name string, after float64) (PageEvent, bool) {
	for _, event := range page.Events {
		if event.Name == name && event.AtMS >= after {
			return event, true
		}
	}
	return PageEvent{}, false
}

func lastPageEventBefore(page PageResult, name string, before float64) (PageEvent, bool) {
	var found PageEvent
	present := false
	for _, event := range page.Events {
		if event.Name == name && event.AtMS <= before {
			found, present = event, true
		}
	}
	return found, present
}

func hasTool(tools []ToolRecord, name string) bool {
	for _, tool := range tools {
		if tool.Name == name && tool.Error == "" && tool.CompletedAtMS >= tool.ReceivedAtMS {
			return true
		}
	}
	return false
}

func transcriptContains(transcript bench.Transcript, alternatives ...string) bool {
	text := strings.ToLower(strings.Join(transcript.AgentTurns(), " "))
	for _, alternative := range alternatives {
		if strings.Contains(text, strings.ToLower(alternative)) {
			return true
		}
	}
	return false
}

func invalidActions(actions []ActionRecord) int {
	count := 0
	for _, action := range actions {
		if action.Error != "" {
			count++
		}
	}
	return count
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
