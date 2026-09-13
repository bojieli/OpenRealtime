package acoustic

import (
	"testing"
	"time"

	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
)

// A recogniser that ends the turn closes the utterance while the gate is still
// counting silence: the same flush and stopped activity a silence candidate
// produces, without waiting for the candidate.
func TestRecognizerTurnEndForceClosesAnOpenUtterance(t *testing.T) {
	harness := mountAcoustic(t, EndpointPolicyConfig{Mode: EndpointAutomatic})
	openSpeech(t, harness, "spoken")
	harness.send("turn_end", "turn-end-1", perceptionelements.TurnEnd{
		StreamID: "spoken", Signal: perceptionelements.TurnEndSignalEager,
	})
	command := payload[GateCommand](t, receive(t, harness.output("command")))
	if command.Action != GateForceClose || command.StreamID != "spoken" || command.Reason != "recognizer_eager" {
		t.Fatalf("turn end command = %+v", command)
	}
	stopped := payload[SpeechActivity](t, receive(t, harness.output("activity")))
	if stopped.Kind != SpeechStopped || stopped.StreamID != "spoken" {
		t.Fatalf("stopped activity = %+v", stopped)
	}
	flush := payload[perceptionelements.Flush](t, receive(t, harness.output("flush")))
	if flush.StreamID != "spoken" || flush.AfterItemID == "" {
		t.Fatalf("flush = %+v", flush)
	}
	awaitEndpointOutcome(t, harness, "turn-end-1", "recognizer_eager")

	// The same stream's turn end again - the recogniser moving from eager to
	// ended - does nothing more.
	harness.send("turn_end", "turn-end-2", perceptionelements.TurnEnd{
		StreamID: "spoken", Signal: perceptionelements.TurnEndSignalEndOfTurn,
	})
	awaitEndpointOutcome(t, harness, "turn-end-2", "duplicate_turn_end")
	assertNoEnvelope(t, harness.output("flush"), 75*time.Millisecond)
}

// A candidate the gate already raised is closed through the ordinary path.
func TestRecognizerTurnEndClosesAPendingCandidate(t *testing.T) {
	harness := mountAcoustic(t, EndpointPolicyConfig{
		Mode: EndpointExternal, CandidateTimeoutMS: 60_000, Fallback: GateClose,
	})
	candidate := reachCandidate(t, harness, "external", 0)
	// The harness sees the candidate on its own copy; the endpoint holds it
	// only once it reports it pending.
	awaitEndpointOutcome(t, harness, "external-frame", "awaiting_verdict_or_tick")
	harness.send("turn_end", "turn-end-pending", perceptionelements.TurnEnd{
		StreamID: "external", Signal: perceptionelements.TurnEndSignalEndOfTurn,
	})
	command := payload[GateCommand](t, receive(t, harness.output("command")))
	if command.Action != GateClose || command.CandidateID != candidate.ID || command.Reason != "recognizer_end_of_turn" {
		t.Fatalf("pending candidate command = %+v, candidate %+v", command, candidate)
	}
	awaitEndpointOutcome(t, harness, "turn-end-pending", "recognizer_end_of_turn")
}

// In manual mode the client owns every turn, so a recogniser's opinion closes
// nothing.
func TestRecognizerTurnEndIsIgnoredWhenTheClientOwnsTurns(t *testing.T) {
	harness := mountAcoustic(t, EndpointPolicyConfig{Mode: EndpointManual})
	harness.send("turn_end", "turn-end-manual", perceptionelements.TurnEnd{
		StreamID: "manual", Signal: perceptionelements.TurnEndSignalEndOfTurn,
	})
	awaitEndpointOutcome(t, harness, "turn-end-manual", "client_owns_turns")
	assertNoEnvelope(t, harness.output("command"), 75*time.Millisecond)
}

func TestRecognizerTurnEndRefusesAnUnknownSignal(t *testing.T) {
	harness := mountAcoustic(t, EndpointPolicyConfig{Mode: EndpointAutomatic})
	harness.send("turn_end", "turn-end-bad", perceptionelements.TurnEnd{StreamID: "spoken", Signal: "soon"})
	awaitEndpointOutcome(t, harness, "turn-end-bad", "invalid_payload")
}
