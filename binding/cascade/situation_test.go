package cascade

import "testing"

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
