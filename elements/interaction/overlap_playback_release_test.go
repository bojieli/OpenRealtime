package interaction

import (
	"fmt"
	"github.com/bojieli/OpenRealtime/element"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
)

func TestOverlapPlaybackReleaseWaitsForPreparationAndPreservesOtherWork(t *testing.T) {
	for _, order := range [][]string{{"model", "segmentation"}, {"segmentation", "model"}} {
		for _, remaining := range []string{"none", "queued-segment", "other-run"} {
			t.Run(order[0]+"-first/"+remaining, func(t *testing.T) {
				h := mountOverlapBargeIn(t, `{"hold_ms":10,"unclassified":"cancel"}`, nil)
				defer h.stop(t)
				const runID, utteranceID = "completed-run", "completed-utterance"
				h.sendAndSync(t, "invocation", overlapInvocationEnvelope("invoked", runID))
				h.sendAndSync(t, "speech", overlapSpeechEnvelope("speech", runID, utteranceID, "One."))
				release := overlapReleaseEnvelope("released", runID, utteranceID, "One.", action.Outcome{Completed: true, PlayedMS: 20})
				state := h.sendAndSync(t, "release", release)
				if !state.AgentOutput.Active || state.ActiveTTS != 0 || state.ActivePlayback != 0 {
					t.Fatalf("release must retire speech while preparation remains active: %+v", state)
				}
				assertNoEnvelope(t, h.output(t, "safe_release"))
				segments := 1
				switch remaining {
				case "queued-segment":
					// Even a speech segment whose fork has not drained must remain
					// pending when the terminal declares the emitted population.
					segments = 2
				case "other-run":
					h.sendAndSync(t, "invocation", overlapInvocationEnvelope("other-invoked", "other-run"))
				}
				for i, stage := range order {
					terminal := overlapModelEnvelope("model-terminal", runID)
					if stage == "segmentation" {
						terminal = overlapSegmentationEnvelope("segment-terminal", runID, OutcomeCompleted)
						terminal.Payload = SegmentationOutcome{Kind: OutcomeCompleted, RunID: runID, Segments: segments}
					}
					state = h.sendAndSync(t, stage, terminal)
					if i == 0 {
						assertNoEnvelope(t, h.output(t, "safe_release"))
					}
				}
				forwarded := receive(t, h.output(t, "safe_release"))
				boundary := forwarded.Payload.(speechelements.PlaybackRelease)
				if !reflect.DeepEqual(boundary.Receipt, release.Payload) ||
					!reflect.DeepEqual(boundary.AgentOutput, state.AgentOutput) ||
					boundary.AgentOutput.Active != (remaining != "none") ||
					!slices.Contains(forwarded.CausalParents, release.ItemID) ||
					!slices.Contains(forwarded.CausalParents, boundary.AgentOutputItemID) {
					t.Fatalf("completion lost its exact receipt or current work: %+v", boundary)
				}
				if remaining == "queued-segment" {
					h.sendAndSync(t, "speech", overlapSpeechEnvelope("second", runID, "second-utterance", "Two."))
					h.sendAndSync(t, "release", overlapReleaseEnvelope("second-release", runID, "second-utterance", "Two.", action.Outcome{Completed: true, PlayedMS: 20}))
					last := receive(t, h.output(t, "safe_release")).Payload.(speechelements.PlaybackRelease)
					if last.AgentOutput.Active {
						t.Fatalf("last segment left completed work active: %+v", last)
					}
				}
				// A late replay cannot produce another completion by flushing
				// already-consumed pending entries.
				h.sendAndSync(t, "model", overlapModelEnvelope("model-replay", runID))
				assertNoEnvelope(t, h.output(t, "safe_release"))
			})
		}
	}
}

