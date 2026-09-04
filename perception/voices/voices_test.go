package voices_test

import (
	"context"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/perception/voices"
)

type scripted struct {
	vectors [][]float32
	calls   int
}

type delayed struct {
	started chan struct{}
	release chan struct{}
	vector  []float32
}

func (embedder *delayed) Embed(context.Context, []byte, uint32) ([]float32, error) {
	close(embedder.started)
	<-embedder.release
	return embedder.vector, nil
}

func (s *scripted) Embed(context.Context, []byte, uint32) ([]float32, error) {
	if s.calls >= len(s.vectors) {
		return nil, nil
	}
	vector := s.vectors[s.calls]
	s.calls++
	return vector, nil
}

// speech is that many seconds of 16 kHz audio.
func speech(seconds float64) []perception.Frame {
	return []perception.Frame{{
		Kind: perception.FrameAudio, Source: "microphone",
		PCM16LE: make([]byte, int(16_000*seconds)*2), SampleRateHz: 16_000,
	}}
}

// enough is comfortably past the enrolment bar, which is longer than the bar
// for comparing against a reference that already exists.
func enough() []perception.Frame { return speech(4) }

func settle(t *testing.T, recogniser *voices.Recogniser, want voices.Verdict) voices.Verdict {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := recogniser.Verdict(); got == want {
			return got
		}
		time.Sleep(2 * time.Millisecond)
	}
	return recogniser.Verdict()
}

func TestTheFirstVoiceOfASessionIsWhoTheSessionIsWith(t *testing.T) {
	embedder := &scripted{vectors: [][]float32{{1, 0, 0}}}
	recogniser := voices.New(embedder, voices.DefaultThreshold, time.Second)
	recogniser.Begin("item-1")
	recogniser.Hear(context.Background(), enough())
	if got := settle(t, recogniser, voices.Familiar); got != voices.Familiar {
		t.Fatalf("the first voice was %q, not the one the session is with", got)
	}
}

func TestADifferentVoiceIsReportedAsDifferent(t *testing.T) {
	embedder := &scripted{vectors: [][]float32{{1, 0, 0}, {0, 1, 0}}}
	recogniser := voices.New(embedder, voices.DefaultThreshold, time.Second)
	recogniser.Begin("item-1")
	recogniser.Hear(context.Background(), enough())
	settle(t, recogniser, voices.Familiar)
	recogniser.Begin("item-2")
	recogniser.Hear(context.Background(), enough())
	if got := settle(t, recogniser, voices.Different); got != voices.Different {
		t.Fatalf("a stranger was reported as %q", got)
	}
}

func TestAwaitJoinsAComparisonAlreadyInFlight(t *testing.T) {
	embedder := &delayed{
		started: make(chan struct{}), release: make(chan struct{}), vector: []float32{1, 0, 0},
	}
	recogniser := voices.New(embedder, voices.DefaultThreshold, time.Second)
	recogniser.Begin("item-1")
	recogniser.Hear(context.Background(), enough())
	<-embedder.started

	result := make(chan voices.Verdict, 1)
	go func() {
		wait, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		result <- recogniser.Await(wait)
	}()
	select {
	case verdict := <-result:
		t.Fatalf("Await returned %q before the comparison settled", verdict)
	case <-time.After(20 * time.Millisecond):
	}
	close(embedder.release)
	if verdict := <-result; verdict != voices.Familiar {
		t.Fatalf("Await returned %q after enrolment settled", verdict)
	}
}

func TestTheSameVoiceSayingSomethingElseIsStillTheSameVoice(t *testing.T) {
	embedder := &scripted{vectors: [][]float32{{1, 0, 0}, {0.9, 0.436, 0}}}
	recogniser := voices.New(embedder, voices.DefaultThreshold, time.Second)
	recogniser.Begin("item-1")
	recogniser.Hear(context.Background(), enough())
	settle(t, recogniser, voices.Familiar)
	recogniser.Begin("item-2")
	recogniser.Hear(context.Background(), enough())
	if got := settle(t, recogniser, voices.Familiar); got != voices.Familiar {
		t.Fatalf("the same person was reported as %q", got)
	}
}

