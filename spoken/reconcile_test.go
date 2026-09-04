package spoken

import (
	"fmt"
	"strings"
	"testing"
)

// heardAt builds a recogniser result with one word every step milliseconds.
func heardAt(step uint64, words ...string) []Word {
	heard := make([]Word, 0, len(words))
	for index, word := range words {
		heard = append(heard, Word{
			Text: word, StartMS: uint64(index) * step, EndMS: uint64(index+1) * step,
		})
	}
	return heard
}

// The case the whole feature was asked for: the agent reads out a long run of
// numbers and is cut off partway. A synthesiser is given digits and a
// recogniser writes words, so a word-for-word correspondence does not exist -
// and without one, the numbers are the part of the utterance that never gets a
// time, which is exactly the part the resumption depends on.
func TestDigitsAlignAgainstTheWordsTheyAreReadAs(t *testing.T) {
	reference := "1, 2, 3, 4, 5, 6, 7, 8, 9, 10."
	heard := heardAt(500, "one", "two", "three", "four", "five",
		"six", "seven", "eight", "nine", "ten")
	timeline := Reconcile(reference, heard, 5000)
	if !timeline.Measured {
		t.Fatal("the times came from the audio, so the layout is measured")
	}
	if len(timeline.Words) != 10 {
		t.Fatalf("ten numbers laid out as %d words: %+v", len(timeline.Words), timeline.Words)
	}
	// Every reference word keeps the form the synthesiser was given. Resuming
	// from the recogniser's spelling would have the agent read its own text
	// back in somebody else's words.
	if timeline.Words[6].Text != "7," {
		t.Fatalf("the seventh word is %q, not the text that was synthesised", timeline.Words[6].Text)
	}
	for index, word := range timeline.Words {
		wantStart, wantEnd := uint64(index)*500, uint64(index+1)*500
		if word.StartMS != wantStart || word.EndMS != wantEnd {
			t.Fatalf("%q sits at %d-%dms, want %d-%dms",
				word.Text, word.StartMS, word.EndMS, wantStart, wantEnd)
		}
	}
	mark := timeline.At(3200)
	if mark.Spoken != "1, 2, 3, 4, 5, 6," {
		t.Fatalf("heard %q", mark.Spoken)
	}
	if mark.Cut != "7," {
		t.Fatalf("the stop landed inside %q", mark.Cut)
	}
	if mark.Pending != "7, 8, 9, 10." {
		t.Fatalf("what is left to say is %q", mark.Pending)
	}
}

// The reverse mismatch, and the one that breaks a word-level alignment
// outright: one written number is two spoken words.
func TestOneWrittenNumberSpokenAsTwoWordsStillGetsOneInterval(t *testing.T) {
	timeline := Reconcile("21 22 23", heardAt(400,
		"twenty", "one", "twenty", "two", "twenty", "three"), 2400)
	if len(timeline.Words) != 3 {
		t.Fatalf("three numbers became %d words", len(timeline.Words))
	}
	for index, want := range []struct{ start, end uint64 }{{0, 800}, {800, 1600}, {1600, 2400}} {
		word := timeline.Words[index]
		if word.StartMS != want.start || word.EndMS != want.end {
			t.Fatalf("%q sits at %d-%dms, want %d-%dms",
				word.Text, word.StartMS, word.EndMS, want.start, want.end)
		}
	}
	if mark := timeline.At(800); mark.Spoken != "21" || mark.Pending != "22 23" {
		t.Fatalf("split at 800ms: heard %q, left %q", mark.Spoken, mark.Pending)
	}
}

