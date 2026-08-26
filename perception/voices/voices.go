// Package voices tells the person the agent is talking to from another voice
// in the room.
//
// A cascade hears one microphone, transcribes it, and works in text from there.
// That discards the one fact separating a question put to the agent from a
// question put to somebody else standing nearby: whose voice it was. Without
// it the situation handed to the interaction model says the user asked about
// the milk, and a model told the user asked about the milk answers about the
// milk. It is not a failure of judgement; it is a correct answer to a false
// premise.
//
// So the premise is checked. The first voice of a session is the person the
// session is with, and every utterance after it is compared against that.
package voices

import (
	"context"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/perception"
)

// Verdict is what is known about who is speaking.
type Verdict string

const (
	// Unknown covers both "not enough audio yet" and "nobody could be asked".
	// The two are the same to a caller: there is no evidence, so the prior
	// stands, and the prior is that the person talking is the one whose
	// session this is.
	Unknown Verdict = "unknown"
	// Familiar is the voice the session was opened with.
	Familiar Verdict = "familiar"
	// Different is somebody else.
	Different Verdict = "different"
)

// Embedder turns speech into a unit-length vector that depends on the speaker
// and not much on what they said.
type Embedder interface {
	Embed(ctx context.Context, pcm []byte, rateHz uint32) ([]float32, error)
}

// DefaultThreshold is the cosine similarity above which two utterances are the
// same person.
//
// Measured with ECAPA-TDNN over the benchmark's own voices, with each speaker
// enrolled from a fixed reference: the same speaker scores 0.60 and 0.77
// across different sentences, and different speakers score 0.15 to 0.17. The
// gap is wide enough that the exact number hardly matters, which is the point
// - a threshold picked in the middle of a gap that size is not a tuned
// constant, it is a reading of a bimodal measurement.
const DefaultThreshold = 0.40

// DefaultMinimum is how much speech is needed before asking.
//
// Under about a second the embedding is dominated by whatever phonemes
// happened to be in it, and a wrong answer here is worse than no answer: no
// answer leaves the prior in place, and a wrong one tells the agent the person
// it is talking to is a stranger.
const DefaultMinimum = time.Second

// Recogniser watches one session.
type Recogniser struct {
	embedder  Embedder
	threshold float64
	minimum   time.Duration

	mu        sync.Mutex
	reference []float32
	utterance string
	buffered  []byte
	rate      uint32
	asked     bool
	verdict   Verdict
}

// New returns a recogniser, or nil when there is nobody to ask. A nil
// recogniser answers Unknown to everything, which is what a caller with no
// embedder should see.
func New(embedder Embedder, threshold float64, minimum time.Duration) *Recogniser {
	if embedder == nil {
		return nil
	}
	if threshold <= 0 {
		threshold = DefaultThreshold
	}
	if minimum <= 0 {
		minimum = DefaultMinimum
	}
	return &Recogniser{embedder: embedder, threshold: threshold, minimum: minimum, verdict: Unknown}
}

// Begin starts a new utterance. What was heard of the last one is not evidence
// about this one.
func (recogniser *Recogniser) Begin(utterance string) {
	if recogniser == nil {
		return
	}
	recogniser.mu.Lock()
	defer recogniser.mu.Unlock()
	recogniser.utterance = utterance
	recogniser.buffered = nil
	recogniser.asked = false
	recogniser.verdict = Unknown
}

// Hear takes the audio admitted for the current utterance.
//
// The embedding is asked for once, as soon as there is enough speech, and off
// the caller's goroutine: this sits on the path between hearing a word and
// deciding whether to answer it, and a verdict that arrives a moment late
// costs nothing while a request that blocks costs every turn.
func (recogniser *Recogniser) Hear(ctx context.Context, frames []perception.Frame) {
	if recogniser == nil {
		return
	}
	recogniser.mu.Lock()
	for _, frame := range frames {
		if frame.Kind != perception.FrameAudio || len(frame.PCM16LE) == 0 {
			continue
		}
		recogniser.rate = frame.SampleRateHz
		recogniser.buffered = append(recogniser.buffered, frame.PCM16LE...)
	}
	// Two bytes to a sample, the frames being 16-bit little-endian.
	samples := len(recogniser.buffered) / 2
	enough := recogniser.rate > 0 &&
		time.Duration(samples)*time.Second/time.Duration(recogniser.rate) >= recogniser.minimum
	if recogniser.asked || !enough {
		recogniser.mu.Unlock()
		return
	}
	recogniser.asked = true
	utterance := recogniser.utterance
	audio := append([]byte(nil), recogniser.buffered...)
	rate := recogniser.rate
	recogniser.mu.Unlock()

	go recogniser.settle(ctx, utterance, audio, rate)
}

func (recogniser *Recogniser) settle(ctx context.Context, utterance string, audio []byte, rate uint32) {
	vector, err := recogniser.embedder.Embed(ctx, audio, rate)
	if err != nil || len(vector) == 0 {
		return
	}
	recogniser.mu.Lock()
	defer recogniser.mu.Unlock()
	// The utterance moved on while the embedding was in flight, so this
	// answers a question about a turn nobody is deciding any more.
	if recogniser.utterance != utterance {
		return
	}
	if recogniser.reference == nil {
		// Whoever opened the session is who the session is with.
		recogniser.reference = vector
		recogniser.verdict = Familiar
		return
	}
	if similarity(recogniser.reference, vector) >= recogniser.threshold {
		recogniser.verdict = Familiar
		return
	}
	recogniser.verdict = Different
}

// Verdict is what is known about the current utterance.
func (recogniser *Recogniser) Verdict() Verdict {
	if recogniser == nil {
		return Unknown
	}
	recogniser.mu.Lock()
	defer recogniser.mu.Unlock()
	return recogniser.verdict
}

// similarity is a dot product, because the embedder returns unit vectors.
func similarity(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var total float64
	for index := range a {
		total += float64(a[index]) * float64(b[index])
	}
	return total
}
