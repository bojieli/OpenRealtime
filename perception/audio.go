package perception

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// GateConfig configures the acoustic gate.
type GateConfig struct {
	// Threshold is the Realtime-compatible sensitivity in [0,1].
	Threshold float64
	// PrefixPaddingMS is how much audio before the onset is kept, so the first
	// syllable is not clipped off the front of an utterance.
	PrefixPaddingMS int
	// SilenceDurationMS is how much silence ends an utterance.
	SilenceDurationMS int
	// SpeechDurationMS is how much voiced audio must accumulate before an
	// utterance starts. Zero selects 120 ms.
	//
	// The gate had hysteresis on one side only: half a second of silence to
	// believe a turn ended, and a single block above the threshold to believe
	// one began. A door slam, a keyboard, a lip smack all clear an energy
	// threshold, so anything percussive opened an utterance - and a recogniser
	// asked what was in it answers honestly that there were no words, which is
	// then indistinguishable from the user having spoken.
	//
	// It costs nothing to wait, because the prefix buffer is already keeping
	// this audio: what the threshold delays is the decision, not the sound. A
	// syllable is comfortably longer than any transient, so the smallest thing
	// a person can say still opens a turn with its onset intact.
	SpeechDurationMS int
}

// DefaultGateConfig matches the Realtime server-VAD defaults.
func DefaultGateConfig() GateConfig {
	return GateConfig{Threshold: 0.5, PrefixPaddingMS: 300, SilenceDurationMS: 500, SpeechDurationMS: 120}
}

// GateResult is what the acoustic gate decided about a block of audio.
type GateResult struct {
	Started      bool
	Stopped      bool
	Audio        []byte
	AudioStartMS int
	AudioEndMS   int
	SilenceNS    uint64
}

// EnergyGate is a content-independent acoustic gate.
//
// It combines an absolute energy threshold with an adaptive noise floor and
// applies the configured prefix and silence hysteresis. It never examines
// transcript text: this is the sub-millisecond, no-I/O half of the observer
// contract, and a gate that read words would be neither.
type EnergyGate struct {
	config     GateConfig
	sampleRate uint32
	speaking   bool
	prefix     []byte
	silence    uint64
	// onset is the run of voiced audio heard while the gate is still closed,
	// and voiced is its length. A gap clears both: a transient is loud but not
	// sustained, and that is the whole difference between a door and a word.
	//
	// It is kept apart from prefix so that "prefix padding" keeps meaning what
	// it says - audio from before the onset - rather than quietly becoming
	// "padding plus however long we waited".
	onset    []byte
	voiced   uint64
	total    uint64
	noiseRMS float64
}

// NewEnergyGate validates the configuration and creates a gate.
func NewEnergyGate(config GateConfig, sampleRate uint32) (*EnergyGate, error) {
	if config.Threshold < 0 || config.Threshold > 1 {
		return nil, errors.New("gate threshold must be between zero and one")
	}
	if config.PrefixPaddingMS < 0 || config.SilenceDurationMS <= 0 {
		return nil, errors.New("gate prefix must be non-negative and silence duration positive")
	}
	if config.SpeechDurationMS < 0 {
		return nil, errors.New("gate speech duration must be non-negative")
	}
	if sampleRate == 0 {
		return nil, errors.New("gate sample rate must be positive")
	}
	return &EnergyGate{config: config, sampleRate: sampleRate, noiseRMS: 32}, nil
}

// Speaking reports whether the gate currently believes the user is audible.
func (gate *EnergyGate) Speaking() bool { return gate.speaking }

// SilenceNS reports how long the gate has seen silence within an utterance.
func (gate *EnergyGate) SilenceNS() uint64 {
	return gate.silence * uint64(time.Second) / uint64(gate.sampleRate)
}

// ForceStop ends the current utterance immediately and reports where it
// ended, whether or not the silence hysteresis was satisfied.
//
// It exists for turn projection: a policy that anticipates the end of a turn
// before silence confirms it has to be able to close the turn, and a gate that
// could only be closed by its own threshold would make the projection an
// opinion nobody could act on. It reports false when there was no utterance
// open, so a projection that arrives just after an ordinary endpoint is a
// no-op rather than a second endpoint.
func (gate *EnergyGate) ForceStop() (endMS int, stopped bool) {
	if !gate.speaking {
		return 0, false
	}
	gate.speaking = false
	gate.silence = 0
	gate.voiced = 0
	gate.prefix, gate.onset = nil, nil
	return samplesToMS(gate.total, gate.sampleRate), true
}

