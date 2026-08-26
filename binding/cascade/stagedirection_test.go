package cascade

import "testing"

// A voice asked to stay quiet sometimes writes "(silence)" instead of nothing,
// and every character here is synthesised - so the one thing the turn existed
// to avoid is exactly what the person hears. Measured once in five on a
// counting policy before the instruction told it how to say nothing.
func TestADescriptionOfSilenceIsNotSpeech(t *testing.T) {
	for _, text := range []string{"(silence)", "[silence]", "*silence*", "(no response)", "( Silence )", "[pause]"} {
		if !isStageDirection(text) {
			t.Fatalf("%q would have been spoken out loud", text)
		}
	}
	// Ordinary speech that happens to be about silence, or bracketed for its
	// own reasons, is speech.
	for _, text := range []string{
		"There was a long silence after that.",
		"(the third one)",
		"Nothing came back from the lookup, so I will try again.",
		"(I checked twice and the answer is the same)",
	} {
		if isStageDirection(text) {
			t.Fatalf("%q is speech and would have been swallowed", text)
		}
	}
}
