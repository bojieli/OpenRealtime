// Package acoustic exposes acoustic admission and endpoint ownership as
// independently composable graph elements.  The admission element answers the
// physical question "is speech audible?"; the endpoint policy answers the
// interaction question "does this pause end the turn?".
package acoustic

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/bojieli/OpenRealtime/element"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
	coreperception "github.com/bojieli/OpenRealtime/perception"
)

const (
	// Admission implementation:2 binds an endpoint flush to the exact final
	// admitted-audio item so downstream ASR cannot reorder independent input
	// lanes across the utterance boundary.
	admissionImplementationRevision = "implementation:2"
	endpointImplementationRevision  = "implementation:1"
	admissionRuntimeID              = "builtin://openrealtime/elements/acoustic.EnergyAdmission"
	endpointRuntimeID               = "builtin://openrealtime/elements/acoustic.EndpointPolicy"

	defaultMemoryLimit      = 128
	maximumMemoryLimit      = 4096
	defaultMaxFrameBytes    = 1 << 20
	maximumFrameBytes       = 32 << 20
	defaultMaxSampleRate    = 384_000
	maximumSampleRate       = 768_000
	maximumSourceLength     = 256
	maximumIdentifierLength = 256
	maximumPrefixMS         = 10_000
	maximumSilenceMS        = 60_000
	maximumSpeechMS         = 10_000
	maximumTimeoutMS        = 10 * 60 * 1000
)

var (
	rawAudioType         = element.Stream(element.Named("audio.InputFrame"))
	admittedAudioType    = element.Trigger(element.Named("audio.FrameBatch"))
	audioFlushType       = element.Trigger(element.Named("audio.Flush"))
	audioCancelType      = element.Interrupt(element.Named("audio.StreamID"))
	activityType         = element.Stream(element.Named("acoustic.SpeechActivity"))
	candidateType        = element.Trigger(element.Named("acoustic.EndpointCandidate"))
	gateCommandType      = element.Trigger(element.Named("acoustic.GateCommand"))
	policyStateType      = element.State(element.Named("acoustic.EndpointPolicyState"))
	admissionStateType   = element.State(element.Named("acoustic.AdmissionState"))
	admissionOutcomeType = element.Event(
		element.Named("acoustic.AdmissionOutcome"),
	)
	timingTickType      = element.Trigger(element.Named("timing.Tick"))
	commitType          = element.Trigger(element.Named("audio.Commit"))
	verdictType         = element.Trigger(element.Named("acoustic.EndpointVerdict"))
	endpointOutcomeType = element.Event(
		element.Named("acoustic.EndpointOutcome"),
	)
)

// ActivityType returns the exact typed acoustic-transition contract used by
// graph-native interaction policies. Acoustic activity is evidence only; it
// does not itself imply that any downstream work should be interrupted.
func ActivityType() element.Type { return activityType.Clone() }

// EndpointMode names who owns a silence candidate.
type EndpointMode string

const (
	EndpointAutomatic EndpointMode = "automatic"
	EndpointManual    EndpointMode = "manual"
	EndpointExternal  EndpointMode = "external"
)

func (mode EndpointMode) valid() bool {
	switch mode {
	case EndpointAutomatic, EndpointManual, EndpointExternal:
		return true
	default:
		return false
	}
}

// GateAction is the complete vocabulary accepted by EnergyAdmission.
type GateAction string

const (
	GateClose      GateAction = "close"
	GateReopen     GateAction = "reopen"
	GateForceClose GateAction = "force_close"
)

func (action GateAction) endpointVerdict() bool {
	return action == GateClose || action == GateReopen
}

// InputFrame addresses one immutable raw audio frame to a logical stream.
type InputFrame struct {
	StreamID string               `json:"stream_id"`
	Frame    coreperception.Frame `json:"frame"`
}

type SpeechActivityKind string

const (
	SpeechStarted SpeechActivityKind = "started"
	SpeechStopped SpeechActivityKind = "stopped"
)

