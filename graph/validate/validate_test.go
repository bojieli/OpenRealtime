package validate_test

import (
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/validate"
)

func TestCycleNeedsExplicitCausalBreak(t *testing.T) {
	graph := cycleGraph(t, false)
	if !hasFinding(validate.Check(graph, validate.Core), "E_CAUSAL_CYCLE") {
		t.Fatal("expected causal cycle error")
	}
	graph = cycleGraph(t, true)
	if findings := validate.Errors(validate.Check(graph, validate.Core)); len(findings) != 0 {
		t.Fatalf("causal break did not make cycle legal: %+v", findings)
	}
}

func TestExternalEffectNeedsConnectedTypedAuthority(t *testing.T) {
	authorized := element.Event(element.Named("Authorized", element.Named("computer.Action")))
	identity := identityFor(t, "action.Executor", []element.Port{{
		Name: "action", Direction: element.Input, Type: authorized,
		Cardinality: element.One,
	}}, []element.Effect{{Name: "computer.click", External: true, Authority: "Authorized"}})
	graph, err := ir.Freeze(ir.Graph{
		FormatVersion: ir.FormatVersion, ID: "authority", Revision: 1,
		Nodes: []ir.Node{{
			ID: "executor", Element: identity,
			Ports:   []ir.Port{{Name: "action", Direction: element.Input, Type: authorized, Cardinality: element.One}},
			Effects: []element.Effect{{Name: "computer.click", External: true, Authority: "Authorized"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinding(validate.Check(graph, validate.Core), "E_AUTHORITY_UNCONNECTED") {
		t.Fatal("expected unconnected authority error")
	}
}

func TestRealtimeProfileSurfacesIncompleteReactionChoices(t *testing.T) {
	trigger := element.Trigger(element.Named("test.Start"))
	outcome := element.Event(element.Named("test.Outcome"))
	identity := identityFor(t, "test.Worker", []element.Port{
		{Name: "trigger", Direction: element.Input, Type: trigger, Cardinality: element.One},
		{Name: "outcome", Direction: element.Output, Type: outcome, Cardinality: element.One},
	}, nil)
	graph, err := ir.Freeze(ir.Graph{
		FormatVersion: ir.FormatVersion, ID: "lint", Revision: 1,
		Nodes: []ir.Node{{
			ID: "worker", Element: identity,
			Ports: []ir.Port{
				{Name: "trigger", Direction: element.Input, Type: trigger, Cardinality: element.One},
				{Name: "outcome", Direction: element.Output, Type: outcome, Cardinality: element.One},
			},
			Reaction: element.Reaction{Triggers: []string{"trigger"}, Outcomes: []string{"outcome"}},
		}},
		Boundaries: []ir.Boundary{{
			Name: "start", Direction: ir.InputBoundary,
			Endpoint: ir.Endpoint{Node: "worker", Port: "trigger"}, Type: trigger,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	findings := validate.Check(graph, validate.RealtimeAgent)
	if !hasFinding(findings, "W_OUTCOME_UNCONSUMED") {
		t.Fatalf("missing outcome lint: %+v", findings)
	}
}

func cycleGraph(t *testing.T, breakCycle bool) ir.Graph {
	t.Helper()
	value := element.Event(element.Named("test.Value"))
	leftIdentity := identityFor(t, "test.Left", ports(value), nil)
	rightIdentity := identityFor(t, "test.Right", ports(value), nil)
	graph, err := ir.Freeze(ir.Graph{
		FormatVersion: ir.FormatVersion, ID: "cycle", Revision: 1,
		Nodes: []ir.Node{
			{ID: "left", Element: leftIdentity, Ports: irPorts(value), Reaction: element.Reaction{BreaksCycles: breakCycle}},
			{ID: "right", Element: rightIdentity, Ports: irPorts(value)},
		},
		Edges: []ir.Edge{
			{ID: "left-right", From: ir.Endpoint{Node: "left", Port: "out"}, To: ir.Endpoint{Node: "right", Port: "in"}, Type: value, Delivery: ir.Lossless, Ordering: "fifo", Depth: 1},
			{ID: "right-left", From: ir.Endpoint{Node: "right", Port: "out"}, To: ir.Endpoint{Node: "left", Port: "in"}, Type: value, Delivery: ir.Lossless, Ordering: "fifo", Depth: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func ports(value element.Type) []element.Port {
	return []element.Port{
		{Name: "in", Direction: element.Input, Type: value, Cardinality: element.One},
		{Name: "out", Direction: element.Output, Type: value, Cardinality: element.One},
	}
}

func irPorts(value element.Type) []ir.Port {
	return []ir.Port{
		{Name: "in", Direction: element.Input, Type: value, Cardinality: element.One},
		{Name: "out", Direction: element.Output, Type: value, Cardinality: element.One},
	}
}

func identityFor(t *testing.T, name string, ports []element.Port, effects []element.Effect) element.Identity {
	t.Helper()
	descriptor := element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          name, Revision: 1, Ports: ports, Effects: effects,
	}
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func hasFinding(findings []validate.Finding, code string) bool {
	for _, finding := range findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}