// Reopen continues an utterance the gate has just closed.
//
// It exists for the session whose client owns turn detection: the gate's
// silence threshold is still the right answer to "is the user audible", and
// the wrong answer to "has the turn ended", so the first is kept and the
// second is discarded. Sample accounting is untouched, so the turn's eventual
// end is still measured from its real beginning.
func (gate *EnergyGate) Reopen() {
	gate.speaking = true
	gate.silence = 0
}

// Push advances the gate over one block of PCM16 audio.
func (gate *EnergyGate) Push(pcm16 []byte) (GateResult, error) {
	if len(pcm16) == 0 || len(pcm16)%2 != 0 {
		return GateResult{}, errors.New("acoustic gate requires non-empty PCM16 audio")
	}
	samples := uint64(len(pcm16) / 2)
	gate.total += samples
	rms := pcmRMS(pcm16)
	absolute := 128 + gate.config.Threshold*1_024
	trigger := math.Max(absolute, gate.noiseRMS*3.5)
	voiced := rms >= trigger

	if !gate.speaking {
		if !voiced {
			gate.noiseRMS = 0.98*gate.noiseRMS + 0.02*rms
			// A gap shortens the run rather than ending it. Speech is not
			// continuously loud - a stop consonant is a moment of near
			// silence inside a syllable - so a run that reset on the first
			// quiet block would need the speaker to shout through their own
			// plosives. A transient decays to nothing in the time it takes
			// the next block to arrive, which is the distinction that matters.
			if gate.voiced <= samples {
				gate.appendPrefix(gate.onset)
				gate.onset, gate.voiced = nil, 0
			} else {
				gate.voiced -= samples
			}
			gate.appendPrefix(pcm16)
			return GateResult{}, nil
		}
		// Held rather than emitted, because whether this is the beginning of a
		// turn is not yet known. Waiting costs nothing: the audio is here.
		gate.onset = append(gate.onset, pcm16...)
		gate.voiced += samples
		speechLimit := uint64(gate.config.SpeechDurationMS) * uint64(gate.sampleRate) / 1_000
		if gate.voiced < speechLimit {
			return GateResult{}, nil
		}
		held := uint64(len(gate.prefix)+len(gate.onset)) / 2
		gate.speaking = true
		gate.silence = 0
		gate.voiced = 0
		audio := append(slices.Clone(gate.prefix), gate.onset...)
		gate.prefix, gate.onset = nil, nil
		return GateResult{
			Started: true, Audio: audio,
			AudioStartMS: samplesToMS(gate.total-held, gate.sampleRate),
		}, nil
	}

	if voiced {
		gate.silence = 0
	} else {
		gate.silence += samples
	}
	result := GateResult{Audio: slices.Clone(pcm16), SilenceNS: gate.SilenceNS()}
	silenceLimit := uint64(gate.config.SilenceDurationMS) * uint64(gate.sampleRate) / 1_000
	if gate.silence >= silenceLimit {
		result.Stopped = true
		result.AudioEndMS = samplesToMS(gate.total, gate.sampleRate)
		gate.speaking = false
		gate.silence = 0
		gate.voiced = 0
		gate.prefix, gate.onset = nil, nil
	}
	return result, nil
}

func (gate *EnergyGate) appendPrefix(audio []byte) {
	if len(audio) == 0 {
		return
	}
	maximumSamples := uint64(gate.config.PrefixPaddingMS) * uint64(gate.sampleRate) / 1_000
	maximumBytes := int(maximumSamples * 2)
	if maximumBytes == 0 {
		gate.prefix = nil
		return
	}
	gate.prefix = append(gate.prefix, audio...)
	if len(gate.prefix) > maximumBytes {
		gate.prefix = slices.Clone(gate.prefix[len(gate.prefix)-maximumBytes:])
	}
}

