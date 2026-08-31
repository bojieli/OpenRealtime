package element_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
)

// Type.Validate and Type.ValidatePort are the front door of the type system:
// every port on every descriptor goes through them. Four refusals had no
// coverage, including the two that keep a type variable from also being a
// concrete type and keep a protocol from carrying the wrong arity.
func TestTypeVariablesCannotAlsoBeConcreteTypes(t *testing.T) {
	t.Parallel()
	generics := map[string]struct{}{"T": {}}
	if err := element.Var("T").Validate(generics); err != nil {
		t.Fatalf("declared variable = %v, want accepted", err)
	}
	for _, test := range []struct {
		name  string
		value element.Type
		want  string
	}{
		{
			// A value that is both would resolve two ways.
			name:  "variable also carries a name",
			value: element.Type{Variable: "T", Name: "text.Delta"},
			want:  "cannot also have a name or arguments",
		},
		{
			name: "variable also carries arguments",
			value: element.Type{
				Variable: "T", Arguments: []element.Type{element.Named("text.Delta")},
			},
			want: "cannot also have a name or arguments",
		},
		{
			name:  "variable is not a canonical variable name",
			value: element.Type{Variable: "lowercase"},
			want:  "invalid type variable",
		},
		{
			name:  "variable was never declared as a generic",
			value: element.Var("Undeclared"),
			want:  "undeclared type variable",
		},
		{
			name:  "concrete type has no name",
			value: element.Type{},
			want:  "concrete type needs a name",
		},
		{
			name:  "concrete type name is not canonical",
			value: element.Named("not a type"),
			want:  "invalid type name",
		},
		{
			name:  "a nested argument is invalid",
			value: element.Event(element.Named("not a type")),
			want:  "argument 0",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.value.Validate(generics)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("type error = %v, want one containing %q", err, test.want)
			}
		})
	}
}

// A port's root must be a temporal protocol carrying its exact arity. Without
// that, a port could declare a payload with no temporal semantics at all, and
// nothing downstream would know whether it carries one value, a stream, or a
// request awaiting its reply.
func TestPortRootsMustBeATemporalProtocolWithExactArity(t *testing.T) {
	t.Parallel()
	generics := map[string]struct{}{"T": {}}
	for _, sound := range []element.Type{
		element.Event(element.Named("text.Delta")),
		element.Segmented(element.Named("text.Delta"), element.Named("flow.RunID")),
		element.Var("T"),
	} {
		if err := sound.ValidatePort(generics); err != nil {
			t.Fatalf("port type %s = %v, want accepted", sound.String(), err)
		}
	}
	for _, test := range []struct {
		name  string
		value element.Type
		want  string
	}{
		{
			name:  "root is a payload rather than a protocol",
			value: element.Named("text.Delta"),
			want:  "is not a temporal protocol",
		},
		{
			name:  "protocol carries too few arguments",
			value: element.Type{Name: "Segmented", Arguments: []element.Type{element.Named("text.Delta")}},
			want:  "want 2",
		},
		{
			name: "protocol carries too many arguments",
			value: element.Type{Name: "Event", Arguments: []element.Type{
				element.Named("text.Delta"), element.Named("flow.RunID"),
			}},
			want: "want 1",
		},
		{
			name:  "protocol carries no arguments at all",
			value: element.Type{Name: "Event"},
			want:  "want 1",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.value.ValidatePort(generics)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("port type error = %v, want one containing %q", err, test.want)
			}
		})
	}
}
