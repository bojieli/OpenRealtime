package realtimegateway

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/continuation"
)

// RuntimeMetrics contains process-local counters that explain scheduling and
// provider work without exposing model content or extending the Realtime wire.
// A gateway restart deliberately starts a new measurement population.
type RuntimeMetrics struct {
	sessionsStarted         atomic.Uint64
	sessionsCompleted       atomic.Uint64
	asrInputFrames          atomic.Uint64
	asrProviderAdvances     atomic.Uint64
	asrProviderFailures     atomic.Uint64
	asrProviderElapsedNS    atomic.Uint64
	asrProviderMaxElapsedNS atomic.Uint64
	asrFinalizeAttempts     atomic.Uint64
	asrFinalizeFailures     atomic.Uint64
	asrFinalizeElapsedNS    atomic.Uint64
	asrFinalizeMaxElapsedNS atomic.Uint64
	asrFinalizations        atomic.Uint64
	repairsRequired         atomic.Uint64
	repairsResolved         atomic.Uint64
	fast                    continuationProviderMetrics
	slow                    continuationProviderMetrics
	fastPreparation         continuationProviderMetrics
	slowPreparation         continuationProviderMetrics
	speech                  speechProviderMetrics
}

// RuntimeMetricsSnapshot is the immutable health/report representation.
type RuntimeMetricsSnapshot struct {
	SessionsStarted         uint64                              `json:"sessions_started"`
	SessionsCompleted       uint64                              `json:"sessions_completed"`
	ASRInputFrames          uint64                              `json:"asr_input_frames"`
	ASRProviderAdvances     uint64                              `json:"asr_provider_advances"`
	ASRProviderFailures     uint64                              `json:"asr_provider_failures"`
	ASRProviderElapsedNS    uint64                              `json:"asr_provider_elapsed_ns"`
	ASRProviderMaxElapsedNS uint64                              `json:"asr_provider_maximum_elapsed_ns"`
	ASRFinalizeAttempts     uint64                              `json:"asr_finalize_attempts"`
	ASRFinalizeFailures     uint64                              `json:"asr_finalize_failures"`
	ASRFinalizeElapsedNS    uint64                              `json:"asr_finalize_elapsed_ns"`
	ASRFinalizeMaxElapsedNS uint64                              `json:"asr_finalize_maximum_elapsed_ns"`
	ASRFinalizations        uint64                              `json:"asr_finalizations"`
	RepairsRequired         uint64                              `json:"repairs_required"`
	RepairsResolved         uint64                              `json:"repairs_resolved"`
	Fast                    ContinuationProviderMetricsSnapshot `json:"fast"`
	Slow                    ContinuationProviderMetricsSnapshot `json:"slow"`
	FastPreparation         ContinuationProviderMetricsSnapshot `json:"fast_preparation"`
	SlowPreparation         ContinuationProviderMetricsSnapshot `json:"slow_preparation"`
	Speech                  SpeechProviderMetricsSnapshot       `json:"speech"`
}

func (metrics *RuntimeMetrics) Snapshot() RuntimeMetricsSnapshot {
	if metrics == nil {
		return RuntimeMetricsSnapshot{}
	}
	return RuntimeMetricsSnapshot{
		SessionsStarted:         metrics.sessionsStarted.Load(),
		SessionsCompleted:       metrics.sessionsCompleted.Load(),
		ASRInputFrames:          metrics.asrInputFrames.Load(),
		ASRProviderAdvances:     metrics.asrProviderAdvances.Load(),
		ASRProviderFailures:     metrics.asrProviderFailures.Load(),
		ASRProviderElapsedNS:    metrics.asrProviderElapsedNS.Load(),
		ASRProviderMaxElapsedNS: metrics.asrProviderMaxElapsedNS.Load(),
		ASRFinalizeAttempts:     metrics.asrFinalizeAttempts.Load(),
		ASRFinalizeFailures:     metrics.asrFinalizeFailures.Load(),
		ASRFinalizeElapsedNS:    metrics.asrFinalizeElapsedNS.Load(),
		ASRFinalizeMaxElapsedNS: metrics.asrFinalizeMaxElapsedNS.Load(),
		ASRFinalizations:        metrics.asrFinalizations.Load(),
		RepairsRequired:         metrics.repairsRequired.Load(),
		RepairsResolved:         metrics.repairsResolved.Load(),
		Fast:                    metrics.fast.snapshot(),
		Slow:                    metrics.slow.snapshot(),
		FastPreparation:         metrics.fastPreparation.snapshot(),
		SlowPreparation:         metrics.slowPreparation.snapshot(),
		Speech:                  metrics.speech.snapshot(),
	}
}

