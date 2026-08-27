package cascade

import (
	"testing"
	"time"
)

// TestATurnIsNotHeldOpenForever is the regression for an interpreting policy
// that said not to wait for the speaker to finish. The floor read it as never
// ending the turn, the gate opened four seconds in and did not close for the
// remaining thirty-one, nothing was ever committed, and the agent said nothing
// at all. The floor's own liveness bound is measured from the last pause and
// the pause clock resets whenever the speaker says something new, so a speaker
// who keeps talking resets it forever.
func TestATurnIsNotHeldOpenForever(t *testing.T) {
	runtime := &runtime{}
	runtime.config.HoldLimit = 20 * time.Second
	const second = uint64(time.Second)

	if runtime.heldTooLong(10 * second) {
		t.Fatal("the first pause of an utterance was already too long")
	}
	if runtime.heldTooLong(25 * second) {
		t.Fatal("fifteen seconds of holding was refused")
	}
	if !runtime.heldTooLong(31 * second) {
		t.Fatal("twenty-one seconds of holding was allowed")
	}
	// A new utterance gets the whole bound again.
	runtime.holdStartNS = 0
	if runtime.heldTooLong(100 * second) {
		t.Fatal("a fresh utterance inherited a spent clock")
	}
}
