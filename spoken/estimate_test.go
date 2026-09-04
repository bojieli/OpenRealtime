package spoken

import "testing"

// The prior is a guess and a voice can be slower than it. When it is, the audio
// that exists overtakes the guess, and a layout that simply took the longer of
// the two would end the last word exactly where playback has got to - reporting
// a sentence as finished at every moment of its life, which is precisely the
// belief that makes an agent carry on past words nobody heard.
func TestAVoiceSlowerThanThePriorStillLeavesSomethingUnsaid(t *testing.T) {
	// Two short words: the prior gives them about half a second, and this
	// voice has already spent two seconds on them without finishing.
	timeline := Estimate("one two", 2000)
	mark := timeline.At(2000)
	if mark.Complete() {
		t.Fatalf("an utterance still being produced claimed to be finished: %+v", mark)
	}
	if mark.Pending != "two" {
		t.Fatalf("what is left is %q", mark.Pending)
	}
}

// And the moment synthesis ends, the headroom has to go, or a sentence that
// ran to its end is reported as unfinished and the agent resumes inside it.
func TestCompletionRemovesTheHeadroom(t *testing.T) {
	if mark := Estimate("one two", 2000).Complete(2000).At(2000); !mark.Complete() {
		t.Fatalf("a finished utterance left %q unsaid", mark.Pending)
	}
	// A voice faster than the prior finishes early, and the layout has to
	// follow it down as well as up.
	if mark := Estimate("one two three four", 0).Complete(600).At(600); !mark.Complete() {
		t.Fatalf("a fast voice finished and left %q unsaid", mark.Pending)
	}
}

// A measured layout is rescaled on completion rather than thrown away: the
// anchors came from the audio and are the best information there is. Only the
// tail that was extrapolated past the audio moves.
func TestCompletingAMeasuredLayoutKeepsItsAnchors(t *testing.T) {
	timeline := Reconcile("one two three four", heardAt(500, "one", "two"), 4000)
	if !timeline.Measured {
		t.Fatal("the layout should be measured")
	}
	completed := timeline.Complete(2000)
	if completed.Words[0].EndMS != 500 || completed.Words[1].EndMS != 1000 {
		t.Fatalf("completion moved a measured anchor: %+v", completed.Words[:2])
	}
	if last := completed.Words[3]; last.EndMS != 2000 {
		t.Fatalf("the last word does not end with the audio: %+v", last)
	}
	if mark := completed.At(2000); !mark.Complete() {
		t.Fatalf("a finished utterance left %q unsaid", mark.Pending)
	}
}
