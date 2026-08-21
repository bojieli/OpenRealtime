package perception

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
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
}

// DefaultGateConfig matches the Realtime server-VAD defaults.
func DefaultGateConfig() GateConfig {
	return GateConfig{Threshold: 0.5, PrefixPaddingMS: 300, SilenceDurationMS: 500}
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
	total      uint64
	noiseRMS   float64
}

// NewEnergyGate validates the configuration and creates a gate.
func NewEnergyGate(config GateConfig, sampleRate uint32) (*EnergyGate, error) {
	if config.Threshold < 0 || config.Threshold > 1 {
		return nil, errors.New("gate threshold must be between zero and one")
	}
	if config.PrefixPaddingMS < 0 || config.SilenceDurationMS <= 0 {
		return nil, errors.New("gate prefix must be non-negative and silence duration positive")
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

// Push advances the gate over one block of PCM16 audio.
func (gate *EnergyGate) Push(pcm16 []byte) (GateResult, error) {
	if len(pcm16) == 0 || len(pcm16)%2 != 0 {
		return GateResult{}, errors.New("acoustic gate requires non-empty PCM16 audio")
	}
	samples := uint64(len(pcm16) / 2)
	startSample := gate.total
	gate.total += samples
	rms := pcmRMS(pcm16)
	absolute := 128 + gate.config.Threshold*1_024
	trigger := math.Max(absolute, gate.noiseRMS*3.5)
	voiced := rms >= trigger

	if !gate.speaking {
		if !voiced {
			gate.noiseRMS = 0.98*gate.noiseRMS + 0.02*rms
			gate.appendPrefix(pcm16)
			return GateResult{}, nil
		}
		prefixSamples := uint64(len(gate.prefix) / 2)
		gate.speaking = true
		gate.silence = 0
		audio := append(slices.Clone(gate.prefix), pcm16...)
		gate.prefix = nil
		return GateResult{
			Started: true, Audio: audio,
			AudioStartMS: samplesToMS(startSample-prefixSamples, gate.sampleRate),
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
		gate.prefix = nil
	}
	return result, nil
}

func (gate *EnergyGate) appendPrefix(audio []byte) {
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
	lastText     string
	lastStable   string
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
func (observer *AudioObserver) Reset() {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	observer.provider = nil
	observer.frameIndex, observer.sampleOffset, observer.sampleRate = 0, 0, 0
	observer.lastText, observer.lastStable = "", ""
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

func (observer *AudioObserver) observationFor(revision v1.PerceptionRevision, capturedNS uint64, final bool) (Observation, bool) {
	text := revision.StableText + revision.UnstableText
	if strings.TrimSpace(text) == "" {
		return Observation{}, false
	}
	if !final && text == observer.lastText {
		return Observation{}, false
	}
	previous := observer.revision
	observer.revision++
	observer.lastText = text
	observer.lastStable = revision.StableText
	observation := Observation{
		Text: text, Observer: observer.config.Name, Source: observer.config.Source,
		Authority: trajectory.AuthorityUser, Revision: observer.revision,
		StableText: revision.StableText, Provisional: !final, Final: final,
		OccurredNS: capturedNS,
	}
	if final && previous > 0 {
		observation.Supersedes = previous
	}
	return observation, true
}

var _ Observer = (*AudioObserver)(nil)
