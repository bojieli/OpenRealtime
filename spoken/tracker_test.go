package spoken

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// silence is one utterance's worth of audio at the rate the synthesisers here
// produce. Its content never matters: the aligner in these tests is scripted,
// and what is under test is the bookkeeping around it.
func silence(ms int) []byte { return make([]byte, 24*2*ms) }

const testRate = 24_000

type scriptedAligner struct {
	mu      sync.Mutex
	words   []Word
	err     error
	calls   int
	release chan struct{}
}

func (aligner *scriptedAligner) Words(ctx context.Context, audio Audio) ([]Word, error) {
	aligner.mu.Lock()
	release, err, words := aligner.release, aligner.err, aligner.words
	aligner.calls++
	aligner.mu.Unlock()
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return words, err
}

func (aligner *scriptedAligner) count() int {
	aligner.mu.Lock()
	defer aligner.mu.Unlock()
	return aligner.calls
}

// A deployment with no spare recogniser still must not resume from words
// nobody heard. Without an aligner the tracker is the proportional layout and
// the whole lifecycle around it, which is the feature at lower resolution
// rather than the feature switched off.
func TestWithoutARecogniserTheCutIsStillFound(t *testing.T) {
	tracker := NewTracker(TrackerConfig{})
	tracker.Begin("speech-1", "one two three four five six seven eight")
	tracker.Audio("speech-1", silence(4000), testRate)
	tracker.Synthesised("speech-1")
	tracker.Played("speech-1", 2000)

	mark, ok := tracker.Current()
	if !ok {
		t.Fatal("an utterance is being spoken and the tracker does not know it")
	}
	if mark.Measured {
		t.Fatal("nothing listened to this audio")
	}
	if mark.Complete() || !mark.Started() {
		t.Fatalf("half an utterance: %+v", mark)
	}
	final := tracker.End(context.Background(), "speech-1", 2000)
	if final.Spoken == "" || !strings.HasPrefix("one two three four five six seven eight", final.Spoken) {
		t.Fatalf("the cut recorded %q as heard", final.Spoken)
	}
	if final.PlayedMS != 2000 {
		t.Fatalf("the record carries %dms of playback", final.PlayedMS)
	}
	if _, still := tracker.Current(); still {
		t.Fatal("the utterance ended and is still reported as being spoken")
	}
}

