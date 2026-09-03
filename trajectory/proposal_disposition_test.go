package trajectory

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestToolProposalDispositionCommitsAsTypedTerminalEvidence(t *testing.T) {
	t.Parallel()
	proposal, disposition, _ := proposalDispositionFixture()
	store := NewStore()
	if err := store.AppendBatch([]Item{proposal, disposition}); err != nil {
		t.Fatalf("append proposal and disposition: %v", err)
	}

	// AppendBatch must own a defensive copy of both the payload and its causal
	// edges. Mutating caller-owned state cannot rewrite canonical history.
	disposition.CausalParentIDs[0] = "mutated-parent"
	disposition.ToolProposalDisposition.ProposalItemID = "mutated-proposal"
	disposition.ToolProposalDisposition.Name = "mutated-name"
	snapshot := store.Snapshot()
	got := snapshot.Items[1]
	if !reflect.DeepEqual(got.CausalParentIDs, []string{"proposal"}) ||
		got.ToolProposalDisposition.ProposalItemID != "proposal" ||
		got.ToolProposalDisposition.Name != "computer.wait" {
		t.Fatalf("caller mutation changed canonical disposition: %+v", got)
	}

	// Snapshot must be defensive in the other direction as well.
	snapshot.Items[1].CausalParentIDs[0] = "snapshot-parent"
	snapshot.Items[1].ToolProposalDisposition.CallID = "snapshot-call"
	fresh := store.Snapshot()
	if !reflect.DeepEqual(fresh.Items[1].CausalParentIDs, []string{"proposal"}) ||
		fresh.Items[1].ToolProposalDisposition.CallID != "wait-1" {
		t.Fatalf("snapshot mutation changed canonical disposition: %+v", fresh.Items[1])
	}

	encoded, err := json.Marshal(fresh.Items[1].ToolProposalDisposition)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "arguments") || strings.Contains(string(encoded), "duration_ms") {
		t.Fatalf("disposition retained raw proposal arguments: %s", encoded)
	}

	terminal, evidence := TerminalToolProposalIDs(fresh)
	if !hasOnlyID(terminal, "proposal") || !hasOnlyID(evidence, "disposition") {
		t.Fatalf("terminal proposal evidence = %#v, %#v", terminal, evidence)
	}
}

func TestToolProposalDispositionClosedKindsRoundTrip(t *testing.T) {
	t.Parallel()
	for _, kind := range []ToolProposalDispositionKind{
		ToolProposalToolPolicySuppressed,
		ToolProposalRepetitionSuppressed,
	} {
		kind := kind
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			proposal, disposition, _ := proposalDispositionFixture()
			disposition.ToolProposalDisposition.Kind = kind
			wire, err := json.Marshal(disposition)
			if err != nil {
				t.Fatal(err)
			}
			var decoded Item
			if err := json.Unmarshal(wire, &decoded); err != nil {
				t.Fatal(err)
			}
			store := NewStore()
			if err := store.AppendBatch([]Item{proposal, decoded}); err != nil {
				t.Fatalf("append round-tripped %q disposition: %v", kind, err)
			}
			got := store.Snapshot().Items[1].ToolProposalDisposition
			if got == nil || got.Kind != kind || got.ProposalItemID != proposal.ID {
				t.Fatalf("round-tripped disposition = %+v", got)
			}
		})
	}
}

