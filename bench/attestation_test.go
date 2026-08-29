package bench_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

func testDigest(character byte) string {
	return "sha256:" + strings.Repeat(string(character), 64)
}

func attestationFixture(t *testing.T) (ir.Graph, bench.ArtifactIdentity, bench.LiveResolution) {
	t.Helper()
	message := element.Event(element.Named("text.Message"))
	graph, err := ir.Freeze(ir.Graph{
		FormatVersion: ir.FormatVersion,
		ID:            "bench-agent",
		Revision:      7,
		Nodes: []ir.Node{
			{
				ID: "source", Element: element.Identity{
					Name: "test.Source", Revision: 1, Digest: testDigest('1'),
				},
				Implementation:  "go://test/source@1",
				ConfigReference: "values://bench-agent/source", ConfigDigest: testDigest('a'),
				Ports: []ir.Port{{
					Name: "out", Direction: element.Output, Type: message, Cardinality: element.One,
				}},
			},
			{
				ID: "model", Element: element.Identity{
					Name: "test.Model", Revision: 3, Digest: testDigest('2'),
				},
				Implementation:  "go://test/model@3",
				ConfigReference: "values://bench-agent/model", ConfigDigest: testDigest('b'),
				Ports: []ir.Port{
					{Name: "in", Direction: element.Input, Type: message, Cardinality: element.One},
					{Name: "out", Direction: element.Output, Type: message, Cardinality: element.One},
				},
			},
			{
				ID: "sink", Element: element.Identity{
					Name: "test.Sink", Revision: 2, Digest: testDigest('3'),
				},
				Implementation:  "go://test/sink@2",
				ConfigReference: "values://bench-agent/sink", ConfigDigest: testDigest('c'),
				Ports: []ir.Port{{
					Name: "in", Direction: element.Input, Type: message, Cardinality: element.One,
				}},
			},
		},
		Edges: []ir.Edge{
			{
				ID: "source-to-model", From: ir.Endpoint{Node: "source", Port: "out"},
				To: ir.Endpoint{Node: "model", Port: "in"}, Type: message,
				Delivery: ir.Lossless, Ordering: "fifo", Depth: 8,
			},
			{
				ID: "model-to-sink", From: ir.Endpoint{Node: "model", Port: "out"},
				To: ir.Endpoint{Node: "sink", Port: "in"}, Type: message,
				Delivery: ir.Lossless, Ordering: "fifo", Depth: 4,
			},
		},
	})
	if err != nil {
		t.Fatalf("freeze test graph: %v", err)
	}
	nodes := make(map[string]ir.Node, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
	}
	configuration := bench.ArtifactIdentity{
		ID: "values://bench-agent", Revision: "openrealtime.ai/config/v1alpha1",
		Digest: testDigest('d'),
	}
	adapter := bench.ArtifactIdentity{
		ID: "adapter://deepseek-chat", Revision: "git:8f2ac1", Digest: testDigest('e'),
	}
	resolution := bench.LiveResolution{
		// Deliberately reverse graph order: constructors must canonicalize
		// without retaining or mutating the caller's slices.
		Elements: []bench.ElementResolution{
			{
				Node: "sink", Element: nodes["sink"].Element,
				Implementation: nodes["sink"].Implementation,
				Runtime:        bench.ArtifactIdentity{ID: "builtin://speech-sink", Revision: "v2"},
			},
			{
				Node: "model", Element: nodes["model"].Element,
				Implementation: nodes["model"].Implementation,
				Runtime: bench.ArtifactIdentity{
					ID: "worker://model-4", Revision: "image:2026-08-29", Digest: testDigest('f'),
				},
				Capabilities: []bench.CapabilityIdentity{
					{
						Name: "generation", Contract: "model.text.generate/v2",
						Provider: bench.ArtifactIdentity{
							ID: "deepseek/deepseek-v3.2", Revision: "weights:2026-07-01",
							Digest: testDigest('4'),
						},
						Adapter: &adapter,
					},
				},
			},
			{
				Node: "source", Element: nodes["source"].Element,
				Implementation: nodes["source"].Implementation,
				Runtime:        bench.ArtifactIdentity{ID: "builtin://trigger-source", Revision: "v1"},
			},
		},
		Paths: []bench.SelectedPath{{
			Name: "spoken-answer", Edges: []string{"source-to-model", "model-to-sink"},
		}},
	}
	return graph, configuration, resolution
}

