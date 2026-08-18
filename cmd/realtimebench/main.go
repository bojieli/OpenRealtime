// Command realtimebench measures the complete internal ASR -> fast/slow ->
// tool -> TTS trajectory with real provider adapters. It does not extend the
// OpenAI Realtime wire protocol.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/bojieli/OpenRealtime/adapters/gemini"
	"github.com/bojieli/OpenRealtime/adapters/openaicompat"
	"github.com/bojieli/OpenRealtime/adapters/openaitts"
	"github.com/bojieli/OpenRealtime/adapters/qwenasr"
	"github.com/bojieli/OpenRealtime/admission"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/asrbuffer"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interleavebench"
	"github.com/bojieli/OpenRealtime/livebench"
	"github.com/bojieli/OpenRealtime/realtimebench"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "realtimebench:", err)
		os.Exit(1)
	}
}

type options struct {
	tasks                      string
	taskID                     string
	audio                      string
	reference                  string
	output                     string
	wavDirectory               string
	totalTimeout               time.Duration
	requestTimeout             time.Duration
	asrURL                     string
	asrModel                   string
	asrFrame                   time.Duration
	asrProviderChunk           time.Duration
	asrPaced                   bool
	fastProvider               string
	fastModel                  string
	fastBaseURL                string
	fastEffort                 string
	fastTokens                 int
	prepareFast                bool
	prepareSlow                bool
	slowPreparationMinInterval time.Duration
	slowProvider               string
	slowModel                  string
	slowBaseURL                string
	slowEffort                 string
	slowTokens                 int
	maxSlowInvocations         int
	ttsURL                     string
	ttsModel                   string
	ttsVoice                   string
	gpuAdmission               bool
	gpuCapacity                int
	gpuReserve                 int
	asrCost                    int
	fastCost                   int
	slowCost                   int
	ttsCost                    int
	runtime                    runtimeValues
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("realtimebench", flag.ContinueOnError)
	var config options
	flags.StringVar(&config.tasks, "tasks", "benchmarks/interleave/v0.1/tasks.json", "validated fast/slow task JSON")
	flags.StringVar(&config.taskID, "task-id", "", "task ID whose prompt is spoken in --audio")
	flags.StringVar(&config.audio, "audio", "", "PCM16 WAV containing the selected task prompt")
	flags.StringVar(&config.reference, "reference", "", "ASR reference transcript; task prompt when empty")
	flags.StringVar(&config.output, "output", "artifacts/realtimebench/report.json", "secret-free JSON report")
	flags.StringVar(&config.wavDirectory, "wav-dir", "", "optional directory for synthesized assistant segments")
	flags.DurationVar(&config.totalTimeout, "timeout", 10*time.Minute, "complete pipeline timeout")
	flags.DurationVar(&config.requestTimeout, "request-timeout", 2*time.Minute, "individual provider request timeout")
	flags.StringVar(&config.asrURL, "asr-url", qwenasr.DefaultBaseURL, "Qwen3-ASR service base URL")
	flags.StringVar(&config.asrModel, "asr-model", qwenasr.DefaultModel, "exact ASR model recorded in evidence")
	flags.DurationVar(&config.asrFrame, "asr-frame", 50*time.Millisecond, "internal scheduler input cadence")
	flags.DurationVar(&config.asrProviderChunk, "asr-provider-chunk", 200*time.Millisecond, "minimum ASR provider chunk; zero sends ticks directly")
	flags.BoolVar(&config.asrPaced, "asr-paced", true, "replay input at wall-clock speed")
	flags.StringVar(&config.fastProvider, "fast-provider", "vllm", "fast continuation provider: vllm or gemini")
	flags.StringVar(&config.fastModel, "fast-model", "qwen-fast", "exact fast model")
	flags.StringVar(&config.fastBaseURL, "fast-base-url", openaicompat.DefaultBaseURL, "vLLM-compatible fast base URL")
	flags.StringVar(&config.fastEffort, "fast-effort", "minimal", "minimal, low, medium, or high")
	flags.IntVar(&config.fastTokens, "fast-max-tokens", 96, "fast output-token limit")
	flags.BoolVar(&config.prepareFast, "prepare-fast", true, "prepare latest-revision fast output before endpoint and commit only an exact semantic match")
	flags.BoolVar(&config.prepareSlow, "prepare-slow", false, "continue each prepared fast trajectory with slow reasoning before endpoint; requires --prepare-fast")
	flags.DurationVar(&config.slowPreparationMinInterval, "slow-preparation-min-interval", 0, "minimum wall-clock interval between speculative slow launches; exact final commit bypasses the remaining wait")
	flags.StringVar(&config.slowProvider, "slow-provider", "gemini", "slow continuation provider: gemini or vllm")
	flags.StringVar(&config.slowModel, "slow-model", gemini.DefaultModel, "exact slow model")
	flags.StringVar(&config.slowBaseURL, "slow-base-url", openaicompat.DefaultBaseURL, "vLLM-compatible slow base URL")
	flags.StringVar(&config.slowEffort, "slow-effort", "high", "minimal, low, medium, or high")
	flags.IntVar(&config.slowTokens, "slow-max-tokens", 2048, "slow output-token limit per invocation")
	flags.IntVar(&config.maxSlowInvocations, "max-slow-invocations", 6, "slow tool-loop safety bound")
	flags.StringVar(&config.ttsURL, "tts-url", "http://127.0.0.1:8081/v1/audio/speech", "OpenAI-compatible streaming speech endpoint")
	flags.StringVar(&config.ttsModel, "tts-model", openaitts.DefaultModel, "exact TTS model")
	flags.StringVar(&config.ttsVoice, "tts-voice", "default", "TTS voice identifier")
	flags.BoolVar(&config.gpuAdmission, "gpu-admission", false, "coordinate co-located local providers through one priority governor")
	flags.IntVar(&config.gpuCapacity, "gpu-capacity", 100, "abstract GPU admission capacity")
	flags.IntVar(&config.gpuReserve, "gpu-interactive-reserve", 25, "capacity unavailable to speculative/background work")
	flags.IntVar(&config.asrCost, "gpu-asr-cost", 25, "per-ASR-invocation admission cost")
	flags.IntVar(&config.fastCost, "gpu-fast-cost", 75, "fast-continuation admission cost")
	flags.IntVar(&config.slowCost, "gpu-slow-cost", 75, "local slow-continuation admission cost")
	flags.IntVar(&config.ttsCost, "gpu-tts-cost", 50, "streaming-TTS admission cost")
	flags.Var(&config.runtime, "runtime", "repeatable non-secret evidence metadata key=value")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("realtimebench accepts flags only")
	}
	if err := validateOptions(config); err != nil {
		return err
	}

	tasks, err := interleavebench.LoadTasks(config.tasks)
	if err != nil {
		return err
	}
	task, err := selectTask(tasks, config.taskID)
	if err != nil {
		return err
	}
	tools, err := interleavebench.NewTaskToolSet(task)
	if err != nil {
		return err
	}
	audio, err := livebench.ReadWAV(config.audio)
	if err != nil {
		return fmt.Errorf("read input audio: %w", err)
	}
	inputHash, err := livebench.HashFile(config.audio)
	if err != nil {
		return fmt.Errorf("hash input audio: %w", err)
	}
	reference := strings.TrimSpace(config.reference)
	if reference == "" {
		reference = task.Prompt
	}

	var gpuGovernor *admission.Governor
	if config.gpuAdmission {
		gpuGovernor, err = admission.NewGovernor(admission.Config{
			Capacity: config.gpuCapacity, ReservedInteractive: config.gpuReserve,
		})
		if err != nil {
			return fmt.Errorf("configure GPU admission: %w", err)
		}
	}

	asr, err := qwenasr.New(qwenasr.Config{
		BaseURL: config.asrURL, Model: config.asrModel,
		BearerToken: os.Getenv("QWEN_ASR_API_KEY"), RequestTimeout: config.requestTimeout,
	})
	if err != nil {
		return fmt.Errorf("configure ASR: %w", err)
	}
	var perception v1.PerceptionProvider = asr
	if gpuGovernor != nil {
		perception = &admission.PerceptionProvider{
			Provider: perception, Governor: gpuGovernor,
			Request: admission.Request{Class: admission.ClassInteractive, Cost: config.asrCost, Label: "asr"},
		}
	}
	if config.asrProviderChunk > 0 {
		perception, err = asrbuffer.New(asrbuffer.Config{Provider: perception, MinimumChunk: config.asrProviderChunk})
		if err != nil {
			return fmt.Errorf("configure ASR cadence buffer: %w", err)
		}
	}
	fastEffort, err := parseEffort(config.fastEffort)
	if err != nil {
		return fmt.Errorf("fast effort: %w", err)
	}
	slowEffort, err := parseEffort(config.slowEffort)
	if err != nil {
		return fmt.Errorf("slow effort: %w", err)
	}
	fastBase, err := makeProvider(config.fastProvider, config.fastModel, config.fastBaseURL, trajectory.PhaseFast, fastEffort, config.requestTimeout)
	if err != nil {
		return fmt.Errorf("configure fast provider: %w", err)
	}
	var fast continuation.Provider = fastBase
	var fastPreparation continuation.Provider = fastBase
	if gpuGovernor != nil && strings.EqualFold(strings.TrimSpace(config.fastProvider), "vllm") {
		fast = &admission.ContinuationProvider{
			Provider: fastBase, Governor: gpuGovernor,
			Request: admission.Request{Class: admission.ClassInteractive, Cost: config.fastCost, Label: "fast-final"},
		}
		fastPreparation = &admission.ContinuationProvider{
			Provider: fastBase, Governor: gpuGovernor,
			Request: admission.Request{
				Class: admission.ClassSpeculative, Cost: config.fastCost,
				Preemptible: true, Label: "fast-preparation",
			},
		}
	}
	slowBase, err := makeProvider(config.slowProvider, config.slowModel, config.slowBaseURL, trajectory.PhaseSlow, slowEffort, config.requestTimeout)
	if err != nil {
		return fmt.Errorf("configure slow provider: %w", err)
	}
	var slow continuation.Provider = slowBase
	var slowPreparation continuation.Provider = slowBase
	if gpuGovernor != nil && strings.EqualFold(strings.TrimSpace(config.slowProvider), "vllm") {
		slow = &admission.ContinuationProvider{
			Provider: slowBase, Governor: gpuGovernor,
			Request: admission.Request{Class: admission.ClassBackground, Cost: config.slowCost, Preemptible: true, Label: "slow-final"},
		}
		slowPreparation = &admission.ContinuationProvider{
			Provider: slowBase, Governor: gpuGovernor,
			Request: admission.Request{Class: admission.ClassBackground, Cost: config.slowCost, Preemptible: true, Label: "slow-preparation"},
		}
	}
	tts, err := openaitts.New(openaitts.Config{
		Endpoint: config.ttsURL, Model: config.ttsModel, Voice: config.ttsVoice,
		BearerToken: os.Getenv("OPENAI_TTS_API_KEY"), RequestTimeout: config.requestTimeout,
		FallbackSampleRate: 24_000, OutputSampleRateHz: 24_000,
	})
	if err != nil {
		return fmt.Errorf("configure TTS: %w", err)
	}
	var speech v1.StreamingSpeechProvider = tts
	if gpuGovernor != nil {
		speech = &admission.SpeechProvider{
			Provider: speech, Governor: gpuGovernor,
			Request: admission.Request{Class: admission.ClassInteractive, Cost: config.ttsCost, Label: "tts"},
		}
	}

	runtime := config.runtime.Map()
	runtime["asr_scheduler_frame"] = config.asrFrame.String()
	if config.asrProviderChunk > 0 {
		runtime["asr_provider_chunk"] = config.asrProviderChunk.String()
	} else {
		runtime["asr_provider_chunk"] = "direct"
	}
	runtime["asr_transport"] = "qwen-asr-stateful-http"
	runtime["tts_transport"] = "openai-compatible-speech-http"
	runtime["fast_preparation"] = fmt.Sprint(config.prepareFast)
	runtime["slow_preparation"] = fmt.Sprint(config.prepareSlow)
	runtime["slow_preparation_min_interval"] = config.slowPreparationMinInterval.String()
	if gpuGovernor != nil {
		runtime["gpu_admission"] = "explicit-class-reserved-capacity"
		runtime["gpu_admission_capacity"] = fmt.Sprint(config.gpuCapacity)
		runtime["gpu_interactive_reserve"] = fmt.Sprint(config.gpuReserve)
	}

	ctx, cancel := context.WithTimeout(context.Background(), config.totalTimeout)
	report, runErr := realtimebench.Run(ctx, realtimebench.Config{
		Scenario: realtimebench.Scenario{
			ID: task.ID, InputSHA256: inputHash, ReferenceText: reference,
			ExpectedSubstrings: task.ExpectedSubstrings, RequiredToolCalls: task.RequiredToolCalls,
			ExpectedToolCalls: task.ExpectedToolCalls, AllowToolErrors: task.AllowToolErrors,
			Audio: audio,
		},
		ASR: perception, FastProvider: fast, FastPreparationProvider: fastPreparation,
		SlowProvider: slow, SlowPreparationProvider: slowPreparation,
		TTS: speech, Tools: tools,
		ASRFrameDuration: config.asrFrame, Paced: config.asrPaced,
		FastMaxOutputTokens: config.fastTokens, SlowMaxOutputTokens: config.slowTokens,
		MaxSlowInvocations: config.maxSlowInvocations, Runtime: runtime,
		AdmissionGovernor:          gpuGovernor,
		PrepareFastBeforeEndpoint:  config.prepareFast,
		PrepareSlowBeforeEndpoint:  config.prepareSlow,
		SlowPreparationMinInterval: config.slowPreparationMinInterval,
	})
	cancel()
	if wavErr := writeSpeechWAVs(config.wavDirectory, &report); wavErr != nil {
		runErr = errors.Join(runErr, wavErr)
	}
	if err := realtimebench.WriteReport(config.output, report); err != nil {
		return errors.Join(runErr, err)
	}

	summary := struct {
		Report                     string   `json:"report"`
		Passed                     bool     `json:"passed"`
		ASRFinal                   string   `json:"asr_final"`
		EndpointToFastFirstEventMS *float64 `json:"endpoint_to_fast_first_event_ms,omitempty"`
		EndpointToFastSafePointMS  *float64 `json:"endpoint_to_fast_safe_point_ms,omitempty"`
		EndpointToFirstAudioMS     *float64 `json:"endpoint_to_first_audio_ms,omitempty"`
		FastPreparationAccepted    *bool    `json:"fast_preparation_accepted,omitempty"`
		BackgroundPreparation      bool     `json:"background_preparation_committed"`
		PreparedStagesReplayed     uint64   `json:"prepared_stages_replayed"`
		PacedSlowWaits             uint64   `json:"paced_slow_waits"`
		PacedSlowCancellations     uint64   `json:"paced_slow_cancellations"`
		CommitPacingBypasses       uint64   `json:"commit_pacing_bypasses"`
		ToolProposals              int      `json:"tool_proposals"`
		ToolCalls                  int      `json:"tool_calls"`
		SpeechSegments             int      `json:"speech_segments"`
	}{
		Report: config.output, Passed: report.Passed, ASRFinal: report.ASR.FinalTranscript,
		EndpointToFastFirstEventMS: report.EndpointToFastFirstEventMS,
		EndpointToFastSafePointMS:  report.EndpointToFastSafePointMS,
		EndpointToFirstAudioMS:     report.EndpointToFirstAudioMS,
		ToolProposals:              len(report.ToolProposals), ToolCalls: len(report.ToolCalls),
		SpeechSegments: len(report.Speech),
	}
	if report.FastPreparation != nil {
		accepted := report.FastPreparation.Accepted
		summary.FastPreparationAccepted = &accepted
	}
	if report.BackgroundPreparation != nil {
		summary.BackgroundPreparation = report.BackgroundPreparation.Committed
		summary.PreparedStagesReplayed = report.BackgroundPreparation.ReplayedStages
		summary.PacedSlowWaits = report.BackgroundPreparation.PacedStageWaits
		summary.PacedSlowCancellations = report.BackgroundPreparation.PacedStageCancels
		summary.CommitPacingBypasses = report.BackgroundPreparation.CommitBypasses
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(summary); err != nil {
		return errors.Join(runErr, err)
	}
	if runErr != nil {
		return runErr
	}
	if !report.Passed {
		return fmt.Errorf("benchmark scoring failed: %s", strings.Join(report.ScoreFailures, "; "))
	}
	return nil
}