// SpeechActivity exposes acoustic transitions without claiming turn
// ownership. A stopped event is emitted only after an endpoint command accepts
// the candidate; a rejected pause is represented by a candidate and reopen.
type SpeechActivity struct {
	Kind         SpeechActivityKind `json:"kind"`
	StreamID     string             `json:"stream_id"`
	Source       string             `json:"source"`
	AtNS         uint64             `json:"at_ns,omitempty"`
	SampleRateHz uint32             `json:"sample_rate_hz"`
	AudioStartMS int                `json:"audio_start_ms,omitempty"`
	AudioEndMS   int                `json:"audio_end_ms,omitempty"`
}

// EndpointCandidate is evidence that the energy gate observed sustained
// silence. It is not itself an endpoint decision.
type EndpointCandidate struct {
	ID           string `json:"id"`
	StreamID     string `json:"stream_id"`
	Source       string `json:"source"`
	DetectedNS   uint64 `json:"detected_ns,omitempty"`
	AudioEndMS   int    `json:"audio_end_ms"`
	SilenceNS    uint64 `json:"silence_ns"`
	SampleRateHz uint32 `json:"sample_rate_hz"`
	Sequence     uint64 `json:"sequence"`
}

// GateCommand resolves a silence candidate or explicitly commits an active
// manual stream. CandidateID is mandatory for close/reopen and optional for a
// force-close.
type GateCommand struct {
	Action      GateAction `json:"action"`
	CandidateID string     `json:"candidate_id,omitempty"`
	StreamID    string     `json:"stream_id,omitempty"`
	Reason      string     `json:"reason,omitempty"`
}

// EndpointVerdict is supplied by a separately composed interaction policy in
// external mode.
type EndpointVerdict struct {
	CandidateID string     `json:"candidate_id"`
	StreamID    string     `json:"stream_id,omitempty"`
	Action      GateAction `json:"action"`
	Reason      string     `json:"reason,omitempty"`
}

// TimingTick drives deadlines. EndpointPolicy never creates a hidden timer.
// The first tick after a candidate starts its timeout, and later ticks must be
// monotonic in that same producer-defined domain. Capture timestamps are
// observation evidence and are deliberately never compared with NowNS.
type TimingTick struct {
	NowNS uint64 `json:"now_ns"`
}

