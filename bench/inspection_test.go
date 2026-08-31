package bench_test

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

func TestResolutionFromInspectionUsesLiveIdentitiesAndSemanticPaths(t *testing.T) {
	graph, configuration, expected := attestationFixture(t)
	snapshot := liveInspectionFixture(t, graph, configuration, expected)

	resolution, err := bench.ResolutionFromInspection(graph, configuration, expected, snapshot)
	if err != nil {
		t.Fatalf("convert live inspection: %v", err)
	}
	canonical, err := bench.ParseExpectedResolution(mustExpectedResolution(t, expected))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resolution, canonical) {
		t.Fatalf("live resolution differs\nwant: %+v\n got: %+v", canonical, resolution)
	}
	if resolution.Paths[0].Name != "spoken-answer" ||
		resolution.Paths[0].Name == snapshot.Flows["trace:task-7"].Correlation {
		t.Fatalf("correlation ID became a semantic path name: %+v", resolution.Paths)
	}

	// The benchmark evidence must not retain aliases into a later mutable live
	// snapshot. This covers nested adapter identities and route slices as well
	// as the outer resolution values.
	snapshot.Nodes["model"].Resolution.Runtime.ID = "worker://mutated"
	snapshot.Nodes["model"].Resolution.Capabilities[0].Provider.ID = "mutated/provider"
	snapshot.Nodes["model"].Resolution.Capabilities[0].Adapter.ID = "mutated/adapter"
	flow := snapshot.Flows["trace:task-7"]
	flow.Edges[1] = "mutated-edge"
	snapshot.Flows["trace:task-7"] = flow
	model := resolutionElement(t, resolution, "model")
	if model.Runtime.ID != "worker://model-4" ||
		model.Capabilities[0].Provider.ID != "deepseek/deepseek-v3.2" ||
		model.Capabilities[0].Adapter.ID != "adapter://deepseek-chat" ||
		resolution.Paths[0].Edges[0] != "source-to-model" {
		t.Fatal("converted resolution retained aliases into inspect.Live")
	}
}

func TestAuthorExpectedResolutionFromInspectionRequiresExactLiveProbe(t *testing.T) {
	graph, configuration, expected := attestationFixture(t)
	deployment := benchmarkDeploymentEvidence()
	expected.Deployment = &deployment
	snapshot := liveInspectionFixture(t, graph, configuration, expected)
	for nodeID, live := range snapshot.Nodes {
		live.Resolution.RuntimeEvidence = inspect.EvidenceLive
		live.Resolution.CapabilitiesEvidence = inspect.EvidenceLive
		snapshot.Nodes[nodeID] = live
	}

	authored, err := bench.AuthorExpectedResolutionFromInspection(graph, configuration, snapshot)
	if err != nil {
		t.Fatalf("author expected resolution: %v", err)
	}
	expected.Paths = nil
	canonical, err := bench.ParseExpectedResolution(mustExpectedResolution(t, expected))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(authored, canonical) {
		t.Fatalf("authored resolution differs\nwant: %+v\n got: %+v", canonical, authored)
	}

	// The reviewed contract must own every nested identity independently from
	// the mutable management snapshot used to author it.
	snapshot.Deployment.PrivateDeploymentFingerprint = testDigest('1')
	snapshot.Nodes["model"].Resolution.Runtime.ID = "runtime://mutated"
	snapshot.Nodes["model"].Resolution.Capabilities[0].Provider.ID = "provider://mutated"
	model := resolutionElement(t, authored, "model")
	if authored.Deployment.PrivateDeploymentFingerprint != deployment.PrivateDeploymentFingerprint ||
		model.Runtime.ID != "worker://model-4" ||
		model.Capabilities[0].Provider.ID != "deepseek/deepseek-v3.2" {
		t.Fatal("authored expected resolution retained aliases into the probe snapshot")
	}

	valid := liveInspectionFixture(t, graph, configuration, expected)
	for nodeID, live := range valid.Nodes {
		live.Resolution.RuntimeEvidence = inspect.EvidenceLive
		live.Resolution.CapabilitiesEvidence = inspect.EvidenceLive
		valid.Nodes[nodeID] = live
	}
	for _, test := range []struct {
		name string
		want string
		edit func(*inspect.Live)
	}{
		{"missing deployment", "no exact deployment", func(value *inspect.Live) { value.Deployment = nil }},
		{"mounted lifecycle", "state is \"mounted\"", func(value *inspect.Live) { value.State = "mounted" }},
		{"mounted node", "state is \"mounted\"", func(value *inspect.Live) {
			node := value.Nodes["source"]
			node.State = "mounted"
			value.Nodes["source"] = node
		}},
		{"registered runtime", "runtime has \"registered\"", func(value *inspect.Live) {
			value.Nodes["source"].Resolution.RuntimeEvidence = inspect.EvidenceRegistered
		}},
		{"registered empty capabilities", "capabilities have \"registered\"", func(value *inspect.Live) {
			value.Nodes["source"].Resolution.CapabilitiesEvidence = inspect.EvidenceRegistered
		}},
		{"missing node", "missing graph node", func(value *inspect.Live) { delete(value.Nodes, "sink") }},
		{"extra node", "extra node", func(value *inspect.Live) { value.Nodes["extra"] = inspect.NodeLive{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid.Clone()
			test.edit(&candidate)
			_, err := bench.AuthorExpectedResolutionFromInspection(graph, configuration, candidate)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid probe was accepted: %v", err)
			}
		})
	}
}

