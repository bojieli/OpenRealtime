package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/bench/capability"
	"github.com/bojieli/OpenRealtime/internal/audio"
)

// attachInput preserves the full time axis; it never shifts the conversation
// to conceal a missed response window. Controls mute only the feedback group.
func attachInput(dir, prepared string, pair capability.Pair, variant capability.Variant) error {
	original := strings.TrimSuffix(variant.ID, "-nofeedback")
	input, err := audio.ReadFile(filepath.Join(prepared, original+".input.wav"))
	if err != nil {
		return err
	}
	rate := input.Metadata.SampleRateHz
	if variant.ID != original {
		for _, v := range pair.Variants {
			if v.ID != original {
				continue
			}
			for _, event := range v.Events {
				if event.Group != v.Feedback || event.Kind != capability.KindSound {
					continue
				}
				start := int(event.SourceStart*time.Duration(rate)/time.Second) * 2
				end := int(event.SourceEnd*time.Duration(rate)/time.Second) * 2
				if start < 0 || end > len(input.PCM16LE) {
					return fmt.Errorf("feedback interval outside input WAV")
				}
				clear(input.PCM16LE[start:end])
			}
		}
	}
	wav, err := audio.EncodeWAVMono16(input.PCM16LE, rate)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "input.wav"), wav, 0644); err != nil {
		return err
	}
	var output []byte
	decoded, err := audio.ReadFile(filepath.Join(dir, "output.wav"))
	if err == nil {
		if decoded.Metadata.SampleRateHz != rate {
			return fmt.Errorf("input/output sample rates differ")
		}
		output = decoded.PCM16LE
	} else if !errors.Is(err, fs.ErrNotExist) {
		// A branch that correctly stayed silent has no output recording. The
		// read error is wrapped, which os.IsNotExist does not see through.
		return err
	}
	stereo, err := stereoWAV(input.PCM16LE, output, rate)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "conversation.stereo.wav"), stereo, 0644); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, "listening.json"), map[string]any{
		"left": "synthesized user input", "right": "paced assistant recorder",
		"input_regime":   "annotated replay; waveform retained for listening, not ASR input",
		"word_timing":    "uniform within phrase estimate",
		"feedback_muted": variant.ID != original,
		"sample_rate":    rate, "physical_playback": false,
	})
}
func stereoWAV(left, right []byte, rate uint32) ([]byte, error) {
	if rate == 0 || len(left)%2 != 0 || len(right)%2 != 0 {
		return nil, fmt.Errorf("invalid PCM")
	}
	frames := max(len(left), len(right)) / 2
	size := frames * 4
	if uint64(size) > uint64(^uint32(0))-36 {
		return nil, fmt.Errorf("WAV too large")
	}
	result := make([]byte, 44+size)
	copy(result, "RIFF")
	binary.LittleEndian.PutUint32(result[4:], uint32(size+36))
	copy(result[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(result[16:], 16)
	binary.LittleEndian.PutUint16(result[20:], 1)
	binary.LittleEndian.PutUint16(result[22:], 2)
	binary.LittleEndian.PutUint32(result[24:], rate)
	binary.LittleEndian.PutUint32(result[28:], rate*4)
	binary.LittleEndian.PutUint16(result[32:], 4)
	binary.LittleEndian.PutUint16(result[34:], 16)
	copy(result[36:], "data")
	binary.LittleEndian.PutUint32(result[40:], uint32(size))
	for i := 0; i < frames; i++ {
		if i*2 < len(left) {
			copy(result[44+i*4:], left[i*2:i*2+2])
		}
		if i*2 < len(right) {
			copy(result[46+i*4:], right[i*2:i*2+2])
		}
	}
	return result, nil
}
