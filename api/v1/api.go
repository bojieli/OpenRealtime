// Package v1 defines the stable OpenRealtime component API.
//
// Experimental engine internals may change independently. Breaking changes to
// this package require a new semantic import path such as api/v2.
package v1

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
)

const Version = "1.0.0"

var ErrNilConsumer = errors.New("stream consumer must not be nil")

type Capability string

const (
	CapabilityStreamingInput  Capability = "streaming_input"
	CapabilityRevisions       Capability = "revisions"
	CapabilityCancellation    Capability = "cancellation"
	CapabilityDeterministic   Capability = "deterministic"
	CapabilityPCM16Output     Capability = "pcm16_output"
	CapabilityStreamingOutput Capability = "streaming_output"
)

type Capabilities map[Capability]bool

func (capabilities Capabilities) Has(capability Capability) bool {
	return capabilities[capability]
}

type Descriptor struct {
	Name         string       `json:"name"`
	Version      string       `json:"version"`
	Capabilities Capabilities `json:"capabilities"`
}

func (descriptor Descriptor) Validate() error {
	if strings.TrimSpace(descriptor.Name) == "" || strings.TrimSpace(descriptor.Version) == "" {
		return errors.New("provider descriptor requires name and version")
	}
	if descriptor.Capabilities == nil {
		return errors.New("provider descriptor requires a capabilities map")
	}
	return nil
}

type AudioFrame struct {
	Index        uint64 `json:"index"`
	SampleOffset uint64 `json:"sample_offset"`
	SampleRateHz uint32 `json:"sample_rate_hz"`
	PCM16LE      []byte `json:"pcm16le"`
}

func (frame AudioFrame) Validate() error {
	if frame.SampleRateHz == 0 || len(frame.PCM16LE) == 0 || len(frame.PCM16LE)%2 != 0 {
		return errors.New("audio frame requires non-empty even-length PCM16 and a sample rate")
	}
	if uint64(len(frame.PCM16LE)/2) > math.MaxUint64-frame.SampleOffset {
		return errors.New("audio frame end sample overflows")
	}
	return nil
}

func (frame AudioFrame) EndSample() (uint64, error) {
	if err := frame.Validate(); err != nil {
		return 0, err
	}
	return frame.SampleOffset + uint64(len(frame.PCM16LE)/2), nil
}

type PerceptionRevision struct {
	RevisionID   uint64 `json:"revision_id"`
	SourceSample uint64 `json:"source_sample"`
	StableText   string `json:"stable_text"`
	UnstableText string `json:"unstable_text"`
	Delta        string `json:"delta"`
	Final        bool   `json:"final"`
}

type PerceptionProvider interface {
	Descriptor() Descriptor
	PushFrame(context.Context, AudioFrame) ([]PerceptionRevision, error)
	Finalize(context.Context, uint64) (PerceptionRevision, error)
}

type ResponseCandidate struct {
	CandidateID     string `json:"candidate_id"`
	SourceRevision  uint64 `json:"source_revision"`
	Text            string `json:"text"`
	Semantic        bool   `json:"semantic"`
	ValiditySummary string `json:"validity_summary"`
}

type CognitionProvider interface {
	Descriptor() Descriptor
	Respond(context.Context, PerceptionRevision) (ResponseCandidate, error)
}

type GoalSnapshot struct {
	GoalID     string `json:"goal_id"`
	RevisionID uint64 `json:"revision_id"`
	Question   string `json:"question"`
	CreatedNS  uint64 `json:"created_ns"`
	DeadlineNS uint64 `json:"deadline_ns"`
}

func (goal GoalSnapshot) Validate() error {
	if goal.GoalID == "" || goal.RevisionID == 0 || strings.TrimSpace(goal.Question) == "" {
		return errors.New("goal requires an ID, positive revision, and question")
	}
	if goal.DeadlineNS != 0 && goal.DeadlineNS < goal.CreatedNS {
		return errors.New("goal deadline precedes creation")
	}
	return nil
}

type FastAction string

const (
	FastListen      FastAction = "listen"
	FastAcknowledge FastAction = "acknowledge"
	FastAnswer      FastAction = "answer"
	FastDefer       FastAction = "defer"
	FastYield       FastAction = "yield"
)

type ProgressClaim string

const (
	ProgressNone      ProgressClaim = "none"
	ProgressWorking   ProgressClaim = "working"
	ProgressCompleted ProgressClaim = "completed"
)

type FastDecision struct {
	GoalID        string        `json:"goal_id"`
	RevisionID    uint64        `json:"revision_id"`
	Action        FastAction    `json:"action"`
	Text          string        `json:"text"`
	ProgressClaim ProgressClaim `json:"progress_claim"`
	StartSlowPath bool          `json:"start_slow_path"`
}

