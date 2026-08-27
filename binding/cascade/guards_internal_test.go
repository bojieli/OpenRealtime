package cascade

import "testing"

// The guards that decide when the agent may answer again rest on two readings
// of the same speech, and the difference between them is the whole reason a
// second answer waits for a pause. These pin that difference so a later change
// to either reading has to face it.

// Every piece a recogniser commits looks like a finished sentence, because the
// recogniser punctuates what it commits. One instruction arriving as three
// pieces therefore presents three finished sentences a tenth of a second
// apart, and the agent acknowledged each one - six acknowledgements played
// over the speech they were about.
func TestEveryCommittedFragmentLooksFinished(t *testing.T) {
	fragments := []string{
		"Translate everything he says.",
		"As he goes.",
		"And don't wait for him to finish.",
	}
	for _, fragment := range fragments {
		if !finishedSomething(fragment) {
			t.Fatalf("%q does not read as finished, so the guard was never the problem", fragment)
		}
	}
	// Which is the point: punctuation cannot separate these from a sentence
	// somebody actually finished, so a second answer cannot rest on it.
}

// A partial cuts wherever the audio has got to, which is usually mid-word.
// Compared strictly, the mark left by speaking into "A cap" is not a prefix of
// the sentence it becomes, and the turn that ran on the commit was told the
// agent had not spoken into it.
func TestAMarkMatchesThroughAPartialWord(t *testing.T) {
	if !beganWith("A cap", "A capybara wandered over and sat down next to me.") {
		t.Fatal("a mark cut mid-word did not match the sentence it became")
	}
	if beganWith("A heron", "A capybara wandered over") {
		t.Fatal("a different sentence matched")
	}
	if beganWith("", "anything") {
		t.Fatal("an empty mark matched")
	}
}

// The recogniser writes a full stop where somebody paused for breath, so a
// piece can end mid-thought and still carry one. endsASentence reads what the
// recogniser decided; it is not a judgement about whether they had finished.
func TestEndsASentenceReadsTheRecognisersMark(t *testing.T) {
	if !endsASentence("Count the animals out loud.") {
		t.Fatal("a committed sentence did not read as ending one")
	}
	if endsASentence("tell me the moment the build") {
		t.Fatal("an unfinished piece read as ending a sentence")
	}
	if !endsASentence(`"and say nothing else."`) {
		t.Fatal("a closing quote hid the full stop behind it")
	}
}
