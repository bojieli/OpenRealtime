package cascade

import "testing"

// The mark says the agent has already spoken for this much of what they are
// saying, and the voice is told to say nothing that is only about that part.
// So it must record speech that happened, never speech that was attempted: a
// turn that comes back <wait>, or is withheld, has said nothing.
//
// The interjection path used to set it before the model was asked, so an
// attempt that produced nothing still counted. Measured on the interrupting
// scenario, the agent said nothing at all and was then told "you have already
// spoken once for this much of what they are saying", quoting the sentence
// with the wrong date in it - so the one fact worth interrupting for was the
// one fact it believed it had already given. Self-reinforcing, and it silences
// exactly the case that path exists for.
//
// runFast owns the mark now, and sets it where an ordinary turn always did:
// after publishing, with something in it.
func TestNothingIsCoveredUntilSomethingWasActuallySaid(t *testing.T) {
	runtime := &runtime{}
	const said = "we ship it by the thirteenth"
	if got := runtime.heardSinceSpeaking(said); got != said {
		t.Fatalf("before the agent spoke, what is new was %q", got)
	}
	// A turn that ran and said nothing must leave the whole utterance new,
	// which is what makes the next attempt free to say the thing.
	if got := runtime.heardSinceSpeaking(said); got != said {
		t.Fatalf("after a turn that said nothing, what is new was %q", got)
	}
	runtime.markSpoken(said)
	if got := runtime.heardSinceSpeaking(said); got != "" {
		t.Fatalf("after speaking, what is new was %q", got)
	}
}
