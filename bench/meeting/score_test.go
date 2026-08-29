package meeting

import (
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func TestOpenSharePresentNeedsScreenStateToolFactAndSpeech(t *testing.T) {
	task := taskByID(t, "open-share-present")
	page := PageResult{Events: []PageEvent{
		{Name: "document-opened", AtMS: milliseconds(task.Primary().End) + 300},
		{Name: "screen-shared", AtMS: milliseconds(task.Primary().End) + 600},
	}}
	tools := []ToolRecord{{Name: ToolReadLaunchReview, ReceivedAtMS: 6_000, CompletedAtMS: 6_100}}
	transcript := bench.Transcript{Moments: []bench.Moment{
		{Kind: bench.MomentAgentText, Text: "The latest conversion rate is 18.4 percent."},
		{Kind: bench.MomentResponseDone},
	}}
	outcome := score(task, page, nil, tools, transcript)
	if !outcome.Passed {
		t.Fatalf("expected pass: %+v", outcome)
	}

	transcript.Moments[0].Text = "The launch is looking healthy."
	outcome = score(task, page, nil, tools, transcript)
	if outcome.Passed || outcome.Metrics["grounded_answer_rate"] != 0 {
		t.Fatalf("an ungrounded presentation passed: %+v", outcome)
	}
}

func TestOpenSharePresentAcceptsActionsBeforeTheFullUtteranceEndpoint(t *testing.T) {
	task := taskByID(t, "open-share-present")
	page := PageResult{Events: []PageEvent{
		{Name: "document-opened", AtMS: milliseconds(task.Primary().Start) + 1_500},
		{Name: "screen-shared", AtMS: milliseconds(task.Primary().Start) + 3_200},
	}}
	tools := []ToolRecord{{Name: ToolReadLaunchReview, ReceivedAtMS: 6_000, CompletedAtMS: 6_100}}
	transcript := bench.Transcript{Moments: []bench.Moment{
		{Kind: bench.MomentAgentText, Text: "The latest conversion rate is 18.4 percent."},
		{Kind: bench.MomentResponseDone},
	}}
	outcome := score(task, page, nil, tools, transcript)
	if !outcome.Passed || outcome.Metrics["document_open_rate"] != 1 ||
		outcome.Metrics["screen_share_rate"] != 1 {
		t.Fatalf("valid partial-speech actions were erased at the endpoint: %+v", outcome)
	}
}

func TestFollowUpMustLandWhileAnalysisIsOutstanding(t *testing.T) {
	task := taskByID(t, "follow-up-during-analysis")
	cue := task.Cue("follow-up")
	page := PageResult{Events: []PageEvent{{
		Name: "risks-slide", AtMS: milliseconds(cue.End) + 250,
	}}}
	tools := []ToolRecord{{
		Name:          ToolAnalyzeLaunchReview,
		ReceivedAtMS:  milliseconds(cue.Start) - 900,
		CompletedAtMS: milliseconds(cue.End) + 2_000,
	}}
	outcome := score(task, page, nil, tools, bench.Transcript{})
	if !outcome.Passed || outcome.Metrics["followup_during_reasoning_rate"] != 1 {
		t.Fatalf("expected concurrent pass: %+v", outcome)
	}

	tools[0].CompletedAtMS = milliseconds(cue.Start) - 1
	outcome = score(task, page, nil, tools, bench.Transcript{})
	if outcome.Passed || outcome.Metrics["followup_during_reasoning_rate"] != 0 {
		t.Fatalf("serialized analysis passed: %+v", outcome)
	}
}

func TestFollowUpAcceptsActionFromAnUnambiguousPartialBeforeCueEndpoint(t *testing.T) {
	task := taskByID(t, "follow-up-during-analysis")
	cue := task.Cue("follow-up")
	actionAt := milliseconds(cue.Start) + 250
	if actionAt >= milliseconds(cue.End) {
		t.Fatal("fixture does not leave a partial-speech action window")
	}
	page := PageResult{Events: []PageEvent{{Name: "risks-slide", AtMS: actionAt}}}
	tools := []ToolRecord{{
		Name:          ToolAnalyzeLaunchReview,
		ReceivedAtMS:  milliseconds(cue.Start) - 900,
		CompletedAtMS: milliseconds(cue.End) + 2_000,
	}}
	outcome := score(task, page, nil, tools, bench.Transcript{})
	if !outcome.Passed || outcome.Metrics["followup_during_reasoning_rate"] != 1 {
		t.Fatalf("valid partial-speech action was erased: %+v", outcome)
	}
	if latency := outcome.Metrics["followup_action_latency_ms"]; latency >= 0 {
		t.Fatalf("partial-speech latency = %v, want a value relative to the later endpoint", latency)
	}
}

func TestVisualAlertRequiresTimelyActionAndSpeechOnBothSides(t *testing.T) {
	task := taskByID(t, "visual-alert-during-presentation")
	cue := task.Cue("deployment-alert")
	page := PageResult{Events: []PageEvent{{
		Name: "alert-acknowledged", AtMS: milliseconds(cue.Start) + 300,
	}}}
	transcript := bench.Transcript{Moments: []bench.Moment{
		{Kind: bench.MomentAgentAudio, AtMS: milliseconds(cue.Start) - 100, AudioMS: 100},
		{Kind: bench.MomentAgentAudio, AtMS: milliseconds(cue.Start) + 600, AudioMS: 100},
	}}
	outcome := score(task, page, nil, nil, transcript)
	if !outcome.Passed || outcome.Metrics["presentation_continuity_rate"] != 1 {
		t.Fatalf("expected continuous presentation pass: %+v", outcome)
	}

	transcript.Moments = transcript.Moments[:1]
	outcome = score(task, page, nil, nil, transcript)
	if outcome.Passed {
		t.Fatalf("presentation that never resumed passed: %+v", outcome)
	}
}

func TestCorrectionRequiresReversalAfterSpokenCorrection(t *testing.T) {
	task := taskByID(t, "spoken-navigation-correction")
	cue := task.Cue("correction")
	page := PageResult{Events: []PageEvent{
		{Name: "summary-slide", AtMS: milliseconds(cue.Start) - 1_000},
		{Name: "overview-slide", AtMS: milliseconds(cue.End) + 200},
	}}
	outcome := score(task, page, nil, nil, bench.Transcript{})
	if !outcome.Passed {
		t.Fatalf("expected correction pass: %+v", outcome)
	}

	page.Events[1].AtMS = milliseconds(cue.End) + milliseconds(cue.Deadline) + 1
	outcome = score(task, page, nil, nil, bench.Transcript{})
	if outcome.Passed || outcome.Metrics["deadline_miss_count"] != 1 {
		t.Fatalf("late correction passed: %+v", outcome)
	}
}

func taskByID(t *testing.T, id string) Task {
	t.Helper()
	for _, task := range Suite() {
		if task.ID == id {
			return task
		}
	}
	t.Fatalf("task %q not found", id)
	return Task{}
}