func TestOverlapPlaybackReleaseDoesNotWaitForAnUnrelatedRun(t *testing.T) {
	h := mountOverlapBargeIn(t, `{"hold_ms":10,"unclassified":"cancel"}`, nil)
	defer h.stop(t)
	h.sendAndSync(t, "invocation", overlapInvocationEnvelope("slow-invoked", "slow"))
	h.sendAndSync(t, "release", overlapReleaseEnvelope("slow-release", "slow", "slow-utterance", "First.", action.Outcome{Completed: true}))
	assertNoEnvelope(t, h.output(t, "safe_release"))
	h.sendAndSync(t, "invocation", overlapInvocationEnvelope("ready-invoked", "ready"))
	h.sendAndSync(t, "model", overlapModelEnvelope("ready-model", "ready"))
	h.sendAndSync(t, "segmentation", overlapSegmentationEnvelope("ready-segment", "ready", OutcomeCompleted))
	h.sendAndSync(t, "release", overlapReleaseEnvelope("ready-release", "ready", "ready-utterance", "Second.", action.Outcome{Completed: true}))
	ready := receive(t, h.output(t, "safe_release")).Payload.(speechelements.PlaybackRelease)
	if ready.Receipt.Utterance.ID != "ready-utterance" || !ready.AgentOutput.Active {
		t.Fatalf("unrelated preparation blocked completion or disappeared: %+v", ready)
	}
	h.sendAndSync(t, "model", overlapModelEnvelope("slow-model", "slow"))
	h.sendAndSync(t, "segmentation", overlapSegmentationEnvelope("slow-segment", "slow", OutcomeCompleted))
	slow := receive(t, h.output(t, "safe_release")).Payload.(speechelements.PlaybackRelease)
	if slow.Receipt.Utterance.ID != "slow-utterance" || slow.AgentOutput.Active {
		t.Fatalf("deferred completion retained stale work: %+v", slow)
	}
}

func TestOverlapPlaybackReleaseBoundsPendingCompletions(t *testing.T) {
	h := mountOverlapBargeIn(t, `{"max_utterances":1,"hold_ms":10,"unclassified":"cancel"}`, nil)
	defer h.cancel()
	h.sendAndSync(t, "invocation", overlapInvocationEnvelope("invoked", "run"))
	h.sendAndSync(t, "release", overlapReleaseEnvelope("first-release", "run", "first", "One.", action.Outcome{Completed: true}))
	assertNoEnvelope(t, h.output(t, "safe_release"))
	if _, err := h.input(t, "release").Broadcast(t.Context(), overlapReleaseEnvelope("second-release", "run", "second", "Two.", action.Outcome{Completed: true})); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-h.done:
		if err == nil || !strings.Contains(err.Error(), "pending playback releases exceed") {
			t.Fatalf("unbounded pending completion accepted: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending completion overflow did not stop the graph")
	}
}

func TestOverlapPlaybackReleaseBackpressureCannotBlockModelResult(t *testing.T) {
	for _, drain := range []bool{false, true} {
		t.Run(fmt.Sprintf("drain=%t", drain), func(t *testing.T) {
			testOverlapPlaybackReleaseBackpressure(t, drain)
		})
	}
}

func testOverlapPlaybackReleaseBackpressure(t *testing.T, drain bool) {
	t.Helper()
	h := mountOverlapBargeIn(t, `{"hold_ms":10,"unclassified":"cancel"}`, nil)
	defer h.stop(t)
	const runID = "burst"
	h.sendAndSync(t, "invocation", overlapInvocationEnvelope("invoked", runID))
	for i := 0; i < 40; i++ {
		h.sendAndSync(t, "release", overlapReleaseEnvelope(fmt.Sprint("release-", i), runID, fmt.Sprint("utterance-", i), "One.", action.Outcome{Completed: true}))
	}
	terminal := overlapSegmentationEnvelope("segmented", runID, OutcomeCompleted)
	terminal.Payload = SegmentationOutcome{Kind: OutcomeCompleted, RunID: runID, Segments: 40}
	h.sendAndSync(t, "segmentation", terminal)
	h.sendAndSync(t, "model", overlapModelEnvelope("model-terminal", runID))
	result := element.Envelope{Type: SafeModelResultType(), ItemID: "safe-result", SessionID: overlapBargeInSession, RunID: runID, Payload: modelResult(runID, 0, "", false, false)}
	// A real completion consumer needs the committed model result before it
	// can record played history. Keep releases blocked until that result arrives.
	send(t, h.input(t, "result"), result)
	forwarded := receive(t, h.output(t, "safe_result"))
	if !slices.Contains(forwarded.CausalParents, result.ItemID) {
		t.Fatal(forwarded)
	}
	if drain {
		for i := 0; i < 40; i++ {
			_ = receive(t, h.output(t, "safe_release"))
		}
	}
}