// The measured path, end to end: audio accumulates, a listen lands, and the
// boundary afterwards comes from the audio rather than from a model of it.
func TestAListenReplacesTheEstimateWithTheAudio(t *testing.T) {
	aligner := &scriptedAligner{words: heardAt(500, "one", "two", "three", "four")}
	tracker := NewTracker(TrackerConfig{Aligner: aligner, Interval: 10 * time.Millisecond})
	tracker.Begin("speech-1", "1 2 3 4")
	tracker.Audio("speech-1", silence(2000), testRate)
	tracker.Synthesised("speech-1")

	deadline := time.Now().Add(2 * time.Second)
	for {
		if timeline, _ := tracker.Timeline("speech-1"); timeline.Measured {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no listen ever landed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mark := tracker.End(context.Background(), "speech-1", 1000)
	if !mark.Measured {
		t.Fatalf("the boundary was not measured: %+v", mark)
	}
	if mark.Spoken != "1 2" || mark.Pending != "3 4" {
		t.Fatalf("heard %q, left %q", mark.Spoken, mark.Pending)
	}
}

// Ending an utterance is the moment the boundary is written into the record
// every later model reads, so it waits for a listen already running. The wait
// is the point and so is the bound: a recogniser that stopped answering must
// not stop the voice.
func TestTheEndWaitsForAListenAlreadyRunningAndNoLonger(t *testing.T) {
	release := make(chan struct{})
	aligner := &scriptedAligner{
		words: heardAt(500, "one", "two", "three", "four"), release: release,
	}
	tracker := NewTracker(TrackerConfig{Aligner: aligner, Interval: 10 * time.Millisecond})
	tracker.Begin("speech-1", "1 2 3 4")
	tracker.Audio("speech-1", silence(2000), testRate)
	tracker.Synthesised("speech-1")
	for aligner.count() == 0 {
		time.Sleep(time.Millisecond)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(release)
	}()
	if mark := tracker.End(context.Background(), "speech-1", 1000); !mark.Measured {
		t.Fatalf("the end did not wait for the listen it was about to record: %+v", mark)
	}

	stuck := &scriptedAligner{release: make(chan struct{}), words: heardAt(500, "one")}
	bounded := NewTracker(TrackerConfig{
		Aligner: stuck, Interval: 10 * time.Millisecond, Timeout: 50 * time.Millisecond,
	})
	bounded.Begin("speech-2", "1 2 3 4")
	bounded.Audio("speech-2", silence(2000), testRate)
	bounded.Synthesised("speech-2")
	for stuck.count() == 0 {
		time.Sleep(time.Millisecond)
	}
	started := time.Now()
	mark := bounded.End(context.Background(), "speech-2", 1000)
	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("a recogniser that never answered held the voice for %s", waited)
	}
	if mark.Measured {
		t.Fatal("nothing was measured, and the record must say so")
	}
	if mark.Spoken == "" {
		t.Fatal("the estimate still has to answer when the listen does not")
	}
}

// A failed listen is not a session failure and is not nothing either.
// Swallowing it is how a deployment finds out months later that no boundary it
// ever recorded came from audio.
func TestAFailedListenIsReportedAndLeavesTheEstimateStanding(t *testing.T) {
	failures := make(chan error, 4)
	aligner := &scriptedAligner{err: errors.New("recogniser refused")}
	tracker := NewTracker(TrackerConfig{
		Aligner: aligner, Interval: 10 * time.Millisecond,
		Report: func(err error) { failures <- err },
	})
	tracker.Begin("speech-1", "one two three four")
	tracker.Audio("speech-1", silence(2000), testRate)
	tracker.Synthesised("speech-1")
	select {
	case <-failures:
	case <-time.After(2 * time.Second):
		t.Fatal("a listen failed and nobody was told")
	}
	mark := tracker.End(context.Background(), "speech-1", 1000)
	if mark.Measured {
		t.Fatal("a failed listen must not be recorded as a measurement")
	}
	if !mark.Started() || mark.Complete() {
		t.Fatalf("the estimate still has to place the cut: %+v", mark)
	}
}

// Audio keeps arriving while a listen runs, so its answer is always about less
// audio than exists by the time it lands. Accepting a shorter answer over a
// longer one moves every boundary backwards.
func TestAShorterListenNeverReplacesALongerOne(t *testing.T) {
	stale := &scriptedAligner{words: heardAt(450, "one", "two")}
	tracker := NewTracker(TrackerConfig{Aligner: stale})
	tracker.Begin("speech-1", "one two three four")
	tracker.mu.Lock()
	state := tracker.states["speech-1"]
	state.timeline = Reconcile("one two three four", heardAt(500, "one", "two", "three", "four"), 2000)
	state.coveredMS = 2000
	tracker.mu.Unlock()

	tracker.listen("speech-1", "one two three four", Audio{}, 900,
		make(chan struct{}))
	timeline, _ := tracker.Timeline("speech-1")
	if timeline.AudioMS != 2000 {
		t.Fatalf("a listen covering 900ms replaced one covering 2000ms: %+v", timeline)
	}
}

// The retained tail is bounded, and bounding it must not throw away the
// utterance being spoken right now.
func TestTheRetainedTailIsBoundedAndKeepsTheLiveUtterance(t *testing.T) {
	tracker := NewTracker(TrackerConfig{Retain: 2})
	for index := 0; index < 5; index++ {
		id := string(rune('a' + index))
		tracker.Begin(id, "one two")
		tracker.Audio(id, silence(500), testRate)
		tracker.Synthesised(id)
		if index < 4 {
			tracker.End(context.Background(), id, 500)
		}
	}
	if _, ok := tracker.Mark("a"); ok {
		t.Fatal("the oldest utterance was retained past the bound")
	}
	if _, ok := tracker.Current(); !ok {
		t.Fatal("eviction dropped the utterance being spoken")
	}
}

// A synthesiser is paced to realtime by the planner consuming it, so the audio
// that exists tracks the audio that has played almost exactly. Laying the text
// out over the audio that exists therefore reports every utterance as nearly
// finished at every moment of its life - and an agent that believes it has
// finished carries on past words nobody heard, which is the failure this
// package removes.
func TestAnUtteranceStillBeingSynthesisedIsNeverReportedAsFinished(t *testing.T) {
	const text = "one two three four five six seven eight nine ten"
	tracker := NewTracker(TrackerConfig{})
	tracker.Begin("speech-1", text)
	for played := 100; played <= 1500; played += 100 {
		tracker.Audio("speech-1", silence(100), testRate)
		tracker.Played("speech-1", uint64(played))
		mark, _ := tracker.Current()
		if mark.Complete() {
			t.Fatalf("at %dms of an utterance still being produced, it claimed to have said everything", played)
		}
		if !strings.HasPrefix(text, mark.Spoken) {
			t.Fatalf("at %dms the spoken part was %q", played, mark.Spoken)
		}
	}
	// And the moment synthesis ends, the guess is replaced by the truth: an
	// utterance that ran to its end must read as finished, or the agent
	// resumes mid-sentence after saying the whole thing.
	tracker.Synthesised("speech-1")
	final := tracker.End(context.Background(), "speech-1", 1500)
	if !final.Complete() {
		t.Fatalf("the utterance ended and %q was still reported unsaid", final.Pending)
	}
}

// The same invariant on the measured path. A recogniser listening partway
// through an utterance can only report the words that exist, and the ones it
// did not reach must not be squeezed into audio that has already played.
func TestWordsTheRecogniserCouldNotReachAreNotReportedAsSpoken(t *testing.T) {
	// It heard the first four words of a ten-word sentence, because those are
	// the only four that have been synthesised.
	aligner := &scriptedAligner{words: heardAt(300, "one", "two", "three", "four")}
	tracker := NewTracker(TrackerConfig{Aligner: aligner, Interval: 10 * time.Millisecond})
	const text = "one two three four five six seven eight nine ten"
	tracker.Begin("speech-1", text)
	tracker.Audio("speech-1", silence(1200), testRate)
	tracker.Played("speech-1", 1200)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if timeline, _ := tracker.Timeline("speech-1"); timeline.Measured {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no listen ever landed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mark, _ := tracker.Current()
	if !mark.Measured {
		t.Fatalf("the boundary is not measured: %+v", mark)
	}
	if mark.Spoken != "one two three four" {
		t.Fatalf("heard %q, want the four words that were synthesised", mark.Spoken)
	}
	if mark.Pending != "five six seven eight nine ten" {
		t.Fatalf("what is left is %q", mark.Pending)
	}
}
