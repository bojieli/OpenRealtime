package cognition_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/cognition"
)

// TestTheVoiceIsAlwaysToldHowToSayNothing is the regression for three
// acknowledgements of one instruction. The voice was told to say nothing
// rather than agree twice, and the paragraph explaining how to say nothing was
// attached only when a standing policy was already in force - so on every
// other turn the rule asked for a silence the model had no way to produce, and
// a model whose only channel is speech said "I'm ready. Please begin." again.
func TestTheVoiceIsAlwaysToldHowToSayNothing(t *testing.T) {
	if !strings.Contains(cognition.FastInstruction, cognition.WaitToken) {
		t.Fatal("the voice's own instruction never names the way to say nothing")
	}
	if !strings.Contains(cognition.FastInstruction, "say nothing rather than agreeing again") {
		t.Fatal("the rule that needs it is gone")
	}
}
