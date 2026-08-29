package main

import (
	"context"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/scenario"
	"github.com/bojieli/OpenRealtime/binding"
)

func TestScenarioTaskPreservesTaskExecutionEvidence(t *testing.T) {
	evidence, err := (bench.LegacyStatusAttestor{}).Attest(
		context.Background(),
		bench.AttestationRequest{
			Scope: "ordinary-question#1",
			Status: binding.Status{
				Binding: "cascade",
				Architecture: binding.ArchitectureIdentity{
					ID: "cascade.external-policy", Revision: 4,
					Fingerprint: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	result := scenario.Result{
		Passed: true,
		Transcript: bench.Transcript{
			Execution:      &evidence,
			ExecutionError: "",
		},
	}
	outcome := scenarioTask("ordinary-question#1", result, nil)
	if outcome.Execution == nil || outcome.Execution.Fingerprint != evidence.Fingerprint {
		t.Fatalf("scenario execution evidence was dropped: %+v", outcome)
	}
	if outcome.Execution.Scope != outcome.ID {
		t.Fatalf("scenario evidence scope = %q, want task %q", outcome.Execution.Scope, outcome.ID)
	}

	result.Transcript.Execution.Legacy.Binding = "mutated"
	if outcome.Execution.Legacy.Binding != "cascade" {
		t.Fatal("scenario task retained an alias into transcript execution evidence")
	}
}

func TestScenarioTaskPreservesExecutionAttestationFailure(t *testing.T) {
	result := scenario.Result{
		Transcript: bench.Transcript{ExecutionError: "live graph inspector timed out"},
	}
	outcome := scenarioTask("ordinary-question#1", result, nil)
	if outcome.Execution != nil || outcome.ExecutionError != result.Transcript.ExecutionError {
		t.Fatalf("scenario attestation failure was dropped: %+v", outcome)
	}
}
