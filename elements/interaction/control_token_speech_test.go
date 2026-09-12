package interaction

import (
	"encoding/json"
	"testing"

	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	"github.com/bojieli/OpenRealtime/elements/speech"
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

// A turn that speaks and then asks for silence has said what came before the
// token: "Two.<wait>" is a count with the closing token in the same breath,
// and losing the count is worse than an extra sentence. The token is never
// spoken, and nothing after it is.
func TestControlTokenAfterProseKeepsTheProse(t *testing.T) {
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
		Text: "Two." + coreinteraction.WaitToken + " Three.",
	}))
	send(t, text, preparedEnvelope("end", runID, cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextEnd, Index: 2,
	}))

	segment := receive(t, segments)
	if spoken, _ := segment.Payload.(speech.TextSegment); spoken.Text != "Two." {
		t.Fatalf("segment = %+v, want the words before the token and nothing after it", segment.Payload)
	}
	assertNoEnvelope(t, segments)
	outcome := receiveSegmentationOutcome(t, egress(t, mounted, "outcome"), OutcomeCompleted)
	if outcome.Segments != 1 || outcome.Code == "control_token_silence" {
		t.Fatalf("a run that spoke before the token is not a silent run: %+v", outcome)
	}
}

// A streaming provider hands the token over in pieces, and no piece is the
// token. Measured, "<" then "wait" then ">" was segmented as prose and the
// synthesiser said "wait" to a phone menu.
func TestControlTokenSplitAcrossDeltasNeverReachesSpeech(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, segmentGraph,
		map[string]json.RawMessage{"segment": json.RawMessage(`{"minimum_runes":2}`)}, nil)
	defer stopInteractionGraph(t, done, cancel)

	const runID = "split-wait-run"
	text := ingress(t, mounted, "text")
	segments := egress(t, mounted, "segments")
	send(t, text, preparedEnvelope("begin", runID, cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextBegin, Index: 0,
	}))
	for index, piece := range []string{"<", "wait", ">"} {
		send(t, text, preparedEnvelope("delta", runID, cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextChunk, Index: uint64(index + 1), Text: piece,
		}))
	}
	send(t, text, preparedEnvelope("end", runID, cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextEnd, Index: 4,
	}))

	assertNoEnvelope(t, segments)
	outcome := receiveSegmentationOutcome(t, egress(t, mounted, "outcome"), OutcomeCompleted)
	if outcome.Segments != 0 || outcome.Code != "control_token_silence" {
		t.Fatalf("a run that spelled the token in pieces must still be silent: %+v", outcome)
	}
}

// A stream that ends on a lone unfinished word after complete sentences did
// not finish that word; it is not spoken. A run of one word is still said.
func TestDanglingWordAfterSentencesIsNotSpoken(t *testing.T) {
	for _, test := range []struct {
		name string
		text string
		want []string
	}{
		{"cut recitation", "Twelve. Thirteen. Eight", []string{"Twelve.", "Thirteen."}},
		{"one word answer", "Yes", []string{"Yes"}},
		{"finished tail", "Twelve. Thirteen.", []string{"Twelve.", "Thirteen."}},
	} {
		t.Run(test.name, func(t *testing.T) {
			mounted, done, cancel := mountInteractionGraph(t, segmentGraph,
				map[string]json.RawMessage{"segment": json.RawMessage(`{"minimum_runes":2}`)}, nil)
			defer stopInteractionGraph(t, done, cancel)
			text := ingress(t, mounted, "text")
			segments := egress(t, mounted, "segments")
			send(t, text, preparedEnvelope("begin", "dangling", cognitionelements.PreparedTextDelta{Boundary: cognitionelements.TextBegin, Index: 0}))
			send(t, text, preparedEnvelope("delta", "dangling", cognitionelements.PreparedTextDelta{Boundary: cognitionelements.TextChunk, Index: 1, Text: test.text}))
			send(t, text, preparedEnvelope("end", "dangling", cognitionelements.PreparedTextDelta{Boundary: cognitionelements.TextEnd, Index: 2}))
			for _, want := range test.want {
				if got := receive(t, segments).Payload.(speech.TextSegment).Text; got != want {
					t.Fatalf("segment = %q, want %q", got, want)
				}
			}
			assertNoEnvelope(t, segments)
		})
	}
}
