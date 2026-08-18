// Package pcm contains stateful, provider-neutral PCM stream utilities.
package pcm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Resampler incrementally converts mono signed 16-bit little-endian PCM with
// linear interpolation. A Resampler is stateful and must be used by one stream
// at a time. Push never invents a right-hand sample at a frame boundary;
// Finalize supplies the terminal hold sample and fixes the exact output length.
// This makes output independent of how input is partitioned into frames.
type Resampler struct {
	inputRate  uint32
	outputRate uint32

	buffer      []int16
	bufferStart uint64
	totalInput  uint64
	nextOutput  uint64
	finalized   bool
}

// NewResampler creates a mono PCM16 stream resampler.
func NewResampler(inputRate, outputRate uint32) (*Resampler, error) {
	if inputRate == 0 || outputRate == 0 {
		return nil, errors.New("PCM resampler rates must be positive")
	}
	return &Resampler{inputRate: inputRate, outputRate: outputRate}, nil
}

// InputRate returns the immutable source sample rate.
func (resampler *Resampler) InputRate() uint32 { return resampler.inputRate }

// OutputRate returns the immutable destination sample rate.
func (resampler *Resampler) OutputRate() uint32 { return resampler.outputRate }

// Push consumes another sample-aligned PCM16LE fragment and returns all output
// samples that no longer depend on future input.
func (resampler *Resampler) Push(input []byte) ([]byte, error) {
	if resampler.finalized {
		return nil, errors.New("PCM resampler is finalized")
	}
	if len(input)%2 != 0 {
		return nil, errors.New("PCM16 input length must be even")
	}
	if len(input) == 0 {
		return nil, nil
	}
	count := uint64(len(input) / 2)
	if count > math.MaxUint64-resampler.totalInput {
		return nil, errors.New("PCM input sample count overflows")
	}
	for offset := 0; offset < len(input); offset += 2 {
		resampler.buffer = append(resampler.buffer, int16(binary.LittleEndian.Uint16(input[offset:offset+2])))
	}
	resampler.totalInput += count
	return resampler.produce(false)
}

// Finalize returns the terminal samples. It is valid exactly once.
func (resampler *Resampler) Finalize() ([]byte, error) {
	if resampler.finalized {
		return nil, errors.New("PCM resampler is already finalized")
	}
	resampler.finalized = true
	return resampler.produce(true)
}

func (resampler *Resampler) produce(final bool) ([]byte, error) {
	if resampler.totalInput == 0 {
		return nil, nil
	}
	targetCount := uint64(0)
	if final {
		if resampler.totalInput > (math.MaxUint64-uint64(resampler.inputRate)/2)/uint64(resampler.outputRate) {
			return nil, errors.New("PCM output sample count overflows")
		}
		targetCount = (resampler.totalInput*uint64(resampler.outputRate) + uint64(resampler.inputRate)/2) / uint64(resampler.inputRate)
	}

	output := make([]byte, 0)
	for {
		if final && resampler.nextOutput >= targetCount {
			break
		}
		if resampler.nextOutput > math.MaxUint64/uint64(resampler.inputRate) {
			return nil, errors.New("PCM resampling position overflows")
		}
		position := resampler.nextOutput * uint64(resampler.inputRate)
		left := position / uint64(resampler.outputRate)
		fraction := position % uint64(resampler.outputRate)
		if left >= resampler.totalInput {
			break
		}
		right := left
		if fraction != 0 {
			if left+1 >= resampler.totalInput {
				if !final {
					break
				}
			} else {
				right = left + 1
			}
		}
		leftSample, err := resampler.sample(left)
		if err != nil {
			return nil, err
		}
		rightSample, err := resampler.sample(right)
		if err != nil {
			return nil, err
		}
		value := (int64(leftSample)*(int64(resampler.outputRate)-int64(fraction)) +
			int64(rightSample)*int64(fraction)) / int64(resampler.outputRate)
		var encoded [2]byte
		binary.LittleEndian.PutUint16(encoded[:], uint16(int16(value)))
		output = append(output, encoded[:]...)
		resampler.nextOutput++
	}

	if final {
		resampler.buffer = nil
		resampler.bufferStart = resampler.totalInput
		return output, nil
	}
	resampler.discardConsumedPrefix()
	return output, nil
}

func (resampler *Resampler) sample(index uint64) (int16, error) {
	if index < resampler.bufferStart || index-resampler.bufferStart >= uint64(len(resampler.buffer)) {
		return 0, fmt.Errorf("PCM resampler lost source sample %d", index)
	}
	return resampler.buffer[index-resampler.bufferStart], nil
}

func (resampler *Resampler) discardConsumedPrefix() {
	if len(resampler.buffer) == 0 || resampler.nextOutput > math.MaxUint64/uint64(resampler.inputRate) {
		return
	}
	nextLeft := resampler.nextOutput * uint64(resampler.inputRate) / uint64(resampler.outputRate)
	if nextLeft <= resampler.bufferStart {
		return
	}
	drop := min(nextLeft-resampler.bufferStart, uint64(len(resampler.buffer)))
	resampler.buffer = resampler.buffer[drop:]
	resampler.bufferStart += drop
	if len(resampler.buffer) == 0 {
		resampler.buffer = nil
	}
}
