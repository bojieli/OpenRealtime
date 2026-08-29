package inspect_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

func TestGraphDiffReportsEverySemanticLayerDeterministically(t *testing.T) {
	before, after := graphDiffFixture(t)
	first, err := inspect.DiffGraphs(before, after)
	if err != nil {
		t.Fatal(err)
	}
	second, err := inspect.DiffGraphs(before, after)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("graph diff is nondeterministic\n%s\n%s", firstJSON, secondJSON)
	}
	if first.Empty() || first.Metadata == nil ||
		!reflect.DeepEqual(first.Metadata.Fields, []string{"revision", "lineage"}) {
		t.Fatalf("metadata diff = %+v", first.Metadata)
	}
	if got := changeKeys(first.Nodes, func(change inspect.NodeChange) string {
		return string(change.Kind) + ":" + change.Node
	}); !reflect.DeepEqual(got, []string{"added:added", "removed:removed", "modified:source"}) {
		t.Fatalf("node changes = %v", got)
	}
	source := first.Nodes[2]
	for _, field := range []string{
		"element", "implementation", "config_reference", "config_digest",
		"deployment_reference", "deployment_digest", "reaction", "state_schema",
		"config_schema", "dependencies", "effects",
	} {
		if !slices.Contains(source.Fields, field) {
			t.Fatalf("source node diff omitted %q: %+v", field, source)
		}
	}
	if source.Before == nil || source.After == nil ||
		source.After.Implementation != "go://test/source/v2" ||
		source.After.ConfigReference != "values://source" ||
		source.After.DeploymentReference != "deployment://diff/source" {
		t.Fatalf("runtime/config selections are not inspectable: %+v", source)
	}
	if got := changeKeys(first.Ports, func(change inspect.PortChange) string {
		return string(change.Kind) + ":" + change.Node + "." + change.Port
	}); !reflect.DeepEqual(got, []string{
		"added:added.out", "removed:removed.out", "modified:source.out", "added:source.trigger_alt",
	}) {
		t.Fatalf("port changes = %v", got)
	}
	if len(first.Edges) != 1 || first.Edges[0].Edge != "stream" ||
		!reflect.DeepEqual(first.Edges[0].Fields, []string{"depth"}) {
		t.Fatalf("edge changes = %+v", first.Edges)
	}
	if len(first.Boundaries) != 1 || first.Boundaries[0].Boundary != "trigger" ||
		!reflect.DeepEqual(first.Boundaries[0].Fields, []string{"endpoint"}) {
		t.Fatalf("boundary changes = %+v", first.Boundaries)
	}
	if len(first.Scopes) != 1 || first.Scopes[0].Scope != "voice" ||
		!reflect.DeepEqual(first.Scopes[0].Fields, []string{"composite", "nodes"}) {
		t.Fatalf("scope changes = %+v", first.Scopes)
	}
	if strings.Contains(string(firstJSON), "compatible") || strings.Contains(string(firstJSON), `"safe"`) {
		t.Fatalf("structural diff inferred reconciliation safety: %s", firstJSON)
	}
}

func TestGraphDiffIgnoresOnlySourceProvenanceAndRejectsStaleIR(t *testing.T) {
	left := fixture(t)
	right := fixture(t)
	left.Nodes[0].Source = &ir.Source{Path: "left.ortg", Line: 1, Column: 2}
	right.Nodes[0].Source = &ir.Source{Path: "generated.json", Line: 90, Column: 8}
	left.Edges[0].Source = &ir.Source{Path: "left.ortg", Line: 3}
	right.Edges[0].Source = &ir.Source{Path: "generated.json", Line: 100}
	left.Boundaries[0].Source = &ir.Source{Path: "left.ortg", Line: 4}
	right.Boundaries[0].Source = &ir.Source{Path: "generated.json", Line: 101}
	diff, err := inspect.DiffGraphs(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if !diff.Empty() {
		t.Fatalf("non-semantic source provenance changed the diff: %+v", diff)
	}

	right.Fingerprint = "sha256:" + strings.Repeat("f", 64)
	if _, err := inspect.DiffGraphs(left, right); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("stale Graph IR was diffed: %v", err)
	}
}

