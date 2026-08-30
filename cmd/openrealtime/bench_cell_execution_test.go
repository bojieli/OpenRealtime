package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/perception"
	serverplugin "github.com/bojieli/OpenRealtime/server"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestBenchmarkExecutionAttachmentPreservesHistoricalOmissionAndEnablesLegacyStatus(t *testing.T) {
	cell := bench.Reference()
	historicalID := cell.ID()
	if err := attachBenchmarkExecution(&cell, ""); err != nil {
		t.Fatalf("omitted execution changed historical authoring: %v", err)
	}
	attestor, err := sharedDriverAttestor(cell.Execution, nil)
	if err != nil || attestor != nil || cell.ID() != historicalID || cell.Execution.Required() {
		t.Fatalf("omitted execution changed the historical cell: %+v, %T, %v", cell, attestor, err)
	}

	requirement, err := bench.RequireLegacy("cascade", binding.ArchitectureIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "legacy.execution.json")
	if err := bench.WriteExecutionRequirement(path, requirement); err != nil {
		t.Fatal(err)
	}
	if err := attachBenchmarkExecution(&cell, path); err != nil {
		t.Fatalf("attach reviewed legacy requirement: %v", err)
	}
	attestor, err = sharedDriverAttestor(cell.Execution, nil)
	if err != nil {
		t.Fatalf("configure negotiated legacy status evidence: %v", err)
	}
	if _, ok := attestor.(bench.LegacyStatusAttestor); !ok {
		t.Fatalf("legacy requirement selected %T, want LegacyStatusAttestor", attestor)
	}
	if cell.ID() == historicalID || cell.Execution.Kind != bench.ExecutionLegacy {
		t.Fatalf("legacy execution contract is absent from cell identity: %+v", cell.Execution)
	}
}

func TestGraphNativeSharedDriverRequiresAConfiguredLiveAttestor(t *testing.T) {
	fixture := writeGraphExecutionFixture(t)
	requirement, err := bench.RequireGraph(fixture.graph, bench.ArtifactIdentity{
		ID: "values://" + fixture.graph.ID, Revision: "openrealtime.ai/config/v1alpha1",
		Digest: fixture.bound.Fingerprint,
	}, fixture.resolution)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sharedDriverAttestor(requirement, nil); err == nil ||
		!strings.Contains(err.Error(), "configured live runtime attestor") {
		t.Fatalf("graph-native requirement ran without an evidence source: %v", err)
	}
	configured := bench.RuntimeAttestorFunc(func(
		context.Context, bench.AttestationRequest,
	) (bench.ExecutionEvidence, error) {
		return bench.ExecutionEvidence{}, nil
	})
	actual, err := sharedDriverAttestor(requirement, configured)
	if err != nil || actual == nil {
		t.Fatalf("configured graph attestor was refused: %T, %v", actual, err)
	}
}

func TestGenericBenchmarkCLIsRefuseUnresolvedGraphNativeExecutionBeforeWork(t *testing.T) {
	fixture := writeGraphExecutionFixture(t)
	requirement, err := bench.RequireGraph(fixture.graph, bench.ArtifactIdentity{
		ID: "values://" + fixture.graph.ID, Revision: "openrealtime.ai/config/v1alpha1",
		Digest: fixture.bound.Fingerprint,
	}, fixture.resolution)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "graph.execution.json")
	if err := bench.WriteExecutionRequirement(path, requirement); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		run  func([]string, io.Writer) error
		args []string
		want string
	}{
		{name: "meeting", run: runMeeting, args: []string{"-execution", path}, want: "live runtime attestor"},
		{name: "realtime-cu", run: runRealtimeCU, args: []string{"-execution", path}, want: "live runtime attestor"},
		{name: "fdb", run: runFDB, args: []string{"-execution", path}, want: "live runtime attestor"},
		{name: "fd-bench", run: runFDBench, args: []string{"-conditions", "clean", "-execution", path}, want: "live runtime attestor"},
		{name: "fdb-v3", run: runFDBv3, args: []string{"-execution", path}, want: "live runtime attestor"},
		{name: "tau-voice", run: runTauVoice, args: []string{"-execution", path}, want: "-inspection-graph"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			err := test.run(test.args, &output)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("graph-native CLI reached its environment without evidence: %v\n%s",
					err, output.String())
			}
		})
	}
}

