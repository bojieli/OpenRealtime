package timeline_test

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/timeline"
)

// Each transcript revision the gateway reports is one mark on the ASR lane,
// partial or final, with its words.
func TestATranscriptRevisionProjectsOntoTheASRLane(t *testing.T) {
	events := timeline.Project(binding.DebugEvent{
		Category: "asr", Name: "asr.transcript", Phase: "update",
		Attributes: map[string]any{"final": false, "revision": uint64(25)},
		Payload:    map[string]any{"text": "And yesterday, I met a capybara"},
	})
	if len(events) != 1 {
		t.Fatalf("events = %+v, want one", events)
	}
	got := events[0]
	if got.Lane != timeline.LaneASR || got.Kind != "partial" || got.Phase != "point" ||
		got.Text != "And yesterday, I met a capybara" || got.Detail != "rev 25" {
		t.Fatalf("partial projected as %+v", got)
	}
	final := timeline.Project(binding.DebugEvent{
		Name: "asr.transcript", Attributes: map[string]any{"final": true}, Payload: map[string]any{"text": "done"},
	})
	if len(final) != 1 || final[0].Kind != "final" || final[0].Detail != "" {
		t.Fatalf("final projected as %+v", final)
	}
}

// An element emits its own Go type; the projection reads it through its JSON
// form, so a typed payload and its map form project identically.
func TestATypedPayloadProjectsLikeItsJSON(t *testing.T) {
	type transition struct {
		UtteranceID string `json:"utterance_id"`
		Stage       string `json:"stage"`
		State       string `json:"state"`
		PlayedNS    uint64 `json:"played_ns,omitempty"`
	}
	events := timeline.Project(binding.DebugEvent{
		Name:    "playback_status",
		Payload: map[string]any{"value": transition{UtteranceID: "utt-1", Stage: "playback", State: "played", PlayedNS: 500_000_000}},
	})
	if len(events) != 1 || events[0].Kind != "playback" || events[0].Phase != "end" || events[0].Span != "utt-1" ||
		events[0].Detail != "played 500 ms" {
		t.Fatalf("typed transition projected as %+v", events)
	}
}

// A policy decision is one mark on the policy lane, and the standing pass
// that ran with it is one mark per question on the background lane plus a
// summary of what it left in force.
func TestADecisionProjectsItsChoiceAndTheBackgroundQuestions(t *testing.T) {
	events := timeline.Project(binding.DebugEvent{
		Name: "semantic_decision",
		Payload: map[string]any{"value": map[string]any{
			"choice": "speak", "event": "final", "decision_stage": "policy", "confidence": 0.93,
			"started_ns": float64(1_000_000_000), "finished_ns": float64(1_250_000_000),
			"standing_pinned": float64(1), "standing_after": float64(1),
			"evidence": "transcript event: final\nheard: count the animals",
			"standing": map[string]any{
				"utterance": "count the animals as I mention them",
				"pinned":    []any{"count the animals as they mention them (conversation, counting)"},
				"in_force":  []any{"count the animals as they mention them (conversation, counting)"},
				"calls": []any{
					map[string]any{"question": "extract", "answer": "pin conversation count the animals as they mention them", "duration_ms": 180.5},
					map[string]any{"question": "ground", "answer": "yes", "duration_ms": 40.0},
					map[string]any{"question": "scope", "answer": "passing", "duration_ms": 35.0},
				},
			},
		}},
	})
	if len(events) != 5 {
		t.Fatalf("events = %+v, want choice + three questions + summary", events)
	}
	choice := events[0]
	if choice.Lane != timeline.LanePolicy || choice.Kind != "speak" || choice.Text != "transcript event: final\nheard: count the animals" {
		t.Fatalf("choice projected as %+v", choice)
	}
	for _, want := range []string{"on final", "conf 0.93", "250 ms", "pinned 1", "pins 1"} {
		if !strings.Contains(choice.Detail, want) {
			t.Fatalf("choice detail %q omits %q", choice.Detail, want)
		}
	}
	if choice.DurationMS != 250 {
		t.Fatalf("choice duration = %v, want 250", choice.DurationMS)
	}
	if events[1].Lane != timeline.LaneBackground || events[1].Kind != "extract" ||
		events[1].Text != "pin conversation count the animals as they mention them" || events[1].DurationMS != 180.5 {
		t.Fatalf("first question projected as %+v", events[1])
	}
	if events[3].Kind != "scope" || events[3].Text != "passing" {
		t.Fatalf("scope question projected as %+v", events[3])
	}
	summary := events[4]
	if summary.Lane != timeline.LaneBackground || summary.Kind != "standing" ||
		!strings.Contains(summary.Text, "pinned: count the animals") || !strings.Contains(summary.Detail, "count the animals as I mention") {
		t.Fatalf("summary projected as %+v", summary)
	}
}

