package spoken

import (
	"context"
	"sync"
	"time"
)

// Audio is one utterance's synthesised audio, as the synthesiser produced it.
type Audio struct {
	PCM16LE      []byte
	SampleRateHz uint32
}

// Aligner reports where the words are in a piece of audio.
//
// It is a recogniser with word times, and the interface says nothing about
// which one: a local Whisper, a hosted transcription endpoint, or a forced
// aligner all answer the same question. What it must not be asked to do is
// decide what was said - Reconcile owns that, because the text is already
// known and the recogniser's version of it is strictly worse.
type Aligner interface {
	Words(ctx context.Context, audio Audio) ([]Word, error)
}

// TrackerConfig configures the per-session tracker.
type TrackerConfig struct {
	// Aligner is optional. Without one every boundary is proportional, which
	// is the whole feature working at lower resolution rather than the feature
	// switched off - a deployment with no spare recogniser still stops
	// resuming from words nobody heard.
	Aligner Aligner
	// Interval is how much new audio is worth another listen while an
	// utterance is still being synthesised. Zero selects a second, which is
	// short enough that a cut lands inside measured audio and long enough that
	// an ordinary sentence is listened to once or twice rather than ten times.
	Interval time.Duration
	// Timeout bounds one listen, and separately bounds how long the end of an
	// utterance waits for one already running.
	Timeout time.Duration
	// Retain is how many finished utterances stay readable afterwards. The
	// trajectory keeps the durable record; this is a short tail for the
	// callers that ask again a moment later.
	Retain int
	// Report receives alignment failures. A failed listen is not a session
	// failure - the proportional layout still answers - but it is not nothing
	// either, and swallowing it is how a deployment discovers months later
	// that nothing has ever been measured.
	Report func(error)
}

// Tracker follows every utterance from its first synthesised sample to the
// moment playback stops, and answers what had actually been said by then.
//
// One per session. Speech is serial by construction - a second voice over the
// first is not a feature - so at most one utterance is ever in flight, and the
// bookkeeping stays a map only because callers ask about utterances that have
// already ended.
type Tracker struct {
	config TrackerConfig

	mu       sync.Mutex
	states   map[string]*utteranceState
	order    []string
	current  string
	sequence uint64
}

type utteranceState struct {
	text     string
	rate     uint32
	pcm      []byte
	audioMS  uint64
	playedMS uint64
	// synthesised says the provider finished producing audio, which is what
	// turns the expected duration in the layout into the real one.
	synthesised bool
	timeline    Timeline
	// heard is the last thing a recogniser reported about this audio. It is
	// kept because the layout has to be rebuilt whenever the utterance's
	// believed duration changes, and rebuilding it from the recogniser's words
	// is exact where rebuilding it from the previous layout would be a rescale
	// of a rescale.
	heard []Word
	// coveredMS is how much audio the current measured layout listened to.
	coveredMS uint64
	// aligning is closed when the listen in flight finishes, so that the end
	// of an utterance can wait for the answer it is about to record.
	aligning chan struct{}
	ended    bool
	final    Mark
}

// NewTracker creates a tracker. A nil aligner is allowed and documented.
func NewTracker(config TrackerConfig) *Tracker {
	if config.Interval <= 0 {
		config.Interval = time.Second
	}
	if config.Timeout <= 0 {
		config.Timeout = 2 * time.Second
	}
	if config.Retain <= 0 {
		config.Retain = 8
	}
	return &Tracker{config: config, states: map[string]*utteranceState{}}
}