func TestExternalExecutionPreflightRejectsBeforeCredentialOrNetworkWork(t *testing.T) {
	requirement, err := bench.RequireLegacy("cascade", binding.ArchitectureIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var reads atomic.Int32
	_, err = captureExternalExecutionEvidence(
		ctx, requirement, "", "ws://127.0.0.1:1/v1/realtime", "TOKEN_ENV", "",
		func(string) string { reads.Add(1); return "must-not-be-read" },
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled external evidence preflight = %v", err)
	}
	if reads.Load() != 0 {
		t.Fatalf("external evidence preflight read credentials after cancellation")
	}

	reads.Store(0)
	evidence, err := captureExternalExecutionEvidence(
		context.Background(), bench.ExecutionRequirement{}, "", "", "TOKEN_ENV", "",
		func(string) string { reads.Add(1); return "must-not-be-read" },
	)
	if err != nil || evidence != nil || reads.Load() != 0 {
		t.Fatalf("unattested external preflight = %+v, %v, reads=%d", evidence, err, reads.Load())
	}
}

func TestExternalExecutionPreflightCapturesLiveLegacyEvidence(t *testing.T) {
	requirement, err := bench.RequireLegacy("cascade", binding.ArchitectureIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := serverplugin.NewBundle(serverplugin.BundleConfig{
		ProfileName: "openrealtime.server.test-external-evidence", ProfileRevision: 1,
		Provider: externalEvidenceProvider{},
		ProviderArtifact: inspect.ArtifactIdentity{
			ID: "go://openrealtime/test/external-evidence-provider", Revision: "build-1",
			Digest: "sha256:" + strings.Repeat("a", 64),
		},
		GatewayArtifact: inspect.ArtifactIdentity{
			ID: "go://openrealtime/test/external-evidence-gateway", Revision: "build-1",
			Digest: "sha256:" + strings.Repeat("b", 64),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	realm, err := bundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := realm.Close(ctx); err != nil {
			t.Errorf("close external evidence server: %v", err)
		}
	})
	httpServer := httptest.NewServer(realm.Handler())
	t.Cleanup(httpServer.Close)

	evidence, err := captureExternalExecutionEvidence(
		context.Background(), requirement, "",
		"ws"+strings.TrimPrefix(httpServer.URL, "http")+"/v1/realtime",
		"TOKEN_ENV", "", func(string) string { return "" },
	)
	if err != nil {
		t.Fatalf("capture live legacy evidence: %v", err)
	}
	if evidence == nil || evidence.Kind != bench.ExecutionLegacy ||
		evidence.Scope != "" || evidence.Legacy == nil || evidence.Legacy.Binding != "cascade" {
		t.Fatalf("captured external evidence = %+v", evidence)
	}
	if err := requirement.Match(evidence); err != nil {
		t.Fatalf("captured evidence does not match requirement: %v", err)
	}
}

type externalEvidenceProvider struct{}

func (externalEvidenceProvider) Name() string { return "external-evidence-test" }
func (externalEvidenceProvider) Ownership() binding.Ownership {
	return binding.Ownership{
		Perception: binding.OwnerEngine, FastCognition: binding.OwnerEngine,
		SlowCognition: binding.OwnerEngine, Action: binding.OwnerEngine,
		Interaction: binding.OwnerEngine, Floor: binding.OwnerEngine,
	}
}
func (externalEvidenceProvider) Capabilities() binding.Capabilities {
	return binding.Capabilities{FastSlow: true}
}
func (externalEvidenceProvider) Start(
	_ context.Context, options binding.Options,
) (binding.Runtime, error) {
	if options.Sink == nil {
		return nil, errors.New("external evidence test requires a sink")
	}
	return externalEvidenceRuntime{}, nil
}

type externalEvidenceRuntime struct{}

func (externalEvidenceRuntime) Update(context.Context, binding.Settings) error { return nil }
func (externalEvidenceRuntime) Audio(context.Context, perception.Frame) error  { return nil }
func (externalEvidenceRuntime) Video(context.Context, perception.Frame) error {
	return binding.ErrUnsupported
}
func (externalEvidenceRuntime) Text(context.Context, binding.TextInput) error { return nil }
func (externalEvidenceRuntime) ToolResult(context.Context, trajectory.ToolResult) error {
	return nil
}
func (externalEvidenceRuntime) CommitAudio(context.Context) error                  { return nil }
func (externalEvidenceRuntime) CreateResponse(context.Context) error               { return nil }
func (externalEvidenceRuntime) Cancel(context.Context, string) error               { return nil }
func (externalEvidenceRuntime) Truncate(context.Context, binding.Truncation) error { return nil }
func (externalEvidenceRuntime) Trajectory() trajectory.Snapshot                    { return trajectory.Snapshot{} }
func (externalEvidenceRuntime) Status() binding.Status {
	return binding.Status{Binding: "cascade"}
}
func (externalEvidenceRuntime) Close(context.Context, error) error { return nil }
