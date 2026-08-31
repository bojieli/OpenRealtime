package element_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
)

// A descriptor's dependencies and effects are the authority it asks the runtime
// for, so their names are matched by graph wiring and by the descriptor lock.
// Five of validateNamedContracts' refusals had no coverage. A name that does
// not match the contract pattern cannot be resolved by anything, and a repeated
// one is ambiguous: two declarations, one name, and nothing to say which the
// runtime satisfied.
func contractDescriptor(
	dependencies []element.Dependency, effects []element.Effect,
) element.Descriptor {
	valueType := element.Event(element.Named("test.Value"))
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "test.Contracts",
		Revision:      1,
		Ports: []element.Port{
			{Name: "in", Direction: element.Input, Type: valueType, Cardinality: element.One, Required: true, DefaultDepth: 1},
		},
		Reaction:     element.Reaction{Triggers: []string{"in"}, MaxConcurrency: 1},
		Dependencies: dependencies,
		Effects:      effects,
	}
}

func TestDescriptorNamedContractsMustBeResolvableAndUnambiguous(t *testing.T) {
	t.Parallel()
	sound := contractDescriptor(
		[]element.Dependency{{Name: "clock"}, {Name: "secrets/store"}},
		[]element.Effect{
			{Name: "registry", Reversible: true},
			{Name: "payment", External: true, Authority: "authority.User"},
		},
	)
	if err := sound.Validate(); err != nil {
		t.Fatalf("well-formed contracts = %v, want accepted", err)
	}

	for _, test := range []struct {
		name       string
		descriptor element.Descriptor
		want       string
	}{
		{
			name: "dependency name does not match the contract pattern",
			descriptor: contractDescriptor(
				[]element.Dependency{{Name: "the clock"}}, nil),
			want: "invalid dependency",
		},
		{
			name:       "dependency name is empty",
			descriptor: contractDescriptor([]element.Dependency{{Name: ""}}, nil),
			want:       "invalid dependency",
		},
		{
			name: "the same dependency is declared twice",
			descriptor: contractDescriptor(
				[]element.Dependency{{Name: "clock"}, {Name: "clock"}}, nil),
			want: "repeats dependency",
		},
		{
			name: "effect name does not match the contract pattern",
			descriptor: contractDescriptor(nil,
				[]element.Effect{{Name: "9lives"}}),
			want: "invalid effect",
		},
		{
			name: "the same effect is declared twice",
			descriptor: contractDescriptor(nil,
				[]element.Effect{{Name: "registry"}, {Name: "registry"}}),
			want: "repeats effect",
		},
		{
			name: "external effect names an unresolvable authority",
			descriptor: contractDescriptor(nil,
				[]element.Effect{{Name: "payment", External: true, Authority: "the user"}}),
			want: "invalid authority",
		},
		{
			// An external effect cannot be undone by the runtime; claiming
			// otherwise would make a rollback look available that is not.
			name: "external effect claims to be reversible",
			descriptor: contractDescriptor(nil,
				[]element.Effect{{Name: "payment", External: true, Reversible: true, Authority: "authority.User"}}),
			want: "cannot claim an external effect is reversible",
		},
		{
			name: "external effect names no authority",
			descriptor: contractDescriptor(nil,
				[]element.Effect{{Name: "payment", External: true}}),
			want: "must name its required authority type",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.descriptor.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("descriptor error = %v, want one containing %q", err, test.want)
			}
		})
	}
}
