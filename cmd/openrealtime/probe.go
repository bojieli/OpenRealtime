package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/internal/audio"
	"github.com/bojieli/OpenRealtime/pcm"
	"github.com/bojieli/OpenRealtime/realtimeclient"
)

// runProbe drives a running server through one turn and reports what happened.
//
// It exists because "is my install working" should be answerable without
// writing a client. It speaks nothing but the protocol, which also makes it
// the smallest honest demonstration that the protocol is sufficient.
func runProbe(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime probe", flag.ContinueOnError)
	var (
		url       string
		transport string
		tokenEnv  string
		wavPath   string
		realTime  bool
		timeout   time.Duration
		silenceMS int
	)
	flags.StringVar(&url, "url", "ws://127.0.0.1:8765/v1/realtime", "server endpoint")
	flags.StringVar(&transport, "transport", "websocket", "how to connect: websocket or webrtc")
	flags.StringVar(&tokenEnv, "token-env", "OPENREALTIME_TOKEN", "environment variable holding the bearer token")
	flags.StringVar(&wavPath, "audio", "", "16-bit PCM WAV file to speak; a generated tone is used when empty")
	flags.BoolVar(&realTime, "realtime", true, "stream at the audio's own rate rather than as fast as possible")
	flags.DurationVar(&timeout, "timeout", 90*time.Second, "how long to wait for the turn to complete")
	flags.IntVar(&silenceMS, "trailing-silence-ms", 1200, "silence appended so server endpointing fires")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	samples, err := probeAudio(wavPath, silenceMS)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if strings.EqualFold(strings.TrimSpace(transport), "webrtc") {
		result, err := probeWebRTC(ctx, url, samples, realTime, output)
		if err != nil {
			return err
		}
		return reportProbe(result, output)
	}

	client, err := realtimeclient.Dial(ctx, realtimeclient.Config{
		URL: url, Token: os.Getenv(tokenEnv),
	})
	if err != nil {
		return err
	}
	defer client.Close()

	results := make(chan probeResult, 1)
	go func() { results <- collectProbe(ctx, client, output) }()

	if err := client.Send(ctx, map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime",
			"audio": map[string]any{
				"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
				"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
			},
		},
	}); err != nil {
		return err
	}

	const frameSamples = 2400 // 100 ms at 24 kHz
	started := time.Now()
	for offset := 0; offset < len(samples); offset += frameSamples {
		end := min(offset+frameSamples, len(samples))
		if err := client.Send(ctx, map[string]any{
			"type":  "input_audio_buffer.append",
			"audio": base64.StdEncoding.EncodeToString(encodePCM16(samples[offset:end])),
		}); err != nil {
			return err
		}
		if realTime {
			elapsed := time.Duration(end) * time.Second / 24_000
			if wait := elapsed - time.Since(started); wait > 0 {
				time.Sleep(wait)
			}
		}
	}

	select {
	case result := <-results:
		return reportProbe(result, output)
	case <-ctx.Done():
		return errors.New("the turn did not complete before the timeout")
	}
}

// appendTranscript joins successive utterances into what was actually said.
func appendTranscript(existing, next string) string {
	next = strings.TrimSpace(next)
	if next == "" {
		return existing
	}
	if existing == "" {
		return next
	}
	return existing + " " + next
}

// reportProbe prints what happened and decides whether it counts as working.
func reportProbe(result probeResult, output io.Writer) error {
	fmt.Fprintln(output)
	fmt.Fprintf(output, "transcript : %s\n", result.transcript)
	fmt.Fprintf(output, "spoken     : %s\n", strings.Join(result.spoken, " | "))
	fmt.Fprintf(output, "audio      : %.2f s in %d frames\n", result.audioSeconds, result.audioFrames)
	fmt.Fprintf(output, "first audio: %s after the endpoint\n", result.firstAudio.Round(time.Millisecond))
	if len(result.toolCalls) > 0 {
		fmt.Fprintf(output, "tool calls : %s\n", strings.Join(result.toolCalls, ", "))
	}
	if result.err != nil {
		return result.err
	}
	if result.transcript == "" {
		return errors.New("the server produced no transcript")
	}
	if len(result.spoken) == 0 {
		return errors.New("the server produced no speech")
	}
	if result.audioFrames == 0 {
		return errors.New("the server produced no audio")
	}
	return nil
}