func validateOptions(config options) error {
	if strings.TrimSpace(config.taskID) == "" || strings.TrimSpace(config.audio) == "" {
		return errors.New("--task-id and --audio are required")
	}
	if config.totalTimeout <= 0 || config.requestTimeout <= 0 {
		return errors.New("timeouts must be positive")
	}
	if config.asrFrame <= 0 || config.asrProviderChunk < 0 {
		return errors.New("ASR frame must be positive and provider chunk cannot be negative")
	}
	if config.fastTokens <= 0 || config.slowTokens <= 0 || config.maxSlowInvocations <= 0 {
		return errors.New("model token and invocation limits must be positive")
	}
	if config.prepareSlow && !config.prepareFast {
		return errors.New("--prepare-slow requires --prepare-fast")
	}
	if config.slowPreparationMinInterval < 0 {
		return errors.New("--slow-preparation-min-interval cannot be negative")
	}
	if config.slowPreparationMinInterval > 0 && !config.prepareSlow {
		return errors.New("--slow-preparation-min-interval requires --prepare-slow")
	}
	if config.gpuAdmission {
		if config.gpuCapacity <= 0 || config.gpuReserve < 0 || config.gpuReserve > config.gpuCapacity {
			return errors.New("GPU capacity must be positive and its interactive reserve must be within capacity")
		}
		costs := []struct {
			name string
			cost int
		}{
			{name: "ASR", cost: config.asrCost},
			{name: "fast", cost: config.fastCost},
			{name: "slow", cost: config.slowCost},
			{name: "TTS", cost: config.ttsCost},
		}
		for _, item := range costs {
			if item.cost <= 0 || item.cost > config.gpuCapacity {
				return fmt.Errorf("GPU %s cost must be between 1 and capacity", item.name)
			}
		}
		lowerCapacity := config.gpuCapacity - config.gpuReserve
		if config.prepareFast && strings.EqualFold(strings.TrimSpace(config.fastProvider), "vllm") && config.fastCost > lowerCapacity {
			return errors.New("GPU speculative fast cost must fit outside the interactive reservation")
		}
		if strings.EqualFold(strings.TrimSpace(config.slowProvider), "vllm") && config.slowCost > lowerCapacity {
			return errors.New("GPU local slow cost must fit outside the interactive reservation")
		}
	}
	return nil
}

