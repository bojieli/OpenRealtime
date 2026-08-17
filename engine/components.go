// Package engine defines provider-neutral, cancellation-aware component contracts.
package engine

import "context"

type Capability string

const (
	CapabilityStreamingInput Capability = "streaming_input"
	CapabilityRevisions      Capability = "revisions"
	CapabilityCancellation   Capability = "cancellation"
	CapabilityDeterministic  Capability = "deterministic"
	CapabilityPCM16Output    Capability = "pcm16_output"
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

type SpeechPlan struct {
	CandidateID string
	Text        string
}

type SpeechChunk struct {
	ChunkID      string
	CandidateID  string
	SampleRateHz uint32
	PCM16LE      []byte
	Final        bool
}

func (chunk SpeechChunk) DurationNS() uint64 {
	if chunk.SampleRateHz == 0 {
		return 0
	}
	return uint64(len(chunk.PCM16LE)/2) * 1_000_000_000 / uint64(chunk.SampleRateHz)
}

type SpeechProvider interface {
	Name() string
	Capabilities() Capabilities
	Synthesize(context.Context, SpeechPlan) ([]SpeechChunk, error)
}