type probeResult struct {
	transcript   string
	spoken       []string
	toolCalls    []string
	audioFrames  int
	audioSeconds float64
	firstAudio   time.Duration
	err          error
}

func collectProbe(ctx context.Context, client *realtimeclient.Client, output io.Writer) probeResult {
	var result probeResult
	var endpoint time.Time
	responses := 0
	for {
		select {
		case <-ctx.Done():
			return result
		case event, open := <-client.Events():
			if !open {
				result.err = client.Err()
				return result
			}
			switch event.Type {
			case "input_audio_buffer.speech_stopped":
				endpoint = time.Now()
				fmt.Fprint(output, ".")
			case "conversation.item.input_audio_transcription.completed":
				var decoded struct {
					Transcript string `json:"transcript"`
				}
				_ = event.Decode(&decoded)
				// A recording usually contains several utterances. Reporting
				// only the last one makes a working session look like a broken
				// recogniser.
				result.transcript = appendTranscript(result.transcript, decoded.Transcript)
			case "response.output_audio_transcript.delta":
				var decoded struct {
					Delta string `json:"delta"`
				}
				_ = event.Decode(&decoded)
				if strings.TrimSpace(decoded.Delta) != "" {
					result.spoken = append(result.spoken, decoded.Delta)
				}
			case "response.output_audio.delta":
				var decoded struct {
					Delta string `json:"delta"`
				}
				_ = event.Decode(&decoded)
				payload, err := base64.StdEncoding.DecodeString(decoded.Delta)
				if err == nil {
					result.audioFrames++
					result.audioSeconds += float64(len(payload)/2) / 24_000
					if result.firstAudio == 0 && !endpoint.IsZero() {
						result.firstAudio = time.Since(endpoint)
					}
				}
			case "response.function_call_arguments.done":
				var decoded struct {
					Name string `json:"name"`
				}
				_ = event.Decode(&decoded)
				result.toolCalls = append(result.toolCalls, decoded.Name)
			case "response.done":
				responses++
				// One response carries the fast answer; a second carries the
				// voiced background answer. Two is the complete turn.
				if responses >= 2 && result.audioFrames > 0 {
					return result
				}
			case "error":
				var decoded struct {
					Error struct {
						Message string `json:"message"`
					} `json:"error"`
				}
				_ = event.Decode(&decoded)
				result.err = errors.New(decoded.Error.Message)
				return result
			}
		}
	}
}

func probeAudio(path string, trailingSilenceMS int) ([]int16, error) {
	var samples []int16
	if strings.TrimSpace(path) == "" {
		// A tone is loud enough to open the acoustic gate, which is all a
		// connectivity check needs.
		samples = make([]int16, 24_000)
		for index := range samples {
			samples[index] = int16(6000 * math.Sin(float64(index)*440*2*math.Pi/24_000))
		}
	} else {
		decoded, rate, err := readWAV(path)
		if err != nil {
			return nil, err
		}
		samples = decoded
		if rate != 24_000 {
			resampler, err := pcm.NewResampler(rate, 24_000)
			if err != nil {
				return nil, err
			}
			converted, err := resampler.Push(encodePCM16(samples))
			if err != nil {
				return nil, err
			}
			terminal, err := resampler.Finalize()
			if err != nil {
				return nil, err
			}
			samples = decodePCM16(append(converted, terminal...))
		}
	}
	return append(samples, make([]int16, trailingSilenceMS*24)...), nil
}

func readWAV(path string) ([]int16, uint32, error) {
	decoded, err := audio.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	return decodePCM16(decoded.PCM16LE), decoded.Metadata.SampleRateHz, nil
}

func encodePCM16(samples []int16) []byte {
	encoded := make([]byte, len(samples)*2)
	for index, sample := range samples {
		binary.LittleEndian.PutUint16(encoded[index*2:], uint16(sample))
	}
	return encoded
}

func decodePCM16(raw []byte) []int16 {
	samples := make([]int16, len(raw)/2)
	for index := range samples {
		samples[index] = int16(binary.LittleEndian.Uint16(raw[index*2:]))
	}
	return samples
}
