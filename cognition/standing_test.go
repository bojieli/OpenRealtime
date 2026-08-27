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
		Interjecting: true, Because: "speak-through", Counting: true,
		Standing: []string{"count the animals out loud (1m ago)"},
	})
	if !strings.Contains(counting, "Count every one of them") {
		t.Fatalf("a running commentary was not told what it is for:\n%s", counting)
	}
	// And the same rules on an ordinary turn, because the agent speaks on
	// those too: without them it counted one on a sentence about a river, and
	// that premature one suppressed the real first count.
	plain := instructionFor(t, cognition.Request{
		Counting: true, Standing: []string{"count the animals out loud (1m ago)"},
	})
	if !strings.Contains(plain, "Count every one of them") {
		t.Fatalf("an ordinary turn under a counting policy was not told how to count:\n%s", plain)
	}
	// And a running commentary that is not a count is not told how to count.
	// Attached to every one of them, the arithmetic taught a waiter scenario
	// to count: asked to order the dish that fits, the agent said "4 4 4".
	// The same act, a policy that is not a count: the arithmetic is withheld.
	ordering := instructionFor(t, cognition.Request{
		Interjecting: true, Because: "speak-through", Counting: false,
		Standing: []string{"order the dish that fits when the waiter names one (1m ago)"},
	})
	if strings.Contains(ordering, "Count every one of them") {
		t.Fatalf("ordering a dish was told how to count:\n%s", ordering)
	}
	if !strings.Contains(ordering, "Do exactly what their standing policy asks") {
		t.Fatalf("a running commentary was not told what it is for:\n%s", ordering)
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

// A silent act is told that nothing it writes is heard, and left to decide
// what the situation calls for. Measured on a phone menu, the key was pressed
// correctly and then announced out loud - "I have pressed two to select the
// order status option" - to a recording, which cannot hear it and is still
// talking over the announcement.
//
// What it must not be told is to make a call: whether one applies is the
// judgement this phase is here to make, and an instruction that presumes one
// leaves it pressing a key at a recording that has offered nothing yet.
func TestActingIsToldNothingItWritesIsHeard(t *testing.T) {
	acting := instructionFor(t, cognition.Request{
		Because:  "act-silently",
		Standing: []string{"press the key when the menu offers what they asked for (20s ago)"},
	})
	if !strings.Contains(acting, "Nothing you write on this turn is heard") {
		t.Fatalf("a silent act was not told it is inaudible:\n%s", acting)
	}
	if !strings.Contains(acting, "and nothing if there is not") {
		t.Fatalf("a silent act was told to call something regardless:\n%s", acting)
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
