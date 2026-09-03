package graphs_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/bojieli/OpenRealtime/elements"
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	graphrealtimecu "github.com/bojieli/OpenRealtime/graph/binding/realtimecu"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
)

func TestRealtimeComputerUseToolAndRepetitionAdmissionTopologyCompilesUnlocked(t *testing.T) {
	directory := filepath.Join("components", "realtime-computer-use")
	topology, err := os.ReadFile(filepath.Join(directory, "agent.ortg"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := syntax.Parse("realtime-computer-use/agent.ortg", topology)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := elements.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if err := graphrealtimecu.RegisterElementDescriptors(catalog); err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := realtimeCUNodeElement(compiled.Graph, "repetition_admission"); got != "action.RepetitionAdmission" {
		t.Fatalf("repetition_admission element = %q", got)
	}
	if got := realtimeCUNodeElement(compiled.Graph, "tool_admission"); got != "action.ToolAdmission" {
		t.Fatalf("tool_admission element = %q", got)
	}
	for _, edge := range []struct{ fromNode, fromPort, toNode, toPort string }{
		{"normalize_arguments", "normalized", "tool_admission", "action"},
		{"tool_admission", "admitted", "repetition_admission", "action"},
		{"repetition_admission", "admitted", "confirmation", "action"},
		{"tool_result_commit", "canonical", "repetition_admission", "result"},
		{"tool_admission", "terminal", "effect_terminal_mux", "in"},
		{"repetition_admission", "terminal", "effect_terminal_mux", "in"},
		{"effect_terminal_mux", "out", "activation", "effect_terminal"},
	} {
		delivery, found := realtimeCUEdgeDelivery(
			compiled.Graph, edge.fromNode, edge.fromPort, edge.toNode, edge.toPort,
		)
		if !found || delivery != ir.Lossless {
			t.Errorf("required edge %s.%s -> %s.%s = %q, found=%t; want lossless",
				edge.fromNode, edge.fromPort, edge.toNode, edge.toPort, delivery, found)
		}
	}
	for _, forbidden := range []struct{ fromNode, fromPort, toNode, toPort string }{
		{"normalize_arguments", "normalized", "confirmation", "action"},
		{"normalize_arguments", "normalized", "repetition_admission", "action"},
		{"tool_admission", "admitted", "confirmation", "action"},
	} {
		if _, found := realtimeCUEdgeDelivery(
			compiled.Graph, forbidden.fromNode, forbidden.fromPort, forbidden.toNode, forbidden.toPort,
		); found {
			t.Errorf("repetition policy bypass remains at %s.%s -> %s.%s",
				forbidden.fromNode, forbidden.fromPort, forbidden.toNode, forbidden.toPort)
		}
	}
	foundCanonicalBoundary := false
	for _, boundary := range compiled.Graph.Boundaries {
		if boundary.Name != "canonical_result" || boundary.Direction != ir.OutputBoundary {
			continue
		}
		foundCanonicalBoundary = true
		if boundary.Endpoint.Node != "repetition_admission" ||
			boundary.Endpoint.Port != "canonical_result" ||
			!boundary.Type.Equal(actionelements.CanonicalResultType()) {
			t.Errorf("canonical_result boundary = %+v", boundary)
		}
	}
	if !foundCanonicalBoundary {
		t.Fatal("canonical_result boundary is absent")
	}

	valuesBody, err := os.ReadFile(filepath.Join(directory, "agent.values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	values, err := graphvalues.ParseYAML("realtime-computer-use/agent.values.yaml", valuesBody)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := graphvalues.Bind(compiled.Graph, values)
	if err != nil {
		t.Fatal(err)
	}
	var toolConfig actionelements.ToolAdmissionConfig
	if err := json.Unmarshal(bound.Values["tool_admission"], &toolConfig); err != nil {
		t.Fatal(err)
	}
	if toolConfig.AllowedTools != nil ||
		!slices.Equal(toolConfig.DeniedTools, []string{"computer.screenshot", "computer.wait"}) {
		t.Fatalf("Realtime-CU tool admission policy = %+v", toolConfig)
	}
	var config actionelements.RepetitionAdmissionConfig
	if err := json.Unmarshal(bound.Values["repetition_admission"], &config); err != nil {
		t.Fatal(err)
	}
	wantRepeatable := []string{
		"computer.move", "computer.drag", "computer.key", "computer.scroll",
		"computer.screenshot", "computer.wait",
	}
	if config.Mode != actionelements.RepetitionAdmissionAtMostOnceAfterSuccessPerUserIntent ||
		config.MaxTrackedEffects != 512 || !slices.Equal(config.RepeatableTools, wantRepeatable) {
		t.Fatalf("Realtime-CU repetition policy = %+v", config)
	}
}

func realtimeCUNodeElement(graph ir.Graph, id string) string {
	for _, node := range graph.Nodes {
		if node.ID == id {
			return node.Element.Name
		}
	}
	return ""
}

func realtimeCUEdgeDelivery(
	graph ir.Graph, fromNode, fromPort, toNode, toPort string,
) (ir.Delivery, bool) {
	for _, edge := range graph.Edges {
		if edge.From.Node == fromNode && edge.From.Port == fromPort &&
			edge.To.Node == toNode && edge.To.Port == toPort {
			return edge.Delivery, true
		}
	}
	return "", false
}
