package livebench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var safeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

type TrialConfig struct {
	OutputRoot string
	Condition  string
	Replicate  int
	Attempt    int
}

func RunTrial(ctx context.Context, adapter Adapter, sample Sample, config TrialConfig) (TrialResult, error) {
	if config.Attempt < 1 {
		config.Attempt = 1
	}
	inputPath, inputHash, err := selectedInput(sample, config.Condition)
	if err != nil {
		return TrialResult{}, err
	}
	outputName := "output.wav"
	if config.Condition == "clean" {
		outputName = "clean_output.wav"
	}
	if err := verifyFileHash(inputPath, inputHash, "benchmark input"); err != nil {
		return TrialResult{}, err
	}
	input, err := ReadWAV(inputPath)
	if err != nil {
		return TrialResult{}, fmt.Errorf("read %s input for %s/%s: %w", config.Condition, sample.Scenario, sample.ID, err)
	}
	session, err := adapter.Run(ctx, input)
	if err != nil {
		return TrialResult{}, err
	}
	aligned := AlignChunks(session.Chunks, session.Descriptor.OutputSampleRate)
	// Full-Duplex-Bench expects time-synchronous output with exactly the input
	// timeline. Provider audio after this boundary remains in session metrics.
	aligned = FitDuration(aligned, input.Duration())
	trialDir, resultPath := trialPaths(session.Descriptor, sample, config)
	if err := os.MkdirAll(trialDir, 0o755); err != nil {
		return TrialResult{}, fmt.Errorf("create trial directory: %w", err)
	}
	outputPath := filepath.Join(trialDir, outputName)
	outputHash, err := WriteWAV(outputPath, aligned)
	if err != nil {
		return TrialResult{}, err
	}
	providerDir := safeName.ReplaceAllString(session.Descriptor.Provider+"-"+session.Descriptor.Model, "-")
	trialID := strings.Join([]string{providerDir, sample.Scenario, sample.ID, config.Condition, fmt.Sprintf("r%03d", config.Replicate)}, "/")
	result := TrialResult{
		SchemaVersion: ResultSchemaVersion, TrialID: trialID, Attempt: config.Attempt, Sample: sample, Condition: config.Condition,
		InputSHA256: inputHash, OutputSHA256: outputHash, OutputWAV: outputPath,
		Timing: ScoreTiming(input, aligned, sample.OverlapStartS, sample.OverlapEndS), Session: session,
	}
	result.Session.Chunks = nil
	if err := writeJSONAtomic(resultPath, result); err != nil {
		return TrialResult{}, err
	}
	return result, nil
}

func LoadTrialResult(descriptor Descriptor, sample Sample, config TrialConfig) (TrialResult, bool, error) {
	_, resultPath := trialPaths(descriptor, sample, config)
	data, err := os.ReadFile(resultPath)
	if errors.Is(err, os.ErrNotExist) {
		return TrialResult{}, false, nil
	}
	if err != nil {
		return TrialResult{}, false, fmt.Errorf("read prior trial: %w", err)
	}
	var result TrialResult
	if err := json.Unmarshal(data, &result); err != nil {
		return TrialResult{}, false, fmt.Errorf("decode prior trial %s: %w", resultPath, err)
	}
	if !supportedResultSchema(result.SchemaVersion) || result.Sample.ID != sample.ID || result.Sample.Scenario != sample.Scenario || result.Condition != config.Condition || result.Session.Descriptor != descriptor {
		return TrialResult{}, false, fmt.Errorf("prior trial %s does not match the requested trial", resultPath)
	}
	inputPath, inputHash, err := selectedInput(sample, config.Condition)
	if err != nil {
		return TrialResult{}, false, err
	}
	if result.Sample.Benchmark != sample.Benchmark || result.Sample.Revision != sample.Revision ||
		result.Sample.InputSHA256 != sample.InputSHA256 || result.Sample.CleanSHA256 != sample.CleanSHA256 ||
		result.Sample.MetadataSHA256 != sample.MetadataSHA256 || result.InputSHA256 != inputHash {
		return TrialResult{}, false, fmt.Errorf("prior trial %s does not match the current benchmark hashes", resultPath)
	}
	if err := verifyFileHash(inputPath, inputHash, "benchmark input"); err != nil {
		return TrialResult{}, false, err
	}
	if err := verifyFileHash(result.OutputWAV, result.OutputSHA256, "benchmark output"); err != nil {
		return TrialResult{}, false, err
	}
	if result.Timing.VAD != EnergyVADName {
		return TrialResult{}, false, fmt.Errorf("prior trial %s uses scorer %q; rescore it with the current livebench before resuming", resultPath, result.Timing.VAD)
	}
	result.SchemaVersion = ResultSchemaVersion
	if result.Attempt < 1 {
		result.Attempt = 1
	}
	return result, true, nil
}

