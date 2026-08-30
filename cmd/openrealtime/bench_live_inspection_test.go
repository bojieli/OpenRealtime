package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
)

func TestConfigureSessionBenchmarkAttestorBindsReviewedGraphAndOneCredential(t *testing.T) {
	fixture := writeGraphExecutionFixture(t)
	requirement := requirementForGraphFixture(t, fixture)
	access := benchmarkInspectionAccess("sess_cli_exact")
	snapshot := inspectionSnapshotForFixture(fixture, requirement)
	const secret = "deployment-secret-that-must-not-be-serialized"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path != access.Path {
			t.Errorf("inspection path = %q, want %q", request.URL.Path, access.Path)
		}
		if request.Header.Get("Authorization") != "Bearer "+secret {
			t.Errorf("deployment authorization = %q", request.Header.Get("Authorization"))
		}
		if request.Header.Get(openrealtime.InspectionTokenHeader) != access.Token {
			t.Errorf("session inspection capability was not presented")
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(writer).Encode(snapshot); err != nil {
			t.Errorf("encode inspection snapshot: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	var environmentReads atomic.Int32
	attestor, sessionToken, err := configureSessionBenchmarkAttestor(
		requirement,
		fixture.graphPath,
		"ws"+strings.TrimPrefix(server.URL, "http"),
		"BENCHMARK_TEST_TOKEN",
		func(name string) string {
			environmentReads.Add(1)
			if name != "BENCHMARK_TEST_TOKEN" {
				t.Errorf("environment key = %q", name)
			}
			return secret
		},
	)
	if err != nil {
		t.Fatalf("configure graph inspection attestor: %v", err)
	}
	if sessionToken != secret || environmentReads.Load() != 1 {
		t.Fatalf("session token = %q, environment reads = %d", sessionToken, environmentReads.Load())
	}
	if _, ok := attestor.(bench.GraphAttestor); !ok {
		t.Fatalf("configured attestor = %T, want bench.GraphAttestor", attestor)
	}

	evidence, err := attestor.Attest(context.Background(), bench.AttestationRequest{
		Scope: "task-1",
		Status: binding.Status{Graph: binding.ArchitectureIdentity{
			ID: fixture.graph.ID, Revision: int(fixture.graph.Revision),
			Fingerprint: fixture.graph.Fingerprint,
		}},
		Inspection: &access,
	})
	if err != nil {
		t.Fatalf("attest through authenticated inspection: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("inspection requests = %d, want 1", requests.Load())
	}
	if err := requirement.Match(&evidence); err != nil {
		t.Fatalf("live evidence does not satisfy reviewed requirement: %v", err)
	}
	graphSource, err := os.ReadFile(fixture.graphPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(graphSource, []byte(secret)) {
		t.Fatal("deployment credential was embedded in Graph IR")
	}
}

func TestConfigureSessionBenchmarkAttestorRefusesGraphDriftBeforeCredentialLookup(t *testing.T) {
	fixture := writeGraphExecutionFixture(t)
	requirement := requirementForGraphFixture(t, fixture)
	directory := t.TempDir()
	differentGraphPath := filepath.Join(directory, "different.ir.json")
	differentGraph, err := fixture.unbound.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(differentGraphPath, differentGraph, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		requirement bench.ExecutionRequirement
		graphPath   string
		want        string
	}{
		{
			name: "missing graph option", requirement: requirement,
			want: "-inspection-graph",
		},
		{
			name: "absent artifact", requirement: requirement,
			graphPath: filepath.Join(directory, "absent.ir.json"), want: "read inspection Graph IR",
		},
		{
			name: "graph identity mismatch", requirement: requirement,
			graphPath: differentGraphPath, want: "inspection graph identity",
		},
		{
			name: "reviewed node config mismatch",
			requirement: func() bench.ExecutionRequirement {
				changed := requirement
				copy := *requirement.Graph
				copy.Nodes = append([]bench.GraphNodeEvidence(nil), requirement.Graph.Nodes...)
				copy.Nodes[0].Config.Digest = "sha256:" + strings.Repeat("9", 64)
				changed.Graph = &copy
				return changed
			}(),
			graphPath: fixture.graphPath, want: "does not reproduce the reviewed node configuration",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var reads atomic.Int32
			_, _, err := configureSessionBenchmarkAttestor(
				test.requirement, test.graphPath, "ws://127.0.0.1:8765/v1/realtime",
				"BENCHMARK_TEST_TOKEN", func(string) string {
					reads.Add(1)
					return "secret-must-not-appear"
				},
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("refusal = %v, want substring %q", err, test.want)
			}
			if reads.Load() != 0 {
				t.Fatalf("credential environment was read %d time(s) before refusal", reads.Load())
			}
			if strings.Contains(err.Error(), "secret-must-not-appear") {
				t.Fatalf("refusal exposed a deployment credential: %v", err)
			}
		})
	}
}

func TestPrepareReviewedGraphInspectionPreservesExactDeploymentEvidence(t *testing.T) {
	fixture := writeGraphExecutionFixture(t)
	fixture.resolution.Deployment = &inspect.DeploymentEvidence{
		Public: inspect.ArtifactIdentity{
			ID: "deployment://benchmark-test", Revision: "deployment:1",
			Digest: "sha256:" + strings.Repeat("a", 64),
		},
		PrivateDeploymentFingerprint: "sha256:" + strings.Repeat("b", 64),
	}
	requirement := requirementForGraphFixture(t, fixture)
	prepared, err := prepareReviewedGraphInspection(requirement, fixture.graphPath)
	if err != nil {
		t.Fatalf("prepare deployment-attested graph inspection: %v", err)
	}
	if prepared.expected.Deployment == nil ||
		!reflect.DeepEqual(prepared.expected.Deployment, requirement.Graph.Deployment) {
		t.Fatalf("prepared deployment = %+v, want %+v",
			prepared.expected.Deployment, requirement.Graph.Deployment)
	}
	prepared.expected.Deployment.Public.ID = "deployment://mutated-copy"
	if requirement.Graph.Deployment.Public.ID != "deployment://benchmark-test" {
		t.Fatal("prepared inspection aliases reviewed deployment evidence")
	}
}

func TestInspectionGraphDoesNotChangeLegacyOrUnattestedCompatibility(t *testing.T) {
	legacy, err := bench.RequireLegacy("cascade", binding.ArchitectureIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		requirement bench.ExecutionRequirement
		wantLegacy  bool
	}{
		{name: "unattested"},
		{name: "legacy", requirement: legacy, wantLegacy: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var reads atomic.Int32
			attestor, token, err := configureSessionBenchmarkAttestor(
				test.requirement, "",
				"ws://127.0.0.1:8765/v1/realtime", "TOKEN_ENV",
				func(string) string { reads.Add(1); return "compatibility-token" },
			)
			if err != nil {
				t.Fatalf("compatibility behavior changed: %v", err)
			}
			if token != "compatibility-token" || reads.Load() != 1 {
				t.Fatalf("token = %q, reads = %d", token, reads.Load())
			}
			_, isLegacy := attestor.(bench.LegacyStatusAttestor)
			if isLegacy != test.wantLegacy || (!test.wantLegacy && attestor != nil) {
				t.Fatalf("attestor = %T, want legacy=%t", attestor, test.wantLegacy)
			}
		})
	}
}

func TestInspectionGraphIsNeverSilentlyIgnored(t *testing.T) {
	legacy, err := bench.RequireLegacy("cascade", binding.ArchitectureIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	for _, requirement := range []bench.ExecutionRequirement{{}, legacy} {
		var reads atomic.Int32
		_, _, err := configureSessionBenchmarkAttestor(
			requirement, filepath.Join(t.TempDir(), "not-read.ir.json"),
			"ws://127.0.0.1:8765/v1/realtime", "TOKEN_ENV",
			func(string) string { reads.Add(1); return "secret" },
		)
		if err == nil || !strings.Contains(err.Error(),
			"requires a graph-native -execution requirement") {
			t.Fatalf("irrelevant inspection graph refusal = %v", err)
		}
		if reads.Load() != 0 {
			t.Fatalf("credential environment was read for an irrelevant inspection graph")
		}
	}
}

func TestConfiguredInspectionAttestorRefusesInsecureRemoteOriginWithoutExposingSecret(t *testing.T) {
	fixture := writeGraphExecutionFixture(t)
	requirement := requirementForGraphFixture(t, fixture)
	const secret = "remote-deployment-secret"
	attestor, _, err := configureSessionBenchmarkAttestor(
		requirement, fixture.graphPath, "ws://example.com/v1/realtime", "TOKEN_ENV",
		func(string) string { return secret },
	)
	if err != nil {
		t.Fatal(err)
	}
	access := benchmarkInspectionAccess("sess_remote")
	_, err = attestor.Attest(context.Background(), bench.AttestationRequest{
		Scope: "task-remote",
		Status: binding.Status{Graph: binding.ArchitectureIdentity{
			ID: fixture.graph.ID, Revision: int(fixture.graph.Revision),
			Fingerprint: fixture.graph.Fingerprint,
		}},
		Inspection: &access,
	})
	if err == nil || !strings.Contains(err.Error(), "requires TLS") {
		t.Fatalf("insecure inspection origin refusal = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("insecure-origin refusal exposed deployment credential: %v", err)
	}
}

func TestGenericBenchmarkInspectionGraphMismatchWinsBeforeEnvironmentWork(t *testing.T) {
	fixture := writeGraphExecutionFixture(t)
	requirement := requirementForGraphFixture(t, fixture)
	requirementPath := filepath.Join(t.TempDir(), "reviewed.execution.json")
	if err := bench.WriteExecutionRequirement(requirementPath, requirement); err != nil {
		t.Fatal(err)
	}
	differentGraphPath := filepath.Join(t.TempDir(), "different.ir.json")
	source, err := fixture.unbound.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(differentGraphPath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	missingEnvironment := filepath.Join(t.TempDir(), "environment-must-not-be-opened")
	tests := []struct {
		name string
		run  func([]string, io.Writer) error
		args []string
	}{
		{name: "meeting", run: runMeeting, args: []string{"-browser", missingEnvironment}},
		{name: "realtime-cu", run: runRealtimeCU, args: []string{"-browser", missingEnvironment}},
		{name: "fdb", run: runFDB, args: []string{"-dataset", missingEnvironment}},
		{name: "fd-bench", run: runFDBench, args: []string{"-conditions", "clean", "-dataset", missingEnvironment}},
		{name: "fdb-v3", run: runFDBv3, args: []string{"-dataset", missingEnvironment}},
		{name: "tau-voice", run: runTauVoice, args: []string{"-tau2", missingEnvironment}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			arguments := append([]string{
				"-execution", requirementPath, "-inspection-graph", differentGraphPath,
			}, test.args...)
			err := test.run(arguments, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), "inspection graph identity") {
				t.Fatalf("graph/config refusal did not win before environment work: %v", err)
			}
		})
	}
}

func requirementForGraphFixture(t *testing.T, fixture graphExecutionFixture) bench.ExecutionRequirement {
	t.Helper()
	requirement, err := bench.RequireGraph(fixture.graph, bench.ArtifactIdentity{
		ID:       "values://" + fixture.graph.ID,
		Revision: "openrealtime.ai/config/v1alpha1",
		Digest:   fixture.bound.Fingerprint,
	}, fixture.resolution)
	if err != nil {
		t.Fatal(err)
	}
	return requirement
}

func benchmarkInspectionAccess(sessionID string) openrealtime.InspectionAccess {
	return openrealtime.InspectionAccess{
		SessionID: sessionID,
		Path:      "/v1/realtime/sessions/" + sessionID + "/live",
		Token: "mgmt_" + base64.RawURLEncoding.EncodeToString(
			bytes.Repeat([]byte{0x61}, 32),
		),
		ExpiresAtMS: time.Now().Add(time.Minute).UnixMilli(),
	}
}

func inspectionSnapshotForFixture(
	fixture graphExecutionFixture, requirement bench.ExecutionRequirement,
) inspect.Live {
	configuration := inspectionArtifact(requirement.Graph.Configuration)
	nodes := make(map[string]inspect.NodeLive, len(requirement.Graph.Nodes))
	for _, reviewed := range requirement.Graph.Nodes {
		capabilities := make([]inspect.CapabilityIdentity, 0, len(reviewed.Capabilities))
		for _, capability := range reviewed.Capabilities {
			converted := inspect.CapabilityIdentity{
				Name: capability.Name, Contract: capability.Contract,
				Provider: inspectionArtifact(capability.Provider),
			}
			if capability.Adapter != nil {
				adapter := inspectionArtifact(*capability.Adapter)
				converted.Adapter = &adapter
			}
			capabilities = append(capabilities, converted)
		}
		nodes[reviewed.Node] = inspect.NodeLive{
			State: "running",
			Resolution: &inspect.NodeResolution{
				Element: reviewed.Element, Implementation: reviewed.Implementation,
				Runtime:         inspectionArtifact(reviewed.Runtime),
				RuntimeEvidence: inspect.EvidenceRegistered,
				Capabilities:    capabilities, CapabilitiesEvidence: inspect.EvidenceLive,
			},
		}
	}
	return inspect.Live{
		FormatVersion: inspect.LiveFormatVersion,
		GraphID:       fixture.graph.ID, GraphRevision: fixture.graph.Revision,
		Fingerprint: fixture.graph.Fingerprint, Configuration: &configuration,
		Sequence: 1, ObservedAt: time.Now().UTC(), State: "running", Nodes: nodes,
	}
}

func inspectionArtifact(source bench.ArtifactIdentity) inspect.ArtifactIdentity {
	return inspect.ArtifactIdentity{ID: source.ID, Revision: source.Revision, Digest: source.Digest}
}
