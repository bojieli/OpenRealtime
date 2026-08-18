package realtimegateway

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/asrbuffer"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestRuntimeMetricsSnapshotIsCumulativeAndContentFree(t *testing.T) {
	t.Parallel()
	metrics := &RuntimeMetrics{}
	metrics.sessionsStarted.Add(4)
	metrics.sessionsCompleted.Add(3)
	metrics.asrInputFrames.Add(25)
	metrics.asrProviderAdvances.Add(7)
	metrics.asrFinalizations.Add(2)
	if got := metrics.Snapshot(); got != (RuntimeMetricsSnapshot{
		SessionsStarted: 4, SessionsCompleted: 3, ASRInputFrames: 25,
		ASRProviderAdvances: 7, ASRFinalizations: 2,
	}) {
		t.Fatalf("unexpected runtime metrics: %#v", got)
	}
}

func TestMeasuredContinuationProviderCountsActualWorkAndUsage(t *testing.T) {
	t.Parallel()
	raw := &scriptedProvider{
		descriptor: continuation.Descriptor{
			Provider: "test", Model: "fast", Phase: trajectory.PhaseFast,
			Effort: continuation.EffortMinimal, Streaming: true,
		},
		scripts: []providerScript{{
			events: []continuation.Event{
				{Kind: continuation.EventReasoningDelta, Text: "thinking"},
				{Kind: continuation.EventAssistantDelta, Text: "answer"},
			},
			usage: continuation.Usage{
				InputTokens: 10, OutputTokens: 4, ReasoningTokens: 2, TotalTokens: 14,
			},
		}},
	}
	metrics := &continuationProviderMetrics{}
	provider := &measuredContinuationProvider{provider: raw, metrics: metrics}
	events := 0
	if _, err := provider.Continue(context.Background(), continuation.Request{}, func(continuation.Event) error {
		events++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := metrics.snapshot()
	if events != 2 || got.Invocations != 1 || got.Completed != 1 || got.Failed != 0 || got.Cancelled != 0 ||
		got.Events != 2 || got.FirstEventCount != 1 || got.InputTokens != 10 || got.OutputTokens != 4 ||
		got.ReasoningTokens != 2 || got.TotalTokens != 14 {
		t.Fatalf("unexpected continuation metrics: %#v", got)
	}
}

func TestPreparationProviderMetricsAreDisjointFromForeground(t *testing.T) {
	t.Parallel()
	raw := &scriptedProvider{
		descriptor: continuation.Descriptor{
			Provider: "test", Model: "fast", Phase: trajectory.PhaseFast,
			Effort: continuation.EffortMinimal, Streaming: true,
		},
		scripts: []providerScript{{
			events: []continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "answer"}},
			usage:  continuation.Usage{InputTokens: 8, OutputTokens: 2, TotalTokens: 10},
		}},
	}
	total := &continuationProviderMetrics{}
	preparationMetrics := &continuationProviderMetrics{}
	preparationProvider := &measuredContinuationProvider{provider: raw, metrics: preparationMetrics}
	if _, err := preparationProvider.Continue(context.Background(), continuation.Request{}, func(continuation.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	totalSnapshot := total.snapshot()
	preparationSnapshot := preparationMetrics.snapshot()
	if totalSnapshot.Invocations != 0 || preparationSnapshot.Invocations != 1 || preparationSnapshot.TotalTokens != 10 {
		t.Fatalf("preparation=%#v total=%#v", preparationSnapshot, totalSnapshot)
	}
}

func TestMeasuredProvidersSeparateCancellationFromFailure(t *testing.T) {
	t.Parallel()
	metrics := &continuationProviderMetrics{}
	provider := &measuredContinuationProvider{
		provider: blockingMetricsProvider{}, metrics: metrics,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.Continue(ctx, continuation.Request{}, func(continuation.Event) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled provider error = %v", err)
	}
	got := metrics.snapshot()
	if got.Invocations != 1 || got.Cancelled != 1 || got.Failed != 0 || got.Completed != 0 {
		t.Fatalf("unexpected cancellation metrics: %#v", got)
	}
	failedMetrics := &continuationProviderMetrics{}
	failing := &measuredContinuationProvider{
		provider: failingMetricsProvider{}, metrics: failedMetrics,
	}
	if _, err := failing.Continue(context.Background(), continuation.Request{}, func(continuation.Event) error { return nil }); err == nil {
		t.Fatal("failing provider returned nil")
	}
	failedGot := failedMetrics.snapshot()
	if failedGot.Invocations != 1 || failedGot.Failed != 1 || failedGot.Cancelled != 0 || failedGot.Completed != 0 {
		t.Fatalf("unexpected failure metrics: %#v", failedGot)
	}

	speechMetrics := &speechProviderMetrics{}
	speech := &measuredSpeechProvider{provider: fakeSpeech{}, metrics: speechMetrics}
	if err := speech.Stream(context.Background(), v1.SpeechPlan{CandidateID: "candidate", Text: "answer"}, func(v1.SpeechChunk) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	speechGot := speechMetrics.snapshot()
	if speechGot.Invocations != 1 || speechGot.Completed != 1 || speechGot.Chunks != 1 ||
		speechGot.Samples != 2_400 || speechGot.FirstChunkCount != 1 {
		t.Fatalf("unexpected speech metrics: %#v", speechGot)
	}
}

func TestSessionAggregatesActualASRBoundaryMetrics(t *testing.T) {
	t.Parallel()
	buffer, err := asrbuffer.New(asrbuffer.Config{
		Provider: &finalOnlyASR{}, MinimumChunk: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionMetrics := &RuntimeMetrics{}
	session := &session{config: Config{RuntimeMetrics: sessionMetrics}}
	utterance := &utteranceState{provider: buffer}
	if _, err := buffer.PushFrame(context.Background(), v1.AudioFrame{
		Index: 0, SampleRateHz: 8_000, PCM16LE: make([]byte, 160),
	}); err != nil {
		t.Fatal(err)
	}
	session.recordProviderAdvances(utterance)
	if _, err := buffer.Finalize(context.Background(), 80); err != nil {
		t.Fatal(err)
	}
	session.recordProviderAdvances(utterance)
	got := sessionMetrics.Snapshot()
	if got.ASRProviderAdvances != 1 || got.ASRProviderFailures != 0 ||
		got.ASRFinalizeAttempts != 1 || got.ASRFinalizeFailures != 0 {
		t.Fatalf("unexpected ASR boundary metrics: %#v", got)
	}
}

type blockingMetricsProvider struct{}

type failingMetricsProvider struct{}

func (failingMetricsProvider) Descriptor() continuation.Descriptor {
	return continuation.Descriptor{
		Provider: "test", Model: "failing", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, Streaming: true,
	}
}

func (failingMetricsProvider) Continue(
	context.Context,
	continuation.Request,
	continuation.Emit,
) (continuation.Completion, error) {
	return continuation.Completion{}, errors.New("provider failed")
}

func (blockingMetricsProvider) Descriptor() continuation.Descriptor {
	return continuation.Descriptor{
		Provider: "test", Model: "blocking", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, Streaming: true,
	}
}

func (blockingMetricsProvider) Continue(
	ctx context.Context,
	_ continuation.Request,
	_ continuation.Emit,
) (continuation.Completion, error) {
	select {
	case <-ctx.Done():
		return continuation.Completion{}, ctx.Err()
	case <-time.After(time.Second):
		return continuation.Completion{}, errors.New("provider did not receive cancellation")
	}
}