func graphAttestation(t *testing.T) (bench.ExecutionRequirement, bench.ExecutionEvidence) {
	t.Helper()
	graph, configuration, resolution := attestationFixture(t)
	requirement, err := bench.RequireGraph(graph, configuration, resolution)
	if err != nil {
		t.Fatalf("build graph requirement: %v", err)
	}
	attestor := bench.GraphAttestor{
		Graph: graph, Configuration: configuration,
		Resolve: func(context.Context, bench.AttestationRequest) (bench.LiveResolution, error) {
			return resolution, nil
		},
	}
	evidence, err := attestor.Attest(context.Background(), bench.AttestationRequest{
		Scope: "a", Status: binding.Status{Graph: binding.ArchitectureIdentity{
			ID: graph.ID, Revision: int(graph.Revision), Fingerprint: graph.Fingerprint,
		}},
	})
	if err != nil {
		t.Fatalf("attest graph: %v", err)
	}
	return requirement, evidence
}

func TestGraphEvidenceIsCanonicalImmutableAndStrictlySerializable(t *testing.T) {
	_, evidence := graphAttestation(t)
	if err := evidence.Validate(); err != nil {
		t.Fatalf("frozen evidence is invalid: %v", err)
	}
	if evidence.Graph.Nodes[0].Node != "model" || evidence.Graph.Nodes[1].Node != "sink" ||
		evidence.Graph.Nodes[2].Node != "source" {
		t.Fatalf("nodes were not canonicalized: %+v", evidence.Graph.Nodes)
	}

	clone := evidence.Clone()
	clone.Graph.Nodes[0].Capabilities[0].Provider.ID = "mutated/provider"
	clone.Graph.Nodes[0].Capabilities[0].Adapter.ID = "mutated/adapter"
	clone.Graph.Paths[0].Edges[0].ID = "mutated-edge"
	if evidence.Graph.Nodes[0].Capabilities[0].Provider.ID != "deepseek/deepseek-v3.2" ||
		evidence.Graph.Nodes[0].Capabilities[0].Adapter.ID != "adapter://deepseek-chat" ||
		evidence.Graph.Paths[0].Edges[0].ID != "source-to-model" {
		t.Fatal("Clone retained aliases into immutable execution evidence")
	}

	first, err := bench.MarshalExecutionEvidence(evidence)
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	second, err := bench.MarshalExecutionEvidence(evidence)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("evidence serialization is not deterministic: %v", err)
	}
	decoded, err := bench.ParseExecutionEvidence(first)
	if err != nil {
		t.Fatalf("parse evidence: %v", err)
	}
	if !reflect.DeepEqual(decoded, evidence) {
		t.Fatalf("evidence changed across serialization\nwant: %+v\n got: %+v", evidence, decoded)
	}

	unknown := bytes.Replace(first, []byte(`"kind":`), []byte(`"unknown":true,"kind":`), 1)
	if _, err := bench.ParseExecutionEvidence(unknown); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("strict decoder accepted an unknown field: %v", err)
	}
	tampered := bytes.Replace(first, []byte("deepseek-v3.2"), []byte("deepseek-v3.3"), 1)
	if _, err := bench.ParseExecutionEvidence(tampered); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("tampered evidence passed fingerprint verification: %v", err)
	}
}

