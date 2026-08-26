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

func (s *scripted) Embed(context.Context, []byte, uint32) ([]float32, error) {
	if s.calls >= len(s.vectors) {
		return nil, nil
	}
	vector := s.vectors[s.calls]
	s.calls++
	return vector, nil
}

// second is a second of 16 kHz audio, which is the least the recogniser will
// ask about.
func second() []perception.Frame {
	return []perception.Frame{{
		Kind: perception.FrameAudio, Source: "microphone",
		PCM16LE: make([]byte, 16_000*2), SampleRateHz: 16_000,
	}}
}

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
	recogniser.Hear(context.Background(), second())
	if got := settle(t, recogniser, voices.Familiar); got != voices.Familiar {
		t.Fatalf("the first voice was %q, not the one the session is with", got)
	}
}

func TestADifferentVoiceIsReportedAsDifferent(t *testing.T) {
	embedder := &scripted{vectors: [][]float32{{1, 0, 0}, {0, 1, 0}}}
	recogniser := voices.New(embedder, voices.DefaultThreshold, time.Second)
	recogniser.Begin("item-1")
	recogniser.Hear(context.Background(), second())
	settle(t, recogniser, voices.Familiar)
	recogniser.Begin("item-2")
	recogniser.Hear(context.Background(), second())
	if got := settle(t, recogniser, voices.Different); got != voices.Different {
		t.Fatalf("a stranger was reported as %q", got)
	}
}

func TestTheSameVoiceSayingSomethingElseIsStillTheSameVoice(t *testing.T) {
	embedder := &scripted{vectors: [][]float32{{1, 0, 0}, {0.9, 0.436, 0}}}
	recogniser := voices.New(embedder, voices.DefaultThreshold, time.Second)
	recogniser.Begin("item-1")
	recogniser.Hear(context.Background(), second())
	settle(t, recogniser, voices.Familiar)
	recogniser.Begin("item-2")
	recogniser.Hear(context.Background(), second())
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
	recogniser.Hear(context.Background(), []perception.Frame{{
		Kind: perception.FrameAudio, Source: "microphone",
		PCM16LE: make([]byte, 8_000*2), SampleRateHz: 16_000,
	}})
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
	recogniser.Hear(context.Background(), second())
	if got := recogniser.Verdict(); got != voices.Unknown {
		t.Fatalf("with nobody to ask the verdict was %q", got)
	}
}
