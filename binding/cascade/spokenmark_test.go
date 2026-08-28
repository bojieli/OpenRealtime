package cascade

import (
	"os"
	"strings"
	"testing"
)

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

// Withholding is not a failure - it succeeds at not speaking - so it cannot be
// reported as one. Every way of withholding returned nil, which the caller read
// as "published", and <wait> is text so the emptiness test did not catch it
// either. publishAssistant says whether anything was spoken, separately from
// whether anything went wrong, because those are different facts.
func TestWithholdingIsNotAnErrorAndIsNotSpeaking(t *testing.T) {
	source, err := os.ReadFile("process.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(source)
	start := strings.Index(body, "func (runtime *runtime) publishAssistant(")
	if start < 0 {
		t.Fatal("publishAssistant is gone")
	}
	signature := body[start : start+220]
	if !strings.Contains(signature, "(spoke bool, err error)") {
		t.Fatalf("publishing no longer reports whether it spoke: %q", signature)
	}
	// A withheld turn must never be reported as having spoken.
	for _, line := range strings.Split(body[start:], "\n") {
		if strings.Contains(line, "withholdAssistant(") && strings.Contains(line, "return") &&
			!strings.Contains(line, "return false,") {
			t.Fatalf("a withheld turn is reported as spoken: %q", strings.TrimSpace(line))
		}
	}
}
