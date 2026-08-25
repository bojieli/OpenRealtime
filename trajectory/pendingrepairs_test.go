package trajectory

import "testing"

// One assistant item can be cut off, corrected, and cut off again, which is an
// ordinary thing in a long call. PendingRepairs recorded the target's place in
// the order each time it became pending, so the second cycle listed it twice -
// and the caller, which turns each entry into a resolving item, built a batch
// that resolved the same repair twice. The second one is rejected, the whole
// batch is refused, and the obligation never clears: measured, that reached
// the client as a session error every few seconds until the session died.
func TestARepairRequiredTwiceIsPendingOnce(t *testing.T) {
	t.Parallel()
	snapshot := Snapshot{Items: []Item{
		{
			ID: "required-1", Kind: KindRepair, Repair: &RepairState{
				TargetAssistantItemID: "said", Status: RepairRequired, PlayedAudioMS: 120,
			},
		},
		{
			ID: "resolved-1", Kind: KindRepair, Repair: &RepairState{
				TargetAssistantItemID: "said", Status: RepairResolved,
				RepairAssistantItemID: "correction-1",
			},
		},
		{
			ID: "required-2", Kind: KindRepair, Repair: &RepairState{
				TargetAssistantItemID: "said", Status: RepairRequired, PlayedAudioMS: 340,
			},
		},
	}}
	pending := PendingRepairs(snapshot)
	if len(pending) != 1 {
		t.Fatalf("one target, one obligation; got %d: %#v", len(pending), pending)
	}
	// And it is the live one, not the one that was already corrected.
	if pending[0].RequiredItemID != "required-2" || pending[0].PlayedAudioMS != 340 {
		t.Fatalf("the obligation is not the current one: %#v", pending[0])
	}
}

// Order is canonical, so two targets stay in the order they became pending
// even when one of them has been round the cycle.
func TestPendingRepairsKeepTheOrderTheyBecameDue(t *testing.T) {
	t.Parallel()
	snapshot := Snapshot{Items: []Item{
		{ID: "r-a", Kind: KindRepair, Repair: &RepairState{TargetAssistantItemID: "a", Status: RepairRequired, PlayedAudioMS: 10}},
		{ID: "r-b", Kind: KindRepair, Repair: &RepairState{TargetAssistantItemID: "b", Status: RepairRequired, PlayedAudioMS: 20}},
		{ID: "x-a", Kind: KindRepair, Repair: &RepairState{TargetAssistantItemID: "a", Status: RepairResolved, RepairAssistantItemID: "c"}},
		{ID: "r-a2", Kind: KindRepair, Repair: &RepairState{TargetAssistantItemID: "a", Status: RepairRequired, PlayedAudioMS: 30}},
	}}
	pending := PendingRepairs(snapshot)
	if len(pending) != 2 {
		t.Fatalf("two targets are due; got %#v", pending)
	}
	if pending[0].TargetAssistantItemID != "a" || pending[1].TargetAssistantItemID != "b" {
		t.Fatalf("canonical order was not kept: %#v", pending)
	}
}
