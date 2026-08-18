// Command fdbv3bench runs the official Full-Duplex-Bench v3 corpus through a
// standard OpenAI Realtime-compatible endpoint without changing its evaluator.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/fdbv3"
	"github.com/bojieli/OpenRealtime/livebench"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fdbv3bench:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("expected inspect or run subcommand")
	}
	switch arguments[0] {
	case "inspect":
		return inspect(arguments[1:])
	case "run":
		return runBenchmark(arguments[1:])
	default:
		return fmt.Errorf("unknown subcommand %q; expected inspect or run", arguments[0])
	}
}

func inspect(arguments []string) error {
	flags := flag.NewFlagSet("inspect", flag.ContinueOnError)
	datasetRoot := flags.String("dataset-root", "", "released FDB v3 data root")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *datasetRoot == "" {
		return errors.New("--dataset-root is required")
	}
	samples, err := fdbv3.Discover(*datasetRoot)
	if err != nil {
		return err
	}
	domains := make(map[string]int)
	uniqueScenarios := make(map[string]struct{})
	expectedCalls := 0
	rollback := 0
	for _, sample := range samples {
		domains[sample.Metadata.Domain]++
		uniqueScenarios[sample.ExampleID] = struct{}{}
		expectedCalls += len(sample.Metadata.ExpectedToolCalls)
		if sample.Metadata.StateRollbackTest {
			rollback++
		}
	}
	output := map[string]any{
		"benchmark": fdbv3.BenchmarkName, "revision": fdbv3.BenchmarkRevision,
		"audio_examples": len(samples), "unique_scenarios": len(uniqueScenarios), "domains": domains,
		"expected_tool_calls": expectedCalls, "state_rollback_examples": rollback,
		"samples": samples,
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(output)
}

type attempt struct {
	Sample     string    `json:"sample"`
	Number     int       `json:"number"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	DurationMS float64   `json:"duration_ms"`
	Succeeded  bool      `json:"succeeded"`
	Error      string    `json:"error,omitempty"`
}

type failure struct {
	Sample   string `json:"sample"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error"`
}

type runManifest struct {
	SchemaVersion string               `json:"schema_version"`
	CreatedAt     time.Time            `json:"created_at"`
	UpdatedAt     time.Time            `json:"updated_at"`
	Benchmark     string               `json:"benchmark"`
	Revision      string               `json:"revision"`
	Profile       string               `json:"profile"`
	ProfileSHA256 string               `json:"profile_sha256"`
	Descriptor    livebench.Descriptor `json:"descriptor"`
	Samples       []fdbv3.Sample       `json:"samples"`
	Completed     []string             `json:"completed"`
	Failures      []failure            `json:"failures,omitempty"`
	Attempts      []attempt            `json:"attempts,omitempty"`
}

func runBenchmark(arguments []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	datasetRoot := flags.String("dataset-root", "", "released FDB v3 data root")
	profilePath := flags.String("profile", "benchmarks/fdb-v3/openrealtime-v1.json", "checked FDB v3 session/tool profile")
	runRoot := flags.String("run-root", ".runtime/benchmark-runs/fdb-v3/openrealtime-v1", "run-manifest directory")
	endpoint := flags.String("endpoint", environmentDefault("OPENREALTIME_BASE_URL", "ws://127.0.0.1:8765/v1/realtime"), "OpenAI Realtime-compatible WebSocket endpoint")
	apiKey := flags.String("api-key", os.Getenv("OPENREALTIME_API_KEY"), "endpoint bearer token; defaults to OPENREALTIME_API_KEY")
	model := flags.String("model", "openrealtime-local", "model query and evidence label")
	voice := flags.String("voice", "tau-lisa-brenner-v1", "registered local Fish voice")
	provider := flags.String("provider-label", "openrealtime", "provider suffix used by the official evaluator")
	exampleID := flags.String("example-id", "", "optional exact scenario ID")
	pid := flags.String("pid", "", "optional exact speaker ID")
	limit := flags.Int("limit", 0, "maximum examples after filtering; zero means all")
	chunkDuration := flags.Duration("chunk-duration", 20*time.Millisecond, "Realtime input frame duration")
	tailDuration := flags.Duration("tail-duration", 45*time.Second, "maximum wait for the terminal post-input response")
	trialTimeout := flags.Duration("trial-timeout", 3*time.Minute, "timeout for each infrastructure attempt")
	trialAttempts := flags.Int("trial-attempts", 3, "bounded infrastructure attempts per example")
	retryDelay := flags.Duration("retry-delay", time.Second, "base delay between attempts")
	resume := flags.Bool("resume", true, "reuse matching completed official result files")
	continueOnError := flags.Bool("continue-on-error", true, "record exhausted failures and continue")
	requireComplete := flags.Bool("require-complete", false, "exit nonzero after the sweep unless every planned example completed")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *datasetRoot == "" || strings.TrimSpace(*apiKey) == "" {
		return errors.New("--dataset-root and a non-empty --api-key/OPENREALTIME_API_KEY are required")
	}
	if *limit < 0 || *trialAttempts < 1 || *trialAttempts > 10 || *trialTimeout <= 0 || *retryDelay < 0 {
		return errors.New("limit, trial-attempts, trial-timeout, or retry-delay is invalid")
	}
	profile, err := fdbv3.LoadProfile(*profilePath)
	if err != nil {
		return err
	}
	samples, err := fdbv3.Discover(*datasetRoot)
	if err != nil {
		return err
	}
	samples = fdbv3.Select(samples, *exampleID, *pid, *limit)
	if len(samples) == 0 {
		return errors.New("FDB v3 filters matched no examples")
	}
	adapter, err := livebench.NewOpenAIAdapter(livebench.OpenAIConfig{
		APIKey: *apiKey, Endpoint: *endpoint, Model: *model, Voice: *voice,
		Instructions: profile.Instructions, ChunkDuration: *chunkDuration, TailDuration: *tailDuration,
		Provider: *provider, Architecture: "canonical-local-asr-fast-slow-tts",
		Profile: profile.Profile, Tools: profile.Tools, ExecuteTool: fdbv3.MockExecutor,
		AwaitTerminalResponse: true,
	})
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(*runRoot, "run-"+safeName(*provider)+".json")
	manifest := runManifest{
		SchemaVersion: "1.0.0", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		Benchmark: fdbv3.BenchmarkName, Revision: fdbv3.BenchmarkRevision,
		Profile: profile.Profile, ProfileSHA256: profile.SHA256,
		Descriptor: adapter.Descriptor(), Samples: samples,
	}
	if err := os.MkdirAll(*runRoot, 0o755); err != nil {
		return fmt.Errorf("create FDB v3 run root: %w", err)
	}
	if *resume {
		if prior, found, loadErr := loadManifest(manifestPath); loadErr != nil {
			return loadErr
		} else if found {
			if err := validatePrior(prior, manifest); err != nil {
				return fmt.Errorf("resume %s: %w", manifestPath, err)
			}
			manifest.CreatedAt = prior.CreatedAt
			manifest.Attempts = prior.Attempts
		}
	}
	if err := publishManifest(manifestPath, &manifest); err != nil {
		return err
	}

	for _, sample := range samples {
		label := sample.ExampleID + "_" + sample.PID
		if *resume {
			_, found, loadErr := fdbv3.LoadCompleted(sample, *provider, profile.SHA256, *model)
			if loadErr != nil {
				return loadErr
			}
			if found {
				fmt.Fprintf(os.Stderr, "[fdb-v3] %s (resume)\n", label)
				manifest.Completed = appendUnique(manifest.Completed, label)
				manifest.Failures = removeFailure(manifest.Failures, label)
				if err := publishManifest(manifestPath, &manifest); err != nil {
					return err
				}
				continue
			}
		}
		var lastErr error
		for number := 1; number <= *trialAttempts; number++ {
			fmt.Fprintf(os.Stderr, "[fdb-v3] %s attempt=%d/%d\n", label, number, *trialAttempts)
			started := time.Now().UTC()
			ctx, cancel := contextWithTimeout(*trialTimeout)
			_, runErr := fdbv3.RunSample(ctx, adapter, sample, *provider, profile)
			cancel()
			finished := time.Now().UTC()
			record := attempt{
				Sample: label, Number: number, StartedAt: started, FinishedAt: finished,
				DurationMS: float64(finished.Sub(started)) / float64(time.Millisecond), Succeeded: runErr == nil,
			}
			if runErr != nil {
				record.Error = runErr.Error()
			}
			manifest.Attempts = append(manifest.Attempts, record)
			lastErr = runErr
			if runErr == nil {
				manifest.Completed = appendUnique(manifest.Completed, label)
				manifest.Failures = removeFailure(manifest.Failures, label)
				break
			}
			if number < *trialAttempts {
				timer := time.NewTimer(time.Duration(number) * *retryDelay)
				<-timer.C
			}
		}
		if lastErr != nil {
			manifest.Failures = append(removeFailure(manifest.Failures, label), failure{Sample: label, Attempts: *trialAttempts, Error: lastErr.Error()})
		}
		if err := publishManifest(manifestPath, &manifest); err != nil {
			return err
		}
		if lastErr != nil && !*continueOnError {
			return lastErr
		}
	}
	summary := map[string]any{
		"manifest": manifestPath, "planned": len(samples), "completed": len(manifest.Completed),
		"failures": len(manifest.Failures), "profile_sha256": profile.SHA256,
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(summary); err != nil {
		return err
	}
	if *requireComplete {
		return livebench.ValidateRunCompletion(len(samples), len(manifest.Completed), len(manifest.Failures))
	}
	return nil
}

func contextWithTimeout(duration time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), duration)
}

