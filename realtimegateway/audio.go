package realtimegateway

import (
	"encoding/binary"
	"errors"
	"math"
	"slices"

	"github.com/bojieli/OpenRealtime/pcm"
)

const (
	formatPCMU  = "audio/pcmu"
	formatPCM16 = "audio/pcm"
)

type audioFormat struct {
	Type string `json:"type"`
	Rate uint32 `json:"rate,omitempty"`
}

func (format audioFormat) sampleRate() (uint32, error) {
	switch format.Type {
	case formatPCMU:
		return 8_000, nil
	case formatPCM16:
		if format.Rate != 0 && format.Rate != 24_000 {
			return 0, errors.New("audio/pcm requires a 24000 Hz rate")
		}
		return 24_000, nil
	default:
		return 0, errors.New("gateway supports audio/pcmu and 24 kHz audio/pcm")
	}
}

func decodeAudio(format audioFormat, input []byte) ([]byte, uint32, error) {
	rate, err := format.sampleRate()
	if err != nil {
		return nil, 0, err
	}
	switch format.Type {
	case formatPCMU:
		return decodeMuLaw(input), rate, nil
	case formatPCM16:
		if len(input)%2 != 0 {
			return nil, 0, errors.New("PCM16 input must contain complete samples")
		}
		return slices.Clone(input), rate, nil
	default:
		return nil, 0, errors.New("unsupported audio format")
	}
}

func decodeMuLaw(input []byte) []byte {
	output := make([]byte, len(input)*2)
	for index, encoded := range input {
		value := ^encoded
		mantissa := int(value & 0x0f)
		exponent := uint((value >> 4) & 0x07)
		sample := ((mantissa << 3) + 0x84) << exponent
		sample -= 0x84
		if value&0x80 != 0 {
			sample = -sample
		}
		binary.LittleEndian.PutUint16(output[index*2:], uint16(int16(sample)))
	}
	return output
}

func encodeMuLaw(input []byte) ([]byte, error) {
	if len(input)%2 != 0 {
		return nil, errors.New("PCM16 input must contain complete samples")
	}
	output := make([]byte, len(input)/2)
	for index := range output {
		output[index] = linearToMuLaw(int(binary.LittleEndian.Uint16(input[index*2:])))
	}
	return output, nil
}

func linearToMuLaw(raw int) byte {
	sample := int(int16(raw))
	sign := byte(0)
	if sample < 0 {
		sign = 0x80
		sample = -sample
		if sample > math.MaxInt16 {
			sample = math.MaxInt16
		}
	}
	if sample > 32635 {
		sample = 32635
	}
	sample += 0x84
	exponent := byte(7)
	for mask := 0x4000; exponent > 0 && sample&mask == 0; mask >>= 1 {
		exponent--
	}
	mantissa := byte(sample >> (exponent + 3) & 0x0f)
	return ^(sign | exponent<<4 | mantissa)
}

type vadConfig struct {
	Threshold         float64
	PrefixPaddingMS   int
	SilenceDurationMS int
}

type vadResult struct {
	Started      bool
	Stopped      bool
	Audio        []byte
	AudioStartMS int
	AudioEndMS   int
}

// energyVAD is a content-independent acoustic gate. It combines an explicit
// absolute energy threshold with an adaptive noise-floor ratio and uses the
// configured prefix and silence hysteresis. It never examines transcript text.
type energyVAD struct {
	config     vadConfig
	sampleRate uint32
	speaking   bool
	prefix     []byte
	silence    uint64
	total      uint64
	noiseRMS   float64
}

func newEnergyVAD(config vadConfig, sampleRate uint32) (*energyVAD, error) {
	if config.Threshold < 0 || config.Threshold > 1 {
		return nil, errors.New("VAD threshold must be between zero and one")
	}
	if config.PrefixPaddingMS < 0 || config.SilenceDurationMS <= 0 {
		return nil, errors.New("VAD prefix must be non-negative and silence duration positive")
	}
	if sampleRate == 0 {
		return nil, errors.New("VAD sample rate must be positive")
	}
	return &energyVAD{config: config, sampleRate: sampleRate, noiseRMS: 32}, nil
}

func (detector *energyVAD) Push(pcm16 []byte) (vadResult, error) {
	if len(pcm16) == 0 || len(pcm16)%2 != 0 {
		return vadResult{}, errors.New("VAD requires non-empty PCM16 audio")
	}
	samples := uint64(len(pcm16) / 2)
	startSample := detector.total
	detector.total += samples
	rms := pcmRMS(pcm16)
	absolute := 128 + detector.config.Threshold*1_024
	trigger := math.Max(absolute, detector.noiseRMS*3.5)
	voiced := rms >= trigger

	if !detector.speaking {
		if !voiced {
			detector.noiseRMS = 0.98*detector.noiseRMS + 0.02*rms
			detector.appendPrefix(pcm16)
			return vadResult{}, nil
		}
		prefixSamples := uint64(len(detector.prefix) / 2)
		detector.speaking = true
		detector.silence = 0
		audio := append(slices.Clone(detector.prefix), pcm16...)
		detector.prefix = nil
		return vadResult{
			Started: true, Audio: audio,
			AudioStartMS: samplesToMS(startSample-prefixSamples, detector.sampleRate),
		}, nil
	}

	if voiced {
		detector.silence = 0
	} else {
		detector.silence += samples
	}
	result := vadResult{Audio: slices.Clone(pcm16)}
	silenceLimit := uint64(detector.config.SilenceDurationMS) * uint64(detector.sampleRate) / 1_000
	if detector.silence >= silenceLimit {
		result.Stopped = true
		result.AudioEndMS = samplesToMS(detector.total, detector.sampleRate)
		detector.speaking = false
		detector.silence = 0
		detector.prefix = nil
	}
	return result, nil
}

func (detector *energyVAD) appendPrefix(audio []byte) {
	maximumSamples := uint64(detector.config.PrefixPaddingMS) * uint64(detector.sampleRate) / 1_000
	maximumBytes := int(maximumSamples * 2)
	if maximumBytes == 0 {
		detector.prefix = nil
		return
	}
	detector.prefix = append(detector.prefix, audio...)
	if len(detector.prefix) > maximumBytes {
		detector.prefix = slices.Clone(detector.prefix[len(detector.prefix)-maximumBytes:])
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

type outputEncoder struct {
	format    audioFormat
	resampler *pcm.Resampler
}

func newOutputEncoder(format audioFormat, sourceRate uint32) (*outputEncoder, error) {
	targetRate, err := format.sampleRate()
	if err != nil {
		return nil, err
	}
	resampler, err := pcm.NewResampler(sourceRate, targetRate)
	if err != nil {
		return nil, err
	}
	return &outputEncoder{format: format, resampler: resampler}, nil
}

func (encoder *outputEncoder) Push(input []byte, final bool) ([]byte, error) {
	converted, err := encoder.resampler.Push(input)
	if err != nil {
		return nil, err
	}
	if final {
		terminal, finalizeErr := encoder.resampler.Finalize()
		if finalizeErr != nil {
			return nil, finalizeErr
		}
		converted = append(converted, terminal...)
	}
	if encoder.format.Type == formatPCMU {
		return encodeMuLaw(converted)
	}
	return converted, nil
}