func selectTask(tasks []interleavebench.Task, id string) (interleavebench.Task, error) {
	for _, task := range tasks {
		if task.ID == id {
			return task, nil
		}
	}
	return interleavebench.Task{}, fmt.Errorf("task %q is not present in the task file", id)
}

func makeProvider(name, model, baseURL string, phase trajectory.Phase, effort continuation.Effort, timeout time.Duration) (continuation.Provider, error) {
	toolAuthority := continuation.ToolAuthorityPropose
	if phase == trajectory.PhaseSlow {
		toolAuthority = continuation.ToolAuthorityExecute
	}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "gemini":
		if strings.TrimSpace(model) == "" {
			model = gemini.DefaultModel
		}
		return gemini.New(gemini.Config{
			APIKey: os.Getenv("GEMINI_API_KEY"), Model: model, Phase: phase,
			Effort: effort, ToolAuthority: toolAuthority, IncludeThoughts: false,
			RequestTimeout: timeout,
		})
	case "vllm":
		if strings.TrimSpace(model) == "" {
			return nil, errors.New("vLLM model is required")
		}
		thinking := openaicompat.ThinkingDisabled
		if phase == trajectory.PhaseSlow && effort != continuation.EffortMinimal {
			thinking = openaicompat.ThinkingEnabled
		}
		temperature := 0.0
		seed := int64(0)
		return openaicompat.New(openaicompat.Config{
			APIKey: os.Getenv("OPENAI_COMPAT_API_KEY"), Model: model, BaseURL: baseURL,
			Provider: "vllm", Phase: phase, Effort: effort, ToolAuthority: toolAuthority,
			ThinkingMode: thinking, Temperature: &temperature, Seed: &seed,
			RequestTimeout: timeout,
		})
	default:
		return nil, fmt.Errorf("unknown provider %q; expected gemini or vllm", name)
	}
}

