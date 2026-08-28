package main

import "testing"

// A voice that thinks spends the output allowance on thinking. Measured on the
// interrupting scenario at the default ninety-six, the reply came back empty;
// at 128 it was "That sounds like a"; only past 512 did a whole sentence
// arrive. The number means the length of a spoken turn, so the room to think
// is added to it rather than taken out of it.
func TestAThinkingVoiceGetsRoomToThinkOnTopOfItsSpeech(t *testing.T) {
	quiet := serveOptions{fastTokens: 96, fastEffort: "minimal", explicit: map[string]bool{}}
	if got := spokenAllowance(quiet); got != 96 {
		t.Fatalf("a voice that does not think had its allowance changed: %d", got)
	}
	unset := serveOptions{fastTokens: 96, explicit: map[string]bool{}}
	if got := spokenAllowance(unset); got != 96 {
		t.Fatalf("an unset effort had its allowance changed: %d", got)
	}
	thinking := serveOptions{fastTokens: 96, fastEffort: "low", explicit: map[string]bool{}}
	if got := spokenAllowance(thinking); got <= 96 {
		t.Fatalf("a thinking voice got no room to think: %d", got)
	}
}

// Somebody who set the number themselves has said what they want, and this
// must not quietly mean something else.
func TestAnExplicitLimitIsHonouredExactly(t *testing.T) {
	chosen := serveOptions{
		fastTokens: 300, fastEffort: "high",
		explicit: map[string]bool{"fast-max-tokens": true},
	}
	if got := spokenAllowance(chosen); got != 300 {
		t.Fatalf("an explicit limit was overridden: %d", got)
	}
}