func TestExecutionRequirementArtifactIsDeterministicStrictAndExplicit(t *testing.T) {
	requirement, _ := graphAttestation(t)
	first, err := bench.MarshalExecutionRequirement(requirement)
	if err != nil {
		t.Fatalf("marshal execution requirement: %v", err)
	}
	second, err := bench.MarshalExecutionRequirement(requirement)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("execution requirement serialization is not deterministic: %v", err)
	}
	decoded, err := bench.ParseExecutionRequirement(first)
	if err != nil {
		t.Fatalf("parse execution requirement: %v", err)
	}
	if !reflect.DeepEqual(decoded, requirement) {
		t.Fatalf("execution requirement changed across serialization\nwant: %+v\n got: %+v", requirement, decoded)
	}

	unknown := bytes.Replace(first, []byte(`"kind":`), []byte(`"unknown":true,"kind":`), 1)
	if _, err := bench.ParseExecutionRequirement(unknown); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("strict requirement decoder accepted an unknown field: %v", err)
	}
	duplicate := bytes.Replace(first, []byte(`"kind": "graph-native"`),
		[]byte(`"kind": "graph-native", "kind": "legacy"`), 1)
	if _, err := bench.ParseExecutionRequirement(duplicate); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("strict requirement decoder accepted a duplicate field: %v", err)
	}
	if _, err := bench.ParseExecutionRequirement(append(first, []byte("{}")...)); err == nil ||
		!strings.Contains(err.Error(), "trailing") {
		t.Fatalf("requirement decoder accepted trailing JSON: %v", err)
	}
	if _, err := bench.MarshalExecutionRequirement(bench.ExecutionRequirement{}); err == nil ||
		!strings.Contains(err.Error(), "must select") {
		t.Fatalf("historical zero value became an explicit requirement artifact: %v", err)
	}
	if _, err := bench.ParseExecutionRequirement([]byte("{}\n")); err == nil ||
		!strings.Contains(err.Error(), "must select") {
		t.Fatalf("empty explicit artifact was accepted as attestation: %v", err)
	}

	path := filepath.Join(t.TempDir(), "reviewed", "agent.execution.json")
	if err := bench.WriteExecutionRequirement(path, requirement); err != nil {
		t.Fatalf("write execution requirement: %v", err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, first) {
		t.Fatalf("written requirement is not canonical\nwant: %s\n got: %s", first, written)
	}
	read, err := bench.ReadExecutionRequirement(path)
	if err != nil {
		t.Fatalf("read execution requirement: %v", err)
	}
	if !reflect.DeepEqual(read, requirement) {
		t.Fatalf("file round trip changed requirement: %+v", read)
	}
}

