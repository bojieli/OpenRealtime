package scenarioconversation

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
)

type recordedScenarioCancellation struct {
	boundary string
	envelope element.Envelope
}

func TestCancelRevokesQueuedAndActiveSpeechAfterModelTerminal(t *testing.T) {
	for _, test := range []struct {
		name          string
		activeReceipt bool
		want          []string
	}{
		{name: "segmented before playback receipt", want: []string{"segmentation"}},
		{name: "active playback after model terminal", activeReceipt: true,
			want: []string{"segmentation", "playback", "tts"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var recorded []recordedScenarioCancellation
			record := func(boundary string, typeOf element.Type) element.OutputPort {
				return mediaTestOutput{typeOf: typeOf, broadcast: func(
					_ context.Context, envelope element.Envelope,
				) (element.SendResult, error) {
					recorded = append(recorded, recordedScenarioCancellation{
						boundary: boundary, envelope: envelope.Clone(),
					})
					return element.SendResult{Delivered: 1}, nil
				}}
			}
			session := &session{
				sessionID: "session-a", active: make(map[string]struct{}),
				calls: make(map[string]activeClientCall), playback: make(map[string]playbackReceiptState),
				speechRuns: make(map[string]int),
			}
			session.ports.segmentationCancel = record("segmentation", cognitionelements.CancelType())
			session.ports.playbackCancel = record("playback", speechelements.CancelType())
			session.ports.ttsCancel = record("tts", speechelements.CancelType())
			if err := session.acceptSegmentationOutcome(segmentationEnvelope(
				session.sessionID, "run-a", interactionelements.OutcomeCompleted, 1,
			)); err != nil {
				t.Fatal(err)
			}
			if test.activeReceipt {
				if err := session.acceptPlaybackReceipt(gatewaySpeechAudioBoundary,
					playbackEnvelope(session.sessionID, "run-a", "utterance-a",
						speechelements.PlaybackAudioEmitted, 4)); err != nil {
					t.Fatal(err)
				}
			}

			if err := session.Cancel(context.Background(), "  user interrupted playback  "); err != nil {
				t.Fatal(err)
			}
			got := make([]string, len(recorded))
			for index := range recorded {
				got[index] = recorded[index].boundary
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("cancellation boundaries = %v, want %v", got, test.want)
			}
			segmentation := recorded[0].envelope
			request, ok := segmentation.Payload.(cognitionelements.Cancel)
			if !ok || request.RunID != "run-a" || request.Reason != "user interrupted playback" ||
				segmentation.RunID != "run-a" || segmentation.CancellationScope != "run-a" ||
				!segmentation.Type.Equal(cognitionelements.CancelType()) {
				t.Fatalf("segmentation cancellation = %+v / %#v", segmentation, segmentation.Payload)
			}
			if !test.activeReceipt {
				return
			}
			for index, boundary := range []string{"playback", "tts"} {
				recorded := recorded[index+1]
				request, ok := recorded.envelope.Payload.(speechelements.Cancel)
				if recorded.boundary != boundary || !ok || request.UtteranceID != "utterance-a" ||
					request.Reason != "user interrupted playback" || recorded.envelope.RunID != "run-a" ||
					recorded.envelope.OpportunityID != "utterance-a" ||
					recorded.envelope.CancellationScope != "utterance-a" ||
					!recorded.envelope.Type.Equal(speechelements.CancelType()) ||
					!slices.Equal(recorded.envelope.CausalParents, []string{segmentation.ItemID}) {
					t.Fatalf("%s cancellation = %+v / %#v", boundary, recorded.envelope, recorded.envelope.Payload)
				}
			}

			// Terminal playback consumes the pending run and makes a repeated API
			// cancellation a true no-op rather than replaying exact interrupts.
			if err := session.acceptPlaybackReceipt(gatewaySpeechEndBoundary,
				playbackEnvelope(session.sessionID, "run-a", "utterance-a",
					speechelements.PlaybackEnded, 5)); err != nil {
				t.Fatal(err)
			}
			if _, pending := session.speechRuns["run-a"]; pending || session.playback["utterance-a"].active {
				t.Fatalf("terminal playback retained active speech: runs=%v playback=%+v",
					session.speechRuns, session.playback["utterance-a"])
			}
			recorded = nil
			if err := session.Cancel(context.Background(), "replayed cancellation"); err != nil {
				t.Fatal(err)
			}
			if len(recorded) != 0 {
				t.Fatalf("terminal playback replayed cancellation: %+v", recorded)
			}
		})
	}
}