func pcmRMS(input []byte) float64 {
	var sum float64
	for offset := 0; offset < len(input); offset += 2 {
		value := float64(int16(binary.LittleEndian.Uint16(input[offset:])))
		sum += value * value
	}
	return math.Sqrt(sum / float64(len(input)/2))
}

func samplesToMS(samples uint64, rate uint32) int {
	return int(samples * 1_000 / uint64(rate))
}

// AudioConfig configures the audio observer.
type AudioConfig struct {
	// Provider is the streaming recogniser. One is created per utterance, so
	// recogniser state cannot leak between turns.
	Provider func() (v1.PerceptionProvider, error)
	// Name defaults to "audio".
	Name string
	// Cadence is how often the recogniser is advanced. Zero selects 200 ms,
	// which is the reference configuration.
	Cadence time.Duration
	// Source is the frame source this observer accepts. Empty accepts every
	// audio frame, which is the ordinary single-microphone case.
	Source string
}

// AudioObserver recognises speech into typed revisions.
//
// It emits provisional observations as the recogniser revises, and one final
// observation at the endpoint. Whether provisional observations reach the
// canonical trajectory is an observation policy the binding owns; this
// observer's job is to report what it heard, not to decide what is worth
// committing.
type AudioObserver struct {
	config AudioConfig

	mu           sync.Mutex
	provider     v1.PerceptionProvider
	frameIndex   uint64
	sampleOffset uint64
	sampleRate   uint32
	revision     uint64
	// predecessor is the latest emitted revision in the current utterance.
	// revision remains session-monotonic across Reset so provenance IDs never
	// collide, while predecessor is reset so a new utterance cannot claim to
	// supersede the previous utterance's terminal observation.
	predecessor uint64
	lastText    string
	lastStable  string
}

// NewAudioObserver creates the speech observer.
func NewAudioObserver(config AudioConfig) (*AudioObserver, error) {
	if config.Provider == nil {
		return nil, errors.New("audio observer requires a perception provider factory")
	}
	if strings.TrimSpace(config.Name) == "" {
		config.Name = "audio"
	}
	if config.Cadence <= 0 {
		config.Cadence = 200 * time.Millisecond
	}
	return &AudioObserver{config: config}, nil
}

func (observer *AudioObserver) Name() string { return observer.config.Name }

func (observer *AudioObserver) Cadence() time.Duration { return observer.config.Cadence }

func (observer *AudioObserver) Accepts(frame Frame) bool {
	if frame.Kind != FrameAudio {
		return false
	}
	return observer.config.Source == "" || observer.config.Source == frame.Source
}

// Gate admits every audio frame it is handed.
//
// The acoustic gate runs upstream, in the audio pipeline, because its decision
// also drives the duplex state and the endpoint - it is not only a perception
// decision. By the time frames reach here they have already been admitted.
func (observer *AudioObserver) Gate(Frame) bool { return true }

