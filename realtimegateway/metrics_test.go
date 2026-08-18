package realtimegateway

import "testing"

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
