package action

import (
	"encoding/json"
	"strings"
	"testing"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const normalizeArgumentsGraph = `graph normalize_arguments_test {
    action.NormalizeArguments :: normalize;
    input action = normalize.action;
    output normalized = normalize.normalized;
    output outcome = normalize.outcome;
    output resolved = normalize.resolved;
}`

func normalizableDeclaration(arguments string) (DeclaredAction, legacyaction.ToolSpec) {
	declared := DeclaredAction{
		Admitted:          AdmittedProposal{Proposal: cognitionProposal("call-1", "track_order", arguments)},
		RegistryReference: "tools", RegistryDigest: "sha256:registry", DeclarationDigest: "sha256:declaration",
	}
	spec := legacyaction.ToolSpec{
		Name: "track_order", Description: "track an order",
		Parameters: json.RawMessage(`{"type":"object","properties":{"order_id":{"type":"string"}},"required":["order_id"]}`),
		ArgumentNormalizers: []legacyaction.ToolArgumentNormalizer{{
			Argument: "order_id", Normalizer: legacyaction.ToolParameterCompactASCIIAlphanumericV1,
		}},
	}
	return declared, spec
}

func coordinatePairDeclaration(arguments string) (DeclaredAction, legacyaction.ToolSpec) {
	declared := DeclaredAction{
		Admitted: AdmittedProposal{Proposal: cognitionProposal(
			"click-1", "computer.click_normalized", arguments,
		)},
		RegistryReference: "tools", RegistryDigest: "sha256:registry", DeclarationDigest: "sha256:declaration",
	}
	spec := legacyaction.ToolSpec{
		Name: "computer.click_normalized", Description: "click normalized coordinates",
		Parameters: json.RawMessage(
			`{"type":"object","properties":{"source":{"type":"string"},"x":{"type":"integer","minimum":0,"maximum":1000},"y":{"type":"integer","minimum":0,"maximum":1000}},"required":["source","x","y"],"additionalProperties":false}`,
		),
		ArgumentNormalizers: []legacyaction.ToolArgumentNormalizer{{
			Argument: "x", Normalizer: legacyaction.ToolParameterCoordinatePairXYV1,
		}},
	}
	return declared, spec
}

func cognitionProposal(callID, name, arguments string) cognitionelements.ToolProposal {
	return cognitionelements.ToolProposal{
		Call:     trajectory.ToolCall{CallID: callID, Name: name, Arguments: json.RawMessage(arguments)},
		Declared: true, ProviderAuthority: continuation.ToolAuthorityPropose,
	}
}

func TestNormalizeDeclaredArgumentsUsesOnlyExplicitDeploymentContract(t *testing.T) {
	declared, spec := normalizableDeclaration(`{"order_id":"X Y, Z-8_8"}`)
	original := string(declared.Admitted.Proposal.Call.Arguments)
	normalized, changed, err := normalizeDeclaredAction(declared, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || normalized.EffectiveCall == nil ||
		string(normalized.EffectiveCall.Arguments) != `{"order_id":"XYZ88"}` {
		t.Fatalf("normalized action = %+v, changed=%t", normalized, changed)
	}
	if string(normalized.Admitted.Proposal.Call.Arguments) != original {
		t.Fatalf("canonical model proposal was mutated: %s", normalized.Admitted.Proposal.Call.Arguments)
	}
	if normalized.Normalization == nil || len(normalized.Normalization.Rewrites) != 1 ||
		normalized.Normalization.Rewrites[0].Argument != "order_id" ||
		normalized.Normalization.Rewrites[0].Normalizer != legacyaction.ToolParameterCompactASCIIAlphanumericV1 {
		t.Fatalf("normalization evidence = %+v", normalized.Normalization)
	}
	if err := validateDeclaredNormalizationAgainstSpec(normalized, spec); err != nil {
		t.Fatalf("revalidate exact normalization: %v", err)
	}

	withoutContract := spec
	withoutContract.ArgumentNormalizers = nil
	unchanged, changed, err := normalizeDeclaredAction(declared, withoutContract)
	if err != nil || changed || unchanged.EffectiveCall != nil || unchanged.Normalization != nil ||
		string(unchanged.Admitted.Proposal.Call.Arguments) != original {
		t.Fatalf("unannotated declaration changed: %+v, changed=%t, err=%v", unchanged, changed, err)
	}
}

func TestNormalizeDeclaredArgumentsPreservesEveryUnannotatedByte(t *testing.T) {
	declared, spec := normalizableDeclaration(
		`{ "note" : "A\\u0020B", "order_id" : "X Y-8", "nested" : { "z":2, "a":1 }, "count": 1.0 }`,
	)
	normalized, changed, err := normalizeDeclaredAction(declared, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || normalized.EffectiveCall == nil {
		t.Fatalf("normalization did not produce an effective call: %+v", normalized)
	}
	want := `{ "note" : "A\\u0020B", "order_id" : "XY8", "nested" : { "z":2, "a":1 }, "count": 1.0 }`
	if got := string(normalized.EffectiveCall.Arguments); got != want {
		t.Fatalf("normalization rewrote unannotated bytes:\n got: %s\nwant: %s", got, want)
	}
}

func TestNormalizeDeclaredCoordinatePairPreservesProposalAndReattestsDerivation(t *testing.T) {
	declared, spec := coordinatePairDeclaration(`{ "source":"screen", "x":[255,566] }`)
	normalized, changed, err := normalizeDeclaredAction(declared, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || normalized.EffectiveCall == nil ||
		string(normalized.EffectiveCall.Arguments) != `{ "source":"screen", "x":255,"y":566 }` {
		t.Fatalf("normalized coordinate action = %+v, changed=%t", normalized, changed)
	}
	if got := string(normalized.Admitted.Proposal.Call.Arguments); got != `{ "source":"screen", "x":[255,566] }` {
		t.Fatalf("original coordinate proposal changed: %s", got)
	}
	if normalized.Normalization == nil || len(normalized.Normalization.Rewrites) != 1 ||
		normalized.Normalization.Rewrites[0] != (ArgumentRewrite{
			Argument: "x", Normalizer: legacyaction.ToolParameterCoordinatePairXYV1,
		}) {
		t.Fatalf("coordinate normalization evidence = %+v", normalized.Normalization)
	}
	if err := validateDeclaredNormalizationAgainstSpec(normalized, spec); err != nil {
		t.Fatalf("revalidate coordinate normalization: %v", err)
	}

	forged := cloneDeclared(normalized)
	forged.EffectiveCall.Arguments = json.RawMessage(`{ "source":"screen", "x":255,"y":567 }`)
	forged.Normalization.EffectiveArgumentsDigest = argumentBytesDigest(forged.EffectiveCall.Arguments)
	if err := validateDeclaredNormalizationAgainstSpec(forged, spec); err == nil ||
		!strings.Contains(err.Error(), "differs from deterministic") {
		t.Fatalf("forged coordinate derivation error = %v", err)
	}
}

func TestNormalizeDeclaredCoordinatePairRefusesAmbiguity(t *testing.T) {
	for _, arguments := range []string{
		`{"source":"screen","x":[255,566],"y":566}`,
		`{"source":"screen","x":[255]}`,
		`{"source":"screen","x":[255,566.5]}`,
		`{"source":"screen","x":[255,"566"]}`,
	} {
		declared, spec := coordinatePairDeclaration(arguments)
		if _, _, err := normalizeDeclaredAction(declared, spec); err == nil {
			t.Fatalf("ambiguous coordinate proposal was normalized: %s", arguments)
		}
	}
}

func TestNormalizeDeclaredArgumentsRefusesAmbiguousOrMalformedContracts(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		arguments  string
		parameters string
		normalizer string
		want       string
	}{
		{
			name: "unknown normalizer", arguments: `{"order_id":"A B"}`,
			parameters: `{"type":"object","properties":{"order_id":{"type":"string"}}}`,
			normalizer: "unknown", want: "unsupported normalizer",
		},
		{
			name: "wrong schema type", arguments: `{"order_id":"A B"}`,
			parameters: `{"type":"object","properties":{"order_id":{"type":"number"}}}`,
			want:       "requires type string",
		},
		{
			name: "wrong value type", arguments: `{"order_id":12}`,
			parameters: `{"type":"object","properties":{"order_id":{"type":"string"}}}`,
			want:       "must be a string",
		},
		{
			name: "semantic punctuation", arguments: `{"order_id":"ABC/123"}`,
			parameters: `{"type":"object","properties":{"order_id":{"type":"string"}}}`,
			want:       "unsupported byte",
		},
		{
			name: "duplicate arguments", arguments: `{"order_id":"A B","order_id":"C D"}`,
			parameters: `{"type":"object","properties":{"order_id":{"type":"string"}}}`,
			want:       "duplicate JSON key",
		},
		{
			name: "duplicate schema key", arguments: `{"order_id":"A B"}`,
			parameters: `{"type":"object","properties":{"order_id":{"type":"string","type":"string"}}}`,
			want:       "duplicate JSON key",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			declared, spec := normalizableDeclaration(testCase.arguments)
			spec.Parameters = json.RawMessage(testCase.parameters)
			if testCase.normalizer != "" {
				spec.ArgumentNormalizers[0].Normalizer = testCase.normalizer
			}
			_, _, err := normalizeDeclaredAction(declared, spec)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("normalize error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestNormalizationEvidenceCannotAuthorizeTamperedEffectiveCall(t *testing.T) {
	declared, spec := normalizableDeclaration(`{"order_id":"X Y Z88"}`)
	normalized, changed, err := normalizeDeclaredAction(declared, spec)
	if err != nil || !changed {
		t.Fatalf("normalize: changed=%t, err=%v", changed, err)
	}

	for _, testCase := range []struct {
		name   string
		mutate func(*DeclaredAction)
	}{
		{name: "effective value", mutate: func(value *DeclaredAction) {
			value.EffectiveCall.Arguments = json.RawMessage(`{"order_id":"OTHER"}`)
			value.Normalization.EffectiveArgumentsDigest = argumentBytesDigest(value.EffectiveCall.Arguments)
		}},
		{name: "algorithm", mutate: func(value *DeclaredAction) {
			value.Normalization.Rewrites[0].Normalizer = "other"
		}},
		{name: "declaration", mutate: func(value *DeclaredAction) {
			value.Normalization.DeclarationDigest = "sha256:other"
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			tampered := cloneDeclared(normalized)
			testCase.mutate(&tampered)
			if err := validateDeclaredNormalizationAgainstSpec(tampered, spec); err == nil {
				t.Fatal("tampered normalization unexpectedly revalidated")
			}
		})
	}
}

func TestNormalizeArgumentsElementPublishesAuditableEffectiveCall(t *testing.T) {
	tools := NewToolRegistries()
	_, spec := normalizableDeclaration(`{"order_id":"X Y Z88"}`)
	if err := tools.Register("tools", []legacyaction.ToolSpec{spec}); err != nil {
		t.Fatal(err)
	}
	set, err := tools.resolve("tools")
	if err != nil {
		t.Fatal(err)
	}
	tool, found, err := set.lookup(spec.Name)
	if err != nil || !found {
		t.Fatalf("resolve registered tool: found=%t, err=%v", found, err)
	}
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(ToolRegistryService, tools); err != nil {
		t.Fatal(err)
	}
	mounted, done, cancel := mountGraph(t, normalizeArgumentsGraph,
		map[string]json.RawMessage{"normalize": json.RawMessage(`{}`)}, services)
	defer stopMounted(t, mounted, done, cancel)
	_ = receive(t, mustEgressAction(t, mounted, "resolved"))

	admitted := testAdmittedProposal()
	admitted.Proposal.Call = trajectory.ToolCall{
		CallID: "track-call", Name: spec.Name, Arguments: json.RawMessage(`{"order_id":"X Y Z88"}`),
	}
	declared := DeclaredAction{
		Admitted: admitted, Confirmation: legacyaction.ConfirmNever,
		RegistryReference: set.reference, RegistryDigest: set.digest, DeclarationDigest: tool.digest,
	}
	send(t, mustIngressAction(t, mounted, "action"), element.Envelope{
		Type: declaredType, ItemID: "declared", SessionID: admitted.SessionID, RunID: admitted.ModelRunID,
		Payload: declared,
	})

	normalizedEnvelope := receive(t, mustEgressAction(t, mounted, "normalized"))
	normalized, ok := normalizedEnvelope.Payload.(DeclaredAction)
	if !ok {
		t.Fatalf("normalized payload type = %T", normalizedEnvelope.Payload)
	}
	if normalized.EffectiveCall == nil ||
		string(normalized.EffectiveCall.Arguments) != `{"order_id":"XYZ88"}` ||
		string(normalized.Admitted.Proposal.Call.Arguments) != `{"order_id":"X Y Z88"}` {
		t.Fatalf("normalized action = %+v", normalized)
	}
	if err := validateDeclaredNormalizationAgainstSpec(normalized, spec); err != nil {
		t.Fatalf("published normalization does not replay: %v", err)
	}
	outcome := receive(t, mustEgressAction(t, mounted, "outcome")).Payload.(Outcome)
	if outcome.Kind != OutcomeSucceeded || outcome.Stage != "argument_normalization" ||
		outcome.Operation != "normalize" || outcome.Code != "normalized" || outcome.CallID != "track-call" {
		t.Fatalf("normalization outcome = %+v", outcome)
	}
}

func TestNormalizeArgumentsElementLeavesCanonicalArgumentsByteExactAndDerivationFree(t *testing.T) {
	tools := NewToolRegistries()
	_, spec := normalizableDeclaration(`{"order_id":"XYZ88"}`)
	if err := tools.Register("tools", []legacyaction.ToolSpec{spec}); err != nil {
		t.Fatal(err)
	}
	set, err := tools.resolve("tools")
	if err != nil {
		t.Fatal(err)
	}
	tool, found, err := set.lookup(spec.Name)
	if err != nil || !found {
		t.Fatalf("resolve registered tool: found=%t, err=%v", found, err)
	}
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(ToolRegistryService, tools); err != nil {
		t.Fatal(err)
	}
	mounted, done, cancel := mountGraph(t, normalizeArgumentsGraph,
		map[string]json.RawMessage{"normalize": json.RawMessage(`{}`)}, services)
	defer stopMounted(t, mounted, done, cancel)
	_ = receive(t, mustEgressAction(t, mounted, "resolved"))

	const canonical = `{ "unrelated" : 1.0, "order_id" : "XYZ88", "nested":{"z":2,"a":1} }`
	admitted := testAdmittedProposal()
	admitted.Proposal.Call = trajectory.ToolCall{
		CallID: "track-call", Name: spec.Name, Arguments: json.RawMessage(canonical),
	}
	declared := DeclaredAction{
		Admitted: admitted, Confirmation: legacyaction.ConfirmNever,
		RegistryReference: set.reference, RegistryDigest: set.digest, DeclarationDigest: tool.digest,
	}
	send(t, mustIngressAction(t, mounted, "action"), element.Envelope{
		Type: declaredType, ItemID: "declared", SessionID: admitted.SessionID, RunID: admitted.ModelRunID,
		Payload: declared,
	})

	result := receive(t, mustEgressAction(t, mounted, "normalized")).Payload.(DeclaredAction)
	if got := string(result.Admitted.Proposal.Call.Arguments); got != canonical {
		t.Fatalf("canonical argument bytes changed:\n got: %s\nwant: %s", got, canonical)
	}
	if result.EffectiveCall != nil || result.Normalization != nil {
		t.Fatalf("canonical arguments gained derivation metadata: %+v", result)
	}
	outcome := receive(t, mustEgressAction(t, mounted, "outcome")).Payload.(Outcome)
	if outcome.Kind != OutcomeSucceeded || outcome.Code != "unchanged" {
		t.Fatalf("canonical normalization outcome = %+v", outcome)
	}
}

func TestToolRegistryIdentityBindsRuntimeArgumentNormalizers(t *testing.T) {
	_, withNormalizer := normalizableDeclaration(`{"order_id":"ABC123"}`)
	withoutNormalizer := withNormalizer
	withoutNormalizer.ArgumentNormalizers = nil
	first := NewToolRegistries()
	second := NewToolRegistries()
	if err := first.Register("tools", []legacyaction.ToolSpec{withNormalizer}); err != nil {
		t.Fatal(err)
	}
	if err := second.Register("tools", []legacyaction.ToolSpec{withoutNormalizer}); err != nil {
		t.Fatal(err)
	}
	withSet, _ := first.resolve("tools")
	withoutSet, _ := second.resolve("tools")
	if withSet.digest == withoutSet.digest ||
		withSet.tools[withNormalizer.Name].digest == withoutSet.tools[withoutNormalizer.Name].digest {
		t.Fatal("runtime argument-normalization policy did not change immutable registry/declaration identity")
	}
}
