package scenarioconversation

import (
	"errors"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	acousticelements "github.com/bojieli/OpenRealtime/elements/acoustic"
)

func recognizerCloseSession() *session {
	return &session{
		sessionID: "session-a", audioStream: 1,
		closedAudio: make(map[string]struct{}), utterances: make(map[string]struct{}),
		audioOps: make(map[string]*pendingAudio),
	}
}

// A recogniser's turn end force-closes the stream through the endpoint policy.
// Its outcome names no audio caller still waiting, and it is still the end of
// the utterance.
func TestRecognizerForceCloseAdvancesTheAudioStream(t *testing.T) {
	session := recognizerCloseSession()
	current := session.audioStreamID(1)
	outcome := acousticelements.AdmissionOutcome{
		Kind: acousticelements.OutcomeSucceeded, Operation: "command",
		StreamID: current, Code: "force_closed",
	}
	if err := session.acceptAdmissionOutcome(element.Envelope{
		ItemID: "command-outcome", SessionID: session.sessionID, Payload: outcome,
	}); err != nil {
		t.Fatal(err)
	}
	if session.audioStream != 2 {
		t.Fatalf("audio stream = %d after a recogniser close, want 2", session.audioStream)
	}
	if _, closed := session.closedAudio[current]; !closed {
		t.Fatal("the force-closed stream is not recorded for its delayed activity")
	}

	// A force close of a stream the adapter already left is history, not a
	// second advance.
	if err := session.acceptAdmissionOutcome(element.Envelope{
		ItemID: "stale-command-outcome", SessionID: session.sessionID, Payload: outcome,
	}); err != nil {
		t.Fatal(err)
	}
	if session.audioStream != 2 {
		t.Fatalf("a stale force close advanced the stream to %d", session.audioStream)
	}
}

// Delivering an audio result wakes its caller before that caller removes the
// pending entry. A recognizer close can retain that frame as a causal parent
// and arrive during this interval; completing audio did not close the stream.
func TestRecognizerForceCloseAfterCompletedAudioAdvancesStream(t *testing.T) {
	session := recognizerCloseSession()
	stream := session.audioStreamID(1)
	pending := &pendingAudio{itemID: "frame-1", streamID: stream, result: make(chan error, 1)}
	session.audioOps[pending.itemID] = pending
	send := func(operation, code string) error {
		return session.acceptAdmissionOutcome(element.Envelope{
			ItemID: operation + ":outcome", SessionID: session.sessionID,
			CausalParents: []string{pending.itemID},
			Payload: acousticelements.AdmissionOutcome{
				Kind: acousticelements.OutcomeSucceeded, Operation: operation,
				StreamID: stream, Code: code,
			},
		})
	}
	if err := send("audio", "admitted"); err != nil {
		t.Fatal(err)
	}
	if result := <-pending.result; result != nil {
		t.Fatal(result)
	}
	if err := send("command", "force_closed"); err != nil {
		t.Fatal(err)
	}
	if session.audioStream != 2 {
		t.Fatalf("recognizer close left audio stream at %d, want 2", session.audioStream)
	}
	if _, closed := session.closedAudio[stream]; !closed {
		t.Fatal("closed stream was not retained for delayed activity")
	}
	if err := send("command", "force_closed"); err != nil {
		t.Fatal(err)
	}
	if session.audioStream != 2 {
		t.Fatal("duplicate close advanced the stream twice")
	}
	select {
	case result := <-pending.result:
		t.Fatalf("close answered completed audio again: %v", result)
	default:
	}
	if err := send("audio", "admitted"); err == nil {
		t.Fatal("duplicate audio outcome was accepted")
	}
}