// AudioCommit is the client-owned/manual endpoint trigger.
type AudioCommit struct {
	StreamID string `json:"stream_id,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type AdmissionPhase string

const (
	AdmissionAwaitingPolicy AdmissionPhase = "awaiting_policy"
	AdmissionIdle           AdmissionPhase = "idle"
	AdmissionListening      AdmissionPhase = "listening"
	AdmissionSpeaking       AdmissionPhase = "speaking"
	AdmissionAwaiting       AdmissionPhase = "awaiting_endpoint"
)

// AdmissionState is bounded, payload-free operational state suitable for
// graph inspection.
type AdmissionState struct {
	Sequence              uint64         `json:"sequence"`
	Phase                 AdmissionPhase `json:"phase"`
	Mode                  EndpointMode   `json:"mode,omitempty"`
	PolicySequence        uint64         `json:"policy_sequence,omitempty"`
	StreamID              string         `json:"stream_id,omitempty"`
	Source                string         `json:"source,omitempty"`
	SampleRateHz          uint32         `json:"sample_rate_hz,omitempty"`
	CandidateID           string         `json:"candidate_id,omitempty"`
	SpeechActive          bool           `json:"speech_active,omitempty"`
	FramesSeen            uint64         `json:"frames_seen,omitempty"`
	FramesAdmitted        uint64         `json:"frames_admitted,omitempty"`
	GateGeneration        uint64         `json:"gate_generation,omitempty"`
	CancellationsRetained int            `json:"cancellations_retained,omitempty"`
	TerminalsRetained     int            `json:"terminals_retained,omitempty"`
}

// EndpointPolicyState makes endpoint ownership, pending work, and its explicit
// tick deadline visible to the rest of the graph.
type EndpointPolicyState struct {
	Sequence               uint64       `json:"sequence"`
	Mode                   EndpointMode `json:"mode"`
	PendingCandidateID     string       `json:"pending_candidate_id,omitempty"`
	PendingStreamID        string       `json:"pending_stream_id,omitempty"`
	PendingVerdictID       string       `json:"pending_verdict_id,omitempty"`
	PendingVerdictStreamID string       `json:"pending_verdict_stream_id,omitempty"`
	DeadlineNS             uint64       `json:"deadline_ns,omitempty"`
	LastTickNS             uint64       `json:"last_tick_ns,omitempty"`
	Fallback               GateAction   `json:"fallback,omitempty"`
	CancellationsRetained  int          `json:"cancellations_retained,omitempty"`
	TerminalsRetained      int          `json:"terminals_retained,omitempty"`
}

type OutcomeKind string

const (
	OutcomeSucceeded OutcomeKind = "succeeded"
	OutcomePending   OutcomeKind = "pending"
	OutcomeCanceled  OutcomeKind = "canceled"
	OutcomeRefused   OutcomeKind = "refused"
	OutcomeIgnored   OutcomeKind = "ignored"
	OutcomeFailed    OutcomeKind = "failed"
)

type AdmissionOutcome struct {
	Kind        OutcomeKind `json:"kind"`
	Operation   string      `json:"operation"`
	StreamID    string      `json:"stream_id,omitempty"`
	CandidateID string      `json:"candidate_id,omitempty"`
	Code        string      `json:"code,omitempty"`
	Message     string      `json:"message,omitempty"`
}

type EndpointOutcome struct {
	Kind        OutcomeKind `json:"kind"`
	Operation   string      `json:"operation"`
	StreamID    string      `json:"stream_id,omitempty"`
	CandidateID string      `json:"candidate_id,omitempty"`
	Action      GateAction  `json:"action,omitempty"`
	Code        string      `json:"code,omitempty"`
	Message     string      `json:"message,omitempty"`
}

// AdmissionConfig bounds both DSP allocation and remembered terminal state.
// Pointer-valued gate settings preserve the distinction between omitted
// defaults and intentionally configured zero values.
type AdmissionConfig struct {
	Threshold          *float64 `json:"threshold,omitempty"`
	PrefixPaddingMS    *int     `json:"prefix_padding_ms,omitempty"`
	SilenceDurationMS  *int     `json:"silence_duration_ms,omitempty"`
	SpeechDurationMS   *int     `json:"speech_duration_ms,omitempty"`
	Source             string   `json:"source,omitempty"`
	CancellationMemory int      `json:"cancellation_memory,omitempty"`
	TerminalMemory     int      `json:"terminal_memory,omitempty"`
	MaxFrameBytes      int      `json:"max_frame_bytes,omitempty"`
	MaxSampleRateHz    uint32   `json:"max_sample_rate_hz,omitempty"`
}

type resolvedAdmissionConfig struct {
	gate               coreperception.GateConfig
	source             string
	cancellationMemory int
	terminalMemory     int
	maxFrameBytes      int
	maxSampleRateHz    uint32
}

// EndpointPolicyConfig selects endpoint ownership. In external mode the
// timeout is evaluated only when TimingTick arrives.
type EndpointPolicyConfig struct {
	Mode               EndpointMode `json:"mode,omitempty"`
	CandidateTimeoutMS int          `json:"candidate_timeout_ms,omitempty"`
	Fallback           GateAction   `json:"fallback,omitempty"`
	CancellationMemory int          `json:"cancellation_memory,omitempty"`
	TerminalMemory     int          `json:"terminal_memory,omitempty"`
}

type resolvedEndpointConfig struct {
	mode               EndpointMode
	timeoutNS          uint64
	fallback           GateAction
	cancellationMemory int
	terminalMemory     int
}

func EnergyAdmissionDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "acoustic.EnergyAdmission",
		Revision:      1,
		Ports: []element.Port{
			{Name: "audio", Direction: element.Input, Type: rawAudioType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "policy", Direction: element.Input, Type: policyStateType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 4},
			{Name: "command", Direction: element.Input, Type: gateCommandType,
				Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "cancel", Direction: element.Input, Type: audioCancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "admitted", Direction: element.Output, Type: admittedAudioType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "candidate", Direction: element.Output, Type: candidateType,
				Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "activity", Direction: element.Output, Type: activityType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "endpoint", Direction: element.Output, Type: audioFlushType,
				Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "state", Direction: element.Output, Type: admissionStateType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 16},
			{Name: "outcome", Direction: element.Output, Type: admissionOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
		},
		Reaction: element.Reaction{
			Triggers: []string{"audio", "command"}, SampledState: []string{"policy"},
			Interrupts:     []string{"cancel"},
			Outcomes:       []string{"admitted", "candidate", "activity", "endpoint", "state", "outcome"},
			MaxConcurrency: 1, BreaksCycles: true,
		},
		StateSchema:  "schema://openrealtime/acoustic/admission-state/v1",
		ConfigSchema: "schema://openrealtime/acoustic/admission-config/v1",
	}
}

func EndpointPolicyDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "acoustic.EndpointPolicy",
		Revision:      2,
		Ports: []element.Port{
			{Name: "candidate", Direction: element.Input, Type: candidateType,
				Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "tick", Direction: element.Input, Type: timingTickType,
				Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "commit", Direction: element.Input, Type: commitType,
				Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "verdict", Direction: element.Input, Type: verdictType,
				Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "turn_end", Direction: element.Input, Type: perceptionelements.TurnEndType(),
				Cardinality: element.One, DefaultDepth: 4},
			{Name: "cancel", Direction: element.Input, Type: audioCancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "command", Direction: element.Output, Type: gateCommandType,
				Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "state", Direction: element.Output, Type: policyStateType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 16},
			{Name: "outcome", Direction: element.Output, Type: endpointOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
		},
		Reaction: element.Reaction{
			Triggers:   []string{"candidate", "tick", "commit", "verdict", "turn_end"},
			Interrupts: []string{"cancel"}, Outcomes: []string{"command", "state", "outcome"},
			MaxConcurrency: 1, BreaksCycles: true,
		},
		StateSchema:  "schema://openrealtime/acoustic/endpoint-policy-state/v1",
		ConfigSchema: "schema://openrealtime/acoustic/endpoint-policy-config/v1",
	}
}

func decodeAdmissionConfig(source json.RawMessage) (resolvedAdmissionConfig, error) {
	var supplied AdmissionConfig
	if err := elementconfig.Decode(source, &supplied); err != nil {
		return resolvedAdmissionConfig{}, err
	}
	defaults := coreperception.DefaultGateConfig()
	if supplied.Threshold != nil {
		defaults.Threshold = *supplied.Threshold
	}
	if supplied.PrefixPaddingMS != nil {
		defaults.PrefixPaddingMS = *supplied.PrefixPaddingMS
	}
	if supplied.SilenceDurationMS != nil {
		defaults.SilenceDurationMS = *supplied.SilenceDurationMS
	}
	if supplied.SpeechDurationMS != nil {
		defaults.SpeechDurationMS = *supplied.SpeechDurationMS
	}
	if math.IsNaN(defaults.Threshold) || math.IsInf(defaults.Threshold, 0) ||
		defaults.Threshold < 0 || defaults.Threshold > 1 {
		return resolvedAdmissionConfig{}, errors.New("threshold must be finite and between zero and one")
	}
	if defaults.PrefixPaddingMS < 0 || defaults.PrefixPaddingMS > maximumPrefixMS {
		return resolvedAdmissionConfig{}, fmt.Errorf("prefix_padding_ms must be between 0 and %d", maximumPrefixMS)
	}
	if defaults.SilenceDurationMS <= 0 || defaults.SilenceDurationMS > maximumSilenceMS {
		return resolvedAdmissionConfig{}, fmt.Errorf("silence_duration_ms must be between 1 and %d", maximumSilenceMS)
	}
	if defaults.SpeechDurationMS < 0 || defaults.SpeechDurationMS > maximumSpeechMS {
		return resolvedAdmissionConfig{}, fmt.Errorf("speech_duration_ms must be between 0 and %d", maximumSpeechMS)
	}
	sourceName := supplied.Source
	if len(sourceName) > maximumSourceLength {
		return resolvedAdmissionConfig{}, fmt.Errorf("source exceeds %d bytes", maximumSourceLength)
	}
	if err := validateIdentifier("source", sourceName, false); err != nil {
		return resolvedAdmissionConfig{}, err
	}
	cancellationMemory, err := boundedLimit("cancellation_memory", supplied.CancellationMemory, defaultMemoryLimit)
	if err != nil {
		return resolvedAdmissionConfig{}, err
	}
	terminalMemory, err := boundedLimit("terminal_memory", supplied.TerminalMemory, defaultMemoryLimit)
	if err != nil {
		return resolvedAdmissionConfig{}, err
	}
	maxFrameBytes := supplied.MaxFrameBytes
	if maxFrameBytes == 0 {
		maxFrameBytes = defaultMaxFrameBytes
	}
	if maxFrameBytes < 2 || maxFrameBytes > maximumFrameBytes {
		return resolvedAdmissionConfig{}, fmt.Errorf("max_frame_bytes must be between 2 and %d", maximumFrameBytes)
	}
	maxSampleRate := supplied.MaxSampleRateHz
	if maxSampleRate == 0 {
		maxSampleRate = defaultMaxSampleRate
	}
	if maxSampleRate > maximumSampleRate {
		return resolvedAdmissionConfig{}, fmt.Errorf("max_sample_rate_hz must not exceed %d", maximumSampleRate)
	}
	return resolvedAdmissionConfig{
		gate: defaults, source: sourceName, cancellationMemory: cancellationMemory,
		terminalMemory: terminalMemory, maxFrameBytes: maxFrameBytes, maxSampleRateHz: maxSampleRate,
	}, nil
}

func decodeEndpointConfig(source json.RawMessage) (resolvedEndpointConfig, error) {
	var supplied EndpointPolicyConfig
	if err := elementconfig.Decode(source, &supplied); err != nil {
		return resolvedEndpointConfig{}, err
	}
	if supplied.Mode == "" {
		supplied.Mode = EndpointAutomatic
	}
	if !supplied.Mode.valid() {
		return resolvedEndpointConfig{}, fmt.Errorf("unknown endpoint mode %q", supplied.Mode)
	}
	if supplied.Fallback == "" {
		supplied.Fallback = GateClose
	}
	if !supplied.Fallback.endpointVerdict() {
		return resolvedEndpointConfig{}, errors.New("fallback must be close or reopen")
	}
	if supplied.CandidateTimeoutMS < 0 || supplied.CandidateTimeoutMS > maximumTimeoutMS {
		return resolvedEndpointConfig{}, fmt.Errorf("candidate_timeout_ms must be between 0 and %d", maximumTimeoutMS)
	}
	if supplied.Mode == EndpointExternal && supplied.CandidateTimeoutMS <= 0 {
		return resolvedEndpointConfig{}, errors.New("external endpoint mode requires a positive candidate_timeout_ms")
	}
	if supplied.Mode != EndpointExternal && supplied.CandidateTimeoutMS != 0 {
		return resolvedEndpointConfig{}, errors.New("candidate_timeout_ms is only valid in external endpoint mode")
	}
	cancellationMemory, err := boundedLimit("cancellation_memory", supplied.CancellationMemory, defaultMemoryLimit)
	if err != nil {
		return resolvedEndpointConfig{}, err
	}
	terminalMemory, err := boundedLimit("terminal_memory", supplied.TerminalMemory, defaultMemoryLimit)
	if err != nil {
		return resolvedEndpointConfig{}, err
	}
	return resolvedEndpointConfig{
		mode: supplied.Mode, timeoutNS: uint64(supplied.CandidateTimeoutMS) * 1_000_000,
		fallback: supplied.Fallback, cancellationMemory: cancellationMemory, terminalMemory: terminalMemory,
	}, nil
}

func boundedLimit(name string, supplied, fallback int) (int, error) {
	if supplied == 0 {
		supplied = fallback
	}
	if supplied < 1 || supplied > maximumMemoryLimit {
		return 0, fmt.Errorf("%s must be between 1 and %d", name, maximumMemoryLimit)
	}
	return supplied, nil
}
