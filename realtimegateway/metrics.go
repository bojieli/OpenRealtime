package realtimegateway

import "sync/atomic"

// RuntimeMetrics contains process-local counters that explain scheduling and
// provider work without exposing model content or extending the Realtime wire.
// A gateway restart deliberately starts a new measurement population.
type RuntimeMetrics struct {
	sessionsStarted     atomic.Uint64
	sessionsCompleted   atomic.Uint64
	asrInputFrames      atomic.Uint64
	asrProviderAdvances atomic.Uint64
	asrFinalizations    atomic.Uint64
}

// RuntimeMetricsSnapshot is the immutable health/report representation.
type RuntimeMetricsSnapshot struct {
	SessionsStarted     uint64 `json:"sessions_started"`
	SessionsCompleted   uint64 `json:"sessions_completed"`
	ASRInputFrames      uint64 `json:"asr_input_frames"`
	ASRProviderAdvances uint64 `json:"asr_provider_advances"`
	ASRFinalizations    uint64 `json:"asr_finalizations"`
}

func (metrics *RuntimeMetrics) Snapshot() RuntimeMetricsSnapshot {
	if metrics == nil {
		return RuntimeMetricsSnapshot{}
	}
	return RuntimeMetricsSnapshot{
		SessionsStarted:     metrics.sessionsStarted.Load(),
		SessionsCompleted:   metrics.sessionsCompleted.Load(),
		ASRInputFrames:      metrics.asrInputFrames.Load(),
		ASRProviderAdvances: metrics.asrProviderAdvances.Load(),
		ASRFinalizations:    metrics.asrFinalizations.Load(),
	}
}
