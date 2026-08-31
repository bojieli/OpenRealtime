package element_test

import (
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
)

// Canonical, ValidateIdentity, ValidateConcretePort, and ValidateFor are the
// element package's own contract surface -- what a runtime calls to check that
// Graph IR did not alter a contract behind its digest, that a locked identity
// is well formed, that a port type was actually resolved, and that an envelope
// on the wire is the type its edge requires. All four had zero coverage here.

// Canonical sorts declaration-order-insensitive collections, so two descriptors
// that differ only in the order they were written must canonicalize to the same
// thing -- otherwise a digest would depend on authoring order.
func TestCanonicalIsAuthoringOrderIndependentAndValidates(t *testing.T) {
	t.Parallel()
	descriptor := textModelDescriptor()
	canonical, err := descriptor.Canonical()
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	reordered := textModelDescriptor()
	reordered.Ports = []element.Port{reordered.Ports[1], reordered.Ports[0]}
	reordered.Generics = []string{"Unused", "T"}
	reorderedCanonical, err := reordered.Canonical()
	if err != nil {
		t.Fatalf("canonical reordered: %v", err)
	}
	left, err := canonical.Digest()
	if err != nil {
		t.Fatal(err)
	}
	right, err := reorderedCanonical.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatalf("canonical digest depends on authoring order: %s != %s", left, right)
	}

	// It is a validating call, not only a sorting one.
	invalid := textModelDescriptor()
	invalid.Name = ""
	if _, err := invalid.Canonical(); err == nil {
		t.Fatal("Canonical accepted an invalid descriptor")
	}

	// And it is independent: mutating the result cannot reach the original.
	canonical.Ports[0].Name = "mutated"
	if descriptor.Ports[0].Name == "mutated" || descriptor.Ports[1].Name == "mutated" {
		t.Fatal("Canonical aliases the descriptor it was built from")
	}
}

func TestValidateIdentityRefusesUnlockableIdentities(t *testing.T) {
	t.Parallel()
	descriptor := textModelDescriptor()
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err := element.ValidateIdentity(identity); err != nil {
		t.Fatalf("descriptor identity = %v, want accepted", err)
	}
	digest := "sha256:" + strings.Repeat("a", sha256.Size*2)
	for _, test := range []struct {
		name     string
		identity element.Identity
		want     string
	}{
		{
			name:     "no name",
			identity: element.Identity{Revision: 1, Digest: digest},
			want:     "invalid locked element name",
		},
		{
			name:     "name is not a qualified element name",
			identity: element.Identity{Name: "not a name", Revision: 1, Digest: digest},
			want:     "invalid locked element name",
		},
		{
			name:     "revision is zero",
			identity: element.Identity{Name: "cognition.TextModel", Digest: digest},
			want:     "revision must be positive",
		},
		{
			name:     "digest is not prefixed",
			identity: element.Identity{Name: "cognition.TextModel", Revision: 1, Digest: strings.Repeat("a", 64)},
			want:     "invalid digest",
		},
		{
			name:     "digest is the wrong length",
			identity: element.Identity{Name: "cognition.TextModel", Revision: 1, Digest: "sha256:abcd"},
			want:     "invalid digest",
		},
		{
			name: "digest is not hexadecimal",
			identity: element.Identity{
				Name: "cognition.TextModel", Revision: 1,
				Digest: "sha256:" + strings.Repeat("z", sha256.Size*2),
			},
			want: "invalid digest",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := element.ValidateIdentity(test.identity)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("identity error = %v, want one containing %q", err, test.want)
			}
		})
	}
}

// A concrete port type is one that resolution finished with. A type still
// holding a variable has not been resolved, and admitting it would let an
// unresolved graph reach a runtime that assumes concrete types.
func TestValidateConcretePortRefusesUnresolvedTypes(t *testing.T) {
	t.Parallel()
	concrete := element.Segmented(element.Named("text.Delta"), element.Named("flow.RunID"))
	if err := concrete.ValidateConcretePort(); err != nil {
		t.Fatalf("concrete port type = %v, want accepted", err)
	}
	for _, value := range []element.Type{
		element.Var("T"),
		element.Trigger(element.Var("T")),
		element.Segmented(element.Named("text.Delta"), element.Var("Scope")),
	} {
		err := value.ValidateConcretePort()
		if err == nil || !strings.Contains(err.Error(), "still contains a generic variable") {
			t.Fatalf("unresolved type %s = %v, want a generic-variable refusal", value.String(), err)
		}
	}
}

// ValidateFor is the per-envelope check on a live edge: an envelope must be
// identified, concrete, and exactly the type the edge declared. A near-miss
// type is the interesting case, because it is the one a mismatched adapter
// produces.
func TestEnvelopeValidateForRequiresIdentityAndTheEdgeType(t *testing.T) {
	t.Parallel()
	expected := element.Event(element.Named("test.Value"))
	valid := element.Envelope{ItemID: "item-1", Type: expected}
	if err := valid.ValidateFor(expected); err != nil {
		t.Fatalf("matching envelope = %v, want accepted", err)
	}
	for _, test := range []struct {
		name     string
		envelope element.Envelope
		want     string
	}{
		{
			name:     "envelope has no item ID",
			envelope: element.Envelope{Type: expected},
			want:     "requires an item ID",
		},
		{
			name:     "envelope type is unresolved",
			envelope: element.Envelope{ItemID: "item-1", Type: element.Event(element.Var("T"))},
			want:     "still contains a generic variable",
		},
		{
			name:     "envelope carries another payload type",
			envelope: element.Envelope{ItemID: "item-1", Type: element.Event(element.Named("test.Other"))},
			want:     "edge requires",
		},
		{
			name:     "envelope carries another temporal protocol",
			envelope: element.Envelope{ItemID: "item-1", Type: element.Trigger(element.Named("test.Value"))},
			want:     "edge requires",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.envelope.ValidateFor(expected)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("envelope error = %v, want one containing %q", err, test.want)
			}
		})
	}
}
