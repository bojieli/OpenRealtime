package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench/migration"
)

func TestMigrationArchiveCommandUsesCreateOnlyDigestStore(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(t.TempDir(), "result.json")
	if err := os.WriteFile(source, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runBench([]string{"migration", "archive", "-store", directory,
		"-kind", migration.EvidenceKindResult, source}, &output); err != nil {
		t.Fatalf("archive command: %v", err)
	}
	var reference migration.EvidenceRef
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &reference); err != nil {
		t.Fatalf("decode reference %q: %v", output.String(), err)
	}
	payload, err := (migration.LocalStore{Root: directory}).Resolve(reference)
	if err != nil || string(payload) != "{}\n" {
		t.Fatalf("resolve archived result = %q, %v", payload, err)
	}
}

func TestMigrationLaunchFlagsRejectBeforeEndpointWork(t *testing.T) {
	var output bytes.Buffer
	err := runMeeting([]string{
		"-migration-store", t.TempDir(), "-migration-repetition", "trial-1",
		"-endpoint", "ws://127.0.0.1:1/v1/realtime",
	}, &output)
	if err == nil || !strings.Contains(err.Error(), "requires store, registration") {
		t.Fatalf("partial migration preflight error = %v", err)
	}
	if strings.Contains(output.String(), "meeting:") {
		t.Fatalf("meeting task started before migration refusal: %q", output.String())
	}
}

func TestScenarioMigrationRequiresArchitectureArtifact(t *testing.T) {
	var output bytes.Buffer
	err := runScenario([]string{"-migration-store", t.TempDir()}, &output)
	if err == nil || !strings.Contains(err.Error(), "architecture manifest result") {
		t.Fatalf("scenario migration error = %v", err)
	}
}

func TestMigrationCensusRequiresExactTauInventory(t *testing.T) {
	var output bytes.Buffer
	err := runMigration([]string{"census", "-store", t.TempDir()}, &output)
	if err == nil || !strings.Contains(err.Error(), "tau-inventory") {
		t.Fatalf("missing tau inventory error = %v", err)
	}
}

func TestParseMigrationResultInput(t *testing.T) {
	suite, repetition, path, err := parseMigrationResultInput("fdb-v1.5:trial-2=result.json")
	if err != nil || suite != "fdb-v1.5" || repetition != "trial-2" || path != "result.json" {
		t.Fatalf("parsed input = %q %q %q, %v", suite, repetition, path, err)
	}
	if _, _, _, err := parseMigrationResultInput("result.json"); err == nil {
		t.Fatal("input without suite was accepted")
	}
	suite, reference, err := parseMigrationOutcomeInput(
		"fd-bench=artifacts/migration-launch-outcome/receipt.json@" + strings.Repeat("a", 64))
	if err != nil || suite != "fd-bench" || reference.Kind != migration.EvidenceKindLaunchOutcome {
		t.Fatalf("parsed launch outcome = %q %+v, %v", suite, reference, err)
	}
}
