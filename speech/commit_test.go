package speech

import (
	"testing"

	"github.com/bojieli/OpenRealtime/engine"
)

func newTestPlan(t *testing.T) *Plan {
	t.Helper()
	plan, err := NewPlan(Config{
		PlanID: "plan_1", CandidateID: "candidate_1", Text: "reference response",
		SampleRateHz: 24_000, MaxPreparedAheadSamples: 2_400, MaxQueuedAheadSamples: 960,
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func prepareFiveChunks(t *testing.T, plan *Plan) {
	t.Helper()
	for index := uint64(0); index < 5; index++ {
		if err := plan.Prepare(engine.SpeechChunk{
			ChunkID: "chunk", CandidateID: "candidate_1", SampleOffset: index * 480,
			SampleRateHz: 24_000, PCM16LE: make([]byte, 960), Final: index == 4,
		}, index); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInvalidatedPlayedAudioRequiresRepairAndRemainsInHistory(t *testing.T) {
	t.Parallel()
	plan := newTestPlan(t)
	prepareFiveChunks(t, plan)
	if err := plan.QueueThrough(960, 10); err != nil {
		t.Fatal(err)
	}
	if err := plan.MarkPlayedThrough(480, 20); err != nil {
		t.Fatal(err)
	}
	cancellation, err := plan.Cancel(CancelInvalidate, "new evidence invalidated the response", 30)
	if err != nil {
		t.Fatal(err)
	}
	if !cancellation.RepairRequired || cancellation.PlayedSamples != 480 || cancellation.DiscardedQueuedSamples != 480 || cancellation.DiscardedPreparedSamples != 1_440 {
		t.Fatalf("unexpected cancellation: %+v", cancellation)
	}
	if err := plan.ValidateClosed(); err == nil {
		t.Fatal("invalidated played audio closed without repair")
	}
	if err := plan.RecordRepair("Correction: the earlier response was incomplete.", 40); err != nil {
		t.Fatal(err)
	}
	if err := plan.ValidateClosed(); err != nil {
		t.Fatal(err)
	}
	snapshot := plan.Snapshot()
	if snapshot.PlayedSamples != 480 || snapshot.PreparedSamples != 480 || len(snapshot.PlayedHistory) != 1 || len(snapshot.Repairs) != 1 {
		t.Fatalf("played history was not preserved: %+v", snapshot)
	}
}

func TestYieldNeedsNoRepairAndCommitHorizonIsBounded(t *testing.T) {
	t.Parallel()
	plan := newTestPlan(t)
	prepareFiveChunks(t, plan)
	if err := plan.QueueThrough(1_440, 10); err == nil {
		t.Fatal("expected queue beyond commit horizon to fail")
	}
	if err := plan.QueueThrough(960, 11); err != nil {
		t.Fatal(err)
	}
	if err := plan.MarkPlayedThrough(480, 12); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Cancel(CancelYield, "directed user interruption", 13); err != nil {
		t.Fatal(err)
	}
	if err := plan.ValidateClosed(); err != nil {
		t.Fatal(err)
	}
}

func TestCompletedPlanRequiresAllFinalAudioPlayed(t *testing.T) {
	t.Parallel()
	plan := newTestPlan(t)
	prepareFiveChunks(t, plan)
	if err := plan.QueueThrough(960, 10); err != nil {
		t.Fatal(err)
	}
	if err := plan.Complete(11); err == nil {
		t.Fatal("expected incomplete plan to fail")
	}
}
