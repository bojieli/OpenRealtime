package dynacu

import (
	"archive/zip"
	"bufio"
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

type dynaCUNoopPlugin struct{}

func (*dynaCUNoopPlugin) BeginAttempt(
	context.Context, candidate.Attempt,
) (candidate.AttemptEvidence, error) {
	return nil, errors.New("not used")
}

func (*dynaCUNoopPlugin) FinishSuite(context.Context, bench.Result) error { return nil }

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

func runDynaCUInterruptedRecorderSelfTest(t testing.TB) (string, string) {
	t.Helper()
	directory := t.TempDir()
	script := filepath.Join(directory, driverName)
	if err := os.WriteFile(script, driver, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(directory, "raw-evidence")
	command := exec.CommandContext(
		t.Context(), "python3", script, "--recorder-partial-self-test", root,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("interrupted recorder self-test: %v: %s", err, output)
	}
	return root, directory
}

type dynaCUHookLatency struct {
	Iterations           int   `json:"iterations"`
	P99NS                int64 `json:"p99_ns"`
	MaximumNS            int64 `json:"maximum_ns"`
	BackpressureRecorded bool  `json:"backpressure_recorded"`
}

func runDynaCUHookLatencySelfTest(t testing.TB) dynaCUHookLatency {
	t.Helper()
	directory := t.TempDir()
	script := filepath.Join(directory, "driver.py")
	if err := os.WriteFile(script, driver, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "python3", script, "--recorder-backpressure-self-test")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("recorder backpressure self-test: %v: %s", err, output)
	}
	var result dynaCUHookLatency
	if err := json.Unmarshal(bytes.TrimSpace(output), &result); err != nil {
		t.Fatalf("decode recorder latency: %v: %s", err, output)
	}
	return result
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

func BenchmarkDynaCUCaptureHookUnderBackpressure(b *testing.B) {
	for index := 0; index < b.N; index++ {
		result := runDynaCUHookLatencySelfTest(b)
		if !result.BackpressureRecorded {
			b.Fatal("capture backpressure was not retained")
		}
		b.ReportMetric(float64(result.P99NS), "hook-p99-ns/op")
		b.ReportMetric(float64(result.MaximumNS), "hook-max-ns/op")
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

func TestCaptureHookIsBoundedAndUpstreamSendPrecedesObservation(t *testing.T) {
	result := runDynaCUHookLatencySelfTest(t)
	if result.Iterations != 20_000 || !result.BackpressureRecorded || result.P99NS > 1_000_000 {
		t.Fatalf("capture hook latency = %+v, want p99 <= 1ms", result)
	}
	upstream := bytes.Index(driver, []byte("result = OpenAIRealtimeWSBaseline._send(self, ws, event)"))
	observer := bytes.Index(driver, []byte("recorder.observe_send(event, at_us)"))
	if upstream < 0 || observer < 0 || upstream >= observer {
		t.Fatal("embedded recorder observes before the pinned upstream send returns")
	}
}

func TestInterruptedRecorderIsRecoveredAsPlayableReceiptBoundEvidence(t *testing.T) {
	rawRoot, driverDirectory := runDynaCUInterruptedRecorderSelfTest(t)
	entries, err := readDynaCUEvidence(t.Context(), rawRoot)
	if err == nil || len(entries) != 1 || !errors.Is(entries[0].problem, errDynaCURawManifestMissing) {
		t.Fatalf("unsealed partial entry = %+v, %v", entries, err)
	}
	parent := t.TempDir()
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
	}
	result.Finish()
	config := Config{
		AOIDir: driverDirectory, Python: "python3", Output: filepath.Join(parent, "absent.jsonl"),
		EvidenceDirectory: rawRoot, Evidence: bundle, EvidenceOrigin: origin,
		Model: "openrealtime", MaxSteps: 15, StepInterval: 2 * time.Second, Cell: cell,
	}
	if err := config.retainEvidence(t.Context(), lifecycle, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Tasks) != 1 || result.Tasks[0].Completed ||
		!strings.Contains(result.Tasks[0].Error, "external harness process ended") {
		t.Fatalf("recovered deterministic failure = %+v", result.Tasks)
	}
	if err := lifecycle.Finish(result); err != nil {
		t.Fatal(err)
	}
	manifest, _, err := sourcebundle.Verify(t.Context(), sourceDirectory, receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Attempts) != 1 || !manifest.Attempts[0].EvidenceComplete ||
		manifest.Attempts[0].Media == nil || manifest.Attempts[0].Media.Kind != "video" ||
		len(manifest.Attempts[0].Artifacts) != 2 {
		t.Fatalf("recovered source attempt = %+v", manifest.Attempts)
	}
	recovered, err := readDynaCUEvidence(t.Context(), rawRoot)
	if err != nil || len(recovered) != 1 || recovered[0].manifest == nil ||
		!recovered[0].manifest.Complete || !recovered[0].manifest.ProcessInterrupted ||
		recovered[0].manifest.TimestampBasis != dynaCUTimestampBasis {
		t.Fatalf("recovered raw attempt = %+v, %v", recovered, err)
	}
}

func TestRecorderSignalSealsTheActiveAttemptBeforeExit(t *testing.T) {
	directory := t.TempDir()
	script := filepath.Join(directory, "driver.py")
	if err := os.WriteFile(script, driver, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(directory, "raw-evidence")
	command := exec.Command("python3", script, "--recorder-signal-self-test", root)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if strings.TrimSpace(scanner.Text()) == "recorder-ready" {
				ready <- nil
				return
			}
		}
		ready <- scanner.Err()
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("signal self-test did not become ready")
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		_ = command.Process.Kill()
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("interrupted recorder exited successfully")
	}
	entries, err := readDynaCUEvidence(t.Context(), root)
	if err != nil || len(entries) != 1 || entries[0].manifest == nil ||
		!entries[0].manifest.Complete || !entries[0].manifest.ProcessInterrupted ||
		len(entries[0].mediaBytes) == 0 || len(entries[0].wireBytes) == 0 ||
		len(entries[0].traceBytes) == 0 {
		t.Fatalf("signal-sealed raw attempt = %+v, %v", entries, err)
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
		Evidence: &dynaCUNoopPlugin{},
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