func (decision FastDecision) ValidateFor(goal GoalSnapshot) error {
	if decision.GoalID != goal.GoalID || decision.RevisionID != goal.RevisionID {
		return errors.New("fast decision does not match goal revision")
	}
	switch decision.Action {
	case FastListen, FastAcknowledge, FastAnswer, FastDefer, FastYield:
	default:
		return fmt.Errorf("unknown fast action %q", decision.Action)
	}
	switch decision.ProgressClaim {
	case ProgressNone, ProgressWorking, ProgressCompleted:
	default:
		return fmt.Errorf("unknown progress claim %q", decision.ProgressClaim)
	}
	return nil
}

type FastDecisionProvider interface {
	Descriptor() Descriptor
	Decide(context.Context, GoalSnapshot) (FastDecision, error)
}

type DeliberationStatus string

const (
	DeliberationProgress DeliberationStatus = "progress"
	DeliberationFinal    DeliberationStatus = "final"
	DeliberationFailed   DeliberationStatus = "failed"
)

type DeliberationUpdate struct {
	GoalID       string             `json:"goal_id"`
	RevisionID   uint64             `json:"revision_id"`
	Sequence     uint64             `json:"sequence"`
	Status       DeliberationStatus `json:"status"`
	Text         string             `json:"text"`
	Error        string             `json:"error"`
	ComputeUnits uint64             `json:"compute_units"`
	QualityScore uint32             `json:"quality_score"`
}

func (update DeliberationUpdate) ValidateFor(goal GoalSnapshot) error {
	if update.GoalID != goal.GoalID || update.RevisionID != goal.RevisionID {
		return errors.New("deliberation update does not match goal revision")
	}
	switch update.Status {
	case DeliberationProgress:
		if update.Text == "" || update.Error != "" {
			return errors.New("progress update requires text and no error")
		}
	case DeliberationFinal:
		if update.Text == "" || update.Error != "" {
			return errors.New("final update requires text and no error")
		}
	case DeliberationFailed:
		if update.Error == "" || update.Text != "" {
			return errors.New("failed update requires error and no text")
		}
	default:
		return fmt.Errorf("unknown deliberation status %q", update.Status)
	}
	return nil
}

type DeliberationProvider interface {
	Descriptor() Descriptor
	Deliberate(context.Context, GoalSnapshot, func(DeliberationUpdate) error) error
}

type SpeechPlan struct {
	CandidateID string `json:"candidate_id"`
	Text        string `json:"text"`
}

func (plan SpeechPlan) Validate() error {
	if plan.CandidateID == "" || strings.TrimSpace(plan.Text) == "" {
		return errors.New("speech plan requires candidate ID and text")
	}
	return nil
}

type SpeechChunk struct {
	ChunkID      string `json:"chunk_id"`
	CandidateID  string `json:"candidate_id"`
	SampleOffset uint64 `json:"sample_offset"`
	SampleRateHz uint32 `json:"sample_rate_hz"`
	PCM16LE      []byte `json:"pcm16le"`
	Final        bool   `json:"final"`
}

func (chunk SpeechChunk) Validate() error {
	if chunk.ChunkID == "" || chunk.CandidateID == "" || chunk.SampleRateHz == 0 || len(chunk.PCM16LE) == 0 || len(chunk.PCM16LE)%2 != 0 {
		return errors.New("speech chunk requires IDs, sample rate, and non-empty even-length PCM16")
	}
	if uint64(len(chunk.PCM16LE)/2) > math.MaxUint64-chunk.SampleOffset {
		return errors.New("speech chunk end sample overflows")
	}
	return nil
}

func (chunk SpeechChunk) EndSample() (uint64, error) {
	if err := chunk.Validate(); err != nil {
		return 0, err
	}
	return chunk.SampleOffset + uint64(len(chunk.PCM16LE)/2), nil
}

func (chunk SpeechChunk) DurationNS() (uint64, error) {
	if err := chunk.Validate(); err != nil {
		return 0, err
	}
	samples := uint64(len(chunk.PCM16LE) / 2)
	rate := uint64(chunk.SampleRateHz)
	wholeSeconds := samples / rate
	if wholeSeconds > math.MaxUint64/1_000_000_000 {
		return 0, errors.New("speech chunk duration overflows nanoseconds")
	}
	wholeNS := wholeSeconds * 1_000_000_000
	fractionNS := samples % rate * 1_000_000_000 / rate
	if fractionNS > math.MaxUint64-wholeNS {
		return 0, errors.New("speech chunk duration overflows nanoseconds")
	}
	return wholeNS + fractionNS, nil
}

type SpeechProvider interface {
	Descriptor() Descriptor
	Synthesize(context.Context, SpeechPlan) ([]SpeechChunk, error)
}

type StreamingSpeechProvider interface {
	SpeechProvider
	Stream(context.Context, SpeechPlan, func(SpeechChunk) error) error
}

type ProviderSet struct {
	Perception   PerceptionProvider
	Cognition    CognitionProvider
	Speech       SpeechProvider
	Fast         FastDecisionProvider
	Deliberation DeliberationProvider
}