// The one frame in flight while its stream closed is refused as terminal. The
// adapter has moved on, so that frame is the next utterance's first audio and
// is sent again; while the adapter has not moved on, terminal audio stays an
// error.
func TestTerminalAudioIsResentOnlyAfterTheStreamAdvanced(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		advanced bool
		want     error
	}{
		{name: "stream advanced", advanced: true, want: errAudioStreamAdvanced},
		{name: "stream not advanced", advanced: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			session := recognizerCloseSession()
			pending := &pendingAudio{itemID: "frame-1", streamID: session.audioStreamID(1), result: make(chan error, 1)}
			session.audioOps[pending.itemID] = pending
			if testCase.advanced {
				session.audioStream = 2
			}
			err := session.acceptAdmissionOutcome(element.Envelope{
				ItemID: "frame-1:outcome", SessionID: session.sessionID, CausalParents: []string{"frame-1"},
				Payload: acousticelements.AdmissionOutcome{
					Kind: acousticelements.OutcomeIgnored, Operation: "audio",
					StreamID: pending.streamID, Code: "stream_terminal",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			result := <-pending.result
			if testCase.want != nil {
				if !errors.Is(result, testCase.want) {
					t.Fatalf("result = %v, want %v", result, testCase.want)
				}
				return
			}
			if result == nil || errors.Is(result, errAudioStreamAdvanced) {
				t.Fatalf("terminal audio on the current stream was not reported: %v", result)
			}
		})
	}
}

// The gate's silence candidate and a recogniser's turn end can land on the
// same frame. When the force close answers the frame waiting for its
// candidate, the stream has closed all the same.
func TestCandidateFrameAcceptsARecognizerForceClose(t *testing.T) {
	session := recognizerCloseSession()
	pending := &pendingAudio{itemID: "frame-1", streamID: session.audioStreamID(1), result: make(chan error, 1)}
	session.audioOps[pending.itemID] = pending
	frameOutcome := func(itemID string, outcome acousticelements.AdmissionOutcome) error {
		return session.acceptAdmissionOutcome(element.Envelope{
			ItemID: itemID, SessionID: session.sessionID, CausalParents: []string{"frame-1"}, Payload: outcome,
		})
	}
	if err := frameOutcome("frame-1:outcome", acousticelements.AdmissionOutcome{
		Kind: acousticelements.OutcomePending, Operation: "audio", StreamID: pending.streamID,
		Code: "endpoint_candidate", CandidateID: "candidate-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := frameOutcome("turn-end:outcome", acousticelements.AdmissionOutcome{
		Kind: acousticelements.OutcomeSucceeded, Operation: "command", StreamID: pending.streamID,
		Code: "force_closed",
	}); err != nil {
		t.Fatal(err)
	}
	if result := <-pending.result; result != nil {
		t.Fatalf("a force close answering a candidate frame failed the frame: %v", result)
	}
	if session.audioStream != 2 {
		t.Fatalf("audio stream = %d, want 2", session.audioStream)
	}
}

// The loser of the close race arrives after the winner completed the frame.
// It is dropped; a duplicate audio outcome is still a fault.
func TestLosingCloseAfterACompletedFrameIsDropped(t *testing.T) {
	session := recognizerCloseSession()
	pending := &pendingAudio{itemID: "frame-1", streamID: session.audioStreamID(1), result: make(chan error, 1)}
	session.audioOps[pending.itemID] = pending
	send := func(itemID string, outcome acousticelements.AdmissionOutcome) error {
		return session.acceptAdmissionOutcome(element.Envelope{
			ItemID: itemID, SessionID: session.sessionID, CausalParents: []string{"frame-1"}, Payload: outcome,
		})
	}
	if err := send("frame-1:outcome", acousticelements.AdmissionOutcome{
		Kind: acousticelements.OutcomePending, Operation: "audio", StreamID: pending.streamID,
		Code: "endpoint_candidate", CandidateID: "candidate-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := send("turn-end:outcome", acousticelements.AdmissionOutcome{
		Kind: acousticelements.OutcomeSucceeded, Operation: "command", StreamID: pending.streamID, Code: "force_closed",
	}); err != nil {
		t.Fatal(err)
	}
	if err := send("candidate:outcome", acousticelements.AdmissionOutcome{
		Kind: acousticelements.OutcomeIgnored, Operation: "command", StreamID: pending.streamID, Code: "no_pending_candidate",
	}); err != nil {
		t.Fatalf("the losing close of the race failed the session: %v", err)
	}
	if result := <-pending.result; result != nil {
		t.Fatalf("frame result = %v", result)
	}
	select {
	case extra := <-pending.result:
		t.Fatalf("the frame was answered twice: %v", extra)
	default:
	}
	if err := send("frame-1:duplicate", acousticelements.AdmissionOutcome{
		Kind: acousticelements.OutcomeSucceeded, Operation: "audio", StreamID: pending.streamID, Code: "admitted",
	}); err == nil {
		t.Fatal("a duplicate audio outcome for a completed frame was accepted")
	}
}
