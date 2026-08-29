package element_test

import (
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
