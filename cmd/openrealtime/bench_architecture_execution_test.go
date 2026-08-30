package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	archbench "github.com/bojieli/OpenRealtime/bench/architecture"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
)

func TestArchitectureExecutionAttachmentIsExplicitAndIdentityBearing(t *testing.T) {
	cell := archbench.Cell{Name: "reviewed-cell"}
	diagnosticID := cell.MeasurementCell().ID()
	if err := attachArchitectureExecution(&cell, ""); err != nil {
		t.Fatalf("omitted execution artifact changed diagnostic authoring: %v", err)
	}
	if cell.Execution.Required() || cell.MeasurementCell().ID() != diagnosticID {
		t.Fatalf("omitted execution artifact changed the cell: %+v", cell.Execution)
	}

	fixture := writeGraphExecutionFixture(t)
	requirement := requirementForGraphFixture(t, fixture)
	path := filepath.Join(t.TempDir(), "cell.execution.json")
	if err := bench.WriteExecutionRequirement(path, requirement); err != nil {
		t.Fatal(err)
	}
	if err := attachArchitectureExecution(&cell, path); err != nil {
		t.Fatalf("attach reviewed execution requirement: %v", err)
	}
	if cell.Execution.Kind != bench.ExecutionGraphNative || cell.MeasurementCell().ID() == diagnosticID {
		t.Fatalf("execution requirement was not attached to cell identity: %+v", cell.Execution)
	}
}

func TestArchitectureExecutionAttachmentRejectsEvidenceAndEmptyArtifacts(t *testing.T) {
	directory := t.TempDir()
	empty := filepath.Join(directory, "empty.json")
	if err := os.WriteFile(empty, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cell := archbench.Cell{Name: "reviewed-cell"}
	if err := attachArchitectureExecution(&cell, empty); err == nil || !strings.Contains(err.Error(), "must select") {
		t.Fatalf("empty explicit execution artifact was accepted: %v", err)
	}

	fixture := writeGraphExecutionFixture(t)
	requirement := requirementForGraphFixture(t, fixture)
	evidence, err := bench.FreezeExecutionEvidence(bench.ExecutionEvidence{
		FormatVersion: bench.AttestationFormatVersion,
		Kind:          bench.ExecutionGraphNative,
		Scope:         "reviewed-cell",
		Graph:         requirement.Graph,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := bench.MarshalExecutionEvidence(evidence)
	if err != nil {
		t.Fatal(err)
	}
	evidencePath := filepath.Join(directory, "observed-evidence.json")
	if err := os.WriteFile(evidencePath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := attachArchitectureExecution(&cell, evidencePath); err == nil ||
		!strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("observed evidence was accepted as an expected requirement: %v", err)
	}
}

func TestBenchExecutionGraphAuthorsRequirementFromExactIndependentArtifacts(t *testing.T) {
	fixture := writeGraphExecutionFixture(t)
	var output bytes.Buffer
	if err := runBench([]string{
		"execution", "graph",
		"-graph", fixture.graphPath,
		"-values", fixture.valuesPath,
		"-resolution", fixture.resolutionPath,
		"-out", fixture.requirementPath,
	}, &output); err != nil {
		t.Fatalf("author graph execution requirement: %v", err)
	}
	if !strings.Contains(output.String(), fixture.graph.Fingerprint) ||
		!strings.Contains(output.String(), fixture.bound.Fingerprint) {
		t.Fatalf("authoring output omitted exact identities: %s", output.String())
	}
	actual, err := bench.ReadExecutionRequirement(fixture.requirementPath)
	if err != nil {
		t.Fatal(err)
	}
	want, err := bench.RequireGraph(fixture.graph, bench.ArtifactIdentity{
		ID: "values://" + fixture.graph.ID, Revision: graphvalues.APIVersion,
		Digest: fixture.bound.Fingerprint,
	}, fixture.resolution)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("authored requirement differs\nwant: %+v\n got: %+v", want, actual)
	}
}

func TestBenchExecutionRejectsRemovedLegacyKind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "must-not-exist.json")
	err := runBench([]string{
		"execution", "legacy", "-binding", "cascade", "-out", path,
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "must be graph-native") {
		t.Fatalf("removed legacy execution kind was accepted: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("removed legacy command created output: %v", statErr)
	}
}

func TestBenchExecutionGraphRefusesUnboundMismatchedAndIncompleteInputs(t *testing.T) {
	fixture := writeGraphExecutionFixture(t)
	t.Run("unbound graph", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "unbound.ir.json")
		payload, err := fixture.unbound.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		err = runBench([]string{
			"execution", "graph", "-graph", path, "-values", fixture.valuesPath,
			"-resolution", fixture.resolutionPath, "-out", filepath.Join(t.TempDir(), "result.json"),
		}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "no exact configuration") {
			t.Fatalf("unbound Graph IR was accepted: %v", err)
		}
	})
	t.Run("different values", func(t *testing.T) {
		document := fixture.values
		document.Nodes = map[string]json.RawMessage{"model": json.RawMessage(`{"temperature":1}`)}
		payload, err := graphvalues.MarshalJSON(document)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "other.values.json")
		if err := os.WriteFile(path, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		err = runBench([]string{
			"execution", "graph", "-graph", fixture.graphPath, "-values", path,
			"-resolution", fixture.resolutionPath, "-out", filepath.Join(t.TempDir(), "result.json"),
		}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "does not exactly match") {
			t.Fatalf("different values artifact was accepted: %v", err)
		}
	})
	t.Run("incomplete resolution", func(t *testing.T) {
		resolution, err := bench.ReadExpectedResolution(fixture.resolutionPath)
		if err != nil {
			t.Fatal(err)
		}
		resolution.Elements[0].Node = "other"
		path := filepath.Join(t.TempDir(), "incomplete.resolution.json")
		if err := bench.WriteExpectedResolution(path, resolution); err != nil {
			t.Fatal(err)
		}
		err = runBench([]string{
			"execution", "graph", "-graph", fixture.graphPath, "-values", fixture.valuesPath,
			"-resolution", path, "-out", filepath.Join(t.TempDir(), "result.json"),
		}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "missing graph node") {
			t.Fatalf("incomplete expected resolution was accepted: %v", err)
		}
	})
	t.Run("placeholder configuration revision", func(t *testing.T) {
		err := runBench([]string{
			"execution", "graph", "-graph", fixture.graphPath, "-values", fixture.valuesPath,
			"-resolution", fixture.resolutionPath, "-configuration-revision", "latest",
			"-out", filepath.Join(t.TempDir(), "result.json"),
		}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "live placeholder") {
			t.Fatalf("placeholder configuration revision was accepted: %v", err)
		}
	})
}

