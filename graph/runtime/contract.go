package runtime

import (
	"fmt"
	"reflect"
	"sort"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

func verifyNodeContract(node ir.Node, descriptor element.Descriptor) error {
	canonical, err := descriptor.Canonical()
	if err != nil {
		return err
	}
	if node.StateSchema != canonical.StateSchema || node.ConfigSchema != canonical.ConfigSchema {
		return fmt.Errorf("node %s changes descriptor state/config schema", node.ID)
	}
	if !reflect.DeepEqual(node.Reaction, canonical.Reaction) {
		return fmt.Errorf("node %s changes descriptor reaction contract", node.ID)
	}
	if !reflect.DeepEqual(node.Dependencies, canonical.Dependencies) {
		return fmt.Errorf("node %s changes descriptor dependencies", node.ID)
	}
	if !reflect.DeepEqual(node.Effects, canonical.Effects) {
		return fmt.Errorf("node %s changes descriptor effects", node.ID)
	}
	if len(node.Ports) != len(canonical.Ports) {
		return fmt.Errorf("node %s has %d ports, descriptor defines %d", node.ID, len(node.Ports), len(canonical.Ports))
	}
	actual := append([]ir.Port(nil), node.Ports...)
	sort.Slice(actual, func(left, right int) bool { return actual[left].Name < actual[right].Name })
	bindings := make(map[string]element.Type)
	for index, expected := range canonical.Ports {
		port := actual[index]
		if port.Name != expected.Name || port.Direction != expected.Direction ||
			port.Cardinality != expected.Cardinality || port.Required != expected.Required ||
			port.MinConnections != expected.MinConnections || port.LossAllowed != expected.LossAllowed ||
			port.DefaultDepth != expected.DefaultDepth {
			return fmt.Errorf("node %s port %s changes descriptor metadata", node.ID, expected.Name)
		}
		if err := matchResolvedType(expected.Type, port.Type, bindings); err != nil {
			return fmt.Errorf("node %s port %s: %w", node.ID, expected.Name, err)
		}
	}
	return nil
}

func matchResolvedType(pattern, concrete element.Type, bindings map[string]element.Type) error {
	if concrete.ContainsVariable() {
		return fmt.Errorf("resolved type %s still contains a generic variable", concrete.String())
	}
	if pattern.Variable != "" {
		if previous, found := bindings[pattern.Variable]; found {
			if !previous.Equal(concrete) {
				return fmt.Errorf("generic $%s resolves to both %s and %s",
					pattern.Variable, previous.String(), concrete.String())
			}
			return nil
		}
		bindings[pattern.Variable] = concrete.Clone()
		return nil
	}
	if pattern.Name != concrete.Name || len(pattern.Arguments) != len(concrete.Arguments) {
		return fmt.Errorf("resolved type %s does not instantiate descriptor type %s",
			concrete.String(), pattern.String())
	}
	for index := range pattern.Arguments {
		if err := matchResolvedType(pattern.Arguments[index], concrete.Arguments[index], bindings); err != nil {
			return err
		}
	}
	return nil
}