// RescoreManifest recomputes local metrics from immutable input/output WAVs.
// It never makes a provider call and preserves session traces and hashes.
func RescoreManifest(filename string) (RunManifest, error) {
	manifest, err := ReadRunManifest(filename)
	if err != nil {
		return RunManifest{}, err
	}
	for index := range manifest.Completed {
		result := &manifest.Completed[index]
		result.SchemaVersion = ResultSchemaVersion
		if result.Attempt < 1 {
			result.Attempt = 1
		}
		inputPath, inputHash, selectErr := selectedInput(result.Sample, result.Condition)
		if selectErr != nil {
			return RunManifest{}, fmt.Errorf("select input for %s: %w", result.TrialID, selectErr)
		}
		if result.InputSHA256 != inputHash {
			return RunManifest{}, fmt.Errorf("stored input hash for %s does not match its sample", result.TrialID)
		}
		if hashErr := verifyFileHash(inputPath, inputHash, "benchmark input"); hashErr != nil {
			return RunManifest{}, fmt.Errorf("verify %s: %w", result.TrialID, hashErr)
		}
		if hashErr := verifyFileHash(result.OutputWAV, result.OutputSHA256, "benchmark output"); hashErr != nil {
			return RunManifest{}, fmt.Errorf("verify %s: %w", result.TrialID, hashErr)
		}
		input, readErr := ReadWAV(inputPath)
		if readErr != nil {
			return RunManifest{}, fmt.Errorf("read input for %s: %w", result.TrialID, readErr)
		}
		output, readErr := ReadWAV(result.OutputWAV)
		if readErr != nil {
			return RunManifest{}, fmt.Errorf("read output for %s: %w", result.TrialID, readErr)
		}
		result.Timing = ScoreTiming(input, output, result.Sample.OverlapStartS, result.Sample.OverlapEndS)
		if result.Session.Descriptor.Provider == "google" {
			result.Session.Usage = aggregateGeminiUsage(result.Session.Events, result.Session.ConnectionCount)
		}
		resultPath := filepath.Join(filepath.Dir(result.OutputWAV), "result_"+result.Condition+".json")
		if err := writeJSONAtomic(resultPath, result); err != nil {
			return RunManifest{}, err
		}
	}
	manifest.SchemaVersion = ResultSchemaVersion
	if err := WriteRunManifest(filename, manifest); err != nil {
		return RunManifest{}, err
	}
	return manifest, nil
}

func selectedInput(sample Sample, condition string) (string, string, error) {
	switch condition {
	case "overlap":
		return sample.InputPath, sample.InputSHA256, nil
	case "clean":
		return sample.CleanInputPath, sample.CleanSHA256, nil
	default:
		return "", "", fmt.Errorf("condition must be overlap or clean, got %q", condition)
	}
}

func verifyFileHash(filename, expected, label string) error {
	if filename == "" || expected == "" {
		return fmt.Errorf("%s path or SHA-256 is empty", label)
	}
	actual, err := HashFile(filename)
	if err != nil {
		return fmt.Errorf("hash %s %s: %w", label, filename, err)
	}
	if actual != expected {
		return fmt.Errorf("%s SHA-256 mismatch for %s: got %s, want %s", label, filename, actual, expected)
	}
	return nil
}

func trialPaths(descriptor Descriptor, sample Sample, config TrialConfig) (string, string) {
	providerDir := safeName.ReplaceAllString(descriptor.Provider+"-"+descriptor.Model, "-")
	trialDir := filepath.Join(config.OutputRoot, providerDir, sample.Scenario, sample.ID, fmt.Sprintf("replicate-%03d", config.Replicate))
	return trialDir, filepath.Join(trialDir, "result_"+config.Condition+".json")
}

func writeJSONAtomic(filename string, value any) error {
	temporary, err := os.CreateTemp(filepath.Dir(filename), ".livebench-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary result: %w", err)
	}
	temporaryName := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(temporaryName)
		}
	}()
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync result: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close result: %w", err)
	}
	if err := os.Rename(temporaryName, filename); err != nil {
		return fmt.Errorf("publish result: %w", err)
	}
	keep = true
	return nil
}

type RunManifest struct {
	SchemaVersion string        `json:"schema_version"`
	CreatedAt     time.Time     `json:"created_at"`
	Benchmark     string        `json:"benchmark"`
	Revision      string        `json:"revision"`
	Descriptor    Descriptor    `json:"descriptor"`
	Conditions    []string      `json:"conditions"`
	Replicates    int           `json:"replicates"`
	TrialAttempts int           `json:"trial_attempts"`
	Samples       []Sample      `json:"samples"`
	Completed     []TrialResult `json:"completed"`
	Failures      []RunFailure  `json:"failures,omitempty"`
	Attempts      []RunAttempt  `json:"attempts,omitempty"`
}

type RunFailure struct {
	SampleID  string `json:"sample_id"`
	Scenario  string `json:"scenario"`
	Condition string `json:"condition"`
	Replicate int    `json:"replicate"`
	Attempts  int    `json:"attempts"`
	Error     string `json:"error"`
}

type RunAttempt struct {
	SampleID   string    `json:"sample_id"`
	Scenario   string    `json:"scenario"`
	Condition  string    `json:"condition"`
	Replicate  int       `json:"replicate"`
	Attempt    int       `json:"attempt"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	DurationMS float64   `json:"duration_ms"`
	Succeeded  bool      `json:"succeeded"`
	Error      string    `json:"error,omitempty"`
}

func supportedResultSchema(version string) bool {
	return version == ResultSchemaVersion || version == legacyResultSchemaVersion
}

func WriteRunManifest(filename string, manifest RunManifest) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return fmt.Errorf("create manifest directory: %w", err)
	}
	return writeJSONAtomic(filename, manifest)
}
