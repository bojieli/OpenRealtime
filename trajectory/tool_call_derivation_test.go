package trajectory

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	testRegistryDigest    = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testDeclarationDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func TestSchemaNormalizedToolCallPromotion(t *testing.T) {
	t.Parallel()
	proposal, call := normalizedPromotionFixture()
	originalDerivation := call.ToolCallDerivation
	store := NewStore()
	if err := store.AppendBatch([]Item{proposal, call}); err != nil {
		t.Fatalf("append schema-normalized promotion: %v", err)
	}

	// Both append and snapshot boundaries must own the derivation metadata.
	originalDerivation.SourceArgumentsDigest = "sha256:" + strings.Repeat("f", 64)
	first := store.Snapshot()
	if got := first.Items[1].ToolCallDerivation.SourceArgumentsDigest; got != ToolCallArgumentsDigest(proposal.ToolCall.Arguments) {
		t.Fatalf("stored source digest = %q", got)
	}
	first.Items[1].ToolCallDerivation.EffectiveArgumentsDigest = "mutated"
	second := store.Snapshot()
	if got := second.Items[1].ToolCallDerivation.EffectiveArgumentsDigest; got != ToolCallArgumentsDigest(call.ToolCall.Arguments) {
		t.Fatalf("snapshot exposed derivation storage: %q", got)
	}

	promoted := PromotedToolProposalIDs(second)
	if len(promoted) != 1 {
		t.Fatalf("promoted proposals = %#v", promoted)
	}
	if _, found := promoted[proposal.ID]; !found {
		t.Fatalf("schema-normalized promotion was not recognized: %#v", promoted)
	}

	attested, identity, err := store.SnapshotWithPrefixIdentity()
	if err != nil {
		t.Fatalf("identify derived prefix: %v", err)
	}
	attested.Items[1].ToolCallDerivation.RegistryDigest = "sha256:" + strings.Repeat("3", 64)
	if err := VerifyPrefix(attested, identity); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("mutated derivation verified against immutable prefix: %v", err)
	}
}

func TestChangedToolProposalRequiresValidSchemaDerivation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*Item, *Item)
		want   string
	}{
		{
			name: "missing derivation",
			mutate: func(_ *Item, call *Item) {
				call.ToolCallDerivation = nil
			},
			want: "changes the canonical proposal",
		},
		{
			name: "unknown derivation",
			mutate: func(_ *Item, call *Item) {
				call.ToolCallDerivation.Kind = "provider-best-effort"
			},
			want: "unknown derivation kind",
		},
		{
			name: "wrong source digest",
			mutate: func(_ *Item, call *Item) {
				call.ToolCallDerivation.SourceArgumentsDigest = "sha256:" + strings.Repeat("3", 64)
			},
			want: "source argument digest",
		},
		{
			name: "wrong effective digest",
			mutate: func(_ *Item, call *Item) {
				call.ToolCallDerivation.EffectiveArgumentsDigest = "sha256:" + strings.Repeat("4", 64)
			},
			want: "effective argument digest",
		},
		{
			name: "self-described arbitrary effective value",
			mutate: func(_ *Item, call *Item) {
				call.ToolCall.Arguments = json.RawMessage(`{"order_id":"EVIL"}`)
				call.ToolCallDerivation.EffectiveArgumentsDigest = ToolCallArgumentsDigest(call.ToolCall.Arguments)
			},
			want: "effective arguments differ from deterministic derivation replay",
		},
		{
			name: "malformed registry digest",
			mutate: func(_ *Item, call *Item) {
				call.ToolCallDerivation.RegistryDigest = "sha256:registry"
			},
			want: "registry digest is not canonical SHA-256",
		},
		{
			name: "malformed declaration digest",
			mutate: func(_ *Item, call *Item) {
				call.ToolCallDerivation.DeclarationDigest = "SHA256:" + strings.Repeat("2", 64)
			},
			want: "declaration digest is not canonical SHA-256",
		},
		{
			name: "non-runtime producer",
			mutate: func(_ *Item, call *Item) {
				call.Producer.Phase = PhaseSlow
			},
			want: "must be authored by the runtime",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			proposal, call := normalizedPromotionFixture()
			test.mutate(&proposal, &call)
			store := NewStore()
			if err := store.Append(proposal); err != nil {
				t.Fatalf("append proposal: %v", err)
			}
			err := store.Append(call)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("append error = %v, want substring %q", err, test.want)
			}
			if got := store.Snapshot().Version; got != 1 {
				t.Fatalf("rejected derivation changed store version to %d", got)
			}
		})
	}
}