func TestToolProposalDispositionRejectsIdentityAuthorityAndOrderingViolations(t *testing.T) {
	t.Parallel()
	proposal, canonical, promotion := proposalDispositionFixture()

	mutateDisposition := func(change func(*Item)) Item {
		item := cloneItem(canonical)
		change(&item)
		return item
	}
	for _, test := range []struct {
		name string
		item Item
		want string
	}{
		{
			name: "cross invocation",
			item: mutateDisposition(func(item *Item) { item.InvocationID = "other-run" }),
			want: "unknown proposal",
		},
		{
			name: "cross call",
			item: mutateDisposition(func(item *Item) { item.ToolProposalDisposition.CallID = "other-call" }),
			want: "unknown proposal",
		},
		{
			name: "cross source revision",
			item: mutateDisposition(func(item *Item) { item.SourceRevision++ }),
			want: "changes proposal invocation or source revision",
		},
		{
			name: "changed name",
			item: mutateDisposition(func(item *Item) { item.ToolProposalDisposition.Name = "computer.click" }),
			want: "changes proposal call or name",
		},
		{
			name: "changed proposal item",
			item: mutateDisposition(func(item *Item) { item.ToolProposalDisposition.ProposalItemID = "other-proposal" }),
			want: "does not match proposal item",
		},
		{
			name: "missing causal reference",
			item: mutateDisposition(func(item *Item) { item.CausalParentIDs = nil }),
			want: "does not causally reference",
		},
		{
			name: "model authored",
			item: mutateDisposition(func(item *Item) { item.Producer.Phase = PhaseFast }),
			want: "must be authored by the runtime",
		},
		{
			name: "open ended reason",
			item: mutateDisposition(func(item *Item) { item.ToolProposalDisposition.Kind = "developer_defined" }),
			want: "unknown tool proposal disposition kind",
		},
		{
			name: "provider native state",
			item: mutateDisposition(func(item *Item) {
				item.ProviderStateType = "forged/provider-state"
				item.ProviderState = json.RawMessage(`{"arguments":{"duration_ms":1000}}`)
			}),
			want: "requires exactly one disposition payload",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := NewStore()
			if err := store.Append(proposal); err != nil {
				t.Fatal(err)
			}
			before, identityBefore, err := store.SnapshotWithPrefixIdentity()
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Append(test.item); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("append error = %v, want one containing %q", err, test.want)
			}
			after, identityAfter, err := store.SnapshotWithPrefixIdentity()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) || identityAfter != identityBefore {
				t.Fatalf("rejected disposition mutated canonical state:\nbefore=%+v %+v\nafter=%+v %+v",
					before, identityBefore, after, identityAfter)
			}
		})
	}

	t.Run("before proposal", func(t *testing.T) {
		store := NewStore()
		if err := store.Append(canonical); err == nil || !strings.Contains(err.Error(), "does not precede") {
			t.Fatalf("append error = %v, want missing earlier causal parent", err)
		}
		if got := store.Snapshot(); got.Version != 0 || len(got.Items) != 0 {
			t.Fatalf("rejected forward reference mutated store: %+v", got)
		}
	})

	t.Run("duplicate disposition", func(t *testing.T) {
		store := NewStore()
		if err := store.AppendBatch([]Item{proposal, canonical}); err != nil {
			t.Fatal(err)
		}
		duplicate := cloneItem(canonical)
		duplicate.ID = "disposition-duplicate"
		duplicate.MonotonicNS++
		if err := store.Append(duplicate); err == nil || !strings.Contains(err.Error(), "already has a terminal disposition") {
			t.Fatalf("duplicate disposition error = %v", err)
		}
		if got := store.Snapshot().Version; got != 2 {
			t.Fatalf("duplicate disposition changed version to %d", got)
		}
	})

	t.Run("disposition after promotion", func(t *testing.T) {
		store := NewStore()
		if err := store.AppendBatch([]Item{proposal, promotion}); err != nil {
			t.Fatal(err)
		}
		late := cloneItem(canonical)
		late.MonotonicNS = promotion.MonotonicNS + 1
		if err := store.Append(late); err == nil || !strings.Contains(err.Error(), "was already promoted") {
			t.Fatalf("late disposition error = %v", err)
		}
		if got := store.Snapshot().Version; got != 2 {
			t.Fatalf("late disposition changed version to %d", got)
		}
	})

	t.Run("promotion after disposition", func(t *testing.T) {
		store := NewStore()
		if err := store.AppendBatch([]Item{proposal, canonical}); err != nil {
			t.Fatal(err)
		}
		late := cloneItem(promotion)
		late.MonotonicNS = canonical.MonotonicNS + 1
		if err := store.Append(late); err == nil || !strings.Contains(err.Error(), "already has terminal disposition") {
			t.Fatalf("late promotion error = %v", err)
		}
		if got := store.Snapshot().Version; got != 2 {
			t.Fatalf("late promotion changed version to %d", got)
		}
	})
}

func TestToolProposalDispositionBatchIsAtomicAndPrefixBound(t *testing.T) {
	t.Parallel()
	proposal, disposition, promotion := proposalDispositionFixture()
	store := NewStore()
	if err := store.Append(proposal); err != nil {
		t.Fatal(err)
	}
	_, proposalIdentity, err := store.SnapshotWithPrefixIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendBatchAt(1, []Item{disposition}); err != nil {
		t.Fatalf("append disposition at sampled version: %v", err)
	}
	snapshot, dispositionIdentity, err := store.SnapshotWithPrefixIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if proposalIdentity == dispositionIdentity {
		t.Fatalf("disposition did not change prefix identity: %+v", dispositionIdentity)
	}
	if err := VerifyPrefix(snapshot, dispositionIdentity); err != nil {
		t.Fatalf("verify disposition prefix: %v", err)
	}
	tampered := Snapshot{Version: snapshot.Version, Items: cloneItems(snapshot.Items)}
	tampered.Items[1].ToolProposalDisposition.Kind = ToolProposalRepetitionSuppressed
	if err := VerifyPrefix(tampered, dispositionIdentity); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tampered disposition verified against immutable prefix: %v", err)
	}

	rollback := NewStore()
	promotion.MonotonicNS = disposition.MonotonicNS + 1
	if err := rollback.AppendBatch([]Item{proposal, disposition, promotion}); err == nil ||
		!strings.Contains(err.Error(), "already has terminal disposition") {
		t.Fatalf("contradictory transaction error = %v", err)
	}
	if got := rollback.Snapshot(); got.Version != 0 || len(got.Items) != 0 {
		t.Fatalf("failed transaction partially committed: %+v", got)
	}
}

