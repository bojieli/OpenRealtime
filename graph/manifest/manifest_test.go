package manifest_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/manifest"
	"github.com/bojieli/OpenRealtime/graph/syntax"
)

const yamlGraph = `apiVersion: openrealtime.ai/graph/v1alpha1
graph:
  name: voice
  nodes:
    - id: source
      element: test.Source
    - id: sink
      element: test.Sink
  edges:
    - from: source.out
      to: sink.in
      delivery: lossless
  boundaries:
    - name: trigger
      direction: input
      endpoint: source.trigger
`

func TestYAMLJSONAndNetlistLowerToEquivalentTopology(t *testing.T) {
	fromYAML, err := manifest.ParseYAML("voice.yaml", []byte(yamlGraph))
	if err != nil {
		t.Fatal(err)
	}
	document := manifest.FromSyntax(fromYAML)
	jsonSource, err := manifest.MarshalJSON(document)
	if err != nil {
		t.Fatal(err)
	}
	fromJSON, err := manifest.ParseJSON("voice.json", jsonSource)
	if err != nil {
		t.Fatal(err)
	}
	netlist := syntax.Format(fromYAML)
	fromNetlist, err := syntax.Parse("voice.ortg", []byte(netlist))
	if err != nil {
		t.Fatal(err)
	}
	for label, file := range map[string]syntax.File{"json": fromJSON, "netlist": fromNetlist} {
		if got, want := syntax.Format(file), netlist; got != want {
			t.Fatalf("%s topology differs:\n%s\nwant:\n%s", label, got, want)
		}
	}
}

func TestCanonicalYAMLRoundTrip(t *testing.T) {
	file, err := manifest.ParseYAML("voice.yaml", []byte(yamlGraph))
	if err != nil {
		t.Fatal(err)
	}
	first, err := manifest.MarshalYAML(manifest.FromSyntax(file))
	if err != nil {
		t.Fatal(err)
	}
	reparsed, err := manifest.ParseYAML("voice.yaml", first)
	if err != nil {
		t.Fatalf("canonical YAML failed to parse: %v\n%s", err, first)
	}
	second, err := manifest.MarshalYAML(manifest.FromSyntax(reparsed))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("YAML is not canonical:\n%s\n%s", first, second)
	}
}

func TestStrictYAMLRejectsUnsafeOrAmbiguousFeatures(t *testing.T) {
	fixtures := map[string]string{
		"duplicate": `apiVersion: openrealtime.ai/graph/v1alpha1
apiVersion: openrealtime.ai/graph/v1alpha1
graph: {name: x, nodes: []}`,
		"alias": `apiVersion: openrealtime.ai/graph/v1alpha1
graph: &graph {name: x, nodes: []}
copy: *graph`,
		"custom tag": `apiVersion: openrealtime.ai/graph/v1alpha1
graph: !Graph {name: x, nodes: []}`,
		"implicit boolean": `apiVersion: openrealtime.ai/graph/v1alpha1
graph: {name: true, nodes: []}`,
		"unknown field": `apiVersion: openrealtime.ai/graph/v1alpha1
graph: {name: x, nodes: [], mystery: value}`,
	}
	for name, source := range fixtures {
		t.Run(name, func(t *testing.T) {
			if _, err := manifest.ParseYAML("bad.yaml", []byte(source)); err == nil {
				t.Fatal("expected strict YAML parsing to fail")
			}
		})
	}
}

func TestManifestDiagnosticsCarryYAMLLocation(t *testing.T) {
	broken := strings.Replace(yamlGraph, "source.out", "source", 1)
	_, err := manifest.ParseYAML("broken.yaml", []byte(broken))
	if err == nil || !strings.Contains(err.Error(), "broken.yaml:10:") || !strings.Contains(err.Error(), "instance.port") {
		t.Fatalf("unexpected diagnostic: %v", err)
	}
}
