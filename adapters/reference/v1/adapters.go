// Package v1 exposes the deterministic reference providers through api/v1.
package v1

import (
	"context"
	"slices"

	reference "github.com/bojieli/OpenRealtime/adapters/reference"
	stable "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/engine"
)

const adapterVersion = "1.0.0"

type Perception struct{ provider *reference.Perception }

func NewPerception(manifest reference.Manifest) *Perception {
	return &Perception{provider: reference.NewPerception(manifest)}
}

func (provider *Perception) Descriptor() stable.Descriptor {
	return descriptor("reference.manifest_perception.v1", stable.Capabilities{
		stable.CapabilityStreamingInput: true, stable.CapabilityRevisions: true,
		stable.CapabilityCancellation: true, stable.CapabilityDeterministic: true,
	})
}

func (provider *Perception) PushFrame(ctx context.Context, frame stable.AudioFrame) ([]stable.PerceptionRevision, error) {
	if err := frame.Validate(); err != nil {
		return nil, err
	}
	revisions, err := provider.provider.PushFrame(ctx, engine.AudioFrame{
		Index: frame.Index, SampleOffset: frame.SampleOffset, SampleRateHz: frame.SampleRateHz,
		PCM16LE: slices.Clone(frame.PCM16LE),
	})
	if err != nil {
		return nil, err
	}
	result := make([]stable.PerceptionRevision, len(revisions))
	for index, revision := range revisions {
		result[index] = fromEngineRevision(revision)
	}
	return result, nil
}

func (provider *Perception) Finalize(ctx context.Context, endSample uint64) (stable.PerceptionRevision, error) {
	revision, err := provider.provider.Finalize(ctx, endSample)
	if err != nil {
		return stable.PerceptionRevision{}, err
	}
	return fromEngineRevision(revision), nil
}

type Cognition struct{ provider *reference.Cognition }

func NewCognition(responseText string) *Cognition {
	return &Cognition{provider: reference.NewCognition(responseText)}
}

func (provider *Cognition) Descriptor() stable.Descriptor {
	return descriptor("reference.fixed_cognition.v1", stable.Capabilities{
		stable.CapabilityCancellation: true, stable.CapabilityDeterministic: true,
	})
}

func (provider *Cognition) Respond(ctx context.Context, revision stable.PerceptionRevision) (stable.ResponseCandidate, error) {
	candidate, err := provider.provider.Respond(ctx, toEngineRevision(revision))
	if err != nil {
		return stable.ResponseCandidate{}, err
	}
	return stable.ResponseCandidate{
		CandidateID: candidate.CandidateID, SourceRevision: candidate.SourceRevision,
		Text: candidate.Text, Semantic: candidate.Semantic, ValiditySummary: candidate.ValiditySummary,
	}, nil
}

type Speech struct{ provider *reference.Speech }

func NewSpeech(durationMS uint32) *Speech {
	return &Speech{provider: reference.NewSpeech(durationMS)}
}

func (provider *Speech) Descriptor() stable.Descriptor {
	return descriptor("reference.signal_speech.v1", stable.Capabilities{
		stable.CapabilityCancellation: true, stable.CapabilityDeterministic: true,
		stable.CapabilityPCM16Output: true, stable.CapabilityStreamingOutput: true,
	})
}

func (provider *Speech) Synthesize(ctx context.Context, plan stable.SpeechPlan) ([]stable.SpeechChunk, error) {
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	chunks, err := provider.provider.Synthesize(ctx, engine.SpeechPlan{CandidateID: plan.CandidateID, Text: plan.Text})
	if err != nil {
		return nil, err
	}
	return fromEngineChunks(chunks), nil
}

func (provider *Speech) Stream(ctx context.Context, plan stable.SpeechPlan, consume func(stable.SpeechChunk) error) error {
	if err := plan.Validate(); err != nil {
		return err
	}
	if consume == nil {
		return stable.ErrNilConsumer
	}
	return provider.provider.Stream(ctx, engine.SpeechPlan{CandidateID: plan.CandidateID, Text: plan.Text}, func(chunk engine.SpeechChunk) error {
		return consume(fromEngineChunk(chunk))
	})
}

type Fast struct{ provider *reference.Fast }

func NewFast(task reference.DifficultTask, mode reference.FastMode) *Fast {
	return &Fast{provider: reference.NewFast(task, mode)}
}

