package sidecar

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// ConformanceReport records what a sidecar was verified to do.
//
// This suite is the contract. A sidecar that passes it works with the engine
// whatever language it is written in, and one that does not is broken before
// anybody spends a GPU-hour finding out. Every check states what it required,
// so a failure tells the author what to fix rather than that something is
// wrong.
type ConformanceReport struct {
	Suite        string             `json:"suite"`
	Passed       bool               `json:"passed"`
	Version      int                `json:"version"`
	Model        string             `json:"model"`
	OutputRate   int                `json:"output_rate"`
	Capabilities []string           `json:"capabilities"`
	Checks       []ConformanceCheck `json:"checks"`
	Failures     []string           `json:"failures,omitempty"`
}

// ConformanceCheck is one verified property.
type ConformanceCheck struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Passed   bool   `json:"passed"`
	Skipped  bool   `json:"skipped,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// ConformanceOptions bounds the run.
type ConformanceOptions struct {
	Config Config
	// SpeechSeconds is how much audio to send before asking for a turn.
	SpeechSeconds float64
	// TurnTimeout bounds how long one turn may take.
	TurnTimeout time.Duration
}

// RunConformance verifies one sidecar against the contract.
func RunConformance(ctx context.Context, options ConformanceOptions) ConformanceReport {
	if options.SpeechSeconds <= 0 {
		options.SpeechSeconds = 1.5
	}
	if options.TurnTimeout <= 0 {
		options.TurnTimeout = 120 * time.Second
	}
	report := ConformanceReport{Suite: fmt.Sprintf("sidecar-protocol-v%d", Version)}
	record := func(name string, required, passed bool, detail string) {
		report.Checks = append(report.Checks, ConformanceCheck{
			Name: name, Required: required, Passed: passed, Detail: detail,
		})
		if required && !passed {
			report.Failures = append(report.Failures, name+": "+detail)
		}
	}
	skip := func(name, reason string) {
		report.Checks = append(report.Checks, ConformanceCheck{
			Name: name, Skipped: true, Passed: true, Detail: reason,
		})
	}

	const sampleRate = 24_000
	client, err := Dial(ctx, options.Config, Message{
		SampleRate:   sampleRate,
		Instructions: "You are a helpful assistant. Answer briefly.",
		Voice:        "default",
	})
	if err != nil {
		record("handshake completes", true, false, err.Error())
		return report
	}
	defer client.Close()

	ready := client.Ready()
	report.Version, report.Model = ready.Version, ready.Model
	report.OutputRate, report.Capabilities = ready.OutputRate, ready.Capabilities
	record("handshake completes", true, true, fmt.Sprintf("%s at %d Hz", ready.Model, ready.OutputRate))
	record("declares the protocol version it speaks", true, ready.Version == Version,
		fmt.Sprintf("declared %d", ready.Version))
	record("declares its model identity", true, strings.TrimSpace(ready.Model) != "", ready.Model)
	record("declares an output sample rate", true, ready.OutputRate > 0,
		fmt.Sprintf("%d Hz", ready.OutputRate))

	// Speech in, turn out. Everything else is optional; this is the reason a
	// sidecar exists.
	speech := tone(sampleRate, options.SpeechSeconds)
	const frameSamples = 480 // 20 ms
	for offset := 0; offset < len(speech); offset += frameSamples * 2 {
		end := min(offset+frameSamples*2, len(speech))
		if err := client.Audio(speech[offset:end]); err != nil {
			record("accepts input audio", true, false, err.Error())
			return report
		}
	}
	record("accepts input audio", true, true, fmt.Sprintf("%.1f s at %d Hz", options.SpeechSeconds, sampleRate))

	if err := client.Send(Message{Type: TypeRespond}); err != nil {
		record("accepts a respond request", true, false, err.Error())
		return report
	}
	record("accepts a respond request", true, true, "")

	turn := collectTurn(ctx, client, options.TurnTimeout)
	record("produces a turn", true, turn.turnDone,
		fmt.Sprintf("text=%q audio=%d bytes", turn.text, turn.audioBytes))
	record("produces output audio", true, turn.audioBytes > 0,
		fmt.Sprintf("%d bytes", turn.audioBytes))
	record("output audio contains whole PCM16 samples", true, turn.audioBytes%2 == 0,
		fmt.Sprintf("%d bytes", turn.audioBytes))
	record("marks the end of its turn", true, turn.turnDone, "")
	if turn.failure != "" {
		record("reports no error during a normal turn", true, false, turn.failure)
	} else {
		record("reports no error during a normal turn", true, true, "")
	}

	if ready.Has(CapabilityTranscript) {
		record("reports what it heard", true, turn.transcript != "" || turn.sawTranscript,
			fmt.Sprintf("%q", turn.transcript))
	} else {
		skip("reports what it heard", "transcript capability not declared")
	}

	if ready.Has(CapabilityTextInjection) {
		err := client.Send(Message{
			Type: TypeText, Role: "system",
			Text: "The background reasoner has finished: the answer is forty dollars.",
		})
		record("accepts injected text", true, err == nil, fmt.Sprint(err))
	} else {
		skip("accepts injected text", "text injection capability not declared; the binding falls back to hand-off")
	}

	if ready.Has(CapabilityTools) {
		record("declares tool support with a tools capability", true, true, "")
	} else {
		skip("declares tool support with a tools capability", "tools capability not declared")
	}

	// Interrupting mid-turn must be accepted and must not end the session.
	if err := client.Send(Message{Type: TypeRespond}); err == nil {
		time.Sleep(50 * time.Millisecond)
		err = client.Send(Message{Type: TypeInterrupt})
		record("accepts an interrupt", true, err == nil, fmt.Sprint(err))
		drain(client, 2*time.Second)
		record("survives an interrupt", true, client.Err() == nil, fmt.Sprint(client.Err()))
	}

	if err := client.Close(); err != nil {
		record("closes cleanly", true, false, err.Error())
	} else {
		record("closes cleanly", true, true, "")
	}

	report.Passed = len(report.Failures) == 0
	return report
}

type turnResult struct {
	text          string
	transcript    string
	sawTranscript bool
	audioBytes    int
	turnDone      bool
	failure       string
}

func collectTurn(ctx context.Context, client *Client, timeout time.Duration) turnResult {
	var result turnResult
	deadline := time.After(timeout)
	for {
		select {
		case <-ctx.Done():
			return result
		case <-deadline:
			return result
		case message, open := <-client.Frames():
			if !open {
				return result
			}
			switch message.Type {
			case TypeTranscript:
				result.sawTranscript = true
				if message.Final {
					result.transcript = message.Text
				}
			case TypeTextDelta:
				result.text += message.Text
			case TypeTextDone:
				if message.Text != "" {
					result.text = message.Text
				}
			case TypeOutputAudio:
				result.audioBytes += len(message.Payload)
			case TypeTurnDone:
				result.turnDone = true
				return result
			case TypeError:
				result.failure = message.Message()
				if message.Fatal {
					return result
				}
			}
		}
	}
}

func drain(client *Client, window time.Duration) {
	deadline := time.After(window)
	for {
		select {
		case <-deadline:
			return
		case _, open := <-client.Frames():
			if !open {
				return
			}
		}
	}
}

// tone generates speech-shaped audio.
//
// It is a tone rather than a recording so the suite has no fixture dependency:
// what is being verified is that the sidecar accepts audio, produces a turn,
// and honours the framing, not that it transcribes anything in particular.
func tone(sampleRate int, seconds float64) []byte {
	samples := int(float64(sampleRate) * seconds)
	audio := make([]byte, samples*2)
	for index := 0; index < samples; index++ {
		envelope := 0.5 + 0.5*math.Sin(2*math.Pi*3*float64(index)/float64(sampleRate))
		value := int16(6000 * envelope * math.Sin(2*math.Pi*220*float64(index)/float64(sampleRate)))
		binary.LittleEndian.PutUint16(audio[index*2:], uint16(value))
	}
	return audio
}

// MarshalReport renders a report as indented JSON.
func MarshalReport(report ConformanceReport) ([]byte, error) {
	return json.MarshalIndent(report, "", "  ")
}
