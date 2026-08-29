package graph

import (
	"fmt"
	"maps"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
)

// typeTerm is an elaborator-only expression. Variable keys include the node
// instance scope even though descriptors use short names such as T.
type typeTerm struct {
	name      string
	arguments []typeTerm
	variable  string
	label     string
}

func scopedType(scope string, value element.Type) typeTerm {
	if value.Variable != "" {
		return typeTerm{variable: scope + "\x00" + value.Variable, label: value.Variable}
	}
	result := typeTerm{name: value.Name, arguments: make([]typeTerm, len(value.Arguments))}
	for index, argument := range value.Arguments {
		result.arguments[index] = scopedType(scope, argument)
	}
	return result
}

type unifier struct {
	bindings map[string]typeTerm
}

func newUnifier() *unifier { return &unifier{bindings: make(map[string]typeTerm)} }

// tryUnify is transactional so one bad edge cannot leak partial inference
// into diagnostics for otherwise independent connections.
func (solver *unifier) tryUnify(left, right typeTerm) error {
	candidate := &unifier{bindings: maps.Clone(solver.bindings)}
	if err := candidate.unify(left, right); err != nil {
		return err
	}
	solver.bindings = candidate.bindings
	return nil
}

func (solver *unifier) unify(left, right typeTerm) error {
	left = solver.dereference(left)
	right = solver.dereference(right)
	if left.variable != "" {
		if right.variable == left.variable {
			return nil
		}
		if solver.occurs(left.variable, right) {
			return fmt.Errorf("recursive type would bind %s to %s", left.display(), right.display())
		}
		solver.bindings[left.variable] = right
		return nil
	}
	if right.variable != "" {
		return solver.unify(right, left)
	}
	if left.name != right.name || len(left.arguments) != len(right.arguments) {
		return fmt.Errorf("%s is not assignable to %s", left.display(), right.display())
	}
	for index := range left.arguments {
		if err := solver.unify(left.arguments[index], right.arguments[index]); err != nil {
			return err
		}
	}
	return nil
}

func (solver *unifier) dereference(value typeTerm) typeTerm {
	seen := make(map[string]struct{})
	for value.variable != "" {
		if _, cycle := seen[value.variable]; cycle {
			return value
		}
		seen[value.variable] = struct{}{}
		bound, found := solver.bindings[value.variable]
		if !found {
			return value
		}
		value = bound
	}
	return value
}

func (solver *unifier) occurs(variable string, value typeTerm) bool {
	value = solver.dereference(value)
	if value.variable != "" {
		return value.variable == variable
	}
	for _, argument := range value.arguments {
		if solver.occurs(variable, argument) {
			return true
		}
	}
	return false
}

func (solver *unifier) resolve(value typeTerm) element.Type {
	value = solver.dereference(value)
	if value.variable != "" {
		return element.Var(value.label)
	}
	arguments := make([]element.Type, len(value.arguments))
	for index, argument := range value.arguments {
		arguments[index] = solver.resolve(argument)
	}
	return element.Named(value.name, arguments...)
}

func (value typeTerm) display() string {
	if value.variable != "" {
		return "$" + value.label
	}
	if len(value.arguments) == 0 {
		return value.name
	}
	arguments := make([]string, len(value.arguments))
	for index, argument := range value.arguments {
		arguments[index] = argument.display()
	}
	return value.name + "<" + strings.Join(arguments, ", ") + ">"
}