func TestResolutionFromInspectionRefusesUnattestedOrAmbiguousSnapshots(t *testing.T) {
	graph, configuration, expected := attestationFixture(t)
	valid := liveInspectionFixture(t, graph, configuration, expected)
	tests := []struct {
		name   string
		want   string
		mutate func(*inspect.Live)
	}{
		{name: "wrong live format", want: "format is", mutate: func(value *inspect.Live) {
			value.FormatVersion++
		}},
		{name: "graph fingerprint mismatch", want: "live graph is", mutate: func(value *inspect.Live) {
			value.Fingerprint = testDigest('9')
		}},
		{name: "missing configuration", want: "no configuration", mutate: func(value *inspect.Live) {
			value.Configuration = nil
		}},
		{name: "configuration mismatch", want: "live configuration is", mutate: func(value *inspect.Live) {
			value.Configuration.Digest = testDigest('9')
		}},
		{name: "failed lifecycle", want: "failed", mutate: func(value *inspect.Live) {
			value.State, value.Error = "closed", "worker failed"
		}},
		{name: "missing node", want: "missing graph node", mutate: func(value *inspect.Live) {
			delete(value.Nodes, "sink")
		}},
		{name: "extra node", want: "extra node", mutate: func(value *inspect.Live) {
			value.Nodes["other"] = inspect.NodeLive{}
		}},
		{name: "missing resolution", want: "has no resolution", mutate: func(value *inspect.Live) {
			node := value.Nodes["sink"]
			node.Resolution = nil
			value.Nodes["sink"] = node
		}},
		{name: "wrong element", want: "element is", mutate: func(value *inspect.Live) {
			value.Nodes["sink"].Resolution.Element = element.Identity{
				Name: "test.Other", Revision: 1, Digest: testDigest('8'),
			}
		}},
		{name: "wrong implementation", want: "implementation is", mutate: func(value *inspect.Live) {
			value.Nodes["sink"].Resolution.Implementation = "go://test/other@1"
		}},
		{name: "declaration-only runtime", want: "declaration-only", mutate: func(value *inspect.Live) {
			value.Nodes["source"].Resolution.RuntimeEvidence = inspect.EvidenceDeclared
		}},
		{name: "missing live capability evidence", want: "required capabilities", mutate: func(value *inspect.Live) {
			value.Nodes["model"].Resolution.CapabilitiesEvidence = inspect.EvidenceRegistered
		}},
		{name: "placeholder runtime", want: "placeholder", mutate: func(value *inspect.Live) {
			value.Nodes["source"].Resolution.Runtime.Revision = "main"
		}},
		{name: "dropped route trace", want: "trace dropped", mutate: func(value *inspect.Live) {
			value.TraceDropped = 1
		}},
		{name: "truncated route", want: "truncated", mutate: func(value *inspect.Live) {
			flow := value.Flows["trace:task-7"]
			flow.Truncated = true
			value.Flows["trace:task-7"] = flow
		}},
		{name: "unknown route edge", want: "unknown graph edge", mutate: func(value *inspect.Live) {
			flow := value.Flows["trace:task-7"]
			flow.Edges = append(flow.Edges, "not-in-graph")
			value.Flows["trace:task-7"] = flow
		}},
		{name: "unmatched semantic path", want: "was not observed", mutate: func(value *inspect.Live) {
			flow := value.Flows["trace:task-7"]
			flow.Edges = []string{"source-to-model"}
			value.Flows["trace:task-7"] = flow
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := valid.Clone()
			test.mutate(&snapshot)
			_, err := bench.ResolutionFromInspection(graph, configuration, expected, snapshot)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid live snapshot was accepted: %v", err)
			}
		})
	}
}

