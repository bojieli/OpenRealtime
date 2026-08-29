// Package element defines language-neutral contracts for computations that can
// participate in an OpenRealtime graph.
package element

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Type is a structural graph-port type.
//
// A concrete type has a Name and optional Arguments. A type variable has only
// Variable set. Variables are scoped to one element instance by the graph
// elaborator, so two instances that both declare T do not accidentally share
// an inference variable.
//
// Examples:
//
//	Type{Name: "Event", Arguments: []Type{{Name: "interaction.TurnEnd"}}}
//	Type{Name: "Segmented", Arguments: []Type{{Name: "text.Delta"}, {Name: "flow.RunID"}}}
//	Type{Variable: "T"}
type Type struct {
	Name      string `json:"name,omitempty" yaml:"name,omitempty"`
	Arguments []Type `json:"arguments,omitempty" yaml:"arguments,omitempty"`
	Variable  string `json:"variable,omitempty" yaml:"variable,omitempty"`
}

var (
	typeNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]*$`)
	variablePattern = regexp.MustCompile(`^[A-Z][A-Za-z0-9_]*$`)
)

var protocolArity = map[string]int{
	"Event":     1,
	"Stream":    1,
	"Segmented": 2,
	"Revisions": 2,
	"State":     1,
	"Trigger":   1,
	"Interrupt": 1,
	"Request":   2,
	"Reply":     2,
}

// Named constructs a concrete type.
func Named(name string, arguments ...Type) Type {
	return Type{Name: name, Arguments: append([]Type(nil), arguments...)}
}

// Var constructs a generic type variable.
func Var(name string) Type { return Type{Variable: name} }

// Event is the protocol type for one immutable occurrence.
func Event(payload Type) Type { return Named("Event", payload) }

// Stream is the protocol type for an ordered sequence of independently
// meaningful items.
func Stream(payload Type) Type { return Named("Stream", payload) }

// Segmented is a correlated begin/delta/end stream.
func Segmented(payload, key Type) Type { return Named("Segmented", payload, key) }

// Revisions is a sequence of replacements or refinements.
func Revisions(payload, key Type) Type { return Named("Revisions", payload, key) }

// State is latest-value state sampled by reactions. Updating it does not, by
// itself, create a reaction.
func State(payload Type) Type { return Named("State", payload) }

// Trigger is an input occurrence that opens a reaction.
func Trigger(payload Type) Type { return Named("Trigger", payload) }

// Interrupt is a run- or scope-addressed cancellation/control request.
func Interrupt(key Type) Type { return Named("Interrupt", key) }

// Request and Reply are the two explicit halves of a correlated exchange.
func Request(payload, key Type) Type { return Named("Request", payload, key) }
func Reply(payload, key Type) Type   { return Named("Reply", payload, key) }

// IsVariable reports whether the type is a generic variable.
func (value Type) IsVariable() bool { return value.Variable != "" }

// Validate checks that the expression is well formed and that every variable
// is declared by the surrounding descriptor.
func (value Type) Validate(generics map[string]struct{}) error {
	switch {
	case value.Variable != "":
		if value.Name != "" || len(value.Arguments) != 0 {
			return errors.New("a type variable cannot also have a name or arguments")
		}
		if !variablePattern.MatchString(value.Variable) {
			return fmt.Errorf("invalid type variable %q", value.Variable)
		}
		if _, exists := generics[value.Variable]; !exists {
			return fmt.Errorf("undeclared type variable %q", value.Variable)
		}
		return nil
	case value.Name == "":
		return errors.New("a concrete type needs a name")
	case !typeNamePattern.MatchString(value.Name):
		return fmt.Errorf("invalid type name %q", value.Name)
	}
	for index, argument := range value.Arguments {
		if err := argument.Validate(generics); err != nil {
			return fmt.Errorf("type %s argument %d: %w", value.Name, index, err)
		}
	}
	return nil
}

// ValidatePort checks that a descriptor port has one temporal protocol at its
// root. A generic connector may use a root variable and infer the complete
// protocol from its neighbors; ordinary concrete ports may not omit temporal
// semantics.
func (value Type) ValidatePort(generics map[string]struct{}) error {
	if err := value.Validate(generics); err != nil {
		return err
	}
	if value.Variable != "" {
		return nil
	}
	want, protocol := protocolArity[value.Name]
	if !protocol {
		return fmt.Errorf("port type root %q is not a temporal protocol", value.Name)
	}
	if len(value.Arguments) != want {
		return fmt.Errorf("protocol %s has %d argument(s), want %d", value.Name, len(value.Arguments), want)
	}
	return nil
}

// ValidateConcretePort checks a fully elaborated port type.
func (value Type) ValidateConcretePort() error {
	if value.ContainsVariable() {
		return fmt.Errorf("port type %s still contains a generic variable", value.String())
	}
	return value.ValidatePort(nil)
}

// IsProtocol reports whether the outer type is one of the temporal protocol
// families understood by the graph kernel.
func (value Type) IsProtocol() bool {
	_, found := protocolArity[value.Name]
	return found
}

// String renders the canonical diagnostic spelling of a type.
func (value Type) String() string {
	if value.Variable != "" {
		return "$" + value.Variable
	}
	if len(value.Arguments) == 0 {
		return value.Name
	}
	arguments := make([]string, 0, len(value.Arguments))
	for _, argument := range value.Arguments {
		arguments = append(arguments, argument.String())
	}
	return value.Name + "<" + strings.Join(arguments, ", ") + ">"
}

// Equal reports structural equality.
func (value Type) Equal(other Type) bool {
	if value.Name != other.Name || value.Variable != other.Variable ||
		len(value.Arguments) != len(other.Arguments) {
		return false
	}
	for index := range value.Arguments {
		if !value.Arguments[index].Equal(other.Arguments[index]) {
			return false
		}
	}
	return true
}

// ContainsVariable reports whether this expression still needs inference.
func (value Type) ContainsVariable() bool {
	if value.Variable != "" {
		return true
	}
	for _, argument := range value.Arguments {
		if argument.ContainsVariable() {
			return true
		}
	}
	return false
}

// Clone returns a recursively independent type expression.
func (value Type) Clone() Type {
	result := value
	if value.Arguments != nil {
		result.Arguments = make([]Type, len(value.Arguments))
		for index, argument := range value.Arguments {
			result.Arguments[index] = argument.Clone()
		}
	}
	return result
}
