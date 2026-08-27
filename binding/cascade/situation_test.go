package cascade

import (
	"testing"

	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// TestBeforeTheAgentHasSpokenEverythingHeardIsNew is the regression for the
// control scenario going to 0/5. heardSinceSpeaking returned nothing for three
// different situations - the agent has never spoken, the agent spoke and
// nothing has been heard since, and this is a different utterance from the one
// the mark came from - and only the second of those means nothing is new. Once
// a rule in the interaction instruction started reading that line, the first
// case told the model a finished question had already been answered.
func TestBeforeTheAgentHasSpokenEverythingHeardIsNew(t *testing.T) {
	runtime := &runtime{}
	if got := runtime.heardSinceSpeaking("what is the capital of france"); got != "what is the capital of france" {
		t.Fatalf("before the agent had spoken, what is new was %q", got)
	}
	runtime.markSpoken("what is the capital of france")
	if got := runtime.heardSinceSpeaking("what is the capital of france"); got != "" {
		t.Fatalf("with nothing added since, what is new was %q", got)
	}
	if got := runtime.heardSinceSpeaking("what is the capital of france and of spain"); got != "and of spain" {
		t.Fatalf("the added words were %q", got)
	}
	// A different utterance does not continue the one the mark came from.
	if got := runtime.heardSinceSpeaking("how tall is the tower"); got != "how tall is the tower" {
		t.Fatalf("a new utterance reported %q as new", got)
	}
}

// TestRepunctuatingIsNotSayingSomethingNew is the regression for an afternoon
// with two animals in it counted to sixteen. A recogniser rewrites what it has
// already given you - "a warm afternoon and I was walking" becomes "a warm
// afternoon. And I was walking" and back again between revisions of the same
// sentence - so a text prefix test fails and every revision reads as a whole
// new utterance.
func TestRepunctuatingIsNotSayingSomethingNew(t *testing.T) {
	runtime := &runtime{}
	runtime.markSpoken("It was a warm afternoon and I was walking")
	if got := runtime.heardSinceSpeaking("It was a warm afternoon. And I was walking"); got != "" {
		t.Fatalf("re-punctuating reported %q as new", got)
	}
	if got := runtime.heardSinceSpeaking(
		"It was a warm afternoon. And I was walking, a capybara wandered over"); got != "a capybara wandered over" {
		t.Fatalf("what is new was %q", got)
	}
	// A genuinely different sentence is still new.
	if got := runtime.heardSinceSpeaking("then a heron landed"); got != "then a heron landed" {
		t.Fatalf("a new sentence reported %q as new", got)
	}
}

// TestSpeakingAgainNeedsSomethingNewToSpeakAbout is the deterministic half of
// the rule that a standing instruction fires on its condition rather than on
// the arrival of text. A recogniser emits a revision every couple of hundred
// milliseconds and most add nothing but a comma; asked once per revision, an
// afternoon with two animals in it was counted "1 3 3 1".
func TestSpeakingAgainNeedsSomethingNewToSpeakAbout(t *testing.T) {
	runtime := &runtime{}
	runtime.markSpoken("a capybara wandered over")
	if got := runtime.heardSinceSpeaking("a capybara wandered over,"); got != "" {
		t.Fatalf("a comma counted as something new: %q", got)
	}
	if got := runtime.heardSinceSpeaking("a capybara wandered over and a heron landed"); got == "" {
		t.Fatal("a second animal counted as nothing new")
	}
}

// TestWhoSpokeIsRecordedNotJustDecided is the regression for a conversation
// that kept saying "user:" about somebody who was not the user. The speaker
// verdict reached the situation the interaction model reads and stopped there,
// so every later turn - the voice included - read a history in which the
// person the session is with had asked about the milk.
func TestWhoSpokeIsRecordedNotJustDecided(t *testing.T) {
	if otherVoiceSource == "" {
		t.Fatal("a voice that is not the user's has no name")
	}
	item := trajectory.Item{
		Kind: trajectory.KindObservation, Content: "did you get the milk on the way in",
		Observation: &trajectory.ObservationMeta{
			Source: otherVoiceSource, Authority: trajectory.AuthorityUser,
		},
	}
	if got := interaction.SpeakerOf(item); got != otherVoiceSource {
		t.Fatalf("a recorded stranger reads back as %q", got)
	}
	// And the person the session is with still reads as the user.
	item.Observation.Source = "microphone"
	if got := interaction.SpeakerOf(item); got != "user" {
		t.Fatalf("the session's own person reads back as %q", got)
	}
}
