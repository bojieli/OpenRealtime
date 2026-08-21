package gateway

import "sync/atomic"

// Metrics is operational telemetry. It contains counts and never content: a
// metrics endpoint that leaked conversation would be a worse problem than
// having no metrics endpoint.
type Metrics struct {
	sessionsStarted   atomic.Uint64
	sessionsCompleted atomic.Uint64
	sessionsFailed    atomic.Uint64
	audioFramesIn     atomic.Uint64
	videoFramesIn     atomic.Uint64
	audioFramesOut    atomic.Uint64
	toolCallsOut      atomic.Uint64
}

// MetricsSnapshot is a consistent-enough view for reporting.
type MetricsSnapshot struct {
	SessionsStarted   uint64 `json:"sessions_started"`
	SessionsCompleted uint64 `json:"sessions_completed"`
	SessionsFailed    uint64 `json:"sessions_failed"`
	AudioFramesIn     uint64 `json:"audio_frames_in"`
	VideoFramesIn     uint64 `json:"video_frames_in"`
	AudioFramesOut    uint64 `json:"audio_frames_out"`
	ToolCallsOut      uint64 `json:"tool_calls_out"`
}

// Snapshot reads the counters.
func (metrics *Metrics) Snapshot() MetricsSnapshot {
	return MetricsSnapshot{
		SessionsStarted:   metrics.sessionsStarted.Load(),
		SessionsCompleted: metrics.sessionsCompleted.Load(),
		SessionsFailed:    metrics.sessionsFailed.Load(),
		AudioFramesIn:     metrics.audioFramesIn.Load(),
		VideoFramesIn:     metrics.videoFramesIn.Load(),
		AudioFramesOut:    metrics.audioFramesOut.Load(),
		ToolCallsOut:      metrics.toolCallsOut.Load(),
	}
}
