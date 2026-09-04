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
	"errors"
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
// Two corpora, deliberately, because a threshold read off the benchmark it is
// then scored on is a property of the benchmark rather than of the embedder.
//
//	same speaker, unrelated corpus, whole recordings   0.796 to 0.950
//	same speaker, unrelated corpus, halves of one      0.571 to 0.782
//	same speaker, the benchmark's voices               0.60 and 0.77
//	different speakers, the benchmark's voices         0.15 to 0.17
//
// The first two rows come from audio that has nothing to do with any scenario
// here, measured by tools/speakerid/calibrate.py, which needs no labels: the
// two halves of one recording are the same speaker by construction. They put
// the same-speaker floor at about 0.57 even for segments only a second or two
// long, which is the hard case. The benchmark supplies the other side.
//
// Nothing observed falls between 0.18 and 0.56. A threshold in the middle of a
// gap that wide is a reading of a bimodal measurement rather than a tuned
// constant, and the calibrator is checked in so the reading can be repeated on
// any corpus - including one that disagrees.
const DefaultThreshold = 0.40

// DefaultMinimum is how much speech is needed before asking who is talking.
//
// Under a second and a half the embedding is dominated by whatever phonemes
// happened to be in it, and a wrong answer here is worse than no answer: no
// answer leaves the prior in place, and a wrong one tells the agent the person
// it is talking to is a stranger.
//
// A second was an estimate of where that stops being true, and measurement put
// it exactly on the line. One voice against itself, and against another, at
// the threshold above:
//
//	window   same voice   another voice
//	 300ms        0.219           0.145
//	 500ms        0.321           0.177
//	 750ms        0.301           0.243
//	1000ms        0.430           0.219
//	1500ms        0.584           0.202
//	3000ms        0.722           0.246
//
// At a second the same speaker scores 0.430 against a threshold of 0.40, which
// is a coin toss, and every toss that lands wrong relabels the person mid
// sentence. Measured in the suite, a speaker telling one uninterrupted story
// was reported as somebody else in the room 122 times. At a second and a half
// there is a third of a point of clearance on both sides of the threshold.
//
// The cost of waiting is that a genuine second speaker is recognised half a
// second later, which is affordable: somebody else in the room talks for
// seconds at a time, and until the verdict arrives the prior stands rather
// than a guess.
const DefaultMinimum = 1500 * time.Millisecond

// DefaultEnrolment is how much speech is needed before deciding whose session
// this is.
//
// It used to be three seconds, on the reasoning that a comparison that goes
// wrong costs one utterance while a reference that goes wrong is wrong about
// every utterance after it. The asymmetry is real. The number was measured
// against segments of one recording, though, which is the easy case: the same
// speaker in the same breath, in the same acoustic conditions, saying words
// that follow on. Measured the way it is actually used - a reference built
// from one utterance, compared against different utterances later in the
// conversation - it plateaus far earlier:
//
//	enrolment   same speaker   another speaker
//	   1000ms   0.415..0.429      0.093..0.114
//	   1500ms   0.509..0.563      0.060..0.097
//	   2000ms   0.571..0.616      0.114..0.168
//	   3000ms   0.576..0.667      0.091..0.147
//
// Three seconds buys nothing over two. One and a half seconds usually clears
// the threshold, but a short phrase is still content-dominated: in the live
// correction scenario, enrolling from "Right. So" scored only 0.359 against
// a later utterance by the same speaker. Waiting for two seconds moved the
// same-speaker comparison to 0.531 while the other speaker remained at 0.154.
//
// What three seconds cost is the whole mechanism, because the buffer is per
// utterance and nobody speaks in three-second sentences on purpose. A person
// who opened with "I'm just going to get on with this for a bit" - 2.09
// seconds - never enrolled at all, and the failure is not that identification
// degrades. It is that the prior takes over: with no reference, whoever is
// talking is the person whose session this is. So the stranger who asked
// somebody else about the milk was read as the user, twice, and the agent
// answered a question that was not addressed to it in every run.
//
// Failing to enrol is worse than waiting indefinitely, but two seconds is
// reached by the next substantive utterance in that conversation and gives a
// stable reference instead of permanently enrolling from a filler phrase.
const DefaultEnrolment = 2 * time.Second

