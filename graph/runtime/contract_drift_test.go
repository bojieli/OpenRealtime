package runtime

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

// verifyNodeContract is what makes "descriptor-locked" mean anything: it is the
// check that a node in the frozen IR still states exactly the contract its
// element's descriptor declares, rather than a widened or narrowed one. Five of
// its seven refusals had no coverage, so a node could have quietly claimed
// different dependencies, different effects, or a different port population
// without a test failing.

func contractDriftDescriptor() element.Descriptor {
	valueType := element.Event(element.Named("test.Value"))
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "test.Contract",
		Revision:      1,
		Ports: []element.Port{
			{Name: "in", Direction: element.Input, Type: valueType, Cardinality: element.One, Required: true, DefaultDepth: 2},
			{Name: "out", Direction: element.Output, Type: valueType, Cardinality: element.One, Required: true, DefaultDepth: 2},
		},
		Reaction:     element.Reaction{Triggers: []string{"in"}, Outcomes: []string{"out"}, MaxConcurrency: 1},
		Dependencies: []element.Dependency{{Name: "clock"}},
		Effects:      []element.Effect{{Name: "registry", Reversible: true}},
	}
}

func contractDriftNode(descriptor element.Descriptor) ir.Node {
	valueType := element.Event(element.Named("test.Value"))
	return ir.Node{
		ID: "node",
		Ports: []ir.Port{
			{Name: "in", Direction: element.Input, Type: valueType, Cardinality: element.One, Required: true, DefaultDepth: 2},
			{Name: "out", Direction: element.Output, Type: valueType, Cardinality: element.One, Required: true, DefaultDepth: 2},
		},
		Reaction:     descriptor.Reaction,
		Dependencies: descriptor.Dependencies,
		Effects:      descriptor.Effects,
		StateSchema:  descriptor.StateSchema,
		ConfigSchema: descriptor.ConfigSchema,
	}
}

func TestNodeContractRefusesEveryDescriptorDrift(t *testing.T) {
	t.Parallel()
	descriptor := contractDriftDescriptor()
	if err := verifyNodeContract(contractDriftNode(descriptor), descriptor); err != nil {
		t.Fatalf("undrifted node = %v, want accepted", err)
	}
	valueType := element.Event(element.Named("test.Value"))
	for _, test := range []struct {
		name string
		edit func(*ir.Node, *element.Descriptor)
		want string
	}{
		{
			name: "descriptor itself is not valid",
			edit: func(_ *ir.Node, d *element.Descriptor) { d.Name = "" },
			want: "name",
		},
		{
			name: "node widens the state schema",
			edit: func(node *ir.Node, _ *element.Descriptor) { node.StateSchema = "vendor/state" },
			want: "changes descriptor state/config schema",
		},
		{
			name: "node widens the config schema",
			edit: func(node *ir.Node, _ *element.Descriptor) { node.ConfigSchema = "vendor/config" },
			want: "changes descriptor state/config schema",
		},
		{
			name: "node changes its reaction contract",
			edit: func(node *ir.Node, _ *element.Descriptor) {
				node.Reaction = element.Reaction{Triggers: []string{"in"}, Outcomes: []string{"out"}, MaxConcurrency: 4}
			},
			want: "changes descriptor reaction contract",
		},
		{
			// A node that declares a dependency its descriptor does not is a
			// node asking the runtime for authority the element never declared.
			name: "node claims an undeclared dependency",
			edit: func(node *ir.Node, _ *element.Descriptor) {
				node.Dependencies = []element.Dependency{{Name: "clock"}, {Name: "secrets"}}
			},
			want: "changes descriptor dependencies",
		},
		{
			name: "node drops a declared dependency",
			edit: func(node *ir.Node, _ *element.Descriptor) { node.Dependencies = nil },
			want: "changes descriptor dependencies",
		},
		{
			name: "node claims an undeclared effect",
			edit: func(node *ir.Node, _ *element.Descriptor) {
				node.Effects = []element.Effect{
					{Name: "registry", Reversible: true},
					{Name: "payment", External: true, Authority: "user"},
				}
			},
			want: "changes descriptor effects",
		},
		{
			// Silently making a declared effect reversible would let an
			// irreversible external action be rolled back on paper.
			name: "node relabels an effect as reversible",
			edit: func(node *ir.Node, _ *element.Descriptor) {
				node.Effects = []element.Effect{{Name: "registry", Reversible: false}}
			},
			want: "changes descriptor effects",
		},
		{
			name: "node adds a port the descriptor does not define",
			edit: func(node *ir.Node, _ *element.Descriptor) {
				node.Ports = append(node.Ports, ir.Port{
					Name: "extra", Direction: element.Output, Type: valueType,
					Cardinality: element.One, DefaultDepth: 2,
				})
			},
			want: "ports, descriptor defines",
		},
		{
			name: "node drops a declared port",
			edit: func(node *ir.Node, _ *element.Descriptor) { node.Ports = node.Ports[:1] },
			want: "ports, descriptor defines",
		},
		{
			name: "node changes a port's direction",
			edit: func(node *ir.Node, _ *element.Descriptor) { node.Ports[0].Direction = element.Output },
			want: "changes descriptor metadata",
		},
		{
			name: "node changes a port's queue depth",
			edit: func(node *ir.Node, _ *element.Descriptor) { node.Ports[0].DefaultDepth = 64 },
			want: "changes descriptor metadata",
		},
		{
			name: "node makes a required port optional",
			edit: func(node *ir.Node, _ *element.Descriptor) { node.Ports[0].Required = false },
			want: "changes descriptor metadata",
		},
		{
			name: "node resolves a port to another type",
			edit: func(node *ir.Node, _ *element.Descriptor) {
				node.Ports[0].Type = element.Event(element.Named("test.Other"))
			},
			want: "does not instantiate descriptor type",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			current := contractDriftDescriptor()
			node := contractDriftNode(current)
			test.edit(&node, &current)
			err := verifyNodeContract(node, current)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("contract error = %v, want one containing %q", err, test.want)
			}
		})
	}
}