// Begin announces an utterance that is about to be synthesised.
func (tracker *Tracker) Begin(id, text string) {
	if tracker == nil || id == "" {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.states[id] = &utteranceState{text: text, timeline: Estimate(text, 0)}
	tracker.order = append(tracker.order, id)
	tracker.current = id
	tracker.evict()
}

// Audio accepts one chunk of synthesised audio, in the order it was produced.
func (tracker *Tracker) Audio(id string, chunk []byte, sampleRateHz uint32) {
	if tracker == nil || id == "" || len(chunk) == 0 || sampleRateHz == 0 {
		return
	}
	tracker.mu.Lock()
	state, exists := tracker.states[id]
	if !exists || state.ended {
		tracker.mu.Unlock()
		return
	}
	state.rate = sampleRateHz
	state.pcm = append(state.pcm, chunk...)
	state.audioMS = durationMS(len(state.pcm), sampleRateHz)
	// The expected duration moves with the audio, so the layout is rebuilt.
	// Without this it stays frozen at the prior and every utterance the prior
	// underestimated reports itself as finished partway through.
	state.relayout()
	tracker.maybeListen(id, state)
	tracker.mu.Unlock()
}

// Synthesised says the provider produced its last sample.
//
// It is the moment the estimated layout stops guessing at a total duration and
// starts using the real one, so it matters even where no aligner is configured.
func (tracker *Tracker) Synthesised(id string) {
	if tracker == nil || id == "" {
		return
	}
	tracker.mu.Lock()
	state, exists := tracker.states[id]
	if !exists || state.ended {
		tracker.mu.Unlock()
		return
	}
	state.synthesised = true
	state.relayout()
	tracker.maybeListen(id, state)
	tracker.mu.Unlock()
}

// Played records how much audio has been handed to the world.
func (tracker *Tracker) Played(id string, playedMS uint64) {
	if tracker == nil || id == "" {
		return
	}
	tracker.mu.Lock()
	if state, exists := tracker.states[id]; exists && !state.ended {
		state.playedMS = playedMS
	}
	tracker.mu.Unlock()
}

// Mark is what the utterance had said as of the audio handed out so far.
func (tracker *Tracker) Mark(id string) (Mark, bool) {
	if tracker == nil || id == "" {
		return Mark{}, false
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	state, exists := tracker.states[id]
	if !exists {
		return Mark{}, false
	}
	if state.ended {
		return state.final, true
	}
	return state.timeline.At(state.playedMS), true
}

// MarkAt re-splits a finished utterance at a different playback position.
//
// The client is the authority on what was actually heard: the server knows
// what it sent, and only the client knows where playback stopped. When it says
// so afterwards, the boundary moves, and the layout that was worked out for
// the utterance is exactly what makes moving it possible.
func (tracker *Tracker) MarkAt(id string, playedMS uint64) (Mark, bool) {
	if tracker == nil || id == "" {
		return Mark{}, false
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	state, exists := tracker.states[id]
	if !exists {
		return Mark{}, false
	}
	mark := state.timeline.At(playedMS)
	if state.ended {
		state.final = mark
	}
	return mark, true
}

// Current is the mark of the utterance being spoken now.
func (tracker *Tracker) Current() (Mark, bool) {
	if tracker == nil {
		return Mark{}, false
	}
	tracker.mu.Lock()
	id := tracker.current
	tracker.mu.Unlock()
	if id == "" {
		return Mark{}, false
	}
	return tracker.Mark(id)
}

// End closes an utterance at the position playback actually reached and
// returns the boundary that will be recorded for it.
//
// It waits, briefly, for a listen already in flight. The wait is deliberate and
// it is bounded: this is the one moment where the answer is about to be written
// into the trajectory and read by every model afterwards, and a cut recorded
// from the proportional layout when a measured one arrives fifty milliseconds
// later is a worse record for the rest of the session. The bound is the point -
// a recogniser that has stopped answering must not stop the voice.
func (tracker *Tracker) End(ctx context.Context, id string, playedMS uint64) Mark {
	if tracker == nil || id == "" {
		return Mark{}
	}
	tracker.mu.Lock()
	state, exists := tracker.states[id]
	if !exists {
		tracker.mu.Unlock()
		return Mark{}
	}
	if state.ended {
		final := state.final
		tracker.mu.Unlock()
		return final
	}
	state.playedMS = playedMS
	waiting := state.aligning
	tracker.mu.Unlock()

	if waiting != nil {
		timer := time.NewTimer(tracker.config.Timeout)
		select {
		case <-waiting:
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	state, exists = tracker.states[id]
	if !exists {
		return Mark{}
	}
	if !state.ended {
		state.ended = true
		state.final = state.timeline.At(playedMS)
		state.final.PlayedMS = playedMS
		// The audio has served its purpose and is the only large thing here.
		state.pcm = nil
		if tracker.current == id {
			tracker.current = ""
		}
	}
	return state.final
}

// Timeline exposes the layout an utterance ended with, for evidence and tests.
func (tracker *Tracker) Timeline(id string) (Timeline, bool) {
	if tracker == nil || id == "" {
		return Timeline{}, false
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	state, exists := tracker.states[id]
	if !exists {
		return Timeline{}, false
	}
	return state.timeline, true
}

// maybeListen starts an alignment when there is new audio worth listening to.
// It is called with the lock held and never blocks.
func (tracker *Tracker) maybeListen(id string, state *utteranceState) {
	if tracker.config.Aligner == nil || state.aligning != nil || len(state.pcm) == 0 {
		return
	}
	if state.timeline.Measured && state.coveredMS >= state.audioMS {
		return
	}
	if !state.synthesised && state.audioMS < state.coveredMS+uint64(tracker.config.Interval/time.Millisecond) {
		return
	}
	audio := Audio{
		PCM16LE: append([]byte(nil), state.pcm...), SampleRateHz: state.rate,
	}
	covered := state.audioMS
	text := state.text
	done := make(chan struct{})
	state.aligning = done
	tracker.sequence++
	go tracker.listen(id, text, audio, covered, done)
}

func (tracker *Tracker) listen(id, text string, audio Audio, coveredMS uint64, done chan struct{}) {
	defer close(done)
	ctx, cancel := context.WithTimeout(context.Background(), tracker.config.Timeout)
	defer cancel()
	heard, err := tracker.config.Aligner.Words(ctx, audio)
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	state, exists := tracker.states[id]
	if exists && state.aligning == done {
		state.aligning = nil
	}
	if err != nil {
		if tracker.config.Report != nil {
			tracker.config.Report(err)
		}
		return
	}
	if !exists || state.ended {
		return
	}
	if coveredMS < state.coveredMS {
		// A later listen already covered more of this utterance. Replacing it
		// with an earlier, shorter one would move every boundary backwards.
		return
	}
	state.heard, state.coveredMS = heard, coveredMS
	state.relayout()
	if !state.timeline.Measured {
		// Nothing in the transcript could be matched to the text, so the
		// proportional layout stands and this listen taught us nothing.
		state.heard = nil
	}
	// More audio arrived while this listen was running, or the utterance
	// finished; either way there is a longer one to do.
	tracker.maybeListen(id, state)
}

// relayout rebuilds this utterance's layout for its current believed duration.
//
// Every input to a layout can change while an utterance is in flight - more
// audio arrives, synthesis ends, a recogniser answers - and each of them moves
// where the words sit. Rebuilding from the recogniser's own words is exact;
// rescaling the previous layout would be a rescale of a rescale.
func (state *utteranceState) relayout() {
	if len(state.heard) > 0 {
		if state.synthesised {
			state.timeline = Reconcile(state.text, state.heard, state.audioMS).
				Complete(state.audioMS)
			return
		}
		state.timeline = Reconcile(state.text, state.heard, ExpectedSpan(state.text, state.audioMS))
		return
	}
	if state.synthesised {
		state.timeline = Estimate(state.text, state.audioMS).Complete(state.audioMS)
		return
	}
	state.timeline = Estimate(state.text, state.audioMS)
}

// evict keeps the retained tail bounded. It is called with the lock held.
func (tracker *Tracker) evict() {
	for len(tracker.order) > tracker.config.Retain {
		oldest := tracker.order[0]
		tracker.order = tracker.order[1:]
		if oldest != tracker.current {
			delete(tracker.states, oldest)
		}
	}
}

func durationMS(bytes int, sampleRateHz uint32) uint64 {
	if sampleRateHz == 0 || bytes <= 0 {
		return 0
	}
	return uint64(bytes/2) * 1000 / uint64(sampleRateHz)
}
