package spoken

import (
	"strings"
	"testing"
)

// The whole package exists for this boundary, so it is the first thing
// asserted: audio that stopped partway through an utterance leaves the words
// whose audio finished on one side and the words nobody heard on the other.
func TestTheBoundaryFallsWhereThePlaybackStopped(t *testing.T) {
	timeline := Timeline{
		Text: "one two three four", Measured: true, AudioMS: 4000,
		Words: []Word{
			{Text: "one", StartMS: 0, EndMS: 1000},
			{Text: "two", StartMS: 1000, EndMS: 2000},
			{Text: "three", StartMS: 2000, EndMS: 3000},
			{Text: "four", StartMS: 3000, EndMS: 4000},
		},
	}
	mark := timeline.At(2000)
	if mark.Spoken != "one two" {
		t.Fatalf("heard %q, want %q", mark.Spoken, "one two")
	}
	if mark.Pending != "three four" {
		t.Fatalf("unheard %q, want %q", mark.Pending, "three four")
	}
	if mark.Cut != "" {
		t.Fatalf("a stop on a word boundary cut no word, got %q", mark.Cut)
	}
	if mark.Complete() {
		t.Fatal("half an utterance is not a complete one")
	}
	if whole := timeline.At(4000); !whole.Complete() || whole.Spoken != timeline.Text {
		t.Fatalf("playing it all left %q pending", whole.Pending)
	}
}

// A word whose last syllable was cut off is not a word the listener received,
// and it is not a word the listener missed either. Both readings produce a
// wrong resumption - one skips it, the other has no way to say why the agent
// repeated it - so it is reported as its own thing and opens what is left.
func TestAWordCutInHalfOpensWhatIsLeftToSay(t *testing.T) {
	timeline := Timeline{
		Text: "twenty one twenty two twenty three", Measured: true, AudioMS: 3000,
		Words: []Word{
			{Text: "twenty", StartMS: 0, EndMS: 500},
			{Text: "one", StartMS: 500, EndMS: 1000},
			{Text: "twenty", StartMS: 1000, EndMS: 1500},
			{Text: "two", StartMS: 1500, EndMS: 2000},
			{Text: "twenty", StartMS: 2000, EndMS: 2500},
			{Text: "three", StartMS: 2500, EndMS: 3000},
		},
	}
	mark := timeline.At(2200)
	if mark.Spoken != "twenty one twenty two" {
		t.Fatalf("heard %q", mark.Spoken)
	}
	if mark.Cut != "twenty" {
		t.Fatalf("the stop landed inside %q, want twenty", mark.Cut)
	}
	if mark.Pending != "twenty three" {
		t.Fatalf("what is left is %q, want %q", mark.Pending, "twenty three")
	}
	if !mark.Started() {
		t.Fatal("audio went out, so something was started")
	}
}

// Nothing known about where the words are must not be reported as everything
// spoken. That default is the exact failure the package removes, and it is the
// one a caller would get for free by leaving the layout empty.
func TestAnUnknownLayoutClaimsNothingWasHeard(t *testing.T) {
	mark := Timeline{Text: "we ship it by the third"}.At(9999)
	if mark.Spoken != "" {
		t.Fatalf("claimed %q was heard with no layout to say so", mark.Spoken)
	}
	if mark.Pending != "we ship it by the third" {
		t.Fatalf("what is left is %q", mark.Pending)
	}
}

// The proportional layout is a fallback that has to work, because a deployment
// with no spare recogniser still must not resume from words nobody heard.
func TestTheProportionalLayoutPutsTheBoundaryNearTheRightWord(t *testing.T) {
	const text = "one two three four five six seven eight nine ten"
	timeline := Estimate(text, 5000)
	if timeline.Measured {
		t.Fatal("nothing listened to this audio, so it is not measured")
	}
	mark := timeline.At(2500)
	heard := len(strings.Fields(mark.Spoken))
	if heard < 4 || heard > 6 {
		t.Fatalf("half the audio carried %d of ten words: %q", heard, mark.Spoken)
	}
	if strings.Contains(mark.Spoken, "ten") {
		t.Fatalf("the last word cannot be inside the first half: %q", mark.Spoken)
	}
}

// While synthesis is running the total duration is not known, and estimating
// against the audio produced so far would report every utterance as nearly
// finished. The prior covers that window - and has to stop covering it, or a
// completed utterance stays half-said for ever.
func TestTheDurationPriorGivesWayToTheRealDuration(t *testing.T) {
	const text = "the build finished about a minute ago"
	early := Estimate(text, 200)
	if early.AudioMS != 200 {
		t.Fatalf("audio so far is 200ms, recorded %d", early.AudioMS)
	}
	if mark := early.At(200); mark.Complete() {
		t.Fatalf("200ms of a sentence is not the whole sentence: %q", mark.Spoken)
	}
	whole := early.Complete(2400)
	if mark := whole.At(2400); !mark.Complete() {
		t.Fatalf("the utterance ran to its end and left %q unsaid", mark.Pending)
	}
	// A measured layout is not improved by being stretched over a duration,
	// so completing one must leave it alone.
	measured := Timeline{
		Text: text, Measured: true, AudioMS: 2400,
		Words: []Word{{Text: "the", StartMS: 0, EndMS: 2400}},
	}
	if got := measured.Complete(9999); len(got.Words) != 1 || !got.Measured {
		t.Fatal("completing a measured layout rebuilt it as an estimate")
	}
}

// One utterance can carry several assistant items, and the cut is a fact about
// the audio rather than about any one of them. Recording the utterance's mark
// against each item would report an item that was fully spoken and an item that
// was never reached identically.
func TestOneCutIsRecordedAgainstEachItemItCovers(t *testing.T) {
	mark := Mark{
		Spoken: "the deadline is the third", Cut: "which",
		Pending: "which gives us", Measured: true,
	}
	parts := Distribute(mark, []string{"the deadline is the third.", "which gives us"})
	if len(parts) != 2 {
		t.Fatalf("two items produced %d marks", len(parts))
	}
	if !parts[0].Complete() || parts[0].Spoken != "the deadline is the third." {
		t.Fatalf("the first item was fully spoken, got %+v", parts[0])
	}
	if parts[1].Spoken != "" || parts[1].Pending != "which gives us" {
		t.Fatalf("the second item was never heard, got %+v", parts[1])
	}
	if parts[1].Cut != "which" {
		t.Fatalf("the cut belongs to the item it fell in, got %q", parts[1].Cut)
	}
}
