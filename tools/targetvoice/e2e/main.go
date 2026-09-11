// Command e2e runs a diagnostic through the real public Realtime pipeline.
// It is not an attested benchmark publication or a human listening assessment.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/scenario"
)

func writePCM(path string, samples []int16) error {
	b := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(s))
	}
	return os.WriteFile(path, b, 0600)
}

func main() {
	endpoint := flag.String("url", "ws://127.0.0.1:18766/v1/realtime", "real pipeline endpoint")
	out := flag.String("out", "", "new directory for diagnostic evidence")
	mode := flag.String("mode", "competitor", "competitor, competitor-idle, user-stop, or quiet")
	repeat := flag.Int("repeat", 1, "sequential attempts")
	flag.Parse()
	if *out == "" || (*mode != "competitor" && *mode != "competitor-idle" && *mode != "user-stop" && *mode != "quiet") || *repeat < 1 {
		panic("out, supported mode, and positive repeat required")
	}
	if err := os.Mkdir(*out, 0700); err != nil {
		panic(err)
	}
	voice := scenario.SpeechVoice{Endpoint: "http://127.0.0.1:8123/v1/tts", Model: "fishaudio/fish-speech-1.5", Default: "user", Voices: map[string]string{"user": "user", "other": "other"}}
	initial, err := voice.Speak(context.Background(), "user", "Please explain five useful ways to organize a workday. Give a detailed explanation of each one.")
	if err != nil {
		panic(err)
	}
	if len(initial) < 24000*3 || len(initial) > 24000*10 {
		panic(fmt.Sprintf("initial speech length unsuitable for clean enrollment: %.2fs", float64(len(initial))/24000))
	}
	var cue []int16
	if *mode != "quiet" {
		speaker, text := "other", "Stop talking. What is the capital of France?"
		if *mode == "user-stop" {
			speaker, text = "user", "Stop talking."
		}
		cue, err = voice.Speak(context.Background(), speaker, text)
		if err != nil {
			panic(err)
		}
	}
	for i := 1; i <= *repeat; i++ {
		prefix := filepath.Join(*out, fmt.Sprintf("trial-%02d", i))
		samples := make([]int16, 40*24000)
		if *mode == "competitor-idle" {
			samples = make([]int16, 23*24000)
		}
		copy(samples, initial)
		if *mode == "competitor-idle" {
			copy(samples[12000*24:], cue)
		}
		config := bench.SessionConfig{Endpoint: *endpoint, Realtime: true, Timeout: 90 * time.Second, WorkingTimeout: 20 * time.Second, PostPlaybackQuiet: 2 * time.Second, Quiet: true, CaptureRuntimeEvidence: true,
			Instructions: "You are a helpful voice assistant. Answer the user directly. When asked to explain five ways to organize a workday, describe all five in order, using one or two complete sentences for each, about one hundred words total. Do not ask a follow-up question. Respect a request to stop speaking.",
			CaptureAudio: func(c bench.SessionAudioCapture) error {
				if err := writePCM(prefix+".input.pcm", c.RoomPCM16); err != nil {
					return err
				}
				end := 0
				for _, chunk := range c.Agent {
					end = max(end, int(chunk.AtMS*24)+len(chunk.PCM16))
				}
				agent := make([]int16, end)
				for _, chunk := range c.Agent {
					copy(agent[int(chunk.AtMS*24):], chunk.PCM16)
				}
				return writePCM(prefix+".agent.pcm", agent)
			},
		}
		if len(cue) > 0 && *mode != "competitor-idle" {
			config.SpeechCues = []bench.SpeechCue{{Name: *mode, PCM16: cue, EarliestMS: 10000, LatestMS: 22000, LookbackMS: 800, MinimumActiveMS: 500, RecentMS: 200}}
		}
		transcript, runErr := bench.PlaySamples(context.Background(), config, samples)
		result := struct {
			Mode       string           `json:"mode"`
			InitialMS  float64          `json:"initial_ms"`
			Error      string           `json:"error,omitempty"`
			Transcript bench.Transcript `json:"transcript"`
		}{Mode: *mode, InitialMS: float64(len(initial)) / 24, Transcript: transcript}
		if runErr != nil {
			result.Error = runErr.Error()
		}
		raw, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(prefix+".json", raw, 0600); err != nil {
			panic(err)
		}
		fmt.Printf("%s trial %d: error=%q cues=%+v\n", *mode, i, result.Error, transcript.SpeechCues)
		if runErr != nil {
			return
		}
	}
}
