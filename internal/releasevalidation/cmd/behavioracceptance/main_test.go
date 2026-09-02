package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/internal/releasevalidation"
)

func TestRunRetainsFailedReportAndRefusesToOverwriteIt(t *testing.T) {
	directory := t.TempDir()
	targetPath := filepath.Join(directory, "targets.json")
	candidatePath := filepath.Join(directory, "candidate.json")
	resultPath := filepath.Join(directory, "result.json")
	reportPath := filepath.Join(directory, "report.json")
	unavailable := func(reason string) releasevalidation.Registration {
		return releasevalidation.Registration{
			Status: releasevalidation.RegistrationUnavailable,
			Reason: reason,
		}
	}
	targets := releasevalidation.BehavioralTargets{
		FormatVersion: releasevalidation.BehavioralTargetsVersion,
		Suites: []releasevalidation.BehavioralSuiteTarget{{
			ID: "fixture", ResultKind: releasevalidation.ResultKindBench,
			Suite: "fixture", ExpectedPopulation: 1, CaseKey: releasevalidation.CaseKeyExact,
			Aggregate: releasevalidation.AggregateTarget{Registration: unavailable("aggregate target unavailable")},
			Cases:     releasevalidation.CaseTargetSet{Registration: unavailable("per-case target unavailable")},
			Safety:    releasevalidation.EvidenceTargetSet{Registration: unavailable("safety target unavailable")},
			Deadline:  releasevalidation.EvidenceTargetSet{Registration: unavailable("deadline target unavailable")},
			Latency:   releasevalidation.EvidenceTargetSet{Registration: unavailable("latency target unavailable")},
		}},
	}
	candidate := releasevalidation.FrozenCandidate{
		FormatVersion:    releasevalidation.FrozenCandidateVersion,
		CandidateID:      "fixture-final",
		Revision:         strings.Repeat("a", 40),
		ExecutableSHA256: "sha256:" + strings.Repeat("b", 64),
		Machine: bench.Machine{
			CPU: "fixture", Cores: 1, OS: "linux", Arch: "amd64", GoVersion: "go1.25.0",
		},
		Suites: []releasevalidation.FrozenCandidateSuite{{
			ID: "fixture", ExecutionRequirementSHA256: "sha256:" + strings.Repeat("c", 64),
			Lineage: []releasevalidation.RunLineage{{
				CampaignID: "final", Kind: releasevalidation.RunFinalFull, Population: 1,
			}},
		}},
	}
	writeJSON(t, targetPath, targets)
	writeJSON(t, candidatePath, candidate)
	if err := os.WriteFile(resultPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	arguments := []string{
		"-targets", targetPath,
		"-candidate", candidatePath,
		"-result", "fixture=" + resultPath,
		"-report", reportPath,
	}
	var stdout, stderr bytes.Buffer
	if code := run(arguments, &stdout, &stderr); code != 1 {
		t.Fatalf("failed acceptance exit = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.String() != "behavioral acceptance: failed\n" || stderr.Len() != 0 {
		t.Fatalf("failed acceptance output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	original, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report releasevalidation.BehavioralAcceptanceReport
	if err := json.Unmarshal(original, &report); err != nil || report.Accepted ||
		report.Outcome != releasevalidation.BehavioralFailed {
		t.Fatalf("retained failed report = %+v, %v", report, err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run(arguments, &stdout, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "write behavioral acceptance report") {
		t.Fatalf("overwrite exit/output = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	after, err := os.ReadFile(reportPath)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatalf("create-only report changed after overwrite refusal: %v", err)
	}
}

func TestRunRejectsIncompleteInvocation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 2 || stdout.Len() != 0 ||
		!strings.Contains(stderr.String(), "requires -candidate, -report") {
		t.Fatalf("incomplete invocation = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(payload, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