func TestTerminalToolProposalIDsRequiresValidOrderedPrefixEvidence(t *testing.T) {
	t.Parallel()
	proposal, disposition, promotion := proposalDispositionFixture()
	valid := Snapshot{Version: 2, Items: []Item{proposal, disposition}}
	terminal, evidence := TerminalToolProposalIDs(valid)
	if !hasOnlyID(terminal, proposal.ID) || !hasOnlyID(evidence, disposition.ID) {
		t.Fatalf("valid terminal evidence = %#v, %#v", terminal, evidence)
	}

	// The read-only analysis must not normalize or otherwise mutate even nested
	// caller-owned state.
	before, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	TerminalToolProposalIDs(valid)
	after, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("terminal analysis mutated input:\nbefore=%s\nafter=%s", before, after)
	}

	bad := func(items ...Item) Snapshot {
		return Snapshot{Version: uint64(len(items)), Items: items}
	}
	duplicate := cloneItem(disposition)
	duplicate.ID = "duplicate-disposition"
	duplicate.MonotonicNS++
	changedInvocation := cloneItem(disposition)
	changedInvocation.InvocationID = "other-run"
	changedCall := cloneItem(disposition)
	changedCall.ToolProposalDisposition.CallID = "other-call"
	changedSource := cloneItem(disposition)
	changedSource.SourceRevision++
	changedName := cloneItem(disposition)
	changedName.ToolProposalDisposition.Name = "computer.click"
	changedItem := cloneItem(disposition)
	changedItem.ToolProposalDisposition.ProposalItemID = "other-proposal"
	missingEdge := cloneItem(disposition)
	missingEdge.CausalParentIDs = nil
	unknownKind := cloneItem(disposition)
	unknownKind.ToolProposalDisposition.Kind = "untyped"
	malformedTail := Item{ID: "malformed", Kind: KindObservation, Producer: Producer{Phase: PhaseUser}}
	promotionAfter := cloneItem(promotion)
	promotionAfter.MonotonicNS = disposition.MonotonicNS + 1
	dispositionAfter := cloneItem(disposition)
	dispositionAfter.MonotonicNS = promotion.MonotonicNS + 1

	for _, test := range []struct {
		name     string
		snapshot Snapshot
	}{
		{name: "shape mismatch", snapshot: Snapshot{Version: 3, Items: []Item{proposal, disposition}}},
		{name: "disposition before proposal", snapshot: bad(disposition, proposal)},
		{name: "cross invocation", snapshot: bad(proposal, changedInvocation)},
		{name: "cross call", snapshot: bad(proposal, changedCall)},
		{name: "cross source", snapshot: bad(proposal, changedSource)},
		{name: "changed name", snapshot: bad(proposal, changedName)},
		{name: "changed item", snapshot: bad(proposal, changedItem)},
		{name: "missing causal edge", snapshot: bad(proposal, missingEdge)},
		{name: "duplicate disposition", snapshot: bad(proposal, disposition, duplicate)},
		{name: "unknown disposition kind", snapshot: bad(proposal, unknownKind)},
		{name: "disposition after promotion", snapshot: bad(proposal, promotion, dispositionAfter)},
		{name: "promotion after disposition", snapshot: bad(proposal, disposition, promotionAfter)},
		{name: "malformed trailing evidence", snapshot: bad(proposal, disposition, malformedTail)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			terminal, evidence := TerminalToolProposalIDs(test.snapshot)
			if len(terminal) != 0 || len(evidence) != 0 {
				t.Fatalf("malformed evidence acquired terminal status: %#v, %#v", terminal, evidence)
			}
		})
	}
}

func proposalDispositionFixture() (proposal Item, disposition Item, promotion Item) {
	call := ToolCall{
		CallID: "wait-1", Name: "computer.wait",
		Arguments: json.RawMessage(`{"duration_ms":1000}`),
	}
	proposal = Item{
		ID: "proposal", Kind: KindToolProposal, MonotonicNS: 1,
		SourceRevision: 7, InvocationID: "run-1",
		Producer: Producer{Phase: PhaseFast}, ToolCall: &call,
	}
	disposition = Item{
		ID: "disposition", Kind: KindToolProposalDisposition, MonotonicNS: 2,
		CausalParentIDs: []string{proposal.ID}, SourceRevision: proposal.SourceRevision,
		InvocationID: proposal.InvocationID, Producer: Producer{Phase: PhaseRuntime},
		ToolProposalDisposition: &ToolProposalDisposition{
			ProposalItemID: proposal.ID, CallID: call.CallID, Name: call.Name,
			Kind: ToolProposalToolPolicySuppressed,
		},
	}
	promotion = Item{
		ID: "call", Kind: KindToolCall, MonotonicNS: 2,
		CausalParentIDs: []string{proposal.ID}, SourceRevision: proposal.SourceRevision,
		InvocationID: proposal.InvocationID, Producer: Producer{Phase: PhaseRuntime},
		ToolCall: &call,
	}
	return proposal, disposition, promotion
}

func hasOnlyID(values map[string]struct{}, id string) bool {
	if len(values) != 1 {
		return false
	}
	_, found := values[id]
	return found
}