func TestPlaybackReceiptTrackingHonorsSequenceAcrossConcurrentBoundaries(t *testing.T) {
	session := &session{
		sessionID: "session-a", playback: make(map[string]playbackReceiptState),
		speechRuns: make(map[string]int),
	}
	// The terminal lane may be scheduled before the earlier audio lane. Its
	// higher sequence must retain the terminal tombstone.
	if err := session.acceptPlaybackReceipt(gatewayTurnEndBoundary,
		playbackEnvelope(session.sessionID, "run-a", "utterance-a",
			speechelements.PlaybackReleased, 6)); err != nil {
		t.Fatal(err)
	}
	if err := session.acceptPlaybackReceipt(gatewaySpeechAudioBoundary,
		playbackEnvelope(session.sessionID, "run-a", "utterance-a",
			speechelements.PlaybackAudioEmitted, 4)); err != nil {
		t.Fatal(err)
	}
	state := session.playback["utterance-a"]
	if state.sequence != 6 || !state.terminal || state.active || state.kind != speechelements.PlaybackReleased {
		t.Fatalf("out-of-order playback state = %+v", state)
	}
	// Segmentation completion can also arrive after terminal playback. It must
	// account for the retained tombstone instead of reviving a pending run.
	if err := session.acceptSegmentationOutcome(segmentationEnvelope(
		session.sessionID, "run-a", interactionelements.OutcomeCompleted, 1,
	)); err != nil {
		t.Fatal(err)
	}
	if _, pending := session.speechRuns["run-a"]; pending {
		t.Fatalf("late segmentation completion revived terminal playback: %v", session.speechRuns)
	}
	if err := session.acceptPlaybackReceipt(gatewaySpeechBeginBoundary,
		playbackEnvelope(session.sessionID, "run-a", "utterance-a",
			speechelements.PlaybackBegun, 7)); err == nil ||
		!strings.Contains(err.Error(), "after terminal") {
		t.Fatalf("post-terminal playback revival error = %v", err)
	}
}

func segmentationEnvelope(
	sessionID, runID string, kind interactionelements.OutcomeKind, segments int,
) element.Envelope {
	return element.Envelope{
		Type: interactionelements.SegmentationOutcomeType(), ItemID: "segmentation-" + runID,
		SessionID: sessionID, RunID: runID, CancellationScope: runID,
		Payload: interactionelements.SegmentationOutcome{
			Kind: kind, RunID: runID, Segments: segments,
		},
	}
}

func playbackEnvelope(
	sessionID, runID, utteranceID string, kind speechelements.PlaybackReceiptKind,
	sequence uint64,
) element.Envelope {
	receipt := speechelements.PlaybackReceipt{
		Kind: kind, Sequence: sequence,
		Utterance: action.Utterance{ID: utteranceID, Text: "audible response"},
	}
	if kind == speechelements.PlaybackAudioEmitted {
		receipt.Frame = action.Frame{
			PCM16LE: []byte{1, 0, 2, 0}, SampleRateHz: 24_000, Duration: 2 * time.Millisecond,
		}
	}
	return element.Envelope{
		Type: speechelements.PlaybackReceiptType(), ItemID: "receipt-" + utteranceID,
		SessionID: sessionID, SourceID: utteranceID, RunID: runID,
		CancellationScope: utteranceID, Payload: receipt,
	}
}