func TestGraphNativeResultsRefuseMissingMismatchedOrCorruptEvidence(t *testing.T) {
	requirement, evidence := graphAttestation(t)
	result := complete("graph-native", 1, 1)
	historicalID := result.Cell.ID()
	historicalJSON, err := json.Marshal(result.Cell)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(historicalJSON, []byte(`"execution"`)) {
		t.Fatalf("zero execution contract changed historical cell JSON: %s", historicalJSON)
	}
	result.Cell.Execution = requirement
	if result.Cell.ID() == historicalID {
		t.Fatal("graph execution contract is absent from the benchmark cell identity")
	}
	attestedJSON, err := json.Marshal(result.Cell)
	if err != nil || !bytes.Contains(attestedJSON, []byte(`"execution"`)) {
		t.Fatalf("attested cell omitted its execution contract: %s (%v)", attestedJSON, err)
	}
	if err := result.Reportable(); err == nil || !strings.Contains(err.Error(), "evidence is missing") {
		t.Fatalf("graph-native result accepted a task without evidence: %v", err)
	}

	copy := evidence.Clone()
	result.Tasks[0].Execution = &copy
	if err := result.Reportable(); err != nil {
		t.Fatalf("matching graph evidence was refused: %v", err)
	}

	wrongScope := evidence.Clone()
	wrongScope.Scope = "another-task"
	wrongScope, err = bench.FreezeExecutionEvidence(wrongScope)
	if err != nil {
		t.Fatal(err)
	}
	result.Tasks[0].Execution = &wrongScope
	if err := result.Reportable(); err == nil || !strings.Contains(err.Error(), "another-task") {
		t.Fatalf("evidence from another task scope was accepted: %v", err)
	}

	mismatch := evidence.Clone()
	mismatch.Graph.Nodes[0].Runtime.Revision = "image:other"
	mismatch, err = bench.FreezeExecutionEvidence(mismatch)
	if err != nil {
		t.Fatalf("freeze valid mismatch: %v", err)
	}
	result.Tasks[0].Execution = &mismatch
	if err := result.Reportable(); err == nil || !strings.Contains(err.Error(), "resolutions differ") {
		t.Fatalf("different live resolution was accepted: %v", err)
	}

	configMismatch := evidence.Clone()
	configMismatch.Graph.Configuration.Digest = testDigest('9')
	configMismatch, err = bench.FreezeExecutionEvidence(configMismatch)
	if err != nil {
		t.Fatalf("freeze config mismatch: %v", err)
	}
	result.Tasks[0].Execution = &configMismatch
	if err := result.Reportable(); err == nil || !strings.Contains(err.Error(), "configuration artifact") {
		t.Fatalf("different configuration identity was accepted: %v", err)
	}

	pathless := evidence.Clone()
	pathless.Graph.Paths = nil
	pathless, err = bench.FreezeExecutionEvidence(pathless)
	if err != nil {
		t.Fatalf("freeze pathless evidence: %v", err)
	}
	result.Tasks[0].Execution = &pathless
	if err := result.Reportable(); err == nil || !strings.Contains(err.Error(), "required selected path") {
		t.Fatalf("missing selected route was accepted: %v", err)
	}

	corrupt := evidence.Clone()
	corrupt.Graph.Configuration.Digest = testDigest('7')
	result.Tasks[0].Execution = &corrupt
	if err := result.Reportable(); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("corrupt evidence was accepted: %v", err)
	}

	result.Tasks[0].Execution = nil
	result.Tasks[0].ExecutionError = "inspector timed out"
	if err := result.Reportable(); err == nil || !strings.Contains(err.Error(), "inspector timed out") {
		t.Fatalf("attestor failure was dropped: %v", err)
	}
}

func TestLegacyEvidenceIsExplicitAndCannotSatisfyGraphCells(t *testing.T) {
	architecture := binding.ArchitectureIdentity{
		ID: "legacy-cascade", Revision: 4, Fingerprint: testDigest('8'),
	}
	legacy, err := (bench.LegacyStatusAttestor{}).Attest(context.Background(), bench.AttestationRequest{
		Scope: "a", Status: binding.Status{Binding: "cascade", Architecture: architecture},
	})
	if err != nil {
		t.Fatalf("attest legacy status: %v", err)
	}
	if legacy.Kind != bench.ExecutionLegacy || legacy.Graph != nil || legacy.Legacy == nil {
		t.Fatalf("legacy evidence was mislabeled: %+v", legacy)
	}
	legacyRequirement, err := bench.RequireLegacy("cascade", architecture)
	if err != nil {
		t.Fatalf("build legacy requirement: %v", err)
	}
	if err := legacyRequirement.Match(&legacy); err != nil {
		t.Fatalf("matching explicit legacy evidence was refused: %v", err)
	}

	graphRequirement, graphEvidence := graphAttestation(t)
	if err := graphRequirement.Match(&legacy); err == nil || !strings.Contains(err.Error(), "kind") {
		t.Fatalf("legacy evidence satisfied a graph cell: %v", err)
	}
	if err := legacyRequirement.Match(&graphEvidence); err == nil || !strings.Contains(err.Error(), "kind") {
		t.Fatalf("graph evidence silently relabeled a legacy cell: %v", err)
	}

	graph := graphEvidence.Graph.Graph
	if _, err := (bench.LegacyStatusAttestor{}).Attest(context.Background(), bench.AttestationRequest{
		Status: binding.Status{Binding: "cascade", Graph: binding.ArchitectureIdentity{
			ID: graph.ID, Revision: int(graph.Revision), Fingerprint: graph.Fingerprint,
		}},
	}); err == nil || !strings.Contains(err.Error(), "GraphAttestor") {
		t.Fatalf("legacy attestor accepted a mounted graph: %v", err)
	}

	// Historical cells remain compatible, but valid attached legacy evidence
	// does not change their unattested execution contract.
	historical := complete("historical", 1, 1)
	historical.Tasks[0].Execution = &legacy
	if historical.Cell.Execution.Required() {
		t.Fatal("legacy evidence retroactively changed the historical cell")
	}
	if err := historical.Reportable(); err != nil {
		t.Fatalf("valid explicit legacy evidence broke historical compatibility: %v", err)
	}
}