func TestSchemaDerivationRejectsForgedRewriteSets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		source    string
		effective string
		rewrites  []ToolCallArgumentRewrite
	}{
		{
			name:      "listed field normalized while an unlisted field also changes",
			source:    `{ "listed" : "A B1", "unlisted" : "KEEP2" }`,
			effective: `{ "listed" : "AB1", "unlisted" : "CHANGED2" }`,
			rewrites: []ToolCallArgumentRewrite{{
				Argument: "listed", Normalizer: "compact-ascii-alphanumeric-v1",
			}},
		},
		{
			name:      "changed field and declared rewrite member disagree",
			source:    `{ "first" : "A B1", "second" : "C-D2" }`,
			effective: `{ "first" : "AB1", "second" : "C-D2" }`,
			rewrites: []ToolCallArgumentRewrite{{
				Argument: "second", Normalizer: "compact-ascii-alphanumeric-v1",
			}},
		},
		{
			name:      "spurious rewrite metadata names an unchanged member",
			source:    `{ "already" : "READY3", "first" : "A B1" }`,
			effective: `{ "already" : "READY3", "first" : "AB1" }`,
			rewrites: []ToolCallArgumentRewrite{
				{Argument: "already", Normalizer: "compact-ascii-alphanumeric-v1"},
				{Argument: "first", Normalizer: "compact-ascii-alphanumeric-v1"},
			},
		},
		{
			name: "multiple changed fields have an incomplete rewrite set",
			source: `{ "untouched" : 1.0, "second" : "C-D2", ` +
				`"nested":{"z":2,"a":1}, "first" : "A B1" }`,
			effective: `{ "untouched" : 1.0, "second" : "CD2", ` +
				`"nested":{"z":2,"a":1}, "first" : "AB1" }`,
			rewrites: []ToolCallArgumentRewrite{{
				Argument: "first", Normalizer: "compact-ascii-alphanumeric-v1",
			}},
		},
		{
			name:      "multiple changed fields have an incomplete and spurious rewrite set",
			source:    `{ "first" : "A B1", "second" : "C-D2", "spurious" : "READY3" }`,
			effective: `{ "first" : "AB1", "second" : "CD2", "spurious" : "READY3" }`,
			rewrites: []ToolCallArgumentRewrite{
				{Argument: "first", Normalizer: "compact-ascii-alphanumeric-v1"},
				{Argument: "spurious", Normalizer: "compact-ascii-alphanumeric-v1"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			proposal, call := derivedPromotionFixture(test.source, test.effective, test.rewrites)
			store := NewStore()
			if err := store.Append(proposal); err != nil {
				t.Fatalf("append proposal: %v", err)
			}
			if err := store.Append(call); err == nil {
				t.Fatal("forged derivation was accepted")
			}
			if got := store.Snapshot().Version; got != 1 {
				t.Fatalf("rejected derivation changed store version to %d", got)
			}
			promoted := PromotedToolProposalIDs(Snapshot{Items: []Item{proposal, call}})
			if _, hidden := promoted[proposal.ID]; hidden {
				t.Fatalf("forged derivation hid proposal %q: %#v", proposal.ID, promoted)
			}
		})
	}
}

func TestSchemaDerivationAcceptsExactMultiFieldBytePreservingRewrite(t *testing.T) {
	t.Parallel()
	const source = `{ "untouched" : 1.0, "second" : "C-D2", "nested":{"z":2,"a":1}, "first" : "A B1" }`
	const effective = `{ "untouched" : 1.0, "second" : "CD2", "nested":{"z":2,"a":1}, "first" : "AB1" }`
	proposal, call := derivedPromotionFixture(source, effective, []ToolCallArgumentRewrite{
		{Argument: "first", Normalizer: "compact-ascii-alphanumeric-v1"},
		{Argument: "second", Normalizer: "compact-ascii-alphanumeric-v1"},
	})
	store := NewStore()
	if err := store.AppendBatch([]Item{proposal, call}); err != nil {
		t.Fatalf("append exact multi-field derivation: %v", err)
	}
	snapshot := store.Snapshot()
	if got := string(snapshot.Items[0].ToolCall.Arguments); got != source {
		t.Fatalf("source proposal bytes changed:\n got: %s\nwant: %s", got, source)
	}
	if got := string(snapshot.Items[1].ToolCall.Arguments); got != effective {
		t.Fatalf("effective call did not preserve unrelated JSON bytes:\n got: %s\nwant: %s", got, effective)
	}
	if _, found := PromotedToolProposalIDs(snapshot)[proposal.ID]; !found {
		t.Fatal("valid multi-field derivation was not recognized as a promotion")
	}
}

func TestSchemaDerivationCannotChangePromotionIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*Item)
		want   string
	}{
		{
			name: "invocation",
			mutate: func(call *Item) {
				call.InvocationID = "other-run"
			},
			want: "no matching canonical proposal",
		},
		{
			name: "source revision",
			mutate: func(call *Item) {
				call.SourceRevision++
			},
			want: "changes proposal invocation or source revision",
		},
		{
			name: "call ID",
			mutate: func(call *Item) {
				call.ToolCall.CallID = "other-call"
			},
			want: "no matching canonical proposal",
		},
		{
			name: "tool name",
			mutate: func(call *Item) {
				call.ToolCall.Name = "other_tool"
			},
			want: "changes the canonical proposal",
		},
		{
			name: "causal proposal edge",
			mutate: func(call *Item) {
				call.CausalParentIDs = nil
			},
			want: "does not causally promote proposal",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			proposal, call := normalizedPromotionFixture()
			test.mutate(&call)
			store := NewStore()
			if err := store.Append(proposal); err != nil {
				t.Fatalf("append proposal: %v", err)
			}
			err := store.Append(call)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("append error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestToolCallDerivationRequiresChangedCausalPromotion(t *testing.T) {
	t.Parallel()
	proposal, call := normalizedPromotionFixture()
	tests := []struct {
		name  string
		items []Item
		want  string
	}{
		{
			name: "standalone derived call",
			items: []Item{func() Item {
				copy := cloneItem(call)
				copy.CausalParentIDs = nil
				return copy
			}()},
			want: "no matching canonical proposal",
		},
		{
			name: "derivation on proposal",
			items: []Item{func() Item {
				copy := cloneItem(proposal)
				derivation := *call.ToolCallDerivation
				copy.ToolCallDerivation = &derivation
				return copy
			}()},
			want: "tool-call derivation is not valid on tool_proposal",
		},
		{
			name: "derivation without a change",
			items: []Item{proposal, func() Item {
				copy := cloneItem(call)
				copy.ToolCall.Arguments = json.RawMessage(`{"order_id":"X Y Z88"}`)
				copy.ToolCallDerivation.EffectiveArgumentsDigest = ToolCallArgumentsDigest(copy.ToolCall.Arguments)
				return copy
			}()},
			want: "carries a derivation without changing proposal arguments",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := NewStore()
			err := store.AppendBatch(test.items)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("append error = %v, want substring %q", err, test.want)
			}
			if got := store.Snapshot().Version; got != 0 {
				t.Fatalf("rejected batch changed store version to %d", got)
			}
		})
	}
}

