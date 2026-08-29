package main

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/binding"
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
		{name: "tau-voice", run: runTauVoice, args: []string{"-execution", path}, want: "external task attestor"},
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