func publishManifest(filename string, manifest *runManifest) error {
	manifest.UpdatedAt = time.Now().UTC()
	sort.Strings(manifest.Completed)
	temporary, err := os.CreateTemp(filepath.Dir(filename), ".fdbv3-run-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary FDB v3 manifest: %w", err)
	}
	name := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(name)
		}
	}()
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(manifest); err != nil {
		return fmt.Errorf("encode FDB v3 manifest: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync FDB v3 manifest: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close FDB v3 manifest: %w", err)
	}
	if err := os.Rename(name, filename); err != nil {
		return fmt.Errorf("publish FDB v3 manifest: %w", err)
	}
	keep = true
	return nil
}

func loadManifest(filename string) (runManifest, bool, error) {
	data, err := os.ReadFile(filename)
	if errors.Is(err, os.ErrNotExist) {
		return runManifest{}, false, nil
	}
	if err != nil {
		return runManifest{}, false, fmt.Errorf("read FDB v3 run manifest: %w", err)
	}
	var manifest runManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return runManifest{}, false, fmt.Errorf("decode FDB v3 run manifest: %w", err)
	}
	return manifest, true, nil
}

func validatePrior(prior, planned runManifest) error {
	priorSamples, _ := json.Marshal(prior.Samples)
	plannedSamples, _ := json.Marshal(planned.Samples)
	if prior.SchemaVersion != planned.SchemaVersion || prior.Benchmark != planned.Benchmark || prior.Revision != planned.Revision ||
		prior.ProfileSHA256 != planned.ProfileSHA256 || prior.Descriptor != planned.Descriptor || string(priorSamples) != string(plannedSamples) {
		return errors.New("prior manifest does not match benchmark, profile, descriptor, or sample hashes")
	}
	return nil
}

func appendUnique(values []string, value string) []string {
	for _, candidate := range values {
		if candidate == value {
			return values
		}
	}
	return append(values, value)
}

func removeFailure(values []failure, sample string) []failure {
	result := values[:0]
	for _, value := range values {
		if value.Sample != sample {
			result = append(result, value)
		}
	}
	return result
}

func safeName(value string) string {
	return regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(value, "-")
}

func environmentDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