type graphExecutionFixture struct {
	unbound, graph                                         ir.Graph
	bound                                                  graphvalues.Bound
	values                                                 graphvalues.Document
	resolution                                             bench.LiveResolution
	graphPath, valuesPath, resolutionPath, requirementPath string
}

func writeGraphExecutionFixture(t *testing.T) graphExecutionFixture {
	t.Helper()
	digest := "sha256:" + strings.Repeat("1", 64)
	message := element.Event(element.Named("test.Message"))
	unbound, err := ir.Freeze(ir.Graph{
		FormatVersion: ir.FormatVersion, ID: "cli-agent", Revision: 1,
		Nodes: []ir.Node{{
			ID: "model", Element: element.Identity{Name: "test.Model", Revision: 1, Digest: digest},
			Implementation: "go://test/model/v1",
			Ports: []ir.Port{{
				Name: "out", Direction: element.Output, Type: message, Cardinality: element.One,
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	values := graphvalues.Document{
		APIVersion: graphvalues.APIVersion, Graph: unbound.ID,
		Nodes: map[string]json.RawMessage{"model": json.RawMessage(`{"temperature":0}`)},
	}
	bound, err := graphvalues.Bind(unbound, values)
	if err != nil {
		t.Fatal(err)
	}
	resolution := bench.LiveResolution{Elements: []bench.ElementResolution{{
		Node: "model", Element: bound.Graph.Nodes[0].Element,
		Implementation: bound.Graph.Nodes[0].Implementation,
		Runtime:        bench.ArtifactIdentity{ID: "image://openrealtime/model", Revision: "image:2026-08-29"},
		Capabilities: []bench.CapabilityIdentity{{
			Name: "generation", Contract: "model.text.generate/v1",
			Provider: bench.ArtifactIdentity{ID: "deepseek/deepseek-v3.2", Revision: "weights:2026-07-01"},
		}},
	}}}
	directory := t.TempDir()
	fixture := graphExecutionFixture{
		unbound: unbound, graph: bound.Graph, bound: bound, values: values, resolution: resolution,
		graphPath:       filepath.Join(directory, "agent.ir.json"),
		valuesPath:      filepath.Join(directory, "agent.values.json"),
		resolutionPath:  filepath.Join(directory, "agent.resolution.json"),
		requirementPath: filepath.Join(directory, "agent.execution.json"),
	}
	graphPayload, err := bound.Graph.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	valuesPayload, err := graphvalues.MarshalJSON(values)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.graphPath, graphPayload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.valuesPath, valuesPayload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := bench.WriteExpectedResolution(fixture.resolutionPath, resolution); err != nil {
		t.Fatal(err)
	}
	return fixture
}
