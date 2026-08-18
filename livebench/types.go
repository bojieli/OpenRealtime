// Package livebench runs reproducible, provider-neutral spoken-dialogue benchmarks.
package livebench

import (
	"context"
	"encoding/json"
	"time"
)

const (
	ResultSchemaVersion       = "1.2.0"
	legacyResultSchemaVersion = "1.1.0"
)

// Audio is little-endian, signed PCM16 mono audio without a container header.
type Audio struct {
	PCM16        []byte `json:"-"`
	SampleRateHz int    `json:"sample_rate_hz"`
}

func (audio Audio) Duration() time.Duration {
	if audio.SampleRateHz <= 0 {
		return 0
	}
	return time.Duration(len(audio.PCM16)/2) * time.Second / time.Duration(audio.SampleRateHz)
}

type Descriptor struct {
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	Transport        string `json:"transport"`
	Architecture     string `json:"architecture"`
	Profile          string `json:"profile"`
	InputSampleRate  int    `json:"input_sample_rate_hz"`
	OutputSampleRate int    `json:"output_sample_rate_hz"`
}

type OutputChunk struct {
	Arrival time.Duration
	PCM16   []byte
	// Flush marks a provider interruption. Audio queued beyond Arrival is
	// discarded before subsequent chunks are aligned, matching live playback.
	Flush bool
}

// WireEvent is a compact, secret-free record. Audio bodies are represented by
// their byte length and SHA-256 rather than copied as base64 into result files.
type WireEvent struct {
	Sequence        uint64          `json:"sequence"`
	MonotonicNS     int64           `json:"monotonic_ns"`
	Direction       string          `json:"direction"`
	Type            string          `json:"type"`
	Payload         json.RawMessage `json:"payload,omitempty"`
	AudioBytes      int             `json:"audio_bytes,omitempty"`
	AudioSHA256     string          `json:"audio_sha256,omitempty"`
	ProviderEventID string          `json:"provider_event_id,omitempty"`
}

type Usage struct {
	InputTokens       int64           `json:"input_tokens,omitempty"`
	OutputTokens      int64           `json:"output_tokens,omitempty"`
	InputAudioTokens  int64           `json:"input_audio_tokens,omitempty"`
	OutputAudioTokens int64           `json:"output_audio_tokens,omitempty"`
	Complete          bool            `json:"complete"`
	SessionsExpected  int             `json:"sessions_expected,omitempty"`
	SessionsObserved  int             `json:"sessions_observed,omitempty"`
	Provider          json.RawMessage `json:"provider,omitempty"`
}

type SessionResult struct {
	Descriptor        Descriptor         `json:"descriptor"`
	StartedAt         time.Time          `json:"started_at"`
	ConnectionSetupMS float64            `json:"connection_setup_ms"`
	ConnectionCount   int                `json:"connection_count"`
	InputDurationMS   float64            `json:"input_duration_ms"`
	ElapsedMS         float64            `json:"elapsed_ms"`
	FirstAudioMS      *float64           `json:"first_audio_ms,omitempty"`
	OutputAudioMS     float64            `json:"output_audio_ms"`
	OutputTranscript  string             `json:"output_transcript,omitempty"`
	InputTranscripts  []string           `json:"input_transcripts,omitempty"`
	UserSpeechEndMS   *float64           `json:"user_speech_end_ms,omitempty"`
	UserSpeechEndsMS  []float64          `json:"user_speech_ends_ms,omitempty"`
	ToolCalls         []RealtimeToolCall `json:"tool_calls,omitempty"`
	Usage             Usage              `json:"usage,omitempty"`
	Chunks            []OutputChunk      `json:"-"`
	Events            []WireEvent        `json:"events"`
}

// RealtimeTool is the standard function-tool shape accepted by a Realtime
// session.update event. Parameters must contain a JSON Schema object.
type RealtimeTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// RealtimeToolCall records the authoritative function call observed on the
// wire and the external result used to resume the same Realtime trajectory.
type RealtimeToolCall struct {
	CallID      string          `json:"call_id"`
	Name        string          `json:"name"`
	Arguments   json.RawMessage `json:"arguments"`
	Output      json.RawMessage `json:"output"`
	RequestedMS float64         `json:"requested_ms"`
	CompletedMS float64         `json:"completed_ms"`
}

type Adapter interface {
	Descriptor() Descriptor
	Run(context.Context, Audio) (SessionResult, error)
}

type Sample struct {
	Benchmark      string     `json:"benchmark"`
	Revision       string     `json:"revision"`
	Scenario       string     `json:"scenario"`
	ID             string     `json:"id"`
	InputPath      string     `json:"input_path"`
	CleanInputPath string     `json:"clean_input_path,omitempty"`
	MetadataPath   string     `json:"metadata_path,omitempty"`
	OverlapStartS  *float64   `json:"overlap_start_s,omitempty"`
	OverlapEndS    *float64   `json:"overlap_end_s,omitempty"`
	InputSHA256    string     `json:"input_sha256"`
	CleanSHA256    string     `json:"clean_sha256,omitempty"`
	MetadataSHA256 string     `json:"metadata_sha256,omitempty"`
	DiscoveredAt   *time.Time `json:"-"`
}

type TrialResult struct {
	SchemaVersion string        `json:"schema_version"`
	TrialID       string        `json:"trial_id"`
	Attempt       int           `json:"attempt"`
	Sample        Sample        `json:"sample"`
	Condition     string        `json:"condition"`
	InputSHA256   string        `json:"input_sha256"`
	OutputSHA256  string        `json:"output_sha256"`
	OutputWAV     string        `json:"output_wav"`
	Timing        TimingMetrics `json:"timing"`
	Session       SessionResult `json:"session"`
}

type SpeechSegment struct {
	StartMS float64 `json:"start_ms"`
	EndMS   float64 `json:"end_ms"`
}

// TimingMetrics are neutral observations, not a scenario-specific composite.
// This avoids hiding behavior tradeoffs behind a single vendor score.
type TimingMetrics struct {
	VAD                      string          `json:"vad"`
	UserSegments             []SpeechSegment `json:"user_segments"`
	OutputSegments           []SpeechSegment `json:"output_segments"`
	LatencyStopIntervals     []SpeechSegment `json:"latency_stop_intervals"`
	LatencyResponseIntervals []SpeechSegment `json:"latency_response_intervals"`
	MeanStopLatencyMS        *float64        `json:"mean_stop_latency_ms,omitempty"`
	MeanResponseLatencyMS    *float64        `json:"mean_response_latency_ms,omitempty"`
	FirstOutputMS            *float64        `json:"first_output_ms,omitempty"`
	SpeechDuringOverlap      bool            `json:"speech_during_overlap"`
	OverlapSpeechMS          float64         `json:"overlap_speech_ms"`
}
