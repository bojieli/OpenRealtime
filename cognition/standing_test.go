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

// The three models must not disagree about what happened.
//
// Interaction decides on partials and cognition reads committed items, so a
// turn triggered by something still in a partial reaches the voice with its own
// cause missing. Told to count animals as they were mentioned, it counted from
// one to ten: the animal was in a partial, and the instruction was all it had.
func TestTheVoiceIsShownTheUtteranceThatCausedTheTurn(t *testing.T) {
	prompt := instructionFor(t, cognition.Request{
		Standing: []string{"count the animals out loud as they are mentioned (11s ago)"},
		Heard:    "a capybara wandered over and sat down next to me",
	})
	if !strings.Contains(prompt, "a capybara wandered over") {
		t.Fatalf("the sentence that caused the turn never reached the voice:\n%s", prompt)
	}
	if !strings.Contains(prompt, cognition.HeardInstruction) {
		t.Fatal("the live utterance was included without saying what it is")
	}
	// Once committed it is in the log, and repeating it would show the voice
	// the same sentence twice with no way to tell that it is one.
	if strings.Contains(instructionFor(t, cognition.Request{}), cognition.HeardInstruction) {
		t.Fatal("a request with nothing in flight still announced a live utterance")
	}
}

// An interjection fires on a partial, so early in a session the log is
// genuinely empty and the sentence that triggered the turn is in the request
// rather than in the store. Refusing that refuses the only turn with anything
// to say - and it reached the client as a session error, so whole runs
// produced no audio at all.
func TestARequestCarryingALiveUtteranceIsNotEmpty(t *testing.T) {
	prompt := instructionFor(t, cognition.Request{Heard: "a capybara wandered over"})
	if !strings.Contains(prompt, "capybara") {
		t.Fatalf("the live utterance did not reach the instruction:\n%s", prompt)
	}
}

// The decision layer knows why it called the voice and used to keep it to
// itself. Called to correct a date it had been given, the voice said "got it";
// called to count an animal it had already counted one of, it said nothing.
// Both are reasonable answers to "say something short" and neither answers the
// question that was asked.
func TestTheVoiceIsToldWhatTheTurnWasCalledFor(t *testing.T) {
	correcting := instructionFor(t, cognition.Request{
		Interjecting: true, Because: "interrupt",
		Heard: "and ship it by the thirteenth",
	})
	if !strings.Contains(correcting, "Say the correction itself") {
		t.Fatalf("an interruption was not told it exists to correct something:\n%s", correcting)
	}
	counting := instructionFor(t, cognition.Request{
		Interjecting: true, Because: "speak-through",
		Standing: []string{"count the animals out loud (1m ago)"},
	})
	if !strings.Contains(counting, "Counting works the same way") {
		t.Fatalf("a running commentary was not told what it is for:\n%s", counting)
	}
	// A count is of the things counted, not of the turns spent counting.
	// Told to "say the next number" it counted to sixteen through a story
	// with two animals in it, one number per partial of the same sentence.
	if strings.Contains(counting, "the next number") {
		t.Fatalf("a running commentary was told to count its own turns:\n%s", counting)
	}
	if !strings.Contains(counting, cognition.WaitToken) {
		t.Fatalf("a running commentary had no way to say the condition had not recurred:\n%s", counting)
	}
	// An ordinary turn is not told anything of the sort.
	if plain := instructionFor(t, cognition.Request{}); strings.Contains(plain, "Say the correction itself") {
		t.Fatal("an ordinary turn was told it was correcting something")
	}
}

// A tool call is the whole of what a silent act is for. Measured on a phone
// menu, the key was pressed correctly and then announced out loud - "I have
// pressed two to select the order status option" - to a recording, which
// cannot hear it and is still talking over the announcement.
func TestActingIsToldThatTheCallIsTheAnswer(t *testing.T) {
	acting := instructionFor(t, cognition.Request{
		Because:  "call-tool",
		Standing: []string{"press the key when the menu offers what they asked for (20s ago)"},
	})
	if !strings.Contains(acting, "the tool call is the whole of what this turn is for") {
		t.Fatalf("a silent act was not told the call is the answer:\n%s", acting)
	}
	if !strings.Contains(acting, cognition.WaitToken) {
		t.Fatal("it was not told how to make the call without also announcing it")
	}
	// And an ordinary turn is told none of that, or every answer would come
	// back as a token.
	if plain := instructionFor(t, cognition.Request{}); strings.Contains(plain, "the tool call is the whole") {
		t.Fatal("an ordinary turn was told it was acting rather than speaking")
	}
}