func parseEffort(value string) (continuation.Effort, error) {
	effort := continuation.Effort(strings.ToLower(strings.TrimSpace(value)))
	switch effort {
	case continuation.EffortMinimal, continuation.EffortLow, continuation.EffortMedium, continuation.EffortHigh:
		return effort, nil
	default:
		return "", fmt.Errorf("unsupported value %q", value)
	}
}

func writeSpeechWAVs(directory string, report *realtimebench.Report) error {
	if strings.TrimSpace(directory) == "" || len(report.Speech) == 0 {
		return nil
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create speech WAV directory: %w", err)
	}
	for index := range report.Speech {
		segment := &report.Speech[index]
		name := fmt.Sprintf("%s-%s-%02d.wav", safeName(report.ScenarioID), segment.Phase, segment.Ordinal)
		path := filepath.Join(directory, name)
		hash, err := livebench.WriteWAV(path, livebench.Audio{
			PCM16: segment.Result.OutputPCM16, SampleRateHz: segment.Result.OutputSampleRate,
		})
		if err != nil {
			return fmt.Errorf("write %s speech WAV: %w", segment.Phase, err)
		}
		if report.Runtime == nil {
			report.Runtime = make(map[string]string)
		}
		report.Runtime[fmt.Sprintf("speech_wav_%s_%02d_sha256", segment.Phase, segment.Ordinal)] = hash
	}
	return nil
}

