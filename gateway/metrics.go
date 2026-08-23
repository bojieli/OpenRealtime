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
	// videoFramesDropped counts frames refused by the declared rate cap. It is
	// the number that says whether a client is conforming to the limits the
	// server stated at negotiation.
	videoFramesDropped atomic.Uint64
	audioFramesOut     atomic.Uint64
	toolCallsOut       atomic.Uint64
}

// MetricsSnapshot is a consistent-enough view for reporting.
type MetricsSnapshot struct {
	SessionsStarted    uint64 `json:"sessions_started"`
	SessionsCompleted  uint64 `json:"sessions_completed"`
	SessionsFailed     uint64 `json:"sessions_failed"`
	AudioFramesIn      uint64 `json:"audio_frames_in"`
	VideoFramesIn      uint64 `json:"video_frames_in"`
	VideoFramesDropped uint64 `json:"video_frames_dropped"`
	AudioFramesOut     uint64 `json:"audio_frames_out"`
	ToolCallsOut       uint64 `json:"tool_calls_out"`
}

// Snapshot reads the counters.
func (metrics *Metrics) Snapshot() MetricsSnapshot {
	return MetricsSnapshot{
		SessionsStarted:    metrics.sessionsStarted.Load(),
		SessionsCompleted:  metrics.sessionsCompleted.Load(),
		SessionsFailed:     metrics.sessionsFailed.Load(),
		AudioFramesIn:      metrics.audioFramesIn.Load(),
		VideoFramesIn:      metrics.videoFramesIn.Load(),
		VideoFramesDropped: metrics.videoFramesDropped.Load(),
		AudioFramesOut:     metrics.audioFramesOut.Load(),
		ToolCallsOut:       metrics.toolCallsOut.Load(),
	}
}

// RecogniserSnapshot is process-level telemetry for the recogniser boundary.
//
// It answers one question the session counters cannot: whether the recogniser
// is getting slower. A failing recogniser already ends a session with a named
// error, so the visible signal today is the failure; this is the run-up to it.
// Counters and timing only, like every other number this package reports.
type RecogniserSnapshot struct {
	Utterances uint64 `json:"utterances"`
	// InFlight is how many utterances are open right now. It is reported
	// because the totals include them: a reader comparing two polls should
	// know that part of the difference is one call still running.
	InFlight int `json:"in_flight"`

	AdvanceInvocations uint64 `json:"advance_invocations"`
	AdvanceFailures    uint64 `json:"advance_failures"`
	AdvanceElapsedNS   uint64 `json:"advance_elapsed_ns"`
	// AdvanceMeanElapsedNS is the number to alert on. A maximum jumps once on
	// a single bad call and never comes down; a mean that climbs over an hour
	// is a recogniser degrading.
	AdvanceMeanElapsedNS uint64 `json:"advance_mean_elapsed_ns"`
	AdvanceMaxElapsedNS  uint64 `json:"advance_max_elapsed_ns"`

	FinalizeInvocations   uint64 `json:"finalize_invocations"`
	FinalizeFailures      uint64 `json:"finalize_failures"`
	FinalizeElapsedNS     uint64 `json:"finalize_elapsed_ns"`
	FinalizeMeanElapsedNS uint64 `json:"finalize_mean_elapsed_ns"`
	FinalizeMaxElapsedNS  uint64 `json:"finalize_max_elapsed_ns"`
}
