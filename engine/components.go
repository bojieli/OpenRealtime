// Package engine defines provider-neutral, cancellation-aware component contracts.
package engine

import "context"

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

type AudioFrame struct {
	Index        uint64
	SampleOffset uint64
	SampleRateHz uint32
	PCM16LE      []byte
}

func (frame AudioFrame) EndSample() uint64 {
	return frame.SampleOffset + uint64(len(frame.PCM16LE)/2)
}

type PerceptionRevision struct {
	RevisionID   uint64
	SourceSample uint64
	StableText   string
	UnstableText string
	Delta        string
	Final        bool
}

type PerceptionProvider interface {
	Name() string
	Capabilities() Capabilities
	PushFrame(context.Context, AudioFrame) ([]PerceptionRevision, error)
	Finalize(context.Context, uint64) (PerceptionRevision, error)
}

type ResponseCandidate struct {
	CandidateID     string
	SourceRevision  uint64
	Text            string
	Semantic        bool
	ValiditySummary string
}

type CognitionProvider interface {
	Name() string
	Capabilities() Capabilities
	Respond(context.Context, PerceptionRevision) (ResponseCandidate, error)
}

type GoalSnapshot struct {
	GoalID     string
	RevisionID uint64
	Question   string
	CreatedNS  uint64
	DeadlineNS uint64
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
	GoalID        string
	RevisionID    uint64
	Action        FastAction
	Text          string
	ProgressClaim ProgressClaim
	StartSlowPath bool
}

type FastDecisionProvider interface {
	Name() string
	Capabilities() Capabilities
	Decide(context.Context, GoalSnapshot) (FastDecision, error)
}

type DeliberationStatus string

const (
	DeliberationProgress DeliberationStatus = "progress"
	DeliberationFinal    DeliberationStatus = "final"
	DeliberationFailed   DeliberationStatus = "failed"
)

type DeliberationUpdate struct {
	GoalID       string
	RevisionID   uint64
	Sequence     uint64
	Status       DeliberationStatus
	Text         string
	Error        string
	ComputeUnits uint64
	QualityScore uint32
}

type DeliberationProvider interface {
	Name() string
	Capabilities() Capabilities
	Deliberate(context.Context, GoalSnapshot, func(DeliberationUpdate) error) error
}

type SpeechPlan struct {
	CandidateID string
	Text        string
}

type SpeechChunk struct {
	ChunkID      string
	CandidateID  string
	SampleOffset uint64
	SampleRateHz uint32
	PCM16LE      []byte
	Final        bool
}

func (chunk SpeechChunk) DurationNS() uint64 {
	if chunk.SampleRateHz == 0 {
		return 0
	}
	samples := uint64(len(chunk.PCM16LE) / 2)
	rate := uint64(chunk.SampleRateHz)
	return samples/rate*1_000_000_000 + samples%rate*1_000_000_000/rate
}

type SpeechProvider interface {
	Name() string
	Capabilities() Capabilities
	Synthesize(context.Context, SpeechPlan) ([]SpeechChunk, error)
}

type StreamingSpeechProvider interface {
	SpeechProvider
	Stream(context.Context, SpeechPlan, func(SpeechChunk) error) error
}
