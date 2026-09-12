package interaction

import (
	"encoding/json"
	"testing"

	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
)

// TestControlTokenNeverReachesSpeech covers a token being read out loud.
//
// <wait> is how a model says "stay quiet"; it is an instruction to the runtime
// and never a word. The legacy cascade withheld any turn containing it, but the
// graph-native path had no equivalent, so the token was segmented like prose
// and handed to the synthesiser, which pronounced it.
func TestControlTokenNeverReachesSpeech(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, segmentGraph,
		map[string]json.RawMessage{"segment": json.RawMessage(`{"minimum_runes":2}`)}, nil)
	defer stopInteractionGraph(t, done, cancel)

	const runID = "wait-run"
	text := ingress(t, mounted, "text")
	segments := egress(t, mounted, "segments")
	send(t, text, preparedEnvelope("begin", runID, cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextBegin, Index: 0,
	}))
	// The token alone, which is what the instruction asks the model to emit.
	send(t, text, preparedEnvelope("delta", runID, cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextChunk, Index: 1, Text: coreinteraction.WaitToken,
	}))
	send(t, text, preparedEnvelope("end", runID, cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextEnd, Index: 2,
	}))

	assertNoEnvelope(t, segments)
	outcome := receiveSegmentationOutcome(t, egress(t, mounted, "outcome"), OutcomeCompleted)
	if outcome.Segments != 0 || outcome.Code != "control_token_silence" {
		t.Fatalf("a silent run must say so: %+v", outcome)
	}
}

// A turn that both speaks and asks for silence is a model in two minds. The
// token must not be spoken, and the safe reading of a token whose whole purpose
// is silence is silence - not the prose around it.
func TestControlTokenMixedWithProseSpeaksNothing(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, segmentGraph,
		map[string]json.RawMessage{"segment": json.RawMessage(`{"minimum_runes":2}`)}, nil)
	defer stopInteractionGraph(t, done, cancel)

	const runID = "menu-run"
	text := ingress(t, mounted, "text")
	segments := egress(t, mounted, "segments")
	send(t, text, preparedEnvelope("begin", runID, cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextBegin, Index: 0,
	}))
	send(t, text, preparedEnvelope("delta", runID, cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextChunk, Index: 1,
		Text: "Pressing the key for order status. " + coreinteraction.WaitToken,
	}))
	send(t, text, preparedEnvelope("end", runID, cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextEnd, Index: 2,
	}))

	assertNoEnvelope(t, segments)
}
