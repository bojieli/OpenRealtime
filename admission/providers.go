package admission

import (
	"context"
	"errors"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/continuation"
)

// ContinuationProvider admits every model continuation through one resource
// governor. Class and cost come from deployment configuration, never prompt
// classification.
type ContinuationProvider struct {
	Provider continuation.Provider
	Governor *Governor
	Request  Request
}

func (provider *ContinuationProvider) Descriptor() continuation.Descriptor {
	return provider.Provider.Descriptor()
}

func (provider *ContinuationProvider) Continue(ctx context.Context, request continuation.Request, emit continuation.Emit) (continuation.Completion, error) {
	if provider == nil || provider.Provider == nil || provider.Governor == nil {
		return continuation.Completion{}, errors.New("admitted continuation provider is not configured")
	}
	lease, err := provider.Governor.Acquire(ctx, provider.Request)
	if err != nil {
		return continuation.Completion{}, err
	}
	defer lease.Release()
	completion, providerErr := provider.Provider.Continue(lease.Context(), request, emit)
	if errors.Is(context.Cause(lease.Context()), ErrPreempted) {
		providerErr = errors.Join(providerErr, continuation.ErrPreempted)
	}
	return completion, providerErr
}

// PerceptionProvider protects every stateful ASR boundary. Interactive ASR
// should normally be non-preemptible so a cancelled chunk cannot corrupt a
// provider session.
type PerceptionProvider struct {
	Provider v1.PerceptionProvider
	Governor *Governor
	Request  Request
}

func (provider *PerceptionProvider) Descriptor() v1.Descriptor {
	return provider.Provider.Descriptor()
}

func (provider *PerceptionProvider) PushFrame(ctx context.Context, frame v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	if provider == nil || provider.Provider == nil || provider.Governor == nil {
		return nil, errors.New("admitted perception provider is not configured")
	}
	lease, err := provider.Governor.Acquire(ctx, provider.Request)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	return provider.Provider.PushFrame(lease.Context(), frame)
}

func (provider *PerceptionProvider) Finalize(ctx context.Context, finalSample uint64) (v1.PerceptionRevision, error) {
	if provider == nil || provider.Provider == nil || provider.Governor == nil {
		return v1.PerceptionRevision{}, errors.New("admitted perception provider is not configured")
	}
	lease, err := provider.Governor.Acquire(ctx, provider.Request)
	if err != nil {
		return v1.PerceptionRevision{}, err
	}
	defer lease.Release()
	return provider.Provider.Finalize(lease.Context(), finalSample)
}

// SpeechProvider admits one complete streaming synthesis lease. The lease is
// held until the terminal chunk or error so capacity reflects actual work.
type SpeechProvider struct {
	Provider v1.StreamingSpeechProvider
	Governor *Governor
	Request  Request
}

func (provider *SpeechProvider) Descriptor() v1.Descriptor {
	return provider.Provider.Descriptor()
}

func (provider *SpeechProvider) Stream(ctx context.Context, plan v1.SpeechPlan, emit func(v1.SpeechChunk) error) error {
	if provider == nil || provider.Provider == nil || provider.Governor == nil {
		return errors.New("admitted speech provider is not configured")
	}
	lease, err := provider.Governor.Acquire(ctx, provider.Request)
	if err != nil {
		return err
	}
	defer lease.Release()
	return provider.Provider.Stream(lease.Context(), plan, emit)
}

// Synthesize preserves the stable non-streaming convenience surface while
// holding exactly the same one-request lease as Stream.
func (provider *SpeechProvider) Synthesize(ctx context.Context, plan v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	var chunks []v1.SpeechChunk
	err := provider.Stream(ctx, plan, func(chunk v1.SpeechChunk) error {
		chunk.PCM16LE = append([]byte(nil), chunk.PCM16LE...)
		chunks = append(chunks, chunk)
		return nil
	})
	return chunks, err
}
