package gateway

import (
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"time"

	"github.com/bojieli/OpenRealtime/pcm"
)

const (
	formatPCMU  = "audio/pcmu"
	formatPCM16 = "audio/pcm"
)

// audioFormat is the wire audio format. The gateway supports the two the
// Realtime API defines and nothing else: telephony mu-law at 8 kHz, and 24 kHz
// PCM16. Everything inside the runtime is PCM16 at the component's own rate,
// so this type is only about what crosses the socket.
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

// encodedAudioDuration is how long an encoded block plays for.
func encodedAudioDuration(format audioFormat, bytes int) (time.Duration, error) {
	if bytes <= 0 {
		return 0, errors.New("encoded audio must be non-empty")
	}
	rate, err := format.sampleRate()
	if err != nil {
		return 0, err
	}
	samples := bytes
	if format.Type == formatPCM16 {
		if bytes%2 != 0 {
			return 0, errors.New("encoded PCM audio must contain complete samples")
		}
		samples /= 2
	}
	return time.Duration(samples) * time.Second / time.Duration(rate), nil
}
