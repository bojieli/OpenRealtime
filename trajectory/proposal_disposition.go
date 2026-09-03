package trajectory

// TerminalToolProposalIDs returns the proposal item IDs made terminal by a
// valid ToolProposalDisposition and the item IDs of the dispositions that
// establish that status.
//
// Terminality is intentionally derived only from a complete, valid ordering
// of the supplied items. The helper replays the items through the canonical
// Store validator before trusting any disposition. If the slice contains a
// malformed item, a forward or missing causal edge, a duplicate identity, or
// contradictory promotion/disposition evidence, both returned sets are
// empty. This fail-closed rule matters for provider adapters: an arbitrary
// snapshot must never make a pending proposal look terminal merely because it
// contains disposition-shaped data.
//
// Snapshot shape is part of the evidence: a version that does not equal the
// item count also fails closed. Neither the snapshot nor its nested payloads
// are mutated.
func TerminalToolProposalIDs(snapshot Snapshot) (
	proposalIDs map[string]struct{}, dispositionItemIDs map[string]struct{},
) {
	proposalIDs = make(map[string]struct{})
	dispositionItemIDs = make(map[string]struct{})
	if err := validateSnapshotShape(snapshot); err != nil {
		return proposalIDs, dispositionItemIDs
	}
	if len(snapshot.Items) == 0 {
		return proposalIDs, dispositionItemIDs
	}

	validator := NewStore()
	if err := validator.AppendBatch(snapshot.Items); err != nil {
		return proposalIDs, dispositionItemIDs
	}
	for _, disposition := range validator.toolProposalDispositions {
		proposalIDs[disposition.proposalItemID] = struct{}{}
		dispositionItemIDs[disposition.itemID] = struct{}{}
	}
	return proposalIDs, dispositionItemIDs
}