// The generic path is what lets one descriptor serve many concrete graphs, so
// its two failure modes matter: a port that was never resolved to a concrete
// type, and one variable that resolves inconsistently across ports. Neither had
// coverage, and both would have let an unresolved or contradictory graph mount.
func genericDescriptor() element.Descriptor {
	generic := element.Event(element.Var("T"))
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "test.Generic",
		Revision:      1,
		Generics:      []string{"T"},
		Ports: []element.Port{
			{Name: "in", Direction: element.Input, Type: generic, Cardinality: element.One, Required: true, DefaultDepth: 2},
			{Name: "out", Direction: element.Output, Type: generic, Cardinality: element.One, Required: true, DefaultDepth: 2},
		},
		Reaction: element.Reaction{Triggers: []string{"in"}, Outcomes: []string{"out"}, MaxConcurrency: 1},
	}
}

func genericNode(in, out element.Type) ir.Node {
	return ir.Node{
		ID: "node",
		Ports: []ir.Port{
			{Name: "in", Direction: element.Input, Type: in, Cardinality: element.One, Required: true, DefaultDepth: 2},
			{Name: "out", Direction: element.Output, Type: out, Cardinality: element.One, Required: true, DefaultDepth: 2},
		},
		Reaction: genericDescriptor().Reaction,
	}
}

func TestNodeContractResolvesGenericsConsistentlyOrRefuses(t *testing.T) {
	t.Parallel()
	descriptor := genericDescriptor()
	concrete := element.Event(element.Named("test.Value"))
	if err := verifyNodeContract(genericNode(concrete, concrete), descriptor); err != nil {
		t.Fatalf("consistently resolved generic = %v, want accepted", err)
	}
	for _, test := range []struct {
		name    string
		in, out element.Type
		want    string
	}{
		{
			name: "a port is still generic after resolution",
			in:   element.Event(element.Var("T")), out: concrete,
			want: "still contains a generic variable",
		},
		{
			name: "the same variable resolves to two different types",
			in:   concrete, out: element.Event(element.Named("test.Other")),
			want: "resolves to both",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := verifyNodeContract(genericNode(test.in, test.out), genericDescriptor())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("generic resolution error = %v, want one containing %q", err, test.want)
			}
		})
	}
}
