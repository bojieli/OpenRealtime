package cognition_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/cognition"
)

func instructionFor(t *testing.T, request cognition.Request) string {
	t.Helper()
	return cognition.Instruct("You are an assistant.", request)
}

// A policy somebody set out loud reaches the voice as an instruction, not as
// another line of transcript it may take or leave.
func TestStandingPoliciesReachTheVoice(t *testing.T) {
	prompt := instructionFor(t, cognition.Request{
		Standing: []string{"count the animals out loud as they are mentioned (11s ago)"},
	})
	if !strings.Contains(prompt, "count the animals out loud") {
		t.Fatalf("a standing policy never reached the instruction:\n%s", prompt)
	}
	if !strings.Contains(prompt, cognition.StandingInstruction) {
		t.Fatal("the policy was included without saying what it is")
	}
	bare := instructionFor(t, cognition.Request{})
	if strings.Contains(bare, cognition.StandingInstruction) {
		t.Fatal("a request with no policies still announced some")
	}
}

// Somebody else still holds the floor, so what is called for is the smallest
// thing that serves - never a reply, because there is no pause to put one in.
func TestInterjectingTellsTheVoiceTheTurnIsNotItsOwn(t *testing.T) {
	prompt := instructionFor(t, cognition.Request{Interjecting: true})
	if !strings.Contains(prompt, cognition.InterjectingInstruction) {
		t.Fatalf("an interjection was not marked as one:\n%s", prompt)
	}
	if strings.Contains(instructionFor(t, cognition.Request{}), cognition.InterjectingInstruction) {
		t.Fatal("an ordinary turn was told it was interjecting")
	}
}
