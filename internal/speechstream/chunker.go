// Package speechstream turns a synthesiser's PCM16LE response body into the
// speech chunks the action plane paces out.
//
// Every streaming speech adapter here faces the same three problems, and they
// are not obvious ones: a read boundary can split a sample in half, the output
// rate is rarely the rate the server chose, and exactly one chunk has to carry
// the terminal flag - which means the last complete sample must be withheld
// until the body ends. Solving them once is why this is a package rather than
// a method on each adapter.
package speechstream

import (
	"errors"
	"fmt"
	"io"
	"slices"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/pcm"
)

const (
	// DefaultReadBufferBytes is the read granularity. It is small enough that
	// the first chunk leaves for the client while the synthesiser is still
	// producing, which is the whole point of streaming.
	DefaultReadBufferBytes = 8 << 10
	// DefaultMaxAudioBytes bounds a single utterance in each direction.
	DefaultMaxAudioBytes = int64(128 << 20)
)

// Options configures one conversion.
type Options struct {
	// Label names the provider in error messages. An operator reading
	// "returned no audio" needs to know which endpoint did that.
	Label string
	// SourceRateHz is the rate the response body carries.
	SourceRateHz uint32
	// OutputRateHz is the rate chunks are emitted at.
	OutputRateHz uint32
	// ReadBufferBytes is the read granularity. Zero selects the default.
	ReadBufferBytes int
	// MaxAudioBytes bounds the response and the output independently. Zero
	// selects the default.
	MaxAudioBytes int64
}

// Consume reads body to completion and calls consume for each chunk.
//
// Exactly one non-empty chunk is marked Final, and it is the last one. That is
// why the final output sample is held back rather than emitted as it arrives:
// the stream does not announce its end in advance, so the only way to know
// which chunk is last is to still be holding it when the body closes.
func Consume(
	body io.Reader, options Options, candidateID string, consume func(v1.SpeechChunk) error,
) error {
	if body == nil {
		return errors.New("speech stream requires a response body")
	}
	if consume == nil {
		return v1.ErrNilConsumer
	}
	label := options.Label
	if label == "" {
		label = "speech provider"
	}
	if options.SourceRateHz == 0 || options.OutputRateHz == 0 {
		return fmt.Errorf("%s requires source and output sample rates", label)
	}
	readBuffer := options.ReadBufferBytes
	if readBuffer == 0 {
		readBuffer = DefaultReadBufferBytes
	}
	if readBuffer < 2 {
		return fmt.Errorf("%s read buffer must contain at least two bytes", label)
	}
	maxAudio := options.MaxAudioBytes
	if maxAudio == 0 {
		maxAudio = DefaultMaxAudioBytes
	}
	if maxAudio < 2 {
		return fmt.Errorf("%s maximum audio size must contain at least one PCM16 sample", label)
	}
	resampler, err := pcm.NewResampler(options.SourceRateHz, options.OutputRateHz)
	if err != nil {
		return fmt.Errorf("configure %s resampler: %w", label, err)
	}

	buffer := make([]byte, readBuffer)
	var oddByte []byte
	var heldSample []byte
	var sourceBytes, outputBytes int64
	var sampleOffset, sequence uint64

	emit := func(audio []byte, final bool) error {
		if len(audio) == 0 || len(audio)%2 != 0 {
			return fmt.Errorf("%s produced an invalid PCM16 chunk", label)
		}
		if int64(len(audio)) > maxAudio-outputBytes {
			return fmt.Errorf("%s output exceeds %d bytes", label, maxAudio)
		}
		sequence++
		chunk := v1.SpeechChunk{
			ChunkID: fmt.Sprintf("%s-%06d", candidateID, sequence), CandidateID: candidateID,
			SampleOffset: sampleOffset, SampleRateHz: options.OutputRateHz,
			PCM16LE: slices.Clone(audio), Final: final,
		}
		if err := consume(chunk); err != nil {
			return fmt.Errorf("consume %s chunk: %w", label, err)
		}
		outputBytes += int64(len(audio))
		sampleOffset += uint64(len(audio) / 2)
		return nil
	}
	offer := func(audio []byte) error {
		if len(audio) == 0 {
			return nil
		}
		combined := make([]byte, 0, len(heldSample)+len(audio))
		combined = append(combined, heldSample...)
		combined = append(combined, audio...)
		if len(combined) <= 2 {
			heldSample = combined
			return nil
		}
		cut := len(combined) - 2
		if err := emit(combined[:cut], false); err != nil {
			return err
		}
		heldSample = slices.Clone(combined[cut:])
		return nil
	}
	feed := func(input []byte) error {
		if len(input) == 0 {
			return nil
		}
		if int64(len(input)) > maxAudio-sourceBytes {
			return fmt.Errorf("%s response exceeds %d bytes", label, maxAudio)
		}
		sourceBytes += int64(len(input))
		if len(oddByte) != 0 {
			input = append(append([]byte(nil), oddByte...), input...)
			oddByte = nil
		}
		if len(input)%2 != 0 {
			oddByte = slices.Clone(input[len(input)-1:])
			input = input[:len(input)-1]
		}
		if len(input) == 0 {
			return nil
		}
		converted, err := resampler.Push(input)
		if err != nil {
			return fmt.Errorf("resample %s stream: %w", label, err)
		}
		return offer(converted)
	}

	for {
		read, readErr := body.Read(buffer)
		if read > 0 {
			if err := feed(buffer[:read]); err != nil {
				return err
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return fmt.Errorf("read %s stream: %w", label, readErr)
			}
			break
		}
	}
	if len(oddByte) != 0 {
		return fmt.Errorf("%s stream ended with a partial PCM16 sample", label)
	}
	if sourceBytes == 0 {
		return fmt.Errorf("%s returned no audio", label)
	}
	terminal, err := resampler.Finalize()
	if err != nil {
		return fmt.Errorf("finalize %s resampler: %w", label, err)
	}
	if len(terminal) != 0 {
		heldSample = append(heldSample, terminal...)
	}
	if len(heldSample) == 0 {
		return fmt.Errorf("%s returned no complete output sample", label)
	}
	return emit(heldSample, true)
}
