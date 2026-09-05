package bysentence

import (
	"reflect"
	"testing"
)

func TestClauseMinimumPreservesShortSentencesAndCompletePhrases(t *testing.T) {
	for _, test := range []struct {
		text string
		want []string
	}{
		{"First, find the order number. Then check the date.", []string{"First, find the order number.", "Then check the date."}},
		{"Right, the deadline has moved.", []string{"Right, the deadline has moved."}},
		{"Yes. Find the order number.", []string{"Yes.", "Find the order number."}},
		{"One. Two. Three.", []string{"One.", "Two. Three."}},
		{"Why? Tell me more.", []string{"Why?", "Tell me more."}},
		{"好。 接下来检查日期。", []string{"好。", "接下来检查日期。"}},
	} {
		t.Run(test.text, func(t *testing.T) {
			if got := SplitWithClauseMinimum(test.text, 1, 12); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("speech pieces = %q, want %q", got, test.want)
			}
		})
	}
	for _, mark := range []string{",", ";", ":", "—", "，", "、", "；", "："} {
		short := "First" + mark + " find the order number"
		if got := SplitWithClauseMinimum(short, 1, 12); !reflect.DeepEqual(got, []string{short}) {
			t.Fatalf("short clause was isolated: %q", got)
		}
		long := "Find the order number" + mark
		if got := SplitWithClauseMinimum(long+" then check the date.", 1, 12); !reflect.DeepEqual(got, []string{long, "then check the date."}) {
			t.Fatalf("complete clause was not released: %q", got)
		}
	}
	if got := SplitWithClauseMinimum("Yes. Find the order number.", 12, 1); len(got) != 1 {
		t.Fatalf("clause floor bypassed the overall minimum: %q", got)
	}
}

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

// A comma is where the speaker would draw breath anyway, so it is the cheapest
// honest place to cut. The synthesiser's wait scales with what it was handed -
// 261ms for a word, 607ms for a clause, 919ms for a sentence - so the first
// piece should be the shortest thing that is still a unit of speech.
func TestTheFirstCutTakesAnyPauseNotJustAFullStop(t *testing.T) {
	pieces := Split("The build has finished successfully, and all the tests passed as well.", 12)
	if len(pieces) != 2 {
		t.Fatalf("a comma is a place to cut: %q", pieces)
	}
	if pieces[0] != "The build has finished successfully," {
		t.Fatalf("the first piece is the clause: %q", pieces[0])
	}
	for _, mark := range []string{";", ":"} {
		got := Split("I checked the order twice"+mark+" it has not shipped yet at all.", 12)
		if len(got) != 2 {
			t.Fatalf("%q should be a break: %q", mark, got)
		}
	}
	// The earliest break past the minimum wins, because every rune after it is
	// another few milliseconds before anybody hears anything - but "Right,"
	// on its own is half a second of audio against most of a second to
	// synthesise what follows, so the minimum still refuses it. The guard is
	// about covering the next piece, not about tidiness.
	if got := Split("Right, the deadline has moved to the third of next month.", 12); len(got) != 1 {
		t.Fatalf("a six-rune opening would run out before the rest arrived: %q", got)
	}
	early := Split("Once the review is done, we can ship it by the third.", 12)
	if len(early) != 2 || early[0] != "Once the review is done," {
		t.Fatalf("the first break past the minimum wins: %q", early)
	}
}