// ContinuationProviderMetricsSnapshot counts actual provider calls. Fast and
// Slow are canonical foreground calls; FastPreparation and SlowPreparation
// are private pre-endpoint calls. The classes are disjoint and selected from
// typed execution provenance. Durations are cumulative monotonic elapsed
// nanoseconds so reports can derive rates and averages without lossy rounding.
// An in-flight call temporarily makes Invocations greater than the sum of its
// three terminal outcomes.
type ContinuationProviderMetricsSnapshot struct {
	Invocations            uint64 `json:"invocations"`
	Completed              uint64 `json:"completed"`
	Failed                 uint64 `json:"failed"`
	Cancelled              uint64 `json:"cancelled"`
	Events                 uint64 `json:"events"`
	FirstEventCount        uint64 `json:"first_event_count"`
	CumulativeFirstEventNS uint64 `json:"cumulative_first_event_ns"`
	MaximumFirstEventNS    uint64 `json:"maximum_first_event_ns"`
	CumulativeElapsedNS    uint64 `json:"cumulative_elapsed_ns"`
	MaximumElapsedNS       uint64 `json:"maximum_elapsed_ns"`
	InputTokens            uint64 `json:"input_tokens"`
	CachedInputTokens      uint64 `json:"cached_input_tokens"`
	CachedInputReports     uint64 `json:"cached_input_reports"`
	OutputTokens           uint64 `json:"output_tokens"`
	ReasoningTokens        uint64 `json:"reasoning_tokens"`
	TotalTokens            uint64 `json:"total_tokens"`
}

type continuationProviderMetrics struct {
	invocations            atomic.Uint64
	completed              atomic.Uint64
	failed                 atomic.Uint64
	cancelled              atomic.Uint64
	events                 atomic.Uint64
	firstEventCount        atomic.Uint64
	cumulativeFirstEventNS atomic.Uint64
	maximumFirstEventNS    atomic.Uint64
	cumulativeElapsedNS    atomic.Uint64
	maximumElapsedNS       atomic.Uint64
	inputTokens            atomic.Uint64
	cachedInputTokens      atomic.Uint64
	cachedInputReports     atomic.Uint64
	outputTokens           atomic.Uint64
	reasoningTokens        atomic.Uint64
	totalTokens            atomic.Uint64
}

func (metrics *continuationProviderMetrics) snapshot() ContinuationProviderMetricsSnapshot {
	return ContinuationProviderMetricsSnapshot{
		Invocations: metrics.invocations.Load(), Completed: metrics.completed.Load(),
		Failed: metrics.failed.Load(), Cancelled: metrics.cancelled.Load(),
		Events: metrics.events.Load(), FirstEventCount: metrics.firstEventCount.Load(),
		CumulativeFirstEventNS: metrics.cumulativeFirstEventNS.Load(),
		MaximumFirstEventNS:    metrics.maximumFirstEventNS.Load(),
		CumulativeElapsedNS:    metrics.cumulativeElapsedNS.Load(),
		MaximumElapsedNS:       metrics.maximumElapsedNS.Load(),
		InputTokens:            metrics.inputTokens.Load(),
		CachedInputTokens:      metrics.cachedInputTokens.Load(),
		CachedInputReports:     metrics.cachedInputReports.Load(),
		OutputTokens:           metrics.outputTokens.Load(),
		ReasoningTokens:        metrics.reasoningTokens.Load(),
		TotalTokens:            metrics.totalTokens.Load(),
	}
}

func (metrics *continuationProviderMetrics) finish(
	ctx context.Context,
	started time.Time,
	firstEvent time.Time,
	usage continuation.Usage,
	err error,
) {
	elapsed := uint64(time.Since(started))
	metrics.cumulativeElapsedNS.Add(elapsed)
	updateMaximum(&metrics.maximumElapsedNS, elapsed)
	if !firstEvent.IsZero() {
		firstElapsed := uint64(firstEvent.Sub(started))
		metrics.firstEventCount.Add(1)
		metrics.cumulativeFirstEventNS.Add(firstElapsed)
		updateMaximum(&metrics.maximumFirstEventNS, firstElapsed)
	}
	addPositive(&metrics.inputTokens, usage.InputTokens)
	addPositive(&metrics.cachedInputTokens, usage.CachedInputTokens)
	if usage.CachedInputTokensReported {
		metrics.cachedInputReports.Add(1)
	}
	addPositive(&metrics.outputTokens, usage.OutputTokens)
	addPositive(&metrics.reasoningTokens, usage.ReasoningTokens)
	addPositive(&metrics.totalTokens, usage.TotalTokens)
	switch {
	case ctx.Err() != nil:
		metrics.cancelled.Add(1)
	case err != nil:
		metrics.failed.Add(1)
	default:
		metrics.completed.Add(1)
	}
}

type measuredContinuationProvider struct {
	provider continuation.Provider
	metrics  *continuationProviderMetrics
}

func (provider *measuredContinuationProvider) Descriptor() continuation.Descriptor {
	return provider.provider.Descriptor()
}

func (provider *measuredContinuationProvider) Continue(
	ctx context.Context,
	request continuation.Request,
	emit continuation.Emit,
) (continuation.Completion, error) {
	provider.metrics.invocations.Add(1)
	started := time.Now()
	var firstEvent time.Time
	var firstEventOnce sync.Once
	completion, err := provider.provider.Continue(ctx, request, func(event continuation.Event) error {
		firstEventOnce.Do(func() { firstEvent = time.Now() })
		provider.metrics.events.Add(1)
		return emit(event)
	})
	provider.metrics.finish(ctx, started, firstEvent, completion.Usage, err)
	return completion, err
}

