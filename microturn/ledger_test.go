package microturn

import (
	"testing"

	"github.com/bojieli/OpenRealtime/engine"
)

func TestRevisionLedgerProtectsStablePrefix(t *testing.T) {
	t.Parallel()
	ledger := NewRevisionLedger()
	if err := ledger.Append(engine.PerceptionRevision{RevisionID: 1, SourceSample: 10, StableText: "open"}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Append(engine.PerceptionRevision{RevisionID: 2, SourceSample: 20, StableText: "closed"}); err == nil {
		t.Fatal("expected stable-prefix rewrite to fail")
	}
	if err := ledger.Append(engine.PerceptionRevision{RevisionID: 2, SourceSample: 20, StableText: "open realtime", Final: true}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Append(engine.PerceptionRevision{RevisionID: 3, SourceSample: 30, StableText: "open realtime now"}); err == nil {
		t.Fatal("expected revision after finalization to fail")
	}
}

func TestCandidateLifecycleIsExplicitAndRevisionAware(t *testing.T) {
	t.Parallel()
	revisions := NewRevisionLedger()
	for _, revision := range []engine.PerceptionRevision{
		{RevisionID: 1, SourceSample: 10, StableText: "open"},
		{RevisionID: 2, SourceSample: 20, StableText: "open realtime"},
	} {
		if err := revisions.Append(revision); err != nil {
			t.Fatal(err)
		}
	}
	ledger := NewCandidateLedger(revisions)
	first := engine.ResponseCandidate{CandidateID: "candidate_1", SourceRevision: 1, Text: "first", ValiditySummary: "revision 1"}
	second := engine.ResponseCandidate{CandidateID: "candidate_2", SourceRevision: 2, Text: "second", ValiditySummary: "revision 2"}
	if err := ledger.Prepare(first, 100); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Supersede(first.CandidateID, second, 200, "newer stable prefix"); err != nil {
		t.Fatal(err)
	}
	old, _ := ledger.Get(first.CandidateID)
	if old.State != CandidateSuperseded || old.SupersededByID != second.CandidateID {
		t.Fatalf("unexpected superseded candidate: %+v", old)
	}
	if err := ledger.Cancel(second.CandidateID, 250, "endpoint mismatch"); err != nil {
		t.Fatal(err)
	}
	if _, active := ledger.Active(); active {
		t.Fatal("cancelled candidate remained active")
	}
}