func TestArchivedUnattestedRowsCannotSatisfyAMigratedGraphRequirement(t *testing.T) {
	requirement, _ := graphAttestation(t)
	historical := complete("historical", 1, 1)
	payload, err := json.Marshal(historical)
	if err != nil {
		t.Fatal(err)
	}
	var archived bench.Result
	if err := json.Unmarshal(payload, &archived); err != nil {
		t.Fatal(err)
	}
	if archived.Cell.Execution.Required() || archived.Tasks[0].Execution != nil {
		t.Fatalf("fixture is not an unattested historical result: %+v", archived)
	}

	// Migrating the declared treatment changes the cell identity and makes
	// evidence mandatory for every completed archived row. The old score stays
	// inspectable, but it cannot be relabeled as a graph-native measurement.
	historicalID := archived.Cell.ID()
	archived.Cell.Execution = requirement
	if archived.Cell.ID() == historicalID {
		t.Fatal("graph migration did not change the historical cell identity")
	}
	if err := archived.Reportable(); err == nil || !strings.Contains(err.Error(), "evidence is missing") {
		t.Fatalf("an archived unattested row satisfied a migrated graph requirement: %v", err)
	}
}

func TestGraphAttestorRefusesStatusMismatchAndIncompleteResolution(t *testing.T) {
	graph, configuration, resolution := attestationFixture(t)
	attestor := bench.GraphAttestor{
		Graph: graph, Configuration: configuration,
		Resolve: func(context.Context, bench.AttestationRequest) (bench.LiveResolution, error) {
			return resolution, nil
		},
	}
	wrong := binding.Status{Graph: binding.ArchitectureIdentity{
		ID: graph.ID, Revision: int(graph.Revision), Fingerprint: testDigest('0'),
	}}
	if _, err := attestor.Attest(context.Background(), bench.AttestationRequest{Status: wrong}); err == nil || !strings.Contains(err.Error(), "live graph") {
		t.Fatalf("attestor accepted a different mounted graph: %v", err)
	}

	incomplete := resolution
	incomplete.Elements = incomplete.Elements[1:]
	attestor.Resolve = func(context.Context, bench.AttestationRequest) (bench.LiveResolution, error) {
		return incomplete, nil
	}
	status := binding.Status{Graph: binding.ArchitectureIdentity{
		ID: graph.ID, Revision: int(graph.Revision), Fingerprint: graph.Fingerprint,
	}}
	if _, err := attestor.Attest(context.Background(), bench.AttestationRequest{Status: status}); err == nil || !strings.Contains(err.Error(), "missing graph node") {
		t.Fatalf("attestor accepted incomplete live resolution: %v", err)
	}

	attestor.Resolve = func(context.Context, bench.AttestationRequest) (bench.LiveResolution, error) {
		return resolution, nil
	}
	if _, err := attestor.Attest(context.Background(), bench.AttestationRequest{Status: status}); err == nil ||
		!strings.Contains(err.Error(), "selected routes require") {
		t.Fatalf("attestor accepted task-selected routes without a scope: %v", err)
	}
}