// A decision the runtime took from state alone says so, so a reader can tell
// "the model chose listen" from "nobody asked".
func TestADecisionWithoutAModelSaysNoModelWasAsked(t *testing.T) {
	events := timeline.Project(binding.DebugEvent{
		Name:    "semantic_decision",
		Payload: map[string]any{"value": map[string]any{"choice": "listen", "event": "partial", "decision_stage": "state"}},
	})
	if len(events) != 1 || events[0].Detail != "on partial no model asked" {
		t.Fatalf("state decision projected as %+v", events)
	}
}

// Synthesis and playback are bars: a start and an end joined by the utterance
// id, so the timeline can show how long each took and where they overlapped.
func TestSpeechProjectsAsSpans(t *testing.T) {
	started := timeline.Project(binding.DebugEvent{
		Category: "tts", Name: "tts.utterance_started", CorrelationID: "utt-1",
		Payload: map[string]any{"text": "One."},
	})
	if len(started) != 1 || started[0].Lane != timeline.LaneTTS || started[0].Kind != "speaking" ||
		started[0].Phase != "start" || started[0].Span != "utt-1" || started[0].Text != "One." {
		t.Fatalf("utterance start projected as %+v", started)
	}
	played := timeline.Project(binding.DebugEvent{
		Name: "playback_status",
		Payload: map[string]any{"value": map[string]any{
			"utterance_id": "utt-1", "stage": "playback", "state": "played", "played_ns": float64(1_200_000_000),
		}},
	})
	if len(played) != 1 || played[0].Kind != "playback" || played[0].Phase != "end" ||
		played[0].Span != "utt-1" || played[0].Detail != "played 1200 ms" {
		t.Fatalf("playback end projected as %+v", played)
	}
	synthesis := timeline.Project(binding.DebugEvent{
		Name:    "tts_status",
		Payload: map[string]any{"value": map[string]any{"utterance_id": "utt-1", "stage": "synthesis", "state": "generating"}},
	})
	if len(synthesis) != 1 || synthesis[0].Kind != "synthesis" || synthesis[0].Phase != "start" {
		t.Fatalf("synthesis start projected as %+v", synthesis)
	}
	cancel := timeline.Project(binding.DebugEvent{
		Name: "speech_cancel_request", Payload: map[string]any{"value": "utt-1"},
	})
	if len(cancel) != 1 || cancel[0].Kind != "cancel" || cancel[0].Span != "utt-1" {
		t.Fatalf("cancel projected as %+v", cancel)
	}
}