func TestGraphDiffOwnsRecursiveBeforeAndAfterValues(t *testing.T) {
	before, after := graphDiffFixture(t)
	diff, err := inspect.DiffGraphs(before, after)
	if err != nil {
		t.Fatal(err)
	}
	diff.Nodes[2].After.ConfigReference = "mutated"
	diff.Ports[2].After.Type.Arguments[0].Name = "mutated"
	diff.Edges[0].After.Type.Arguments[0].Name = "mutated"
	diff.Scopes[0].After.Nodes[0] = "mutated"

	again, err := inspect.DiffGraphs(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if again.Nodes[2].After.ConfigReference != "values://source" ||
		again.Ports[2].After.Type.Arguments[0].Name == "mutated" ||
		again.Edges[0].After.Type.Arguments[0].Name == "mutated" ||
		again.Scopes[0].After.Nodes[0] == "mutated" {
		t.Fatal("graph diff retained caller aliases")
	}
	var sourceConfig string
	for _, node := range after.Nodes {
		if node.ID == "source" {
			sourceConfig = node.ConfigReference
		}
	}
	if sourceConfig != "values://source" {
		t.Fatal("mutating graph diff changed source Graph IR")
	}
}

func graphDiffFixture(t *testing.T) (ir.Graph, ir.Graph) {
	t.Helper()
	base := fixture(t)
	valueType := base.Edges[0].Type.Clone()
	removed := ir.Node{
		ID: "removed", Element: base.Nodes[1].Element, Implementation: "go://test/removed/v1",
		Ports: []ir.Port{{
			Name: "out", Direction: element.Output, Type: valueType.Clone(), Cardinality: element.One,
		}},
	}
	base.Nodes = append(base.Nodes, removed)
	base.Scopes = []ir.Scope{{
		ID: "voice", Composite: base.Nodes[0].Element, Nodes: []string{"source"},
	}}
	before, err := ir.Freeze(base)
	if err != nil {
		t.Fatal(err)
	}
	after := cloneGraph(t, before)
	after.Revision = 2
	after.Lineage = []string{before.Fingerprint}
	after.Nodes = slices.DeleteFunc(after.Nodes, func(node ir.Node) bool { return node.ID == "removed" })
	after.Nodes = append(after.Nodes, ir.Node{
		ID: "added", Element: before.Nodes[1].Element, Implementation: "go://test/added/v1",
		Ports: []ir.Port{{
			Name: "out", Direction: element.Output, Type: valueType.Clone(), Cardinality: element.One,
		}},
	})
	for index := range after.Nodes {
		if after.Nodes[index].ID != "source" {
			continue
		}
		node := &after.Nodes[index]
		node.Element = before.Nodes[1].Element
		node.Implementation = "go://test/source/v2"
		node.ConfigReference = "values://source"
		node.ConfigDigest = "sha256:" + strings.Repeat("8", 64)
		node.DeploymentReference = "deployment://diff/source"
		node.DeploymentDigest = "sha256:" + strings.Repeat("9", 64)
		node.StateSchema = "state://source/v2"
		node.ConfigSchema = "config://source/v2"
		node.Dependencies = []element.Dependency{{Name: "clock.monotonic"}}
		node.Effects = []element.Effect{{Name: "subscription.input", Reversible: true}}
		for portIndex := range node.Ports {
			if node.Ports[portIndex].Name == "out" {
				node.Ports[portIndex].DefaultDepth = 7
			}
		}
		triggerType := node.Ports[3].Type.Clone()
		node.Ports = append(node.Ports, ir.Port{
			Name: "trigger_alt", Direction: element.Input,
			Type: triggerType, Cardinality: element.One,
		})
		node.Reaction.Triggers = []string{"trigger_alt"}
	}
	after.Edges[0].Depth = 9
	for index := range after.Boundaries {
		if after.Boundaries[index].Name == "trigger" {
			after.Boundaries[index].Endpoint.Port = "trigger_alt"
		}
	}
	for _, node := range before.Nodes {
		if node.ID == "source" {
			after.Scopes[0].Composite = node.Element
		}
	}
	after.Scopes[0].Nodes = []string{"source", "sink"}
	after, err = ir.Freeze(after)
	if err != nil {
		t.Fatal(err)
	}
	return before, after
}

func cloneGraph(t *testing.T, graph ir.Graph) ir.Graph {
	t.Helper()
	payload, err := graph.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	result, err := ir.Parse(payload)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func changeKeys[T any](changes []T, key func(T) string) []string {
	result := make([]string, len(changes))
	for index, change := range changes {
		result[index] = key(change)
	}
	return result
}
