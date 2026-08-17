// Package replay converts deterministic PCM fixtures to OpenAI Realtime client events.
package replay

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/bojieli/OpenRealtime/internal/audio"
	"github.com/bojieli/OpenRealtime/internal/clock"
	openaiwire "github.com/bojieli/OpenRealtime/protocol/openai"
	"github.com/bojieli/OpenRealtime/trace"
)

type Options struct {
	SessionID       string
	FrameDurationMS uint32
	Events          io.Writer
	Trace           io.Writer
}

type Summary struct {
	InputFrameCount  uint64 `json:"input_frame_count"`
	InputSampleCount uint64 `json:"input_sample_count"`
	EventCount       uint64 `json:"event_count"`
	DurationNS       uint64 `json:"duration_ns"`
}

type appendEvent struct {
	EventID string               `json:"event_id"`
	Type    openaiwire.EventType `json:"type"`
	Audio   string               `json:"audio"`
}

type commitEvent struct {
	EventID string               `json:"event_id"`
	Type    openaiwire.EventType `json:"type"`
}

func WAV(path string, options Options) (Summary, error) {
	if options.SessionID == "" {
		return Summary{}, errors.New("session ID must not be empty")
	}
	if options.FrameDurationMS == 0 {
		return Summary{}, errors.New("frame duration must be positive")
	}
	if options.Events == nil || options.Trace == nil {
		return Summary{}, errors.New("event and trace outputs are required")
	}
	reader, err := audio.OpenPCM16Mono(path)
	if err != nil {
		return Summary{}, err
	}
	defer reader.Close()
	metadata := reader.Metadata()
	samplesPerFrame := uint64(metadata.SampleRateHz) * uint64(options.FrameDurationMS) / 1_000
	if samplesPerFrame == 0 {
		return Summary{}, errors.New("frame duration is shorter than one sample")
	}
	frameBuffer := make([]byte, samplesPerFrame*uint64(audio.PCM16BytesPerSample))
	eventOutput := bufio.NewWriter(options.Events)
	traceOutput := trace.NewWriter(options.Trace)
	validator := openaiwire.NewValidator()
	virtualClock := clock.NewVirtual(0)

	var summary Summary
	var previousTraceID string
	for {
		bytesRead, readErr := reader.ReadFrame(frameBuffer)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return Summary{}, readErr
		}
		sampleCount := uint64(bytesRead) / uint64(audio.PCM16BytesPerSample)
		timestampNS := summary.InputSampleCount * 1_000_000_000 / uint64(metadata.SampleRateHz)
		if err := virtualClock.AdvanceToNS(timestampNS); err != nil {
			return Summary{}, err
		}
		wire := appendEvent{
			EventID: fmt.Sprintf("event_%08d", summary.EventCount),
			Type:    openaiwire.EventInputAudioBufferAppend,
			Audio:   base64.StdEncoding.EncodeToString(frameBuffer[:bytesRead]),
		}
		message, err := encodeAndValidate(wire, validator)
		if err != nil {
			return Summary{}, err
		}
		traceID, err := writeOutputs(
			eventOutput,
			traceOutput,
			options.SessionID,
			summary.EventCount,
			virtualClock.NowNS(),
			previousTraceID,
			message,
		)
		if err != nil {
			return Summary{}, err
		}
		previousTraceID = traceID
		summary.InputFrameCount++
		summary.InputSampleCount += sampleCount
		summary.EventCount++
	}

	summary.DurationNS = summary.InputSampleCount * 1_000_000_000 / uint64(metadata.SampleRateHz)
	if err := virtualClock.AdvanceToNS(summary.DurationNS); err != nil {
		return Summary{}, err
	}
	message, err := encodeAndValidate(commitEvent{
		EventID: fmt.Sprintf("event_%08d", summary.EventCount),
		Type:    openaiwire.EventInputAudioBufferCommit,
	}, validator)
	if err != nil {
		return Summary{}, err
	}
	if _, err := writeOutputs(
		eventOutput,
		traceOutput,
		options.SessionID,
		summary.EventCount,
		virtualClock.NowNS(),
		previousTraceID,
		message,
	); err != nil {
		return Summary{}, err
	}
	summary.EventCount++
	if err := eventOutput.Flush(); err != nil {
		return Summary{}, fmt.Errorf("flush event output: %w", err)
	}
	if err := traceOutput.Flush(); err != nil {
		return Summary{}, fmt.Errorf("flush trace output: %w", err)
	}
	return summary, nil
}

func encodeAndValidate(value any, validator *openaiwire.Validator) (openaiwire.Message, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return openaiwire.Message{}, fmt.Errorf("encode OpenAI event: %w", err)
	}
	message, err := openaiwire.Decode(data)
	if err != nil {
		return openaiwire.Message{}, err
	}
	if err := validator.Validate(openaiwire.ProfileRealtime, openaiwire.DirectionClient, message); err != nil {
		return openaiwire.Message{}, err
	}
	return message, nil
}

func writeOutputs(
	events *bufio.Writer,
	traces *trace.Writer,
	sessionID string,
	sequence uint64,
	monotonicNS uint64,
	previousTraceID string,
	message openaiwire.Message,
) (string, error) {
	raw := message.Raw()
	if _, err := events.Write(raw); err != nil {
		return "", fmt.Errorf("write OpenAI event: %w", err)
	}
	if err := events.WriteByte('\n'); err != nil {
		return "", fmt.Errorf("write OpenAI event newline: %w", err)
	}
	traceID := fmt.Sprintf("trace_%08d", sequence)
	parents := []string{}
	if previousTraceID != "" {
		parents = append(parents, previousTraceID)
	}
	if err := traces.Write(trace.Record{
		SchemaVersion:   trace.SchemaVersion,
		TraceID:         traceID,
		SessionID:       sessionID,
		Sequence:        sequence,
		MonotonicNS:     monotonicNS,
		Direction:       openaiwire.DirectionClient,
		Profile:         openaiwire.ProfileRealtime,
		CausalParentIDs: parents,
		Message:         raw,
	}); err != nil {
		return "", err
	}
	return traceID, nil
}
