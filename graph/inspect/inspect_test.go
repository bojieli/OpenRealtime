package inspect_test

import (
	"reflect"
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
			"Event&lt;test.Value&gt;", "depth 4", "trigger: trigger",
			"interrupt: cancel", "outcome: out", "max concurrency: 2",
			"state schema: schema://test/inspect-state/v1",
			"state transfer: snapshot, restore, quiesce",
			"effect: computer.click [external, irreversible, authority=Authorized]",
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
	for index := range graph.Nodes {
		if graph.Nodes[index].ID != "source" {
			continue
		}
		graph.Nodes[index].Implementation = "go://test/source/v1"
		graph.Nodes[index].ConfigReference = "values://inspect/source"
		graph.Nodes[index].ConfigDigest = "sha256:" + strings.Repeat("7", 64)
		graph.Nodes[index].DeploymentReference = "deployment://inspect/source"
		graph.Nodes[index].DeploymentDigest = "sha256:" + strings.Repeat("8", 64)
	}
	var err error
	graph, err = ir.Freeze(graph)
	if err != nil {
		t.Fatal(err)
	}
	model, err := inspect.Build(graph)
	if err != nil {
		t.Fatal(err)
	}
	var inspectedSource *inspect.Node
	for index := range model.Nodes {
		if model.Nodes[index].ID == "source" {
			inspectedSource = &model.Nodes[index]
		}
	}
	if inspectedSource == nil {
		t.Fatal("inspection model omitted source node")
	}
	if inspectedSource.Implementation != "go://test/source/v1" ||
		inspectedSource.ConfigReference != "values://inspect/source" ||
		inspectedSource.ConfigDigest != "sha256:"+strings.Repeat("7", 64) ||
		inspectedSource.DeploymentReference != "deployment://inspect/source" ||
		inspectedSource.DeploymentDigest != "sha256:"+strings.Repeat("8", 64) {
		t.Fatalf("inspection model omitted node selection identity: %+v", inspectedSource)
	}
	wantReaction := element.Reaction{
		Triggers: []string{"trigger"}, SampledState: []string{"context"},
		Interrupts: []string{"cancel"}, Outcomes: []string{"out"}, MaxConcurrency: 2,
	}
	if got := inspectedSource.Reaction; !reflect.DeepEqual(got, wantReaction) ||
		inspectedSource.StateTransfer == nil || !inspectedSource.StateTransfer.Snapshot ||
		!inspectedSource.StateTransfer.Restore || !inspectedSource.StateTransfer.Quiesce ||
		len(inspectedSource.Effects) != 1 || inspectedSource.Effects[0].Authority != "Authorized" {
		t.Fatalf("inspection model omitted reaction or authority metadata: %+v", inspectedSource)
	}
	inspectedSource.Reaction.Triggers[0] = "mutated"
	inspectedSource.StateTransfer.Restore = false
	inspectedSource.Effects[0].Authority = "mutated"
	for _, node := range graph.Nodes {
		if node.ID == "source" &&
			(node.Reaction.Triggers[0] != "trigger" || !node.StateTransfer.Restore ||
				node.Effects[0].Authority != "Authorized") {
			t.Fatal("inspection model aliases immutable Graph IR reaction or effect metadata")
		}
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

func TestGeneratedViewsPreserveSubgraphHierarchy(t *testing.T) {
	graph := fixture(t)
	composite := graph.Nodes[0].Element
	graph.Scopes = []ir.Scope{{
		ID: "voice", Composite: composite, Nodes: []string{"source"},
		Boundaries: []ir.ScopeBoundary{{
			Name: "trigger", Direction: ir.InputBoundary,
			Endpoint: ir.Endpoint{Node: "source", Port: "trigger"},
			Type:     element.Trigger(element.Named("test.Start")),
		}},
	}}
	graph, err := ir.Freeze(graph)
	if err != nil {
		t.Fatal(err)
	}
	mermaid, err := inspect.Mermaid(graph)
	if err != nil {
		t.Fatal(err)
	}
	dot, err := inspect.DOT(graph)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mermaid, "subgraph s_") || !strings.Contains(mermaid, "voice<br/>") {
		t.Fatalf("Mermaid lost hierarchy:\n%s", mermaid)
	}
	if !strings.Contains(dot, `subgraph "cluster:voice"`) {
		t.Fatalf("DOT lost hierarchy:\n%s", dot)
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
		Reaction: element.Reaction{
			Triggers: []string{"trigger"}, SampledState: []string{"context"},
			Interrupts: []string{"cancel"}, Outcomes: []string{"out"}, MaxConcurrency: 2,
		},
		StateSchema: "schema://test/inspect-state/v1",
		StateTransfer: &element.StateTransferCapabilities{
			Snapshot: true, Restore: true, Quiesce: true,
		},
		Effects: []element.Effect{{Name: "computer.click", External: true, Authority: "Authorized"}},
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
			}, Reaction: sourceDescriptor.Reaction, StateSchema: sourceDescriptor.StateSchema,
				StateTransfer: sourceDescriptor.StateTransfer.Clone(), Effects: sourceDescriptor.Effects},
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
