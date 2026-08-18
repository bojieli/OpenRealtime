// Command interleavebench runs real fast/slow continuation workloads and
// writes a secret-free evidence report.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/gemini"
	"github.com/bojieli/OpenRealtime/adapters/openaicompat"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interleavebench"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "interleavebench:", err)
		os.Exit(1)
	}
}

type options struct {
	tasks              string
	output             string
	fastProvider       string
	fastModel          string
	fastBaseURL        string
	fastEffort         string
	slowProvider       string
	slowModel          string
	slowBaseURL        string
	slowEffort         string
	maxTasks           int
	replicates         int
	taskTimeout        time.Duration
	fastTokens         int
	slowTokens         int
	maxSlowInvocations int
	retainReasoning    bool
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("interleavebench", flag.ContinueOnError)
	var config options
	flags.StringVar(&config.tasks, "tasks", "benchmarks/interleave/v0.1/tasks.json", "validated benchmark task JSON")
	flags.StringVar(&config.output, "output", "artifacts/interleavebench/report.json", "secret-free JSON report")
	flags.StringVar(&config.fastProvider, "fast-provider", "gemini", "gemini or vllm")
	flags.StringVar(&config.fastModel, "fast-model", "", "exact fast model name")
	flags.StringVar(&config.fastBaseURL, "fast-base-url", openaicompat.DefaultBaseURL, "vLLM-compatible fast base URL")
	flags.StringVar(&config.fastEffort, "fast-effort", "minimal", "minimal, low, medium, or high")
	flags.StringVar(&config.slowProvider, "slow-provider", "gemini", "gemini or vllm")
	flags.StringVar(&config.slowModel, "slow-model", "", "exact slow model name")
	flags.StringVar(&config.slowBaseURL, "slow-base-url", openaicompat.DefaultBaseURL, "vLLM-compatible slow base URL")
	flags.StringVar(&config.slowEffort, "slow-effort", "high", "minimal, low, medium, or high")
	flags.IntVar(&config.maxTasks, "max-tasks", 0, "maximum tasks; zero runs all")
	flags.IntVar(&config.replicates, "replicates", 1, "number of sequential trials per task")
	flags.DurationVar(&config.taskTimeout, "task-timeout", 2*time.Minute, "timeout per complete fast/slow rollout")
	flags.IntVar(&config.fastTokens, "fast-max-tokens", 128, "fast output-token limit")
	flags.IntVar(&config.slowTokens, "slow-max-tokens", 2048, "slow output-token limit per invocation, including hidden reasoning")
	flags.IntVar(&config.maxSlowInvocations, "max-slow-invocations", 6, "tool-loop safety bound")
	flags.BoolVar(&config.retainReasoning, "retain-reasoning", false, "retain plaintext reasoning internally; never writes it to the report")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("interleavebench accepts flags only")
	}
	if config.maxTasks < 0 || config.replicates <= 0 || config.taskTimeout <= 0 || config.fastTokens <= 0 || config.slowTokens <= 0 || config.maxSlowInvocations <= 0 {
		return errors.New("task, replicate, and token limits must be positive; max-tasks may be zero")
	}
	fastEffort, err := parseEffort(config.fastEffort)
	if err != nil {
		return fmt.Errorf("fast effort: %w", err)
	}
	slowEffort, err := parseEffort(config.slowEffort)
	if err != nil {
		return fmt.Errorf("slow effort: %w", err)
	}
	fast, err := makeProvider(config.fastProvider, config.fastModel, config.fastBaseURL, trajectory.PhaseFast, fastEffort)
	if err != nil {
		return fmt.Errorf("configure fast provider: %w", err)
	}
	slow, err := makeProvider(config.slowProvider, config.slowModel, config.slowBaseURL, trajectory.PhaseSlow, slowEffort)
	if err != nil {
		return fmt.Errorf("configure slow provider: %w", err)
	}
	tasks, err := interleavebench.LoadTasks(config.tasks)
	if err != nil {
		return err
	}
	report, err := interleavebench.Run(context.Background(), interleavebench.Config{
		FastProvider: fast, SlowProvider: slow, Tasks: tasks, Replicates: config.replicates,
		MaxTasks: config.maxTasks, TaskTimeout: config.taskTimeout,
		FastMaxOutputTokens: config.fastTokens, SlowMaxOutputTokens: config.slowTokens,
		MaxSlowInvocations: config.maxSlowInvocations, RetainReasoning: config.retainReasoning,
	})
	if err != nil {
		return err
	}
	if err := interleavebench.WriteReport(config.output, report); err != nil {
		return err
	}
	summary := struct {
		Report       string  `json:"report"`
		Tasks        int     `json:"tasks"`
		Passed       int     `json:"passed"`
		DurationMS   float64 `json:"duration_ms"`
		FastProvider string  `json:"fast_provider"`
		FastModel    string  `json:"fast_model"`
		SlowProvider string  `json:"slow_provider"`
		SlowModel    string  `json:"slow_model"`
	}{
		Report: config.output, Tasks: report.TaskCount, Passed: report.Passed,
		DurationMS:   report.TotalDurationMS,
		FastProvider: report.Fast.Provider, FastModel: report.Fast.Model,
		SlowProvider: report.Slow.Provider, SlowModel: report.Slow.Model,
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(summary)
}

func makeProvider(name, model, baseURL string, phase trajectory.Phase, effort continuation.Effort) (continuation.Provider, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	toolAuthority := continuation.ToolAuthorityPropose
	if phase == trajectory.PhaseSlow {
		toolAuthority = continuation.ToolAuthorityExecute
	}
	switch name {
	case "gemini":
		if model == "" {
			model = gemini.DefaultModel
		}
		return gemini.New(gemini.Config{
			APIKey: os.Getenv("GEMINI_API_KEY"), Model: model,
			Phase: phase, Effort: effort, ToolAuthority: toolAuthority,
			IncludeThoughts: false,
		})
	case "vllm":
		if strings.TrimSpace(model) == "" {
			return nil, errors.New("--fast-model/--slow-model is required for vllm")
		}
		thinking := openaicompat.ThinkingDisabled
		if phase == trajectory.PhaseSlow && effort != continuation.EffortMinimal {
			thinking = openaicompat.ThinkingEnabled
		}
		temperature := 0.0
		seed := int64(0)
		return openaicompat.New(openaicompat.Config{
			APIKey: os.Getenv("OPENAI_COMPAT_API_KEY"), Model: model,
			BaseURL: baseURL, Provider: "vllm", Phase: phase, Effort: effort,
			ToolAuthority: toolAuthority, ThinkingMode: thinking,
			Temperature: &temperature, Seed: &seed,
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