// A recogniser mishears words, drops small ones and re-punctuates the rest.
// None of that may change what the agent believes it said: the wording is
// already known, and only the times are being taken from the audio.
func TestAMisheardWordDoesNotChangeWhatTheAgentSaid(t *testing.T) {
	const reference = "A heron landed on the far bank and stared at us."
	heard := heardAt(300, "a", "horn", "mounted", "on", "the",
		"far", "bank", "and", "stared", "at", "us")
	timeline := Reconcile(reference, heard, 3300)
	said := make([]string, 0, len(timeline.Words))
	for _, word := range timeline.Words {
		said = append(said, word.Text)
	}
	if strings.Join(said, " ") != reference {
		t.Fatalf("the layout says the agent said %q", strings.Join(said, " "))
	}
	// The words the recogniser did get right anchor the ones it did not, so a
	// boundary late in the sentence is still where the audio put it.
	mark := timeline.At(2100)
	if !strings.HasPrefix(reference, mark.Spoken) {
		t.Fatalf("the spoken part %q is not a prefix of the sentence", mark.Spoken)
	}
	heardWords := len(strings.Fields(mark.Spoken))
	if heardWords < 6 || heardWords > 8 {
		t.Fatalf("2100ms of 3300 carried %d of 11 words: %q", heardWords, mark.Spoken)
	}
}

// A recogniser that answers with nothing usable must leave the proportional
// layout standing rather than produce an empty one - an empty layout reports
// every utterance as never started.
func TestAnUnusableTranscriptFallsBackRatherThanErasingTheLayout(t *testing.T) {
	for name, heard := range map[string][]Word{
		"nothing at all":   nil,
		"only punctuation": {{Text: "...", StartMS: 0, EndMS: 900}},
	} {
		timeline := Reconcile("we can look that up", heard, 900)
		if len(timeline.Words) != 5 {
			t.Fatalf("%s: five words laid out as %d", name, len(timeline.Words))
		}
		if timeline.Measured {
			t.Fatalf("%s: nothing was measured, so the layout must not claim it was", name)
		}
	}
}

// Times must not run backwards. A recogniser can report a word ending before
// the one before it did, and an out-of-order boundary makes the spoken prefix
// come back with a hole in the middle of it.
func TestTheLayoutIsAlwaysInOrder(t *testing.T) {
	heard := []Word{
		{Text: "first", StartMS: 0, EndMS: 500},
		{Text: "second", StartMS: 900, EndMS: 400},
		{Text: "third", StartMS: 300, EndMS: 1500},
	}
	timeline := Reconcile("first second third", heard, 1500)
	previous := uint64(0)
	for _, word := range timeline.Words {
		if word.StartMS < previous || word.EndMS < word.StartMS {
			t.Fatalf("%q runs %d-%dms after %dms: %+v",
				word.Text, word.StartMS, word.EndMS, previous, timeline.Words)
		}
		previous = word.EndMS
	}
	for played := uint64(0); played <= 1500; played += 100 {
		mark := timeline.At(played)
		if !strings.HasPrefix("first second third", mark.Spoken) {
			t.Fatalf("at %dms the spoken part was %q", played, mark.Spoken)
		}
	}
}

// Mandarin is in the suite this runs against, and the alignment must not need a
// script-specific path to place a boundary in it.
func TestAlignmentIsNotSpecificToTheLatinAlphabet(t *testing.T) {
	timeline := Reconcile("你好，很高兴见到你。", []Word{
		{Text: "你好", StartMS: 0, EndMS: 600},
		{Text: "很高兴见到你", StartMS: 600, EndMS: 1800},
	}, 1800)
	if !timeline.Measured || len(timeline.Words) != 1 {
		t.Fatalf("one whitespace-free utterance laid out as %+v", timeline.Words)
	}
	if timeline.Words[0].EndMS != 1800 {
		t.Fatalf("the utterance ends at %dms", timeline.Words[0].EndMS)
	}
}

// The alignment is quadratic, so something has to stop it being asked to do a
// transcript-sized job. Over the bound the proportional layout stands, which is
// a worse answer and not a wrong one.
func TestAnOversizedUtteranceFallsBackInsteadOfPayingForTheMatrix(t *testing.T) {
	var builder strings.Builder
	var heard []Word
	for index := 0; index < 500; index++ {
		fmt.Fprintf(&builder, "elephant%d ", index)
		heard = append(heard, Word{
			Text:    fmt.Sprintf("elephant%d", index),
			StartMS: uint64(index) * 10, EndMS: uint64(index+1) * 10,
		})
	}
	timeline := Reconcile(builder.String(), heard, 5000)
	if timeline.Measured {
		t.Fatal("past the bound nothing is measured")
	}
	if len(timeline.Words) != 500 {
		t.Fatalf("the fallback laid out %d of 500 words", len(timeline.Words))
	}
}