func (provider *Fast) Descriptor() stable.Descriptor {
	return descriptor("reference.fast_decision.v1", stable.Capabilities{
		stable.CapabilityCancellation: true, stable.CapabilityDeterministic: true,
	})
}

func (provider *Fast) Decide(ctx context.Context, goal stable.GoalSnapshot) (stable.FastDecision, error) {
	if err := goal.Validate(); err != nil {
		return stable.FastDecision{}, err
	}
	decision, err := provider.provider.Decide(ctx, toEngineGoal(goal))
	if err != nil {
		return stable.FastDecision{}, err
	}
	return stable.FastDecision{
		GoalID: decision.GoalID, RevisionID: decision.RevisionID, Action: stable.FastAction(decision.Action),
		Text: decision.Text, ProgressClaim: stable.ProgressClaim(decision.ProgressClaim), StartSlowPath: decision.StartSlowPath,
	}, nil
}

type Deliberation struct{ provider *reference.Deliberation }

func NewDeliberation(task reference.DifficultTask, outcome reference.DeliberationOutcome) *Deliberation {
	return &Deliberation{provider: reference.NewDeliberation(task, outcome)}
}

func (provider *Deliberation) Descriptor() stable.Descriptor {
	return descriptor("reference.scripted_deliberation.v1", stable.Capabilities{
		stable.CapabilityCancellation: true, stable.CapabilityDeterministic: true,
	})
}

func (provider *Deliberation) Deliberate(
	ctx context.Context,
	goal stable.GoalSnapshot,
	consume func(stable.DeliberationUpdate) error,
) error {
	if err := goal.Validate(); err != nil {
		return err
	}
	if consume == nil {
		return stable.ErrNilConsumer
	}
	return provider.provider.Deliberate(ctx, toEngineGoal(goal), func(update engine.DeliberationUpdate) error {
		return consume(stable.DeliberationUpdate{
			GoalID: update.GoalID, RevisionID: update.RevisionID, Sequence: update.Sequence,
			Status: stable.DeliberationStatus(update.Status), Text: update.Text, Error: update.Error,
			ComputeUnits: update.ComputeUnits, QualityScore: update.QualityScore,
		})
	})
}

func descriptor(name string, capabilities stable.Capabilities) stable.Descriptor {
	return stable.Descriptor{Name: name, Version: adapterVersion, Capabilities: capabilities}
}

func fromEngineRevision(revision engine.PerceptionRevision) stable.PerceptionRevision {
	return stable.PerceptionRevision{
		RevisionID: revision.RevisionID, SourceSample: revision.SourceSample,
		StableText: revision.StableText, UnstableText: revision.UnstableText,
		Delta: revision.Delta, Final: revision.Final,
	}
}

func toEngineRevision(revision stable.PerceptionRevision) engine.PerceptionRevision {
	return engine.PerceptionRevision{
		RevisionID: revision.RevisionID, SourceSample: revision.SourceSample,
		StableText: revision.StableText, UnstableText: revision.UnstableText,
		Delta: revision.Delta, Final: revision.Final,
	}
}

func fromEngineChunks(chunks []engine.SpeechChunk) []stable.SpeechChunk {
	result := make([]stable.SpeechChunk, len(chunks))
	for index, chunk := range chunks {
		result[index] = fromEngineChunk(chunk)
	}
	return result
}

func fromEngineChunk(chunk engine.SpeechChunk) stable.SpeechChunk {
	return stable.SpeechChunk{
		ChunkID: chunk.ChunkID, CandidateID: chunk.CandidateID, SampleOffset: chunk.SampleOffset,
		SampleRateHz: chunk.SampleRateHz, PCM16LE: slices.Clone(chunk.PCM16LE), Final: chunk.Final,
	}
}

func toEngineGoal(goal stable.GoalSnapshot) engine.GoalSnapshot {
	return engine.GoalSnapshot{
		GoalID: goal.GoalID, RevisionID: goal.RevisionID, Question: goal.Question,
		CreatedNS: goal.CreatedNS, DeadlineNS: goal.DeadlineNS,
	}
}

var (
	_ stable.PerceptionProvider      = (*Perception)(nil)
	_ stable.CognitionProvider       = (*Cognition)(nil)
	_ stable.StreamingSpeechProvider = (*Speech)(nil)
	_ stable.FastDecisionProvider    = (*Fast)(nil)
	_ stable.DeliberationProvider    = (*Deliberation)(nil)
)