// Observe advances the recogniser and returns any revision it produced.
func (observer *AudioObserver) Observe(ctx context.Context, frames []Frame) ([]Observation, error) {
	if len(frames) == 0 {
		return nil, nil
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.provider == nil {
		provider, err := observer.config.Provider()
		if err != nil {
			return nil, fmt.Errorf("start speech recognition: %w", err)
		}
		observer.provider = provider
	}

	var observations []Observation
	for _, frame := range frames {
		if err := frame.Validate(); err != nil {
			return nil, err
		}
		if observer.sampleRate == 0 {
			observer.sampleRate = frame.SampleRateHz
		}
		if frame.SampleRateHz != observer.sampleRate {
			return nil, errors.New("audio sample rate changed within an utterance")
		}
		revisions, err := observer.provider.PushFrame(ctx, v1.AudioFrame{
			Index: observer.frameIndex, SampleOffset: observer.sampleOffset,
			SampleRateHz: frame.SampleRateHz, PCM16LE: frame.PCM16LE,
		})
		if err != nil {
			return observations, err
		}
		observer.frameIndex++
		observer.sampleOffset += uint64(len(frame.PCM16LE) / 2)
		for _, revision := range revisions {
			if observation, ok := observer.observationFor(revision, frame.CapturedNS, false); ok {
				observations = append(observations, observation)
			}
		}
	}
	return observations, nil
}

// Flush finalises the utterance and returns the terminal observation.
func (observer *AudioObserver) Flush(ctx context.Context) ([]Observation, error) {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.provider == nil {
		return nil, nil
	}
	final, err := observer.provider.Finalize(ctx, observer.sampleOffset)
	if err != nil {
		return nil, err
	}
	observation, ok := observer.observationFor(final, 0, true)
	if !ok {
		return nil, nil
	}
	return []Observation{observation}, nil
}

// Reset drops the recogniser so the next utterance starts clean.
//
// A recogniser that holds a connection is closed on the way out. One
// instance exists per utterance by design, so an abandoned utterance - the
// speaker stops, the session ends, a barge-in discards the turn - would
// otherwise strand a socket and the goroutine reading it for every utterance
// the session ever had. A provider with nothing to release does not implement
// the interface and is unaffected.
func (observer *AudioObserver) Reset() {
	_ = observer.Close()
}

// Close resets the utterance and reports provider-release failures to a
// lifecycle owner. Reset retains its historical best-effort signature for the
// Observer interface; graph disposers use Close so cleanup evidence is not
// silently discarded.
func (observer *AudioObserver) Close() error {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	var closeErr error
	if closer, releases := observer.provider.(io.Closer); releases {
		closeErr = closer.Close()
	}
	observer.provider = nil
	observer.frameIndex, observer.sampleOffset, observer.sampleRate = 0, 0, 0
	observer.predecessor = 0
	observer.lastText, observer.lastStable = "", ""
	return closeErr
}

// DurationMS is how much audio the current utterance has consumed.
func (observer *AudioObserver) DurationMS() uint64 {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.sampleRate == 0 {
		return 0
	}
	return observer.sampleOffset * 1_000 / uint64(observer.sampleRate)
}

// SpeechEndpointed reports an endpoint supplied by a recogniser that combines
// ASR with VAD. It is optional: providers without that capability continue to
// be endpointed by the acoustic floor exactly as before.
func (observer *AudioObserver) SpeechEndpointed() bool {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	provider, ok := observer.provider.(interface{ SpeechEndpointed() bool })
	return ok && provider.SpeechEndpointed()
}

// carriesSpeech reports whether a transcript contains anything a person said.
//
// A recogniser asked about audio with no words in it has to answer somehow,
// and the answer is not always an empty string: SenseVoice returns ".", others
// return the punctuation their language model expects around nothing. The
// contract here is "what the user said", and text with no letter and no digit
// in it says nothing - in any script, which is why this asks Unicode rather
// than stripping a list of characters somebody noticed once.
// CarriesSpeech re-exports the api/v1 contract, which is where it belongs: the
// rule has to be the same for every adapter that reports "no words" and every
// consumer that reads it, or one of them turns punctuation into a turn.
func CarriesSpeech(text string) bool { return v1.CarriesSpeech(text) }

func (observer *AudioObserver) observationFor(revision v1.PerceptionRevision, capturedNS uint64, final bool) (Observation, bool) {
	text := revision.StableText + revision.UnstableText
	// An observation is a claim that the user said something, and the log
	// keeps it forever. Recording "the recogniser heard no words" as a thing
	// the user said is what turns room noise into a turn: the agent answers
	// it, that answer cancels whatever it was already saying, and a caller
	// hears their own answer cut off to make room for nothing.
	if !CarriesSpeech(text) {
		return Observation{}, false
	}
	if !final && text == observer.lastText {
		return Observation{}, false
	}
	observer.revision++
	previous := observer.predecessor
	observer.predecessor = observer.revision
	observer.lastText = text
	observer.lastStable = revision.StableText
	observation := Observation{
		Text: text, Observer: observer.config.Name, Source: observer.config.Source,
		Authority: trajectory.AuthorityUser, Revision: observer.revision,
		StableText: revision.StableText, Provisional: !final, Final: final,
		OccurredNS: capturedNS,
	}
	if previous > 0 {
		observation.Supersedes = previous
	}
	return observation, true
}

var _ Observer = (*AudioObserver)(nil)
