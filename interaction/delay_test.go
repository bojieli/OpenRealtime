package interaction_test

import (
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/interaction"
)

// A policy that waits on a stretch of quiet carries how long as a number,
// because the model that takes the decisions cannot reliably compare two of
// them: at four seconds of silence and at forty it chose to speak five times
// out of five either way, and a paragraph telling it to compare fixed one
// phrasing of the transcript and left a near-identical one wrong.
//
// A model reads the language; the runtime compares the numbers.
func TestAPolicyCanNameHowLongToWait(t *testing.T) {
	kind, instruction, ok := interaction.ParsePin(
		"pin conversation after 15s check whether they are still there")
	if !ok || kind != "pin" {
		t.Fatalf("parse = %q %v", kind, ok)
	}
	if instruction.After != 15*time.Second {
		t.Fatalf("the delay was not read: %+v", instruction)
	}
	if instruction.Text != "check whether they are still there" {
		t.Fatalf("the delay was left in the sentence the voice reads: %q", instruction.Text)
	}
	if instruction.Due(8 * time.Second) {
		t.Fatal("eight seconds is not fifteen")
	}
	if !instruction.Due(16 * time.Second) {
		t.Fatal("sixteen is")
	}
	// A policy about something happening rather than about time is always due;
	// what it waits for is the model's to recognise.
	_, counting, _ := interaction.ParsePin("pin conversation count the animals out loud")
	if counting.After != 0 || !counting.Due(0) {
		t.Fatalf("a policy with no delay must always be due: %+v", counting)
	}
	// And a malformed delay is left alone rather than guessed at.
	_, odd, _ := interaction.ParsePin("pin conversation after a while tell me how it went")
	if odd.After != 0 || odd.Text != "after a while tell me how it went" {
		t.Fatalf("a delay that is not a number must stay in the sentence: %+v", odd)
	}
}
