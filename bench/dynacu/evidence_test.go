package dynacu

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
	"github.com/bojieli/OpenRealtime/bench/review/candidate/sourcebundle"
)

func runDynaCURecorderSelfTest(t testing.TB) string {
	t.Helper()
	directory := t.TempDir()
	script := filepath.Join(directory, "driver.py")
	if err := os.WriteFile(script, driver, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(directory, "raw-evidence")
	command := exec.CommandContext(t.Context(), "python3", script, "--recorder-self-test", root)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("recorder self-test: %v: %s", err, output)
	}
	if strings.TrimSpace(string(output)) == "" {
		t.Fatal("recorder self-test emitted no manifest path")
	}
	return root
}

func BenchmarkValidateOneDynaCUSynchronizedAttempt(b *testing.B) {
	root := runDynaCURecorderSelfTest(b)
	inventory, err := listDynaCUEvidence(b.Context(), root)
	if err != nil || len(inventory.entries) != 1 {
		b.Fatalf("inventory = %+v, %v", inventory.entries, err)
	}
	b.ReportAllocs()
	b.SetBytes(22 << 10)
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		entry, err := readDynaCUEvidenceEntryFromDirectory(root, inventory.entries[0].directory)
		if err != nil || entry.problem != nil || len(entry.mediaBytes) == 0 {
			b.Fatalf("validated entry = %+v, %v", entry.manifest, err)
		}
	}
}

