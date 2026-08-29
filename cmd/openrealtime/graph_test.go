package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/element/codec"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

func TestGraphCommandUpdateCheckCompileAndRender(t *testing.T) {
	directory := t.TempDir()
	graphPath := filepath.Join(directory, "agent.ortg")
	descriptorPath := filepath.Join(directory, "elements.json")
	lockPath := filepath.Join(directory, "openrealtime.lock")
	irPath := filepath.Join(directory, "agent.ir.json")
	valuesPath := filepath.Join(directory, "agent.values.yaml")
	deploymentPath := filepath.Join(directory, "agent.deployment.yaml")
	secretsPath := filepath.Join(directory, "agent.secrets.yaml")
	if err := os.WriteFile(graphPath, []byte(`graph agent {
    test.Source :: source;
    test.Sink :: sink;
    source.out -> sink.in;
    input trigger = source.trigger;
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle, err := codec.MarshalJSON(codec.New(graphCommandDescriptors()...))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(descriptorPath, bundle, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(valuesPath, []byte(`apiVersion: openrealtime.ai/config/v1alpha1
graph: agent
nodes:
  source:
    mode: eager
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deploymentPath, []byte(`apiVersion: openrealtime.ai/deployment/v1alpha1
graph: agent
nodes:
  source:
    implementation: test/source-v1
    placement: local
    secrets:
      token: secret://test/source-token
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretsPath, []byte(`apiVersion: openrealtime.ai/secrets/v1alpha1
catalog: agent
secrets:
  secret://test/source-token:
    provider: environment
    locator: TEST_SOURCE_TOKEN
`), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := runGraph([]string{"update", "-descriptor", descriptorPath, "-lock", lockPath, graphPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "sha256:") {
		t.Fatalf("update output = %q", stdout.String())
	}
	stdout.Reset()
	if err := runGraph([]string{"check", "-descriptor", descriptorPath, "-lock", lockPath, "-values", valuesPath, "-deployment", deploymentPath, "-secrets", secretsPath, "-profile", "core", graphPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "sha256:") {
		t.Fatalf("check output = %q", stdout.String())
	}
	stdout.Reset()
	if err := runGraph([]string{"compile", "-descriptor", descriptorPath, "-lock", lockPath, "-values", valuesPath, "-deployment", deploymentPath, "-secrets", secretsPath, "-profile", "core", "-out", irPath, graphPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	irSource, err := os.ReadFile(irPath)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := ir.Parse(irSource)
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Nodes[0].ConfigDigest == "" || compiled.Nodes[0].ConfigReference == "" {
		t.Fatalf("compiled values identity is missing: %+v", compiled.Nodes[0])
	}
	if compiled.Nodes[0].Implementation == "" || compiled.Nodes[0].DeploymentDigest == "" ||
		compiled.Nodes[0].DeploymentReference == "" {
		t.Fatalf("compiled deployment identity is missing: %+v", compiled.Nodes[0])
	}
	stdout.Reset()
	if err := runGraph([]string{"render", "-descriptor", descriptorPath, "-lock", lockPath, "-values", valuesPath, "-deployment", deploymentPath, "-secrets", secretsPath, "-profile", "core", graphPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), compiled.Fingerprint) || !strings.Contains(stdout.String(), "flowchart LR") {
		t.Fatalf("render output does not attest graph:\n%s", stdout.String())
	}
}

func TestGraphFmtAndNormalize(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "compact.ortg")
	if err := os.WriteFile(path, []byte("graph compact{flow.Drop::drop;input in=drop.in;}"), 0o644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runGraph([]string{"fmt", "-w", path}, &output, &output); err != nil {
		t.Fatal(err)
	}
	formatted, _ := os.ReadFile(path)
	if !strings.Contains(string(formatted), "flow.Drop :: drop;") {
		t.Fatalf("formatted source = %s", formatted)
	}
	if err := runGraph([]string{"fmt", "-check", path}, &output, &output); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := runGraph([]string{"normalize", "-format", "json", path}, &output, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"apiVersion": "openrealtime.ai/graph/v1alpha1"`) {
		t.Fatalf("normalized output = %s", output.String())
	}
}

func graphCommandDescriptors() []element.Descriptor {
	valueType := element.Event(element.Named("test.Value"))
	return []element.Descriptor{
		{
			FormatVersion: element.DescriptorFormatVersion, Name: "test.Source", Revision: 1,
			Ports: []element.Port{
				{Name: "trigger", Direction: element.Input, Type: element.Trigger(element.Named("test.Start")), Cardinality: element.One, Required: true},
				{Name: "out", Direction: element.Output, Type: valueType, Cardinality: element.One, Required: true},
			},
			Reaction: element.Reaction{Triggers: []string{"trigger"}, Outcomes: []string{"out"}},
		},
		{
			FormatVersion: element.DescriptorFormatVersion, Name: "test.Sink", Revision: 1,
			Ports: []element.Port{{Name: "in", Direction: element.Input, Type: valueType, Cardinality: element.One, Required: true}},
		},
	}
}