func safeName(value string) string {
	var builder strings.Builder
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || character == '-' || character == '_' {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('-')
		}
	}
	name := strings.Trim(builder.String(), "-")
	if name == "" {
		return "scenario"
	}
	return name
}

type runtimeValues struct {
	values map[string]string
}

func (values *runtimeValues) String() string {
	if values == nil || len(values.values) == 0 {
		return ""
	}
	parts := make([]string, 0, len(values.values))
	for key, value := range values.values {
		parts = append(parts, key+"="+value)
	}
	slices.Sort(parts)
	return strings.Join(parts, ",")
}

func (values *runtimeValues) Set(raw string) error {
	key, value, found := strings.Cut(raw, "=")
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if !found || key == "" || value == "" {
		return errors.New("runtime metadata must be key=value")
	}
	lower := strings.ToLower(key)
	for _, forbidden := range []string{"key", "token", "secret", "password", "authorization", "cookie"} {
		if strings.Contains(lower, forbidden) {
			return fmt.Errorf("runtime metadata key %q may contain a secret", key)
		}
	}
	if values.values == nil {
		values.values = make(map[string]string)
	}
	if _, duplicate := values.values[key]; duplicate {
		return fmt.Errorf("duplicate runtime metadata key %q", key)
	}
	values.values[key] = value
	return nil
}

func (values runtimeValues) Map() map[string]string {
	result := make(map[string]string, len(values.values)+4)
	for key, value := range values.values {
		result[key] = value
	}
	return result
}
