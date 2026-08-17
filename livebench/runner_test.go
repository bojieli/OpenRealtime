package livebench

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadTrialResultVerifiesBenchmarkHashes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	descriptor := Descriptor{
		Provider: "test", Model: "model", Architecture: "native-audio-to-audio",
		OutputSampleRate: 24_000,
	}
	inputPath := filepath.Join(root, "input.wav")
	cleanPath := filepath.Join(root, "clean_input.wav")
	inputHash := writeTestWAV(t, inputPath)
	cleanHash := writeTestWAV(t, cleanPath)
	sample := Sample{
		Benchmark: "benchmark", Revision: "revision", Scenario: "scenario", ID: "1",
		InputPath: inputPath, CleanInputPath: cleanPath,
		InputSHA256: inputHash, CleanSHA256: cleanHash, MetadataSHA256: "metadata-hash",
	}
	config := TrialConfig{OutputRoot: root, Condition: "overlap"}
	trialDir, resultPath := trialPaths(descriptor, sample, config)
	if err := os.MkdirAll(trialDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(trialDir, "output.wav")
	outputHash := writeTestWAV(t, outputPath)
	result := TrialResult{
		SchemaVersion: ResultSchemaVersion, TrialID: "test/scenario/1/overlap/r000",
		Sample: sample, Condition: "overlap", InputSHA256: inputHash,
		OutputSHA256: outputHash, OutputWAV: outputPath,
		Timing: TimingMetrics{VAD: EnergyVADName}, Session: SessionResult{Descriptor: descriptor},
	}
	if err := writeJSONAtomic(resultPath, result); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := LoadTrialResult(descriptor, sample, config); err != nil || !exists {
		t.Fatalf("load valid result: exists=%v err=%v", exists, err)
	}

	changedSample := sample
	changedSample.InputSHA256 = strings.Repeat("0", 64)
	if _, _, err := LoadTrialResult(descriptor, changedSample, config); err == nil {
		t.Fatal("changed input hash was accepted")
	}
	if err := os.WriteFile(outputPath, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadTrialResult(descriptor, sample, config); err == nil {
		t.Fatal("corrupted output was accepted")
	}
}

func writeTestWAV(t *testing.T, filename string) string {
	t.Helper()
	hash, err := WriteWAV(filename, Audio{SampleRateHz: 16_000, PCM16: make([]byte, 320)})
	if err != nil {
		t.Fatal(err)
	}
	return hash
}
