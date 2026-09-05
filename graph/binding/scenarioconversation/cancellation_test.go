package scenarioconversation

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	"github.com/bojieli/OpenRealtime/trajectory"
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
				if err := session.acceptPlaybackReceipt(context.Background(), gatewaySpeechAudioBoundary,
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
			if err := session.acceptPlaybackReceipt(context.Background(), gatewaySpeechEndBoundary,
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

func TestCancelRevokesForegroundSpeechBetweenModelAndSegmentationTerminal(t *testing.T) {
	var recorded []recordedScenarioCancellation
	session := &session{
		sessionID: "session-a", active: make(map[string]struct{}),
		calls: make(map[string]activeClientCall), playback: make(map[string]playbackReceiptState),
		pendingSpeech: make(map[string]struct{}), speechRuns: make(map[string]int),
		terminalRuns: make(map[string]struct{}),
	}
	session.ports.segmentationCancel = mediaTestOutput{
		typeOf: cognitionelements.CancelType(), broadcast: func(
			_ context.Context, envelope element.Envelope,
		) (element.SendResult, error) {
			recorded = append(recorded, recordedScenarioCancellation{
				boundary: "segmentation", envelope: envelope.Clone(),
			})
			return element.SendResult{Delivered: 1}, nil
		},
	}
	if err := session.registerEmittedInvocation(invocationOutcome(
		"run-a", "foreground",
	)); err != nil {
		t.Fatal(err)
	}
	if err := session.acceptModelOutcome(context.Background(), element.Envelope{
		SessionID: session.sessionID, RunID: "run-a",
		Payload: cognitionelements.Outcome{
			Kind: cognitionelements.OutcomeSucceeded, Operation: "generate", RunID: "run-a",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, active := session.active["run-a"]; active {
		t.Fatal("terminal model run remained generation-active")
	}
	if _, pending := session.pendingSpeech["run-a"]; !pending {
		t.Fatal("model terminal discarded the pending segmentation horizon")
	}

	if err := session.Cancel(context.Background(), "user interrupted before segment receipt"); err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 1 || recorded[0].boundary != "segmentation" ||
		recorded[0].envelope.RunID != "run-a" {
		t.Fatalf("terminal-to-segmentation cancellation = %+v", recorded)
	}
}

func TestModelTerminalBeforeInvocationReceiptStillRetainsSpeechHorizon(t *testing.T) {
	var recorded []element.Envelope
	session := &session{
		sessionID: "session-a", active: make(map[string]struct{}),
		calls: make(map[string]activeClientCall), playback: make(map[string]playbackReceiptState),
		pendingSpeech: make(map[string]struct{}), speechRuns: make(map[string]int),
		terminalRuns: make(map[string]struct{}),
	}
	session.ports.segmentationCancel = mediaTestOutput{
		typeOf: cognitionelements.CancelType(), broadcast: func(
			_ context.Context, envelope element.Envelope,
		) (element.SendResult, error) {
			recorded = append(recorded, envelope.Clone())
			return element.SendResult{Delivered: 1}, nil
		},
	}
	if err := session.acceptModelOutcome(context.Background(), element.Envelope{
		SessionID: session.sessionID, RunID: "run-reordered",
		Payload: cognitionelements.Outcome{
			Kind: cognitionelements.OutcomeSucceeded, Operation: "generate", RunID: "run-reordered",
		},
	}); err != nil {
		t.Fatal(err)
	}
	// The invocation outcome drains on another boundary and may arrive after
	// the model terminal. It must not erase or duplicate the conservative
	// speech horizon established by terminal evidence.
	if err := session.registerEmittedInvocation(invocationOutcome(
		"run-reordered", "foreground",
	)); err != nil {
		t.Fatal(err)
	}
	if err := session.Cancel(context.Background(), "reordered interruption"); err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 1 || recorded[0].RunID != "run-reordered" {
		t.Fatalf("reordered terminal cancellation = %+v", recorded)
	}
}

func TestCommittedRunReplayAfterCancellationIsNotAUserVisibleFailure(t *testing.T) {
	sink := &cancellationFailureSink{}
	session := &session{
		sessionID: "session-a", sink: sink,
		active: make(map[string]struct{}), pendingSpeech: make(map[string]struct{}),
		speechlessRuns: make(map[string]struct{}), segmentedRuns: make(map[string]struct{}),
		terminalRuns: make(map[string]struct{}),
	}
	outcome := cognitionelements.Outcome{
		Kind: cognitionelements.OutcomeRefused, Operation: "generate", RunID: "run-canceled",
		Code:    "committed_run_replay",
		Message: "committed generation run is already terminal after cancellation",
	}
	if err := session.acceptModelOutcome(context.Background(), element.Envelope{
		SessionID: session.sessionID, RunID: outcome.RunID, Payload: outcome,
	}); err != nil {
		t.Fatal(err)
	}
	if len(sink.errors) != 0 || sink.turnBegun != 0 || sink.turnEnded != 0 {
		t.Fatalf("cancellation proof became a user-visible failure: %+v", sink)
	}
	if _, terminal := session.terminalRuns[outcome.RunID]; !terminal {
		t.Fatal("cancellation proof did not retain the terminal run tombstone")
	}

	ordinary := outcome
	ordinary.RunID = "run-refused"
	ordinary.Code = "provider_refused"
	ordinary.Message = "provider refused generation"
	if err := session.acceptModelOutcome(context.Background(), element.Envelope{
		SessionID: session.sessionID, RunID: ordinary.RunID, Payload: ordinary,
	}); err != nil {
		t.Fatal(err)
	}
	if len(sink.errors) != 1 || sink.errors[0].Code != ordinary.Code {
		t.Fatalf("ordinary refusal was not reported: %+v", sink.errors)
	}
}

type cancellationFailureSink struct {
	playbackClientSink
	errors []legacy.ErrorEvent
}

func (sink *cancellationFailureSink) Failed(_ context.Context, event legacy.ErrorEvent) {
	sink.errors = append(sink.errors, event)
}

func TestToolOnlyForegroundResultClosesPendingSpeechHorizon(t *testing.T) {
	session := &session{
		sessionID: "session-a", active: make(map[string]struct{}),
		pendingSpeech: make(map[string]struct{}), terminalRuns: make(map[string]struct{}),
	}
	if err := session.registerEmittedInvocation(invocationOutcome(
		"run-tool", "foreground",
	)); err != nil {
		t.Fatal(err)
	}
	if err := session.acceptModelResult(element.Envelope{
		SessionID: session.sessionID, RunID: "run-tool",
		Payload: cognitionelements.Result{
			RunID: "run-tool", ProviderReference: ModelReference,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, pending := session.pendingSpeech["run-tool"]; pending {
		t.Fatal("tool-only foreground result retained a nonexistent speech horizon")
	}
	if err := session.acceptModelResult(element.Envelope{
		SessionID: session.sessionID, RunID: "run-result-first",
		Payload: cognitionelements.Result{
			RunID: "run-result-first", ProviderReference: ModelReference,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.registerEmittedInvocation(invocationOutcome(
		"run-result-first", "foreground",
	)); err != nil {
		t.Fatal(err)
	}
	if _, pending := session.pendingSpeech["run-result-first"]; pending {
		t.Fatal("late invocation receipt revived result-proven speechless run")
	}
}

func TestSegmentationTerminalPreventsLateSpeechHorizonRevival(t *testing.T) {
	session := &session{
		sessionID: "session-a", active: make(map[string]struct{}),
		playback:      make(map[string]playbackReceiptState),
		pendingSpeech: make(map[string]struct{}), speechRuns: make(map[string]int),
		terminalRuns: make(map[string]struct{}),
	}
	if err := session.acceptSegmentationOutcome(segmentationEnvelope(
		session.sessionID, "run-segmented", interactionelements.OutcomeCompleted, 1,
	)); err != nil {
		t.Fatal(err)
	}
	if err := session.acceptPlaybackReceipt(context.Background(), gatewaySpeechEndBoundary,
		playbackEnvelope(session.sessionID, "run-segmented", "utterance-segmented",
			speechelements.PlaybackEnded, 5)); err != nil {
		t.Fatal(err)
	}
	if _, pending := session.speechRuns["run-segmented"]; pending {
		t.Fatal("terminal playback retained the completed speech run")
	}
	if err := session.registerEmittedInvocation(invocationOutcome(
		"run-segmented", "foreground",
	)); err != nil {
		t.Fatal(err)
	}
	if err := session.acceptModelResult(element.Envelope{
		SessionID: session.sessionID, RunID: "run-segmented",
		Payload: cognitionelements.Result{
			RunID: "run-segmented", ProviderReference: ModelReference,
			AssistantText: "text whose segmentation terminal already drained",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.acceptModelOutcome(context.Background(), element.Envelope{
		SessionID: session.sessionID, RunID: "run-segmented",
		Payload: cognitionelements.Outcome{
			Kind: cognitionelements.OutcomeSucceeded, Operation: "generate", RunID: "run-segmented",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, pending := session.pendingSpeech["run-segmented"]; pending {
		t.Fatal("late model boundaries revived a terminal segmentation horizon")
	}
}

func invocationOutcome(runID, role string) policyelements.SessionInvocationOutcome {
	return policyelements.SessionInvocationOutcome{
		Kind: policyelements.SessionInvocationEmitted, Operation: "create",
		GenerationID: runID, Role: role,
	}
}

func TestPlaybackReceiptTrackingHonorsSequenceAcrossConcurrentBoundaries(t *testing.T) {
	client := &playbackClientSink{}
	playbackSink := newSessionPlaybackSink(context.Background(), client,
		scenarioPlaybackDescriptor(), newPresentationState(legacy.Settings{}))
	utterance := action.Utterance{ID: "utterance-a", Text: "audible response"}
	if err := playbackSink.Reserve(utterance); err != nil {
		t.Fatal(err)
	}
	if err := playbackSink.Begin(context.Background(), utterance); err != nil {
		t.Fatal(err)
	}
	if err := playbackSink.End(context.Background(), utterance, action.Outcome{}); err != nil {
		t.Fatal(err)
	}
	store := trajectory.NewStore()
	if err := store.Append(trajectory.Item{
		ID: "assistant-run-a", Kind: trajectory.KindAssistant, InvocationID: "run-a",
		Producer: trajectory.Producer{Phase: trajectory.PhaseSlow},
		Content:  "audible response", Visibility: trajectory.VisibilityPrepared,
	}); err != nil {
		t.Fatal(err)
	}
	session := &session{
		sessionID: "session-a", playback: make(map[string]playbackReceiptState),
		speechRuns: make(map[string]int), bundle: &sessionBundle{playback: playbackSink, store: store},
	}
	bindPlaybackStateLoopback(t, session)
	// The terminal lane may be scheduled before the earlier audio lane. Its
	// higher sequence must retain the terminal tombstone.
	if err := session.acceptPlaybackReceipt(context.Background(), gatewayTurnEndBoundary,
		playbackEnvelope(session.sessionID, "run-a", "utterance-a",
			speechelements.PlaybackReleased, 6)); err != nil {
		t.Fatal(err)
	}
	if err := session.acceptPlaybackReceipt(context.Background(), gatewaySpeechAudioBoundary,
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
	if err := session.acceptPlaybackReceipt(context.Background(), gatewaySpeechBeginBoundary,
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