// The model lane runs from the request to the outcome, joined by the
// generation id, with what the model said in between.
func TestTheModelLaneRunsFromRequestToOutcome(t *testing.T) {
	request := timeline.Project(binding.DebugEvent{
		Name: "invocation_outcome",
		Payload: map[string]any{"value": map[string]any{
			"kind": "emitted", "generation_id": "generation:1", "context_version": float64(49), "choice": "speak",
		}},
	})
	if len(request) != 1 || request[0].Lane != timeline.LaneModel || request[0].Kind != "request" ||
		request[0].Phase != "start" || request[0].Span != "generation:1" || request[0].Detail != "context v49 speak" {
		t.Fatalf("request projected as %+v", request)
	}
	result := timeline.Project(binding.DebugEvent{
		Name: "model_result",
		Payload: map[string]any{"value": map[string]any{
			"run_id": "generation:1", "assistant_text": "Two.", "descriptor": map[string]any{"model": "gemini"},
		}},
	})
	if len(result) != 1 || result[0].Kind != "result" || result[0].Text != "Two." || result[0].Detail != "gemini" {
		t.Fatalf("result projected as %+v", result)
	}
	if updated := timeline.Project(binding.DebugEvent{
		Name: "invocation_outcome", Payload: map[string]any{"value": map[string]any{"kind": "updated"}},
	}); len(updated) != 0 {
		t.Fatalf("an invocation update projected as %+v", updated)
	}
	outcome := timeline.Project(binding.DebugEvent{
		Name: "model_outcome",
		Payload: map[string]any{"value": map[string]any{
			"kind": "succeeded", "run_id": "generation:1", "duration_ns": float64(819_000_000),
		}},
	})
	if len(outcome) != 1 || outcome[0].Kind != "succeeded" || outcome[0].Phase != "end" ||
		outcome[0].Span != "generation:1" || outcome[0].DurationMS != 819 {
		t.Fatalf("outcome projected as %+v", outcome)
	}
}

// Everything else the graph reports stays off the timeline.
func TestBookkeepingDoesNotProject(t *testing.T) {
	for _, name := range []string{"perception_outcome", "final_observation_outcome", "semantic_admission_state", "overlap_state"} {
		if events := timeline.Project(binding.DebugEvent{Name: name, Payload: map[string]any{"value": map[string]any{"x": 1}}}); len(events) != 0 {
			t.Fatalf("%s projected as %+v", name, events)
		}
		if timeline.Projects(name) {
			t.Fatalf("%s claims a place on the timeline", name)
		}
	}
	admitted := timeline.Project(binding.DebugEvent{
		Name: "semantic_admission_outcome", Payload: map[string]any{"value": map[string]any{"kind": "admitted"}},
	})
	if len(admitted) != 0 {
		t.Fatalf("an ordinary admission projected as %+v", admitted)
	}
}

// One line per event, readable down the page: time, session, lane, what
// happened, the words.
func TestLinesReadDownThePage(t *testing.T) {
	at := time.Date(2026, 9, 12, 14, 7, 32, 58_000_000, time.UTC)
	line := timeline.Event{Lane: timeline.LanePolicy, Kind: "speak", Phase: "point", Detail: "on partial conf 0.98 47 ms"}.Line(at, "sess_2")
	if line != "2026-09-12T14:07:32.058Z sess_2 POLICY     speak            on partial conf 0.98 47 ms" {
		t.Fatalf("policy line = %q", line)
	}
	line = timeline.Event{Lane: timeline.LaneTTS, Kind: "speaking", Phase: "start", Span: "utt-1", Text: "One."}.Line(at, "sess_2")
	if line != `2026-09-12T14:07:32.058Z sess_2 TTS        speaking start   "One." [utt-1]` {
		t.Fatalf("tts line = %q", line)
	}
}

// Sessions write concurrently and every line arrives whole.
func TestTheWriterKeepsLinesWhole(t *testing.T) {
	var out bytes.Buffer
	writer := timeline.NewWriter(&out)
	var wait sync.WaitGroup
	for session := range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for revision := range 50 {
				_ = writer.Write(time.Unix(0, 0), "sess_"+string(rune('a'+session)), []timeline.Event{{
					Lane: timeline.LaneASR, Kind: "partial", Phase: "point", Detail: "rev " + string(rune('0'+revision%10)),
					Text: strings.Repeat("word ", 20),
				}})
			}
		}()
	}
	wait.Wait()
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 400 {
		t.Fatalf("lines = %d, want 400", len(lines))
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "1970-01-01T00:00:00.000Z sess_") || !strings.HasSuffix(line, `word "`) {
			t.Fatalf("interleaved line: %q", line)
		}
	}
	if err := timeline.NewWriter(nil).Write(time.Now(), "s", []timeline.Event{{Lane: timeline.LaneASR}}); err != nil {
		t.Fatalf("a nil destination should discard, got %v", err)
	}
}
