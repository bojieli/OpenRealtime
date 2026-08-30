package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func TestBenchmarkExecutionAttachmentAllowsDiagnosticsAndAttachesGraph(t *testing.T) {
	cell := bench.Reference()
	diagnosticID := cell.ID()
	if err := attachBenchmarkExecution(&cell, ""); err != nil {
		t.Fatalf("omitted execution changed diagnostic authoring: %v", err)
	}
	attestor, err := sharedDriverAttestor(cell.Execution, nil)
	if err != nil || attestor != nil || cell.ID() != diagnosticID || cell.Execution.Required() {
		t.Fatalf("omitted execution changed the diagnostic cell: %+v, %T, %v", cell, attestor, err)
	}

	fixture := writeGraphExecutionFixture(t)
	requirement := requirementForGraphFixture(t, fixture)
	path := filepath.Join(t.TempDir(), "graph.execution.json")
	if err := bench.WriteExecutionRequirement(path, requirement); err != nil {
		t.Fatal(err)
	}
	if err := attachBenchmarkExecution(&cell, path); err != nil {
		t.Fatalf("attach reviewed graph requirement: %v", err)
	}
	attestor, err = sharedDriverAttestor(cell.Execution, nil)
	if err == nil || !strings.Contains(err.Error(), "configured live runtime attestor") || attestor != nil {
		t.Fatalf("graph requirement selected an implicit attestor: %T, %v", attestor, err)
	}
	if cell.ID() == diagnosticID || cell.Execution.Kind != bench.ExecutionGraphNative {
		t.Fatalf("graph execution contract is absent from cell identity: %+v", cell.Execution)
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
	fixture := writeGraphExecutionFixture(t)
	requirement := requirementForGraphFixture(t, fixture)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var reads atomic.Int32
	_, err := captureExternalExecutionEvidence(
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
