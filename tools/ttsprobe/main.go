// Command ttsprobe checks a speech-socket service against the incremental
// synthesis acceptance test of the streaming plan, through the Go client the
// runtime uses (adapters/speechsocket).
//
// For each sentence it opens a context, appends the first half, waits, and
// records whether audio for the unfinished prefix arrived before the rest was
// sent (held-back suffix). It then appends the rest, ends the context, and
// records time to first audio, total audio, and the real-time factor. Finally
// it cancels a context mid-audio and counts audio delivered after the cancel,
// which must be zero.
//
//	go run ./tools/ttsprobe -url ws://127.0.0.1:9125/v1/tts/stream -out result.json
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/speechsocket"
)

var sentences = []string{
	"The weather today is sunny with a light breeze, perfect for a walk in the park.",
	"If you want to save money, start by tracking every expense for one month.",
	"Our meeting is scheduled for three o'clock, so please bring the quarterly report.",
	"Indoor plants need bright indirect light and water only when the soil feels dry.",
}

type sentenceResult struct {
	Text                  string  `json:"text"`
	PrefixAudioBeforeRest bool    `json:"prefix_audio_before_rest"`
	FirstAudioMS          float64 `json:"first_audio_ms"`
	AudioMS               float64 `json:"audio_ms"`
	WallMS                float64 `json:"wall_ms"`
	Error                 string  `json:"error,omitempty"`
}

func main() {
	target := flag.String("url", "ws://127.0.0.1:9120/v1/tts/stream", "speech-socket URL")
	voice := flag.String("voice", "default", "voice")
	hold := flag.Duration("hold", 1500*time.Millisecond, "how long the suffix is held back")
	out := flag.String("out", "", "result JSON path")
	flag.Parse()
	config := speechsocket.Config{URL: *target, Voice: *voice}
	report := map[string]any{"url": *target, "hold_ms": hold.Milliseconds()}
	var results []sentenceResult
	var declared speechsocket.Capabilities
	for _, text := range sentences {
		result, capabilities := probe(config, text, *hold)
		declared = capabilities
		results = append(results, result)
		fmt.Fprintf(os.Stderr, "prefix-audio-before-rest=%v first=%.0fms audio=%.0fms wall=%.0fms %s\n",
			result.PrefixAudioBeforeRest, result.FirstAudioMS, result.AudioMS, result.WallMS, result.Error)
	}
	report["declared"] = declared
	report["sentences"] = results
	report["cancel"] = cancelProbe(config)
	encoded, _ := json.MarshalIndent(report, "", "  ")
	if *out != "" {
		if err := os.WriteFile(*out, encoded, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	fmt.Println(string(encoded))
}

func probe(config speechsocket.Config, text string, hold time.Duration) (sentenceResult, speechsocket.Capabilities) {
	result := sentenceResult{Text: text}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	speech, err := speechsocket.Open(ctx, config)
	if err != nil {
		result.Error = err.Error()
		return result, speechsocket.Capabilities{}
	}
	defer speech.Close()
	words := strings.Fields(text)
	prefix := strings.Join(words[:len(words)/2], " ") + " "
	suffix := strings.Join(words[len(words)/2:], " ")
	started := time.Now()
	samples := 0
	first := time.Duration(0)
	if err := speech.Append(ctx, prefix); err != nil {
		result.Error = err.Error()
		return result, speech.Capabilities()
	}
	held := time.After(hold)
	waiting := true
	for waiting {
		select {
		case chunk, ok := <-speech.Audio():
			if !ok {
				result.Error = fmt.Sprint(speech.Err())
				return result, speech.Capabilities()
			}
			if first == 0 {
				first = time.Since(started)
				result.PrefixAudioBeforeRest = true
			}
			samples += len(chunk.PCM16LE) / 2
		case <-held:
			waiting = false
		}
	}
	if err := speech.Append(ctx, suffix); err != nil {
		result.Error = err.Error()
		return result, speech.Capabilities()
	}
	if err := speech.End(ctx); err != nil {
		result.Error = err.Error()
		return result, speech.Capabilities()
	}
	for chunk := range speech.Audio() {
		if first == 0 {
			first = time.Since(started)
		}
		samples += len(chunk.PCM16LE) / 2
	}
	if err := speech.Err(); err != nil {
		result.Error = err.Error()
	}
	result.FirstAudioMS = float64(first.Microseconds()) / 1000
	result.AudioMS = float64(samples) * 1000 / float64(speech.SampleRate())
	result.WallMS = float64(time.Since(started).Microseconds()) / 1000
	return result, speech.Capabilities()
}

func cancelProbe(config speechsocket.Config) map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	speech, err := speechsocket.Open(ctx, config)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer speech.Close()
	long := strings.Repeat("This sentence keeps going so that the cancel lands in the middle of the audio. ", 4)
	if err := speech.Append(ctx, long); err != nil {
		return map[string]any{"error": err.Error()}
	}
	if err := speech.End(ctx); err != nil {
		return map[string]any{"error": err.Error()}
	}
	received := 0
	for chunk := range speech.Audio() {
		received += len(chunk.PCM16LE) / 2
		if float64(received)/float64(speech.SampleRate()) > 1.0 {
			break
		}
	}
	cancelled := time.Now()
	_ = speech.Cancel(context.Background())
	after := 0
	for chunk := range speech.Audio() {
		after += len(chunk.PCM16LE) / 2
	}
	return map[string]any{
		"audio_before_cancel_ms": float64(received) * 1000 / float64(speech.SampleRate()),
		"audio_after_cancel_ms":  float64(after) * 1000 / float64(speech.SampleRate()),
		"closed_ms":              float64(time.Since(cancelled).Microseconds()) / 1000,
		"cancelled":              errors.Is(speech.Err(), context.Canceled),
	}
}
