package interaction

import (
	"fmt"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	acousticelements "github.com/bojieli/OpenRealtime/elements/acoustic"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
)

// Segmentation completion can overtake the independently routed speech lane.
// Its emitted count is the only evidence of queued work until those segments
// reach synthesis/playback. A stop must cancel that whole horizon.
func TestOverlapBargeInCancelsCompletedStreamBeforeQueuedSpeechIsVisible(t *testing.T) {
	for _, order := range []string{"segments first", "one release first", "completion first"} {
		t.Run(order, func(t *testing.T) {
			h := mountOverlapBargeIn(t, `{"hold_ms":10,"unclassified":"cancel"}`, nil)
			defer h.stop(t)
			const runID = "queued-run"
			h.sendAndSync(t, "invocation", overlapInvocationEnvelope("invoke", runID))
			h.sendAndSync(t, "model", overlapModelEnvelope("model-done", runID))
			if order == "segments first" {
				for i := 1; i <= 3; i++ {
					h.sendAndSync(t, "speech", overlapSpeechEnvelope(fmt.Sprint("speech-", i), runID,
						fmt.Sprint("utterance-", i), fmt.Sprint(i, ".")))
				}
			}
			if order == "one release first" {
				h.sendAndSync(t, "release", overlapReleaseEnvelope("release-1", runID, "utterance-1", "1.",
					action.Outcome{Completed: true, PlayedMS: 40}))
				_ = receive(t, h.output(t, "safe_release"))
			}
			completed := overlapSegmentationEnvelope("segments-done", runID, OutcomeCompleted)
			completed.Payload = SegmentationOutcome{Kind: OutcomeCompleted, RunID: runID, Segments: 3}
			state := h.sendAndSync(t, "segmentation", completed)
			if !state.AgentOutput.Active || !state.AgentOutput.Queued || state.AgentOutput.Audible {
				t.Fatalf("completed preparation lost unplayed speech: %+v", state.AgentOutput)
			}
			h.sendAndSync(t, "semantic", overlapSemanticEnvelope("stop", "user-stream", 2, coreinteraction.ActStopSpeaking))
			envelope, decision := receiveOverlapDecision(t, h.output(t, "decision"))
			if decision.Kind != OverlapCanceled || decision.SegmentationCancels != 1 || decision.ModelCancels != 0 {
				t.Fatalf("stop did not revoke the completed stream: %+v", decision)
			}
			assertExactOverlapRunCancel(t, h.output(t, "segmentation_cancel"), envelope.ItemID, runID)
			// No speech-activity object is needed for an explicit semantic stop.
			// Keep the horizon until every exact sink/terminal receipt arrives.
			for i := 1; i <= 3; i++ {
				state = h.sendAndSync(t, "release", overlapReleaseEnvelope(fmt.Sprint("release-", i), runID,
					fmt.Sprint("utterance-", i), fmt.Sprint(i, "."), action.Outcome{Reason: "stopped"}))
				_ = receive(t, h.output(t, "safe_release"))
				if i < 3 && !state.AgentOutput.Active {
					t.Fatalf("release %d retired the remaining queued horizon", i)
				}
			}
			if state.AgentOutput.Active {
				t.Fatalf("all releases left phantom work: %+v", state)
			}
			h.sendAndSync(t, "activity", overlapActivityEnvelope("later-user", "later-stream", acousticelements.SpeechStarted))
			assertNoOverlapCancels(t, h)
			assertNoEnvelope(t, h.output(t, "decision"))
		})
	}
}

func TestOverlapBargeInAcousticStopRevokesCompletedStreamBetweenSegments(t *testing.T) {
	h := mountOverlapBargeIn(t, `{"hold_ms":10,"unclassified":"cancel"}`, nil)
	defer h.stop(t)
	const runID = "between-segments"
	h.sendAndSync(t, "invocation", overlapInvocationEnvelope("invoke", runID))
	h.sendAndSync(t, "model", overlapModelEnvelope("model", runID))
	completed := overlapSegmentationEnvelope("completed", runID, OutcomeCompleted)
	completed.Payload = SegmentationOutcome{Kind: OutcomeCompleted, RunID: runID, Segments: 2}
	h.sendAndSync(t, "segmentation", completed)
	h.sendAndSync(t, "release", overlapReleaseEnvelope("first-played", runID, "first", "One.", action.Outcome{Completed: true, PlayedMS: 40}))
	_ = receive(t, h.output(t, "safe_release"))
	h.sendAndSync(t, "activity", overlapActivityEnvelope("user-start", "stream", acousticelements.SpeechStarted))
	_, observed := receiveOverlapDecision(t, h.output(t, "decision"))
	if observed.Kind != OverlapObserved {
		t.Fatalf("unplayed segment is not observed as overlap: %+v", observed)
	}
	h.scheduler.AdvanceNS(uint64(10 * time.Millisecond))
	cause, decision := receiveOverlapDecision(t, h.output(t, "decision"))
	if decision.Kind != OverlapCanceled || decision.SegmentationCancels != 1 {
		t.Fatalf("deadline missed queued run: %+v", decision)
	}
	assertExactOverlapRunCancel(t, h.output(t, "segmentation_cancel"), cause.ItemID, runID)
	_ = receive(t, h.output(t, "state"))
	_ = receive(t, h.output(t, "agent_output"))
	h.sendAndSync(t, "activity", overlapActivityEnvelope("user-stop", "stream", acousticelements.SpeechStopped))
	// Cancellation before synthesis has no playback release. The synthesis
	// terminal must close the final pending segment without reviving overlap.
	state := h.sendAndSync(t, "tts", overlapTransitionEnvelope("second-canceled", runID, "second",
		speechelements.StageSynthesis, speechelements.StateCancelled))
	if state.AgentOutput.Active {
		t.Fatalf("canceled queued synthesis retained phantom work: %+v", state)
	}
}

func TestOverlapBargeInDeduplicatesRunTerminalsBeyondUtteranceCache(t *testing.T) {
	h := mountOverlapBargeIn(t, `{"hold_ms":10,"unclassified":"cancel","max_utterances":1}`, nil)
	defer h.stop(t)
	const runID = "terminal-dedup"
	h.sendAndSync(t, "invocation", overlapInvocationEnvelope("invoke", runID))
	h.sendAndSync(t, "model", overlapModelEnvelope("model", runID))
	completed := overlapSegmentationEnvelope("completed", runID, OutcomeCompleted)
	completed.Payload = SegmentationOutcome{Kind: OutcomeCompleted, RunID: runID, Segments: 3}
	h.sendAndSync(t, "segmentation", completed)
	for _, id := range []string{"first", "second", "first"} {
		state := h.sendAndSync(t, "release", overlapReleaseEnvelope("release-"+id, runID, id, "One.", action.Outcome{Completed: true}))
		_ = receive(t, h.output(t, "safe_release"))
		if !state.AgentOutput.Active {
			t.Fatalf("duplicate %s retired the unplayed third segment", id)
		}
	}
	state := h.sendAndSync(t, "release", overlapReleaseEnvelope("release-third", runID, "third", "Three.", action.Outcome{Completed: true}))
	_ = receive(t, h.output(t, "safe_release"))
	if state.AgentOutput.Active {
		t.Fatalf("all unique terminals retained work: %+v", state)
	}
}
