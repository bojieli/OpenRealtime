package bysentence

import "testing"

// The first piece is what the agent starts talking on, and the synthesiser's
// wait for its first byte is a function of the text it was handed: 289ms for a
// word, 911ms for a sentence. So the only cut that earns anything is the first
// one.
func TestTheFirstSentenceIsCutOffFromTheRest(t *testing.T) {
	pieces := Split("The build has finished successfully. You can deploy whenever you are ready.", 12)
	if len(pieces) != 2 {
		t.Fatalf("expected a head and a tail, got %q", pieces)
	}
	if pieces[0] != "The build has finished successfully." {
		t.Fatalf("the head is not the first sentence: %q", pieces[0])
	}
	if pieces[1] != "You can deploy whenever you are ready." {
		t.Fatalf("the tail lost something: %q", pieces[1])
	}
	// Three sentences still make two requests. Cutting at every full stop pays
	// a round trip per sentence to save nothing after the first.
	if got := Split("The first one is here. The second one follows it. And a third.", 12); len(got) != 2 {
		t.Fatalf("only the first cut earns anything: %q", got)
	}
}

// A first piece has to be long enough to still be playing when the rest
// arrives, or the split trades a shorter wait for a gap in the middle of a
// sentence - which is worse than the wait, because a wait sounds like thinking
// and a gap sounds like a fault.
//
// "One." is about half a second of audio and the rest takes the better part of
// a second to synthesise, so cutting there would leave four hundred
// milliseconds of silence inside one breath.
func TestAFirstPieceTooShortToCoverTheRestIsNotCut(t *testing.T) {
	if got := Split("One. Two. Three and a bit more text here.", 12); len(got) != 1 {
		t.Fatalf("a two-word opening would run out before the rest arrived: %q", got)
	}
}

// Things that end in a full stop and are not the end of a sentence.
func TestASplitNeedsAGapAfterIt(t *testing.T) {
	for _, text := range []string{
		"The figure is 3.5 percent and the deadline has not moved yet.",
		"Ask Mr. Smith about the invoice when you get a chance today.",
	} {
		if got := Split(text, 12); len(got) != 1 {
			t.Fatalf("%q was cut where there is no sentence end: %q", text, got)
		}
	}
	// And something too short to be worth two requests stays whole.
	if got := Split("Yes.", 12); len(got) != 1 {
		t.Fatalf("a one-word answer must not pay a second round trip: %q", got)
	}
	if got := Split("Two. ", 12); len(got) != 1 {
		t.Fatalf("nothing follows the stop: %q", got)
	}
}