// Recogniser watches one session.
type Recogniser struct {
	embedder  Embedder
	threshold float64
	minimum   time.Duration
	enrolment time.Duration

	mu             sync.Mutex
	reference      []float32
	utterance      string
	buffered       []byte
	rate           uint32
	asked          bool
	verdict        Verdict
	pending        chan struct{}
	referenceReady bool
	compared       bool
	similarity     float64
	lastError      string
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
	enrolment := DefaultEnrolment
	if enrolment < minimum {
		enrolment = minimum
	}
	return &Recogniser{
		embedder: embedder, threshold: threshold,
		minimum: minimum, enrolment: enrolment, verdict: Unknown,
	}
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
	recogniser.pending = nil
	recogniser.compared = false
	recogniser.similarity = 0
	recogniser.lastError = ""
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
	// Enrolling needs more speech than comparing, because the reference is
	// used for the rest of the session and a comparison for one utterance.
	needed := recogniser.minimum
	if recogniser.reference == nil {
		needed = recogniser.enrolment
	}
	enough := recogniser.rate > 0 &&
		time.Duration(samples)*time.Second/time.Duration(recogniser.rate) >= needed
	if recogniser.asked || !enough {
		recogniser.mu.Unlock()
		return
	}
	recogniser.asked = true
	settled := make(chan struct{})
	recogniser.pending = settled
	utterance := recogniser.utterance
	audio := append([]byte(nil), recogniser.buffered...)
	rate := recogniser.rate
	recogniser.mu.Unlock()

	go recogniser.settle(ctx, utterance, audio, rate, settled)
}

func (recogniser *Recogniser) settle(
	ctx context.Context, utterance string, audio []byte, rate uint32, settled chan struct{},
) {
	defer close(settled)
	vector, err := recogniser.embedder.Embed(ctx, audio, rate)
	if err != nil || len(vector) == 0 {
		recogniser.mu.Lock()
		if recogniser.utterance == utterance {
			if err == nil {
				err = errors.New("speaker embedder returned no vector")
			}
			recogniser.lastError = err.Error()
		}
		recogniser.mu.Unlock()
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
		recogniser.referenceReady = true
		recogniser.verdict = Familiar
		return
	}
	recogniser.compared = true
	recogniser.similarity = similarity(recogniser.reference, vector)
	if recogniser.similarity >= recogniser.threshold {
		recogniser.verdict = Familiar
		return
	}
	recogniser.verdict = Different
}

// Evidence is the inspectable state behind a speaker verdict. It is intended
// for opt-in diagnostic traces, not as another policy input: the interaction
// model should receive the attribution, while operators need enough evidence
// to tell a model decision from missing or failed speaker perception.
type Evidence struct {
	Verdict        Verdict
	Buffered       time.Duration
	Asked          bool
	Pending        bool
	ReferenceReady bool
	Compared       bool
	Similarity     float64
	Error          string
}

// Evidence returns one consistent snapshot of speaker recognition state.
func (recogniser *Recogniser) Evidence() Evidence {
	if recogniser == nil {
		return Evidence{Verdict: Unknown}
	}
	recogniser.mu.Lock()
	defer recogniser.mu.Unlock()
	buffered := time.Duration(0)
	if recogniser.rate > 0 {
		buffered = time.Duration(len(recogniser.buffered)/2) * time.Second /
			time.Duration(recogniser.rate)
	}
	pending := recogniser.pending != nil
	if pending {
		select {
		case <-recogniser.pending:
			pending = false
		default:
		}
	}
	return Evidence{
		Verdict: recogniser.verdict, Buffered: buffered, Asked: recogniser.asked,
		Pending: pending, ReferenceReady: recogniser.referenceReady,
		Compared: recogniser.compared, Similarity: recogniser.similarity,
		Error: recogniser.lastError,
	}
}

// Await returns the best verdict available after an in-flight comparison has
// settled or the caller's bound has expired. It never starts a comparison: a
// short utterance that did not meet the evidence minimum remains Unknown.
//
// Partial transcript decisions stay non-blocking. A final transcript is
// different: it is the authorization boundary that can open ordinary
// cognition, and letting it overtake a speaker comparison already in flight
// turns "unknown for another few milliseconds" into "the user asked this".
func (recogniser *Recogniser) Await(ctx context.Context) Verdict {
	if recogniser == nil {
		return Unknown
	}
	recogniser.mu.Lock()
	pending := recogniser.pending
	verdict := recogniser.verdict
	recogniser.mu.Unlock()
	if pending == nil {
		return verdict
	}
	select {
	case <-pending:
		return recogniser.Verdict()
	case <-ctx.Done():
		return recogniser.Verdict()
	}
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
