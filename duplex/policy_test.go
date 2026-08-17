package duplex

import "testing"

func TestPolicySeparatesInterruptionBackchannelAndSideSpeech(t *testing.T) {
	t.Parallel()
	policy := DefaultPolicy()
	tests := []struct {
		kind EvidenceKind
		want Decision
	}{
		{EvidenceDirectedSpeech, DecisionYield},
		{EvidenceBackchannel, DecisionNoteBackchannel},
		{EvidenceSideSpeech, DecisionIgnoreSide},
		{EvidenceAmbiguous, DecisionContinue},
	}
	for _, test := range tests {
		decision, err := policy.Decide(StateSystemSpeaking, Evidence{Kind: test.kind, Confidence: 0.9})
		if err != nil {
			t.Fatal(err)
		}
		if decision != test.want {
			t.Fatalf("kind %s decision = %s, want %s", test.kind, decision, test.want)
		}
	}
}

func TestTurnStateMachineRejectsImpossibleJump(t *testing.T) {
	t.Parallel()
	machine, err := NewStateMachine(StateListening)
	if err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(StateSystemSpeaking); err == nil {
		t.Fatal("listening must not jump directly to system speaking")
	}
	for _, state := range []TurnState{StateSystemPreparing, StateSystemSpeaking, StateOverlapBackchannel, StateSystemSpeaking, StateListening} {
		if err := machine.Transition(state); err != nil {
			t.Fatalf("transition to %s failed: %v", state, err)
		}
	}
}