// SpeechProviderMetricsSnapshot measures actual Fish calls and source audio,
// independently from paced wire frames and nominal benchmark opportunities.
type SpeechProviderMetricsSnapshot struct {
	Invocations            uint64 `json:"invocations"`
	Completed              uint64 `json:"completed"`
	Failed                 uint64 `json:"failed"`
	Cancelled              uint64 `json:"cancelled"`
	Chunks                 uint64 `json:"chunks"`
	Samples                uint64 `json:"samples"`
	FirstChunkCount        uint64 `json:"first_chunk_count"`
	CumulativeFirstChunkNS uint64 `json:"cumulative_first_chunk_ns"`
	MaximumFirstChunkNS    uint64 `json:"maximum_first_chunk_ns"`
	CumulativeElapsedNS    uint64 `json:"cumulative_elapsed_ns"`
	MaximumElapsedNS       uint64 `json:"maximum_elapsed_ns"`
}

type speechProviderMetrics struct {
	invocations            atomic.Uint64
	completed              atomic.Uint64
	failed                 atomic.Uint64
	cancelled              atomic.Uint64
	chunks                 atomic.Uint64
	samples                atomic.Uint64
	firstChunkCount        atomic.Uint64
	cumulativeFirstChunkNS atomic.Uint64
	maximumFirstChunkNS    atomic.Uint64
	cumulativeElapsedNS    atomic.Uint64
	maximumElapsedNS       atomic.Uint64
}

func (metrics *speechProviderMetrics) snapshot() SpeechProviderMetricsSnapshot {
	return SpeechProviderMetricsSnapshot{
		Invocations: metrics.invocations.Load(), Completed: metrics.completed.Load(),
		Failed: metrics.failed.Load(), Cancelled: metrics.cancelled.Load(),
		Chunks: metrics.chunks.Load(), Samples: metrics.samples.Load(),
		FirstChunkCount:        metrics.firstChunkCount.Load(),
		CumulativeFirstChunkNS: metrics.cumulativeFirstChunkNS.Load(),
		MaximumFirstChunkNS:    metrics.maximumFirstChunkNS.Load(),
		CumulativeElapsedNS:    metrics.cumulativeElapsedNS.Load(),
		MaximumElapsedNS:       metrics.maximumElapsedNS.Load(),
	}
}

type measuredSpeechProvider struct {
	provider v1.StreamingSpeechProvider
	metrics  *speechProviderMetrics
}

func (provider *measuredSpeechProvider) Descriptor() v1.Descriptor {
	return provider.provider.Descriptor()
}

func (provider *measuredSpeechProvider) Stream(
	ctx context.Context,
	plan v1.SpeechPlan,
	consume func(v1.SpeechChunk) error,
) error {
	provider.metrics.invocations.Add(1)
	started := time.Now()
	var firstChunk time.Time
	var firstChunkOnce sync.Once
	err := provider.provider.Stream(ctx, plan, func(chunk v1.SpeechChunk) error {
		firstChunkOnce.Do(func() { firstChunk = time.Now() })
		provider.metrics.chunks.Add(1)
		provider.metrics.samples.Add(uint64(len(chunk.PCM16LE) / 2))
		return consume(chunk)
	})
	elapsed := uint64(time.Since(started))
	provider.metrics.cumulativeElapsedNS.Add(elapsed)
	updateMaximum(&provider.metrics.maximumElapsedNS, elapsed)
	if !firstChunk.IsZero() {
		firstElapsed := uint64(firstChunk.Sub(started))
		provider.metrics.firstChunkCount.Add(1)
		provider.metrics.cumulativeFirstChunkNS.Add(firstElapsed)
		updateMaximum(&provider.metrics.maximumFirstChunkNS, firstElapsed)
	}
	switch {
	case ctx.Err() != nil:
		provider.metrics.cancelled.Add(1)
	case err != nil:
		provider.metrics.failed.Add(1)
	default:
		provider.metrics.completed.Add(1)
	}
	return err
}

func (provider *measuredSpeechProvider) Synthesize(ctx context.Context, plan v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	var chunks []v1.SpeechChunk
	err := provider.Stream(ctx, plan, func(chunk v1.SpeechChunk) error {
		chunk.PCM16LE = append([]byte(nil), chunk.PCM16LE...)
		chunks = append(chunks, chunk)
		return nil
	})
	return chunks, err
}

func updateMaximum(target *atomic.Uint64, candidate uint64) {
	for current := target.Load(); candidate > current; current = target.Load() {
		if target.CompareAndSwap(current, candidate) {
			return
		}
	}
}

func addPositive(target *atomic.Uint64, value int64) {
	if value > 0 {
		target.Add(uint64(value))
	}
}
