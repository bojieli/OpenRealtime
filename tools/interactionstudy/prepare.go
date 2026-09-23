package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/speechsocket"
	"github.com/bojieli/OpenRealtime/bench/capability"
	"github.com/bojieli/OpenRealtime/internal/audio"
)

type recordedPhrase struct {
	Group    string        `json:"group"`
	Text     string        `json:"text"`
	PCM      []byte        `json:"-"`
	Rate     uint32        `json:"sample_rate"`
	Duration time.Duration `json:"duration_ns"`
	SHA256   string        `json:"pcm_sha256"`
}

func synthesizePhrase(ctx context.Context, config speechsocket.Config, group, text string) (recordedPhrase, error) {
	result := recordedPhrase{Group: group, Text: text}
	speech, err := speechsocket.Open(ctx, config)
	if err != nil {
		return result, err
	}
	defer speech.Close()
	result.Rate = speech.SampleRate()
	if err = speech.Append(ctx, text); err != nil {
		return result, err
	}
	if err = speech.End(ctx); err != nil {
		return result, err
	}
	for {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case chunk, ok := <-speech.Audio():
			if !ok {
				if err = speech.Err(); err != nil {
					return result, err
				}
				if len(result.PCM) == 0 || len(result.PCM)%2 != 0 {
					return result, fmt.Errorf("empty or invalid PCM")
				}
				result.Duration = time.Duration(len(result.PCM)/2) * time.Second / time.Duration(result.Rate)
				hash := sha256.Sum256(result.PCM)
				result.SHA256 = hex.EncodeToString(hash[:])
				return result, nil
			}
			result.PCM = append(result.PCM, chunk.PCM16LE...)
		}
	}
}

// recordEvents preserves authored inter-phrase gaps, but derives phrase duration
// from actual audio. Word boundaries are uniform estimates, explicitly declared
// in the manifest; annotation delivery waits for the complete phrase.
func recordEvents(ctx context.Context, config speechsocket.Config, events []capability.Event, shift time.Duration) ([]capability.Event, []recordedPhrase, error) {
	groups := []string{}
	byGroup := map[string][]capability.Event{}
	for _, event := range events {
		if _, ok := byGroup[event.Group]; !ok {
			groups = append(groups, event.Group)
		}
		byGroup[event.Group] = append(byGroup[event.Group], event)
	}
	var result []capability.Event
	var recordings []recordedPhrase
	for _, group := range groups {
		original := byGroup[group]
		var words []string
		var wordCount int
		from, to := original[0].SourceStart, original[0].SourceEnd
		for _, event := range original {
			if event.SourceStart < from {
				from = event.SourceStart
			}
			if event.SourceEnd > to {
				to = event.SourceEnd
			}
			if event.Kind == capability.KindWord {
				words = append(words, event.Text)
				wordCount++
			}
		}
		if wordCount == 0 {
			return nil, nil, fmt.Errorf("group %s has no transcript", group)
		}
		recording, err := synthesizePhrase(ctx, config, group, strings.Join(words, " "))
		if err != nil {
			return nil, nil, err
		}
		start := from + shift
		wordIndex := 0
		for _, event := range original {
			switch event.Kind {
			case capability.KindWord:
				event.SourceStart = start + recording.Duration*time.Duration(wordIndex)/time.Duration(wordCount)
				wordIndex++
				event.SourceEnd = start + recording.Duration*time.Duration(wordIndex)/time.Duration(wordCount)
				// Uniform boundaries do not prove a word has finished in the
				// waveform. Until alignment exists, withhold all words until
				// the complete phrase has played, plus declared recognition delay.
				event.AvailableAt = start + recording.Duration + 480*time.Millisecond
			case capability.KindSound:
				event.SourceStart = start
				event.SourceEnd = start + recording.Duration
				event.AvailableAt = start
			case capability.KindCue:
				event.SourceStart = start
				event.SourceEnd = start + recording.Duration
				event.AvailableAt = event.SourceEnd + 200*time.Millisecond
			default:
				return nil, nil, fmt.Errorf("unsupported recording event %s", event.Kind)
			}
			result = append(result, event)
		}
		recordings = append(recordings, recording)
		shift += recording.Duration - (to - from)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].SourceStart < result[j].SourceStart })
	return result, recordings, nil
}

func preparePair(ctx context.Context, dir string, pair capability.Pair, config speechsocket.Config) error {
	if err := os.Mkdir(dir, 0755); err != nil {
		return err
	}
	originalEnd := pair.PrefixEnd()
	prefix, recordings, err := recordEvents(ctx, config, pair.Prefix, 0)
	if err != nil {
		return err
	}
	pair.Prefix = prefix
	metadata := map[string]any{
		"input_regime":   "synthetic-annotated-diagnostic",
		"word_alignment": "uniform-within-phrase-estimate",
		"word_release":   "phrase-end-plus-480ms; conservative until forced alignment",
		"acoustic_cues":  "authored labels; not validated against synthesized prosody",
		"voice":          config.Voice, "shared_prefix_recordings": recordings,
	}
	for index := range pair.Variants {
		variant := &pair.Variants[index]
		branch, recorded, err := recordEvents(ctx, config, variant.Events, pair.PrefixEnd()-originalEnd)
		if err != nil {
			return err
		}
		variant.Events = branch
		allEvents := append(append([]capability.Event{}, prefix...), branch...)
		allRecordings := append(append([]recordedPhrase{}, recordings...), recorded...)
		var pcm []byte
		var rate uint32
		for _, record := range allRecordings {
			if rate != 0 && rate != record.Rate {
				return fmt.Errorf("inconsistent audio rate")
			}
			rate = record.Rate
			var start time.Duration
			for _, event := range allEvents {
				if event.Group == record.Group {
					start = event.SourceStart
					break
				}
			}
			offset := int(start*time.Duration(rate)/time.Second) * 2
			if len(pcm) < offset+len(record.PCM) {
				pcm = append(pcm, make([]byte, offset+len(record.PCM)-len(pcm))...)
			}
			copy(pcm[offset:], record.PCM)
		}
		wav, err := audio.EncodeWAVMono16(pcm, rate)
		if err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(dir, variant.ID+".input.wav"), wav, 0644); err != nil {
			return err
		}
		metadata[variant.ID] = recorded
	}
	if err = pair.Validate(); err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(dir, "pair.json"), pair); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, "recordings.json"), metadata)
}
