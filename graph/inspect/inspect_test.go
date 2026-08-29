package inspect_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

func TestGeneratedViewsAttestExactGraphAndSemantics(t *testing.T) {
	graph := fixture(t)
	mermaid, err := inspect.Mermaid(graph)
	if err != nil {
		t.Fatal(err)
	}
	dot, err := inspect.DOT(graph)
	if err != nil {
		t.Fatal(err)
	}
	for name, view := range map[string]string{"mermaid": mermaid, "dot": dot} {
		for _, required := range []string{
			graph.Fingerprint, "test.Source@1", "test.Sink@1",
			"Event&lt;test.Value&gt;", "depth 4",
		} {
			candidate := required
			if name == "dot" {
				candidate = strings.ReplaceAll(required, "&lt;", "<")
				candidate = strings.ReplaceAll(candidate, "&gt;", ">")
			}
			if !strings.Contains(view, candidate) {
				t.Fatalf("%s view missing %q:\n%s", name, candidate, view)
			}
		}
	}
	if !strings.Contains(mermaid, "stroke-dasharray:5 5") || !strings.Contains(dot, "style=dashed") {
		t.Fatalf("lossy delivery is not visible:\n%s\n%s", mermaid, dot)
	}
}

func TestModelClassifiesTriggerInterruptAndState(t *testing.T) {
	graph := fixture(t)
	model, err := inspect.Build(graph)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]bool{}
	for _, node := range model.Nodes {
		for _, port := range node.Ports {
			roles[port.Role] = true
		}
	}
	for _, role := range []string{"data", "trigger", "interrupt", "state"} {
		if !roles[role] {
			t.Fatalf("inspection model has no %s role: %+v", role, model)
		}
	}
}

func fixture(t *testing.T) ir.Graph {
	t.Helper()
	value := element.Event(element.Named("test.Value"))
	trigger := element.Trigger(element.Named("test.Start"))
	interrupt := element.Interrupt(element.Named("flow.RunID"))
	state := element.State(element.Named("test.Context"))
	sourceDescriptor := element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "test.Source", Revision: 1,
		Ports: []element.Port{
			{Name: "trigger", Direction: element.Input, Type: trigger, Cardinality: element.One},
			{Name: "cancel", Direction: element.Input, Type: interrupt, Cardinality: element.One},
			{Name: "context", Direction: element.Input, Type: state, Cardinality: element.One},
			{Name: "out", Direction: element.Output, Type: value, Cardinality: element.One, LossAllowed: true},
		},
	}
	sinkDescriptor := element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "test.Sink", Revision: 1,
		Ports: []element.Port{{Name: "in", Direction: element.Input, Type: value, Cardinality: element.One, LossAllowed: true}},
	}
	sourceIdentity, err := sourceDescriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	sinkIdentity, err := sinkDescriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	graph, err := ir.Freeze(ir.Graph{
		FormatVersion: ir.FormatVersion, ID: "inspect", Revision: 1,
		Nodes: []ir.Node{
			{ID: "source", Element: sourceIdentity, Ports: []ir.Port{
				{Name: "trigger", Direction: element.Input, Type: trigger, Cardinality: element.One},
				{Name: "cancel", Direction: element.Input, Type: interrupt, Cardinality: element.One},
				{Name: "context", Direction: element.Input, Type: state, Cardinality: element.One},
				{Name: "out", Direction: element.Output, Type: value, Cardinality: element.One, LossAllowed: true},
			}},
			{ID: "sink", Element: sinkIdentity, Ports: []ir.Port{{
				Name: "in", Direction: element.Input, Type: value, Cardinality: element.One, LossAllowed: true,
			}}},
		},
		Edges: []ir.Edge{{
			ID: "stream", From: ir.Endpoint{Node: "source", Port: "out"},
			To: ir.Endpoint{Node: "sink", Port: "in"}, Type: value,
			Delivery: ir.Lossy, Ordering: "fifo", Depth: 4,
		}},
		Boundaries: []ir.Boundary{
			{Name: "trigger", Direction: ir.InputBoundary, Endpoint: ir.Endpoint{Node: "source", Port: "trigger"}, Type: trigger},
			{Name: "cancel", Direction: ir.InputBoundary, Endpoint: ir.Endpoint{Node: "source", Port: "cancel"}, Type: interrupt},
			{Name: "context", Direction: ir.InputBoundary, Endpoint: ir.Endpoint{Node: "source", Port: "context"}, Type: state},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return graph
}
