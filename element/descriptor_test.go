package element_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
)

func TestDescriptorIdentityIgnoresDeclarationOrdering(t *testing.T) {
	left := textModelDescriptor()
	right := textModelDescriptor()
	right.Generics = []string{"Unused", "T"}
	left.Generics = []string{"T", "Unused"}
	right.Ports[0], right.Ports[1] = right.Ports[1], right.Ports[0]
	right.Reaction.Triggers = []string{"trigger"}

	leftIdentity, err := left.Identity()
	if err != nil {
		t.Fatal(err)
	}
	rightIdentity, err := right.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if leftIdentity != rightIdentity {
		t.Fatalf("ordering changed identity:\nleft  %+v\nright %+v", leftIdentity, rightIdentity)
	}
}

func TestDescriptorRejectsUndeclaredTypeVariable(t *testing.T) {
	descriptor := textModelDescriptor()
	descriptor.Generics = nil
	if err := descriptor.Validate(); err == nil {
		t.Fatal("expected undeclared generic to fail")
	}
}

func TestExternalEffectCannotClaimReversibility(t *testing.T) {
	descriptor := textModelDescriptor()
	descriptor.Effects = []element.Effect{{
		Name: "speaker.playback", External: true, Authority: "Committed", Reversible: true,
	}}
	if err := descriptor.Validate(); err == nil {
		t.Fatal("expected external reversible effect to fail")
	}
}

func TestExternalEffectRequiresAuthorityType(t *testing.T) {
	descriptor := textModelDescriptor()
	descriptor.Effects = []element.Effect{{Name: "speaker.playback", External: true}}
	if err := descriptor.Validate(); err == nil {
		t.Fatal("expected external effect without authority to fail")
	}
}

func TestConcretePortRequiresTemporalProtocol(t *testing.T) {
	descriptor := textModelDescriptor()
	descriptor.Ports[0].Type = element.Named("text.String")
	if err := descriptor.Validate(); err == nil {
		t.Fatal("expected bare payload port to fail")
	}
}

func TestCompositeFingerprintChangesDescriptorIdentity(t *testing.T) {
	left := textModelDescriptor()
	right := textModelDescriptor()
	left.CompositeFingerprint = "sha256:" + strings.Repeat("1", 64)
	right.CompositeFingerprint = "sha256:" + strings.Repeat("2", 64)
	leftIdentity, err := left.Identity()
	if err != nil {
		t.Fatal(err)
	}
	rightIdentity, err := right.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if leftIdentity.Digest == rightIdentity.Digest {
		t.Fatal("subgraph body fingerprint did not affect descriptor identity")
	}
}

func TestAbsentStateTransferPreservesLegacyDescriptorIdentity(t *testing.T) {
	descriptor := textModelDescriptor()
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	const legacyDigest = "sha256:1c56f44a0281c19de1fb310dada4c07e9c7388faa067b77e5f388b41230abe8c"
	if identity.Digest != legacyDigest {
		t.Fatalf("descriptor digest = %s, want legacy digest %s", identity.Digest, legacyDigest)
	}
	canonical, err := descriptor.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "state_transfer") {
		t.Fatalf("absent state-transfer contract changed legacy encoding: %s", payload)
	}
}

func TestStateTransferChangesIdentityAndCloneOwnsContract(t *testing.T) {
	without := textModelDescriptor()
	without.StateSchema = "schema://test/model-state/v1"
	with := without.Clone()
	with.StateTransfer = &element.StateTransferCapabilities{
		Snapshot: true, Restore: true, Quiesce: true,
	}
	withoutIdentity, err := without.Identity()
	if err != nil {
		t.Fatal(err)
	}
	withIdentity, err := with.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if withIdentity.Digest == withoutIdentity.Digest {
		t.Fatal("explicit state-transfer capabilities did not affect descriptor identity")
	}

	clone := with.Clone()
	clone.StateTransfer.Restore = false
	if !with.StateTransfer.Restore {
		t.Fatal("descriptor clone aliases state-transfer capabilities")
	}
	canonical, err := with.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	canonical.StateTransfer.Quiesce = false
	if !with.StateTransfer.Quiesce {
		t.Fatal("canonical descriptor aliases state-transfer capabilities")
	}
}

func TestTypeDiagnosticSpelling(t *testing.T) {
	value := element.Segmented(element.Named("text.Delta"), element.Named("flow.RunID"))
	if got, want := value.String(), "Segmented<text.Delta, flow.RunID>"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func textModelDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "cognition.TextModel",
		Revision:      1,
		Generics:      []string{"T", "Unused"},
		Ports: []element.Port{
			{
				Name: "trigger", Direction: element.Input, Cardinality: element.One,
				Type: element.Trigger(element.Var("T")), Required: true, DefaultDepth: 1,
			},
			{
				Name: "text", Direction: element.Output, Cardinality: element.One,
				Type: element.Segmented(element.Named("text.Delta"), element.Named("flow.RunID")),
			},
		},
		Reaction: element.Reaction{Triggers: []string{"trigger"}, MaxConcurrency: 1},
	}
}
