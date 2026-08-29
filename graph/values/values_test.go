package values_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
)

func TestValuesBindCanonicalDigestsIntoGraphIdentity(t *testing.T) {
	graph := oneNodeGraph(t)
	left, err := graphvalues.Bind(graph, graphvalues.Document{
		APIVersion: graphvalues.APIVersion, Graph: graph.ID,
		Nodes: map[string]json.RawMessage{"node": json.RawMessage(`{"model":"fast","budget":32}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	right, err := graphvalues.Bind(graph, graphvalues.Document{
		APIVersion: graphvalues.APIVersion, Graph: graph.ID,
		Nodes: map[string]json.RawMessage{"node": json.RawMessage(`{ "budget": 32, "model": "fast" }`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if left.Graph.Fingerprint != right.Graph.Fingerprint || left.Fingerprint != right.Fingerprint {
		t.Fatalf("format/order changed identity:\n%s %s\n%s %s",
			left.Graph.Fingerprint, left.Fingerprint, right.Graph.Fingerprint, right.Fingerprint)
	}
	if left.Graph.Nodes[0].ConfigReference != "values://values_graph/node" ||
		!strings.HasPrefix(left.Graph.Nodes[0].ConfigDigest, "sha256:") {
		t.Fatalf("bound node = %+v", left.Graph.Nodes[0])
	}
	changed, err := graphvalues.Bind(graph, graphvalues.Document{
		APIVersion: graphvalues.APIVersion, Graph: graph.ID,
		Nodes: map[string]json.RawMessage{"node": json.RawMessage(`{"model":"slow","budget":32}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if changed.Graph.Fingerprint == left.Graph.Fingerprint {
		t.Fatal("a model-value change retained one executable graph identity")
	}
}

func TestValuesYAMLAndJSONAreEquivalentAndStrict(t *testing.T) {
	yamlDocument, err := graphvalues.ParseYAML("values.yaml", []byte(`apiVersion: openrealtime.ai/config/v1alpha1
graph: values_graph
nodes:
  node:
    model: fast
    budget: 32
`))
	if err != nil {
		t.Fatal(err)
	}
	jsonDocument, err := graphvalues.ParseJSON("values.json", []byte(`{
  "apiVersion":"openrealtime.ai/config/v1alpha1",
  "graph":"values_graph",
  "nodes":{"node":{"budget":32,"model":"fast"}}
}`))
	if err != nil {
		t.Fatal(err)
	}
	yamlBound, err := graphvalues.Bind(oneNodeGraph(t), yamlDocument)
	if err != nil {
		t.Fatal(err)
	}
	jsonBound, err := graphvalues.Bind(oneNodeGraph(t), jsonDocument)
	if err != nil {
		t.Fatal(err)
	}
	if yamlBound.Graph.Fingerprint != jsonBound.Graph.Fingerprint {
		t.Fatalf("YAML and JSON executable identities differ: %s %s",
			yamlBound.Graph.Fingerprint, jsonBound.Graph.Fingerprint)
	}
	for _, source := range []string{
		"apiVersion: openrealtime.ai/config/v1alpha1\ngraph: values_graph\nnodes:\n  node: &shared {model: fast}\n",
		"apiVersion: openrealtime.ai/config/v1alpha1\ngraph: values_graph\nnodes:\n  node: {}\n  node: {}\n",
		"apiVersion: openrealtime.ai/config/v1alpha1\ngraph: values_graph\nnodes:\n  node: {budget: 0x20}\n",
		"apiVersion: openrealtime.ai/config/v1alpha1\ngraph: values_graph\nnodes:\n  node: {deadline: 2026-08-29}\n",
	} {
		if _, err := graphvalues.ParseYAML("invalid.yaml", []byte(source)); err == nil {
			t.Fatalf("ambiguous YAML values were accepted:\n%s", source)
		}
	}
}

func TestValuesCanonicalizeEquivalentNumbersWithoutPrecisionLoss(t *testing.T) {
	left, leftDigest, err := graphvalues.Digest(json.RawMessage(`{
        "integer": 1.0,
        "decimal": 12.3000,
        "tiny": 0.000001,
        "large": 90071992547409931234567890
    }`))
	if err != nil {
		t.Fatal(err)
	}
	right, rightDigest, err := graphvalues.Digest(json.RawMessage(`{
        "large": 9.007199254740993123456789e25,
        "tiny": 1e-6,
        "decimal": 123e-1,
        "integer": 10e-1
    }`))
	if err != nil {
		t.Fatal(err)
	}
	if leftDigest != rightDigest || string(left) != string(right) {
		t.Fatalf("equivalent numbers changed identity:\n%s %s\n%s %s",
			leftDigest, left, rightDigest, right)
	}
	if !strings.Contains(string(left), `"large":90071992547409931234567890`) {
		t.Fatalf("large integer lost exactness: %s", left)
	}

	zero, _, err := graphvalues.Digest(json.RawMessage(`{"value":-0e100000}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(zero) != `{"value":0}` {
		t.Fatalf("negative zero canonicalized as %s", zero)
	}
}

func TestValuesMarshalYAMLRoundTripPreservesNumericIdentity(t *testing.T) {
	document, err := graphvalues.ParseJSON("values.json", []byte(`{
        "apiVersion":"openrealtime.ai/config/v1alpha1",
        "graph":"values_graph",
        "nodes":{"node":{"budget":9007199254740993,"temperature":0.70}}
    }`))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := graphvalues.MarshalYAML(document)
	if err != nil {
		t.Fatal(err)
	}
	roundTripped, err := graphvalues.ParseYAML("values.yaml", payload)
	if err != nil {
		t.Fatalf("parse marshaled YAML:\n%s\n%v", payload, err)
	}
	before, err := graphvalues.Bind(oneNodeGraph(t), document)
	if err != nil {
		t.Fatal(err)
	}
	after, err := graphvalues.Bind(oneNodeGraph(t), roundTripped)
	if err != nil {
		t.Fatal(err)
	}
	if before.Graph.Fingerprint != after.Graph.Fingerprint {
		t.Fatalf("YAML round trip changed config identity:\n%s\nbefore=%s after=%s",
			payload, before.Graph.Fingerprint, after.Graph.Fingerprint)
	}
}

func TestRuntimeRequiresExactValuesForBoundGraph(t *testing.T) {
	graph := oneNodeGraph(t)
	bound, err := graphvalues.Bind(graph, graphvalues.Document{
		APIVersion: graphvalues.APIVersion, Graph: graph.ID,
		Nodes: map[string]json.RawMessage{"node": json.RawMessage(`{"model":"fast"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := graphruntime.NewRegistry()
	if err := registry.Register("", passiveFactory{}); err != nil {
		t.Fatal(err)
	}
	for name, runtimeValues := range map[string]map[string]json.RawMessage{
		"missing": nil,
		"changed": {"node": json.RawMessage(`{"model":"slow"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := graphruntime.Mount(context.Background(), graphruntime.Config{
				Graph: bound.Graph, Registry: registry, Values: runtimeValues,
			}); err == nil {
				t.Fatal("mount accepted values that do not satisfy Graph IR")
			}
		})
	}
	if _, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: bound.Graph, Registry: registry, Values: bound.Values,
	}); err != nil {
		t.Fatalf("mount exact bound values: %v", err)
	}
}

func oneNodeGraph(t *testing.T) ir.Graph {
	t.Helper()
	descriptor := passiveDescriptor()
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	graph, err := ir.Freeze(ir.Graph{
		FormatVersion: ir.FormatVersion, ID: "values_graph", Revision: 1,
		Nodes: []ir.Node{{
			ID: "node", Element: identity,
			Ports: []ir.Port{{
				Name: "in", Direction: element.Input, Type: element.Event(element.Named("test.Input")),
				Cardinality: element.One, Required: true,
			}},
			Reaction: descriptor.Reaction, ConfigSchema: descriptor.ConfigSchema,
		}},
		Boundaries: []ir.Boundary{{
			Name: "in", Direction: ir.InputBoundary,
			Endpoint: ir.Endpoint{Node: "node", Port: "in"},
			Type:     element.Event(element.Named("test.Input")),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func passiveDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "test.Passive", Revision: 1,
		Ports: []element.Port{{
			Name: "in", Direction: element.Input, Type: element.Event(element.Named("test.Input")),
			Cardinality: element.One, Required: true,
		}},
		Reaction:     element.Reaction{Triggers: []string{"in"}},
		ConfigSchema: "schema://test/passive/v1",
	}
}

type passiveFactory struct{}

func (passiveFactory) Descriptor() element.Descriptor       { return passiveDescriptor() }
func (passiveFactory) ValidateConfig(json.RawMessage) error { return nil }
func (passiveFactory) Mount(context.Context, element.MountContext) (element.Runnable, error) {
	return element.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}), nil
}