func TestPromotedToolProposalIDsRetainsUnresolvedAndRejectedDerivations(t *testing.T) {
	t.Parallel()
	proposal, call := normalizedPromotionFixture()
	exactProposal := cloneItem(proposal)
	exactProposal.ID = "proposal-exact"
	exactProposal.InvocationID = "run-exact"
	exactProposal.ToolCall = &ToolCall{CallID: "exact", Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`)}
	exactCall := Item{
		ID: "call-exact", Kind: KindToolCall, InvocationID: exactProposal.InvocationID,
		SourceRevision: exactProposal.SourceRevision, CausalParentIDs: []string{exactProposal.ID},
		Producer: Producer{Phase: PhaseRuntime},
		ToolCall: &ToolCall{CallID: "exact", Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`)},
	}
	unresolved := cloneItem(proposal)
	unresolved.ID = "proposal-unresolved"
	unresolved.InvocationID = "run-unresolved"
	unresolved.ToolCall = &ToolCall{CallID: "unresolved", Name: "track_order", Arguments: json.RawMessage(`{"order_id":"A B1"}`)}
	rejected := cloneItem(proposal)
	rejected.ID = "proposal-rejected"
	rejected.InvocationID = "run-rejected"
	rejected.ToolCall = &ToolCall{CallID: "rejected", Name: "track_order", Arguments: json.RawMessage(`{"order_id":"C D2"}`)}
	forgedCall := cloneItem(call)
	forgedCall.ID = "call-forged"
	forgedCall.InvocationID = rejected.InvocationID
	forgedCall.CausalParentIDs = []string{rejected.ID}
	forgedCall.ToolCall = &ToolCall{CallID: "rejected", Name: "track_order", Arguments: json.RawMessage(`{"order_id":"EVIL"}`)}
	forgedCall.ToolCallDerivation = &ToolCallDerivation{
		Kind:                  ToolCallDerivationSchemaNormalizationV1,
		SourceArgumentsDigest: ToolCallArgumentsDigest(rejected.ToolCall.Arguments),
		// Runtime phase plus internally consistent digests are not authority:
		// the closed transformation must reproduce the effective bytes.
		EffectiveArgumentsDigest: ToolCallArgumentsDigest(forgedCall.ToolCall.Arguments),
		RegistryReference:        "deployment.tools",
		RegistryDigest:           testRegistryDigest, DeclarationDigest: testDeclarationDigest,
		Rewrites: []ToolCallArgumentRewrite{{
			Argument: "order_id", Normalizer: "compact-ascii-alphanumeric-v1",
		}},
	}
	forgedCall.Producer = Producer{Phase: PhaseRuntime}

	snapshot := Snapshot{Items: []Item{
		exactProposal, exactCall, proposal, call, unresolved, rejected, forgedCall,
	}}
	promoted := PromotedToolProposalIDs(snapshot)
	if len(promoted) != 2 {
		t.Fatalf("promoted proposal IDs = %#v", promoted)
	}
	for _, id := range []string{exactProposal.ID, proposal.ID} {
		if _, found := promoted[id]; !found {
			t.Errorf("valid promotion %q was not recognized", id)
		}
	}
	for _, id := range []string{unresolved.ID, rejected.ID} {
		if _, hidden := promoted[id]; hidden {
			t.Errorf("unresolved or rejected proposal %q was hidden", id)
		}
	}
}

func normalizedPromotionFixture() (Item, Item) {
	source := json.RawMessage(`{"order_id":"X Y Z88"}`)
	effective := json.RawMessage(`{"order_id":"XYZ88"}`)
	proposal := Item{
		ID: "proposal", Kind: KindToolProposal, InvocationID: "model-run", SourceRevision: 7,
		Producer: Producer{Phase: PhaseSlow},
		ToolCall: &ToolCall{CallID: "call-1", Name: "track_order", Arguments: source},
	}
	call := Item{
		ID: "call", Kind: KindToolCall, InvocationID: proposal.InvocationID,
		SourceRevision: proposal.SourceRevision, CausalParentIDs: []string{proposal.ID},
		Producer: Producer{Phase: PhaseRuntime},
		ToolCall: &ToolCall{CallID: "call-1", Name: "track_order", Arguments: effective},
		ToolCallDerivation: &ToolCallDerivation{
			Kind:                     ToolCallDerivationSchemaNormalizationV1,
			SourceArgumentsDigest:    ToolCallArgumentsDigest(source),
			EffectiveArgumentsDigest: ToolCallArgumentsDigest(effective),
			RegistryReference:        "deployment.tools",
			RegistryDigest:           testRegistryDigest, DeclarationDigest: testDeclarationDigest,
			Rewrites: []ToolCallArgumentRewrite{{
				Argument: "order_id", Normalizer: "compact-ascii-alphanumeric-v1",
			}},
		},
	}
	return proposal, call
}

func derivedPromotionFixture(
	source, effective string, rewrites []ToolCallArgumentRewrite,
) (Item, Item) {
	proposal, call := normalizedPromotionFixture()
	proposal.ToolCall.Arguments = json.RawMessage(source)
	call.ToolCall.Arguments = json.RawMessage(effective)
	call.ToolCallDerivation.SourceArgumentsDigest = ToolCallArgumentsDigest(proposal.ToolCall.Arguments)
	call.ToolCallDerivation.EffectiveArgumentsDigest = ToolCallArgumentsDigest(call.ToolCall.Arguments)
	call.ToolCallDerivation.Rewrites = append([]ToolCallArgumentRewrite(nil), rewrites...)
	return proposal, call
}
