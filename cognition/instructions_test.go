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

// TestAPhaseThatIsNeverHeardIsNotToldToSpeak is the regression for a zero that
// reached the conversation. The policies people set out loud are written as
// instructions to whoever is speaking - "count the animals out loud as I
// mention them" - and the reasoner, which is never heard, answered them: it
// wrote "0", the conversation recorded it, and every count afterwards was
// measured from there.
func TestAPhaseThatIsNeverHeardIsNotToldToSpeak(t *testing.T) {
	policies := []string{"count the animals out loud as I mention them (1m ago)"}
	spoken := cognition.Instruct("", cognition.Request{Standing: policies})
	silent := cognition.Instruct("", cognition.Request{Standing: policies, Silent: true})

	if !strings.Contains(spoken, "govern what you say and when") {
		t.Fatalf("the voice was not told the policies govern it:\n%s", spoken)
	}
	if strings.Contains(silent, "govern what you say and when") {
		t.Fatalf("a phase that is never heard was told the policies govern what it says:\n%s", silent)
	}
	if !strings.Contains(silent, "You are not the voice") {
		t.Fatalf("a phase that is never heard was not told so:\n%s", silent)
	}
	// Both still carry the policy itself: the reasoner needs to know what was
	// asked for, it just must not answer it out loud.
	for _, composed := range []string{spoken, silent} {
		if !strings.Contains(composed, "count the animals out loud") {
			t.Fatalf("the policy itself was dropped:\n%s", composed)
		}
	}
}

// TestAnUnknownAnswerDoesNotTurnAQuestionIntoAnUnfinishedSentence guards the
// distinction the fast voice has to make before it chooses silence. The model
// used to return <wait> for a complete question about a memory it did not
// possess: it inferred that the person had more to say from the absence of an
// answer, and a generation that finished in milliseconds became seconds of
// dead air. Completeness belongs to the user's words; answer availability is a
// separate decision.
func TestAnUnknownAnswerDoesNotTurnAQuestionIntoAnUnfinishedSentence(t *testing.T) {
	for _, obligation := range []string{
		"Never reply with " + cognition.WaitToken + " to a grammatical interrogative in any language",
		"not punctuation supplied by the speech recogniser",
		"Missing memory",
		"never silence",
		"only after determining that there is no new complete question, request, or proposition",
		"An infinitive marker with no following action is unfinished",
	} {
		if !strings.Contains(cognition.FastInstruction, obligation) {
			t.Fatalf("fast instruction lost the turn-completeness obligation %q", obligation)
		}
	}
}