func TestResolutionFromInspectionDoesNotRequireRouteTraceWithoutSemanticPaths(t *testing.T) {
	graph, configuration, expected := attestationFixture(t)
	expected.Paths = nil
	snapshot := liveInspectionFixture(t, graph, configuration, expected)
	snapshot.TraceDropped = 3
	snapshot.Flows = map[string]inspect.FlowLive{
		"trace:old": {Correlation: "trace:old", Edges: []string{"unknown"}, Truncated: true},
	}
	resolution, err := bench.ResolutionFromInspection(graph, configuration, expected, snapshot)
	if err != nil {
		t.Fatalf("irrelevant route telemetry blocked identity attestation: %v", err)
	}
	if len(resolution.Paths) != 0 {
		t.Fatalf("unrequested correlations became selected paths: %+v", resolution.Paths)
	}
}

func TestBenchmarkEvidenceCarriesExactRedactedDeploymentAndRejectsDrift(t *testing.T) {
	graph, configuration, expected := attestationFixture(t)
	deployment := benchmarkDeploymentEvidence()
	expected.Deployment = &deployment
	snapshot := liveInspectionFixture(t, graph, configuration, expected)

	resolution, err := bench.ResolutionFromInspection(graph, configuration, expected, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Deployment == nil || !reflect.DeepEqual(*resolution.Deployment, deployment) {
		t.Fatalf("inspection deployment = %+v", resolution.Deployment)
	}
	requirement, err := bench.RequireGraph(graph, configuration, resolution)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := bench.FreezeExecutionEvidence(bench.ExecutionEvidence{
		FormatVersion: bench.AttestationFormatVersion, Kind: bench.ExecutionGraphNative,
		Scope: "deployment-evidence-test", Graph: requirement.Graph,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := requirement.Match(&frozen); err != nil {
		t.Fatal(err)
	}
	payload, err := bench.MarshalExecutionEvidence(frozen)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"secret://private/reference", "PRIVATE_PROVIDER_LOCATOR", "private-credential-bytes",
	} {
		if bytes.Contains(payload, []byte(forbidden)) {
			t.Fatalf("benchmark evidence leaked %q: %s", forbidden, payload)
		}
	}
	for _, required := range []string{
		deployment.PrivateDeploymentFingerprint, deployment.SecretCatalogFingerprint,
	} {
		if !bytes.Contains(payload, []byte(required)) {
			t.Fatalf("benchmark evidence omitted %q", required)
		}
	}

	drifted := frozen.Clone()
	drifted.Graph.Deployment.SecretCatalogFingerprint = testDigest('9')
	drifted, err = bench.FreezeExecutionEvidence(drifted)
	if err != nil {
		t.Fatal(err)
	}
	if err := requirement.Match(&drifted); err == nil || !strings.Contains(err.Error(), "deployment") {
		t.Fatalf("deployment drift match error = %v", err)
	}
	snapshot.Deployment.SecretCatalogFingerprint = testDigest('8')
	if _, err := bench.ResolutionFromInspection(graph, configuration, expected, snapshot); err == nil ||
		!strings.Contains(err.Error(), "drifted") {
		t.Fatalf("inspection deployment drift error = %v", err)
	}
}

func liveInspectionFixture(
	t testing.TB, graph ir.Graph, configuration bench.ArtifactIdentity, expected bench.LiveResolution,
) inspect.Live {
	t.Helper()
	nodes := make(map[string]ir.Node, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
	}
	live := inspect.Live{
		FormatVersion: inspect.LiveFormatVersion,
		GraphID:       graph.ID, GraphRevision: graph.Revision, Fingerprint: graph.Fingerprint,
		Configuration: &inspect.ArtifactIdentity{
			ID: configuration.ID, Revision: configuration.Revision, Digest: configuration.Digest,
		},
		Sequence: 4, State: "running", Nodes: make(map[string]inspect.NodeLive, len(graph.Nodes)),
		Flows: map[string]inspect.FlowLive{
			"trace:task-7": {
				Correlation: "trace:task-7",
				// Boundary queues are deliberately present to prove that only Graph
				// IR edges contribute to a semantic selected path. The repeated edge
				// also models an interleaved fork/retry traversal.
				Edges: []string{
					"boundary:input", "source-to-model", "source-to-model",
					"model-to-sink", "boundary:output",
				},
			},
		},
	}
	if expected.Deployment != nil {
		copy := expected.Deployment.Clone()
		live.Deployment = &copy
	}
	for _, item := range expected.Elements {
		node := nodes[item.Node]
		resolution := &inspect.NodeResolution{
			Element: node.Element, Implementation: node.Implementation,
			Runtime: inspect.ArtifactIdentity{
				ID: item.Runtime.ID, Revision: item.Runtime.Revision, Digest: item.Runtime.Digest,
			},
			RuntimeEvidence: inspect.EvidenceRegistered,
		}
		if len(item.Capabilities) > 0 {
			resolution.RuntimeEvidence = inspect.EvidenceLive
			resolution.CapabilitiesEvidence = inspect.EvidenceLive
			for _, capability := range item.Capabilities {
				converted := inspect.CapabilityIdentity{
					Name: capability.Name, Contract: capability.Contract,
					Provider: inspect.ArtifactIdentity{
						ID: capability.Provider.ID, Revision: capability.Provider.Revision,
						Digest: capability.Provider.Digest,
					},
				}
				if capability.Adapter != nil {
					converted.Adapter = &inspect.ArtifactIdentity{
						ID: capability.Adapter.ID, Revision: capability.Adapter.Revision,
						Digest: capability.Adapter.Digest,
					}
				}
				resolution.Capabilities = append(resolution.Capabilities, converted)
			}
		}
		live.Nodes[item.Node] = inspect.NodeLive{State: "running", Resolution: resolution}
	}
	return live
}

func BenchmarkResolutionFromInspectionWithDeploymentEvidence(b *testing.B) {
	graph, configuration, expected := attestationFixture(b)
	deployment := benchmarkDeploymentEvidence()
	expected.Deployment = &deployment
	snapshot := liveInspectionFixture(b, graph, configuration, expected)
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if _, err := bench.ResolutionFromInspection(graph, configuration, expected, snapshot); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkDeploymentEvidence() inspect.DeploymentEvidence {
	return inspect.DeploymentEvidence{
		Public: inspect.ArtifactIdentity{
			ID: "deployment://bench-agent", Revision: "openrealtime.ai/deployment/v1alpha1",
			Digest: testDigest('5'),
		},
		PrivateDeploymentFingerprint: testDigest('6'),
		SecretCatalogFingerprint:     testDigest('7'),
		Secrets: []inspect.SecretProviderEvidence{{
			Node: "model", Slot: "token", BindingFingerprint: testDigest('8'),
			Provider: "vault", Runtime: inspect.ArtifactIdentity{
				ID: "provider://vault", Revision: "image:1", Digest: testDigest('9'),
			},
		}},
	}
}

func resolutionElement(t *testing.T, resolution bench.LiveResolution, node string) bench.ElementResolution {
	t.Helper()
	for _, item := range resolution.Elements {
		if item.Node == node {
			return item
		}
	}
	t.Fatalf("resolution has no node %q", node)
	return bench.ElementResolution{}
}