func TestEmbeddedDriverRetainsExactSynchronizedWireMediaAndAction(t *testing.T) {
	root := runDynaCURecorderSelfTest(t)
	entries, err := readDynaCUEvidence(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("raw entries = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.problem != nil || entry.manifest == nil || !entry.manifest.Complete ||
		entry.manifest.AudioChunkCount != 1 || entry.manifest.FrameCount != 1 ||
		entry.manifest.ActionCount != 1 || len(entry.mediaBytes) == 0 ||
		len(entry.traceBytes) == 0 || len(entry.wireBytes) == 0 {
		t.Fatalf("raw entry = %+v, problem=%v", entry.manifest, entry.problem)
	}
	archive, err := zip.NewReader(bytes.NewReader(entry.wireBytes), int64(len(entry.wireBytes)))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"attempt.json": false, "wire-index.json": false,
		"audio/000001.pcm": false, "frames/000001.jpg": false,
	}
	for _, file := range archive.File {
		if _, found := want[file.Name]; found {
			want[file.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("exact wire archive omits %s", name)
		}
	}
	if len(entry.trace.Actions) != 1 || entry.trace.Actions[0].Name != "click" ||
		len(entry.trace.Transcripts) != 1 || entry.trace.Transcripts[0].Text != "heard words" {
		t.Fatalf("action trace = %+v / %+v", entry.trace.Actions, entry.trace.Transcripts)
	}
}

func TestRawEvidenceRefusesSymlinkHardLinkAndTampering(t *testing.T) {
	t.Run("symlink root", func(t *testing.T) {
		target := t.TempDir()
		link := filepath.Join(t.TempDir(), "raw")
		if err := os.Symlink(target, link); err != nil {
			t.Skip(err)
		}
		if _, err := readDynaCUEvidence(t.Context(), link); err == nil {
			t.Fatal("symlink raw root was accepted")
		}
	})
	t.Run("hard linked trace", func(t *testing.T) {
		root := runDynaCURecorderSelfTest(t)
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 1 {
			t.Fatal(err)
		}
		trace := filepath.Join(root, entries[0].Name(), "trace.json")
		if err := os.Link(trace, filepath.Join(t.TempDir(), "alias.json")); err != nil {
			t.Skip(err)
		}
		got, err := readDynaCUEvidence(t.Context(), root)
		if err == nil || len(got) != 1 || got[0].problem == nil {
			t.Fatalf("hard-link result = %+v, %v", got, err)
		}
	})
	t.Run("wire mutation", func(t *testing.T) {
		root := runDynaCURecorderSelfTest(t)
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 1 {
			t.Fatal(err)
		}
		wire := filepath.Join(root, entries[0].Name(), "wire.zip")
		handle, err := os.OpenFile(wire, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := handle.Write([]byte("tamper")); err != nil {
			t.Fatal(err)
		}
		if err := handle.Close(); err != nil {
			t.Fatal(err)
		}
		got, err := readDynaCUEvidence(t.Context(), root)
		if err == nil || len(got) != 1 || got[0].problem == nil {
			t.Fatalf("wire-mutation result = %+v, %v", got, err)
		}
	})
}

func TestAttemptEvidencePreflightIsCreateOnlyAndRefusesVideoOptOut(t *testing.T) {
	parent := t.TempDir()
	origin, err := candidate.NewRunOrigin(
		candidate.OriginHermetic, bench.TransportWebSocket, "ws://127.0.0.1:8765/v1/realtime",
	)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{
		Output:            filepath.Join(parent, "result.jsonl"),
		EvidenceDirectory: filepath.Join(parent, "raw"), EvidenceOrigin: origin,
	}
	if err := config.validateAttemptEvidence(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{config.Output, config.EvidenceDirectory} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("preflight mutated %s: %v", path, err)
		}
	}
	config.WithoutImages = true
	if err := config.validateAttemptEvidence(); err == nil || !strings.Contains(err.Error(), "review video") {
		t.Fatalf("no-images preflight error = %v", err)
	}
	config.WithoutImages = false
	if err := os.WriteFile(config.Output, []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.validateAttemptEvidence(); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing result preflight error = %v", err)
	}
}

func TestRecorderEvidenceSealsThroughCandidateSourcePipeline(t *testing.T) {
	rawRoot := runDynaCURecorderSelfTest(t)
	parent := t.TempDir()
	resultPath := filepath.Join(parent, "result.jsonl")
	recordValue := record{
		TaskID: "self-test", Category: "S_static", Difficulty: "easy",
		Success: true, ResultVal: "passed", Steps: 1,
		TotalTime: 0.2, FinalScore: 1,
	}
	payload, err := json.Marshal(recordValue)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	origin, err := candidate.NewRunOrigin(
		candidate.OriginHermetic, bench.TransportWebSocket, "ws://127.0.0.1:8765/v1/realtime",
	)
	if err != nil {
		t.Fatal(err)
	}
	cell := bench.Reference()
	provenance := bench.Provenance{
		Revision: "test-revision", ExecutableSHA256: strings.Repeat("a", 64),
		StartedAt: time.Unix(1, 0).UTC().Format(time.RFC3339),
		Machine:   bench.Machine{OS: "test", Arch: "test", Cores: 1, GoVersion: "test"},
	}
	sourceDirectory := filepath.Join(parent, "source")
	receiptPath := filepath.Join(parent, "source.receipt.json")
	bundle, err := sourcebundle.New(sourcebundle.Options{
		Directory: sourceDirectory, ReceiptPath: receiptPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := candidate.NewLifecycle(candidate.LifecycleConfig{
		Context: t.Context(), Plugin: bundle, Suite: "dynacu-bench", Cell: cell,
		Provenance: provenance, Origin: origin,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := bench.Result{
		Suite: "dynacu-bench", Cell: cell, Provenance: provenance, Expected: TaskCount,
		Tasks: []bench.TaskOutcome{recordValue.outcome()},
	}
	result.Finish()
	config := Config{
		Output: resultPath, EvidenceDirectory: rawRoot, EvidenceOrigin: origin,
		Model: "openrealtime", MaxSteps: 15, StepInterval: 2 * time.Second, Cell: cell,
	}
	if err := config.retainEvidence(t.Context(), lifecycle, &result); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Finish(result); err != nil {
		t.Fatal(err)
	}
	manifest, receipt, err := sourcebundle.Verify(t.Context(), sourceDirectory, receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.AttemptCount != 1 || len(manifest.Attempts) != 1 ||
		!manifest.Attempts[0].EvidenceComplete || manifest.Attempts[0].Media == nil ||
		manifest.Attempts[0].Media.Kind != "video" || len(manifest.Attempts[0].Artifacts) != 2 {
		t.Fatalf("sealed DynaCU source manifest = %+v", manifest.Attempts)
	}
}

func TestCanceledEvidenceReadRefusesBeforeImport(t *testing.T) {
	root := runDynaCURecorderSelfTest(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := readDynaCUEvidence(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled evidence read = %v", err)
	}
}