// Half a second is not enough to tell a voice from a vowel, and a wrong answer
// is worse than none: none leaves the prior in place.
func TestTooLittleAudioIsNotAnAnswer(t *testing.T) {
	embedder := &scripted{vectors: [][]float32{{1, 0, 0}}}
	recogniser := voices.New(embedder, voices.DefaultThreshold, time.Second)
	recogniser.Begin("item-1")
	recogniser.Hear(context.Background(), speech(0.5))
	if got := recogniser.Verdict(); got != voices.Unknown {
		t.Fatalf("half a second was judged %q", got)
	}
	if embedder.calls != 0 {
		t.Fatalf("the embedder was asked %d times about half a second", embedder.calls)
	}
}

func TestNoEmbedderMeansNothingIsKnown(t *testing.T) {
	var recogniser *voices.Recogniser = voices.New(nil, 0, 0)
	recogniser.Begin("item-1")
	recogniser.Hear(context.Background(), enough())
	if got := recogniser.Verdict(); got != voices.Unknown {
		t.Fatalf("with nobody to ask the verdict was %q", got)
	}
}

// TestEachSessionLearnsItsOwnVoice is the regression for a suite that got
// worse the longer it ran. The recogniser was built once for the process, so
// the first speaker of the first scenario became the person every later
// session was supposedly with, and every user after that was reported as a
// stranger. An isolated run of the same scenario passed, because it was first.
func TestEachSessionLearnsItsOwnVoice(t *testing.T) {
	embedder := &scripted{vectors: [][]float32{{1, 0, 0}, {0, 1, 0}}}
	first := voices.New(embedder, voices.DefaultThreshold, time.Second)
	first.Begin("item-1")
	first.Hear(context.Background(), enough())
	settle(t, first, voices.Familiar)

	// A second conversation, with somebody else. They are not a stranger in
	// their own session.
	next := voices.New(embedder, voices.DefaultThreshold, time.Second)
	next.Begin("item-1")
	next.Hear(context.Background(), enough())
	if got := settle(t, next, voices.Familiar); got != voices.Familiar {
		t.Fatalf("the second session's own user was reported as %q", got)
	}
}

// TestEnrollingNeedsMoreThanComparing is the regression for a scenario with one
// speaker in it that read "someone else in the room: speaking right now". The
// reference was enrolled from about a second of speech, and a second of
// enrolment sits 0.02 above the threshold when compared against the same
// speaker - no margin at all - so the agent spent the session declining to act
// on its own user.
func TestEnrollingNeedsMoreThanComparing(t *testing.T) {
	embedder := &scripted{vectors: [][]float32{{1, 0, 0}, {1, 0, 0}}}
	recogniser := voices.New(embedder, voices.DefaultThreshold, time.Second)

	// A second is enough to compare with, and not enough to enrol from.
	recogniser.Begin("item-1")
	recogniser.Hear(context.Background(), speech(1.2))
	if got := recogniser.Verdict(); got != voices.Unknown {
		t.Fatalf("a second and a bit enrolled somebody: %q", got)
	}
	if embedder.calls != 0 {
		t.Fatalf("the embedder was asked %d times about too little speech", embedder.calls)
	}

	// More speech in the same utterance, and now it enrols.
	recogniser.Hear(context.Background(), speech(4))
	if got := settle(t, recogniser, voices.Familiar); got != voices.Familiar {
		t.Fatalf("four seconds did not enrol anybody: %q", got)
	}
}

// A short opening acknowledgement must not become the session's permanent
// voice reference. Real ECAPA embeddings put "Right. So" below the
// same-speaker threshold when compared with a later substantive utterance;
// the following longer utterance is the first reliable enrolment window.
func TestShortOpeningPhraseDoesNotBecomeTheVoiceReference(t *testing.T) {
	embedder := &scripted{vectors: [][]float32{{1, 0, 0}}}
	recogniser := voices.New(embedder, voices.DefaultThreshold, time.Second)

	recogniser.Begin("acknowledgement")
	recogniser.Hear(context.Background(), speech(1.6))
	if got := recogniser.Verdict(); got != voices.Unknown {
		t.Fatalf("a 1.6 second opening phrase enrolled somebody: %q", got)
	}
	if embedder.calls != 0 {
		t.Fatalf("the embedder was asked %d times about the short opening phrase", embedder.calls)
	}

	recogniser.Begin("substantive")
	recogniser.Hear(context.Background(), speech(2.1))
	if got := settle(t, recogniser, voices.Familiar); got != voices.Familiar {
		t.Fatalf("the substantive utterance did not establish the reference: %q", got)
	}
}
