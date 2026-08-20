// Package interleavebench runs reproducible fast/slow continuation workloads
// against real model providers. Reports deliberately exclude API keys, raw
// reasoning text, and opaque provider state.
package interleavebench

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/agenttool"
	"github.com/bojieli/OpenRealtime/benchspec"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interleave"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	// SchemaVersion identifies the stable, secret-free report shape.
	SchemaVersion = "interleave-benchmark-v0.2"
	maxTaskBytes  = 16 << 20
)

// Task is one deterministic tool-use and reasoning workload. Records form the
// task-local corpus exposed only through records.read.
type Task struct {
	ID                 string                          `json:"id"`
	Prompt             string                          `json:"prompt"`
	Records            map[string]json.RawMessage      `json:"records"`
	ExpectedSubstrings []string                        `json:"expected_substrings"`
	RequiredToolCalls  []string                        `json:"required_tool_calls,omitempty"`
	ExpectedToolCalls  []benchspec.ToolCallExpectation `json:"expected_tool_calls,omitempty"`
	AllowToolErrors    bool                            `json:"allow_tool_errors,omitempty"`
}

// Config supplies the two real continuation profiles and benchmark controls.
type Config struct {
	FastProvider        continuation.Provider
	SlowProvider        continuation.Provider
	Tasks               []Task
	Replicates          int
	MaxTasks            int
	TaskTimeout         time.Duration
	FastMaxOutputTokens int
	SlowMaxOutputTokens int
	MaxSlowInvocations  int
	RetainReasoning     bool
	Clock               func() time.Time
}

// InvocationTiming is one provider call measured at the benchmark boundary.
type InvocationTiming struct {
	Phase         trajectory.Phase   `json:"phase"`
	Ordinal       int                `json:"ordinal"`
	StartOffsetMS float64            `json:"start_offset_ms"`
	FirstEventMS  *float64           `json:"first_event_ms,omitempty"`
	DurationMS    float64            `json:"duration_ms"`
	Usage         continuation.Usage `json:"usage,omitempty"`
	StopReason    string             `json:"stop_reason,omitempty"`
	Error         string             `json:"error,omitempty"`
}

// AssistantSegment is externally speakable text in canonical order.
type AssistantSegment struct {
	Phase       trajectory.Phase `json:"phase"`
	Text        string           `json:"text"`
	Interrupted bool             `json:"interrupted,omitempty"`
}

// TaskResult is a complete, auditable result without private model state.
type TaskResult struct {
	TaskID           string                  `json:"task_id"`
	Replicate        int                     `json:"replicate"`
	Passed           bool                    `json:"passed"`
	ScoreFailures    []string                `json:"score_failures,omitempty"`
	Error            string                  `json:"error,omitempty"`
	DurationMS       float64                 `json:"duration_ms"`
	Invocations      []InvocationTiming      `json:"invocations"`
	Assistant        []AssistantSegment      `json:"assistant"`
	ToolProposals    []trajectory.ToolCall   `json:"tool_proposals,omitempty"`
	ToolCalls        []trajectory.ToolCall   `json:"tool_calls,omitempty"`
	ToolResults      []trajectory.ToolResult `json:"tool_results,omitempty"`
	TrajectoryItems  int                     `json:"trajectory_items"`
	TrajectorySHA256 string                  `json:"trajectory_sha256"`
}

// Report is the top-level benchmark evidence artifact.
type Report struct {
	SchemaVersion   string                  `json:"schema_version"`
	CreatedAt       time.Time               `json:"created_at"`
	Fast            continuation.Descriptor `json:"fast"`
	Slow            continuation.Descriptor `json:"slow"`
	TaskDefinitions int                     `json:"task_definitions"`
	Replicates      int                     `json:"replicates"`
	TaskCount       int                     `json:"task_count"`
	Passed          int                     `json:"passed"`
	Summary         Summary                 `json:"summary"`
	TotalDurationMS float64                 `json:"total_duration_ms"`
	Results         []TaskResult            `json:"results"`
}

// MetricSummary reports a compact empirical distribution. Percentiles use the
// nearest-rank definition over the observed trials.
type MetricSummary struct {
	Count int     `json:"count"`
	Mean  float64 `json:"mean"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	Max   float64 `json:"max"`
}

// Summary aggregates latency and usage without discarding per-trial evidence.
type Summary struct {
	PassRate            float64       `json:"pass_rate"`
	TaskDurationMS      MetricSummary `json:"task_duration_ms"`
	FastFirstEventMS    MetricSummary `json:"fast_first_event_ms"`
	FastDurationMS      MetricSummary `json:"fast_duration_ms"`
	SlowFirstFromTaskMS MetricSummary `json:"slow_first_from_task_ms"`
	TotalToolCalls      int           `json:"total_tool_calls"`
	InputTokens         int64         `json:"input_tokens"`
	CachedInputTokens   int64         `json:"cached_input_tokens"`
	CachedInputReports  int           `json:"cached_input_reports"`
	OutputTokens        int64         `json:"output_tokens"`
	ReasoningTokens     int64         `json:"reasoning_tokens"`
}

// LoadTasks decodes and validates a JSON array without allowing unknown fields.
func LoadTasks(filename string) ([]Task, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open interleave tasks: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxTaskBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read interleave tasks: %w", err)
	}
	if len(data) > maxTaskBytes {
		return nil, errors.New("interleave task file exceeds 16 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var tasks []Task
	if err := decoder.Decode(&tasks); err != nil {
		return nil, fmt.Errorf("decode interleave tasks: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("interleave task file contains trailing JSON")
	}
	if err := ValidateTasks(tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}

// ValidateTasks checks IDs, expected answers, records, and tool requirements.
func ValidateTasks(tasks []Task) error {
	if len(tasks) == 0 {
		return errors.New("interleave benchmark requires at least one task")
	}
	seen := make(map[string]struct{}, len(tasks))
	for index, task := range tasks {
		if strings.TrimSpace(task.ID) == "" || strings.TrimSpace(task.Prompt) == "" {
			return fmt.Errorf("interleave task %d requires ID and prompt", index)
		}
		if _, duplicate := seen[task.ID]; duplicate {
			return fmt.Errorf("duplicate interleave task ID %q", task.ID)
		}
		seen[task.ID] = struct{}{}
		if len(task.ExpectedSubstrings) == 0 {
			return fmt.Errorf("interleave task %q requires expected substrings", task.ID)
		}
		for _, expected := range task.ExpectedSubstrings {
			if normalize(expected) == "" {
				return fmt.Errorf("interleave task %q contains an empty expected substring", task.ID)
			}
		}
		for key, value := range task.Records {
			if strings.TrimSpace(key) == "" || len(value) == 0 || !json.Valid(value) {
				return fmt.Errorf("interleave task %q record %q must contain valid JSON", task.ID, key)
			}
		}
		for _, required := range task.RequiredToolCalls {
			if required != "records.read" {
				return fmt.Errorf("interleave task %q requires unknown benchmark tool %q", task.ID, required)
			}
		}
		if err := benchspec.ValidateToolCallExpectations(task.ExpectedToolCalls); err != nil {
			return fmt.Errorf("interleave task %q: %w", task.ID, err)
		}
		for _, expected := range task.ExpectedToolCalls {
			if expected.Name != "records.read" {
				return fmt.Errorf("interleave task %q expects unknown benchmark tool %q", task.ID, expected.Name)
			}
			var arguments struct {
				Key string `json:"key"`
			}
			if err := json.Unmarshal(expected.Arguments, &arguments); err != nil || strings.TrimSpace(arguments.Key) == "" {
				return fmt.Errorf("interleave task %q expected records.read call requires a string key", task.ID)
			}
			if _, exists := task.Records[arguments.Key]; !exists {
				return fmt.Errorf("interleave task %q expects missing record key %q", task.ID, arguments.Key)
			}
		}
	}
	return nil
}

// Run executes tasks sequentially to avoid hiding provider queueing behind
// client-side concurrency. Provider servers may still batch internally.
func Run(ctx context.Context, config Config) (Report, error) {
	if config.FastProvider == nil || config.SlowProvider == nil {
		return Report{}, errors.New("interleave benchmark requires fast and slow providers")
	}
	if err := ValidateTasks(config.Tasks); err != nil {
		return Report{}, err
	}
	if config.MaxTasks < 0 || config.Replicates < 0 || config.TaskTimeout < 0 {
		return Report{}, errors.New("interleave benchmark limits cannot be negative")
	}
	if config.Replicates == 0 {
		config.Replicates = 1
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	tasks := config.Tasks
	if config.MaxTasks > 0 && config.MaxTasks < len(tasks) {
		tasks = tasks[:config.MaxTasks]
	}
	started := config.Clock()
	report := Report{
		SchemaVersion: SchemaVersion, CreatedAt: started.UTC(),
		Fast: config.FastProvider.Descriptor(), Slow: config.SlowProvider.Descriptor(),
		TaskDefinitions: len(tasks), Replicates: config.Replicates,
		TaskCount: len(tasks) * config.Replicates,
	}
	for replicate := range config.Replicates {
		for _, task := range tasks {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			taskContext := ctx
			cancel := func() {}
			if config.TaskTimeout > 0 {
				taskContext, cancel = context.WithTimeout(ctx, config.TaskTimeout)
			}
			result := runTask(taskContext, config, task, replicate)
			cancel()
			report.Results = append(report.Results, result)
			if result.Passed {
				report.Passed++
			}
		}
	}
	report.TotalDurationMS = milliseconds(config.Clock().Sub(started))
	report.Summary = summarizeResults(report.Results, report.Passed)
	return report, nil
}

func summarizeResults(results []TaskResult, passed int) Summary {
	var taskDurations, fastFirst, fastDurations, slowFirst []float64
	var summary Summary
	if len(results) > 0 {
		summary.PassRate = float64(passed) / float64(len(results))
	}
	for _, result := range results {
		taskDurations = append(taskDurations, result.DurationMS)
		summary.TotalToolCalls += len(result.ToolCalls)
		firstFastRecorded := false
		firstSlowRecorded := false
		for _, invocation := range result.Invocations {
			summary.InputTokens += invocation.Usage.InputTokens
			summary.CachedInputTokens += invocation.Usage.CachedInputTokens
			if invocation.Usage.CachedInputTokensReported {
				summary.CachedInputReports++
			}
			summary.OutputTokens += invocation.Usage.OutputTokens
			summary.ReasoningTokens += invocation.Usage.ReasoningTokens
			switch invocation.Phase {
			case trajectory.PhaseFast:
				if !firstFastRecorded {
					fastDurations = append(fastDurations, invocation.DurationMS)
					if invocation.FirstEventMS != nil {
						fastFirst = append(fastFirst, *invocation.FirstEventMS)
					}
					firstFastRecorded = true
				}
			case trajectory.PhaseSlow:
				if !firstSlowRecorded && invocation.FirstEventMS != nil {
					slowFirst = append(slowFirst, invocation.StartOffsetMS+*invocation.FirstEventMS)
					firstSlowRecorded = true
				}
			}
		}
	}
	summary.TaskDurationMS = metricSummary(taskDurations)
	summary.FastFirstEventMS = metricSummary(fastFirst)
	summary.FastDurationMS = metricSummary(fastDurations)
	summary.SlowFirstFromTaskMS = metricSummary(slowFirst)
	return summary
}

func metricSummary(values []float64) MetricSummary {
	if len(values) == 0 {
		return MetricSummary{}
	}
	values = slices.Clone(values)
	slices.Sort(values)
	var total float64
	for _, value := range values {
		total += value
	}
	nearestRank := func(percentile float64) float64 {
		index := int(math.Ceil(percentile*float64(len(values)))) - 1
		index = max(0, min(index, len(values)-1))
		return values[index]
	}
	return MetricSummary{
		Count: len(values), Mean: total / float64(len(values)),
		P50: nearestRank(0.50), P95: nearestRank(0.95), Max: values[len(values)-1],
	}
}

func runTask(ctx context.Context, config Config, task Task, replicate int) TaskResult {
	started := config.Clock()
	store := trajectory.NewStore()
	if err := store.Append(trajectory.Item{
		ID: "observation-1", Kind: trajectory.KindObservation,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: task.Prompt,
	}); err != nil {
		return TaskResult{TaskID: task.ID, Replicate: replicate, Error: err.Error()}
	}
	registry, err := NewTaskToolSet(task)
	if err != nil {
		return TaskResult{TaskID: task.ID, Replicate: replicate, Error: err.Error()}
	}
	fast := newTimedProvider(config.FastProvider, started, config.Clock)
	slow := newTimedProvider(config.SlowProvider, started, config.Clock)
	engine, err := interleave.New(interleave.Config{
		Store: store, FastProvider: fast, SlowProvider: slow, Tools: registry,
		FastInstruction:     "Give a useful immediate response as the first continuation. You receive the complete tool schemas with proposal-only authority. When missing information or an action is needed, emit a tool proposal; do not claim a result or completion that is not already evidenced in the trajectory.",
		SlowInstruction:     "Continue the exact trajectory as the higher-reasoning phase and independently resolve the latest user request. Fast assistant content and tool proposals are provisional working state, not proof that the task is finished. Only your tool calls execute. Preserve literal identifiers exactly and treat tool errors as authoritative instead of guessing variants. Use tools as needed, then end with one concise, self-contained final answer, correcting any earlier mistake explicitly.",
		FastMaxOutputTokens: config.FastMaxOutputTokens, SlowMaxOutputTokens: config.SlowMaxOutputTokens,
		MaxSlowInvocations: config.MaxSlowInvocations, RetainReasoning: config.RetainReasoning,
	})
	if err != nil {
		return TaskResult{TaskID: task.ID, Replicate: replicate, Error: err.Error()}
	}
	rollout, rolloutErr := engine.Run(ctx, interleave.Request{SourceRevision: 1}, nil)
	snapshot := store.Snapshot()
	result := TaskResult{
		TaskID: task.ID, Replicate: replicate, DurationMS: milliseconds(config.Clock().Sub(started)),
		Invocations: append(fast.Timings(), slow.Timings()...),
		ToolResults: cloneToolResults(rollout.ToolResults), TrajectoryItems: len(snapshot.Items),
	}
	if rolloutErr != nil {
		result.Error = rolloutErr.Error()
	}
	for _, item := range snapshot.Items {
		switch item.Kind {
		case trajectory.KindAssistant:
			result.Assistant = append(result.Assistant, AssistantSegment{
				Phase: item.Producer.Phase, Text: item.Content, Interrupted: item.Interrupted,
			})
		case trajectory.KindToolProposal:
			proposal := *item.ToolCall
			proposal.Arguments = slices.Clone(proposal.Arguments)
			result.ToolProposals = append(result.ToolProposals, proposal)
		case trajectory.KindToolCall:
			call := *item.ToolCall
			call.Arguments = slices.Clone(call.Arguments)
			result.ToolCalls = append(result.ToolCalls, call)
		}
	}
	result.TrajectorySHA256 = hashSnapshot(snapshot)
	result.ScoreFailures = score(task, result)
	result.Passed = len(result.ScoreFailures) == 0 && result.Error == ""
	return result
}

// NewTaskToolSet creates the deterministic records.read registry for one
// validated task. It is exported so end-to-end audio benchmarks can exercise
// the same tool authority and fixtures as the text-only intelligence study.
func NewTaskToolSet(task Task) (*agenttool.Registry, error) {
	if err := ValidateTasks([]Task{task}); err != nil {
		return nil, err
	}
	registry := agenttool.NewRegistry(nil)
	records := make(map[string]json.RawMessage, len(task.Records))
	for key, value := range task.Records {
		records[key] = slices.Clone(value)
	}
	err := registry.Register(agenttool.Definition{
		Tool: continuation.ToolDefinition{
			Name: "records.read", Description: "Read one immutable benchmark record by its exact key.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"key":{"type":"string","description":"Exact record key from the user request."}},"required":["key"],"additionalProperties":false}`),
		},
		Capability: continuation.Capability{
			Name: "records.read", Description: "Read immutable task records by exact key.",
			Available: true, ExecutionPhase: "slow",
		},
		ReadOnly: true,
	}, func(_ context.Context, call trajectory.ToolCall) (any, error) {
		var arguments struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
			return nil, err
		}
		value, exists := records[arguments.Key]
		if !exists {
			return nil, fmt.Errorf("record %q does not exist", arguments.Key)
		}
		return json.RawMessage(slices.Clone(value)), nil
	})
	return registry, err
}

type timedProvider struct {
	provider continuation.Provider
	origin   time.Time
	clock    func() time.Time
	mu       sync.Mutex
	timings  []InvocationTiming
}

func newTimedProvider(provider continuation.Provider, origin time.Time, clock func() time.Time) *timedProvider {
	return &timedProvider{provider: provider, origin: origin, clock: clock}
}

func (provider *timedProvider) Descriptor() continuation.Descriptor {
	return provider.provider.Descriptor()
}

func (provider *timedProvider) Continue(ctx context.Context, request continuation.Request, emit continuation.Emit) (continuation.Completion, error) {
	started := provider.clock()
	var first *time.Time
	completion, err := provider.provider.Continue(ctx, request, func(event continuation.Event) error {
		if first == nil {
			value := provider.clock()
			first = &value
		}
		return emit(event)
	})
	ended := provider.clock()
	timing := InvocationTiming{
		Phase: request.Descriptor.Phase, StartOffsetMS: milliseconds(started.Sub(provider.origin)),
		DurationMS: milliseconds(ended.Sub(started)), Usage: completion.Usage,
		StopReason: completion.StopReason,
	}
	if first != nil {
		value := milliseconds(first.Sub(started))
		timing.FirstEventMS = &value
	}
	if err != nil {
		timing.Error = err.Error()
	}
	provider.mu.Lock()
	timing.Ordinal = len(provider.timings) + 1
	provider.timings = append(provider.timings, timing)
	provider.mu.Unlock()
	return completion, err
}

func (provider *timedProvider) Timings() []InvocationTiming {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]InvocationTiming(nil), provider.timings...)
}

func score(task Task, result TaskResult) []string {
	answer := ""
	for index := len(result.Assistant) - 1; index >= 0; index-- {
		if !result.Assistant[index].Interrupted && strings.TrimSpace(result.Assistant[index].Text) != "" {
			answer = normalize(result.Assistant[index].Text)
			break
		}
	}
	var failures []string
	for _, expected := range task.ExpectedSubstrings {
		if !strings.Contains(answer, normalize(expected)) {
			failures = append(failures, fmt.Sprintf("final answer does not contain %q", expected))
		}
	}
	failures = append(failures, benchspec.ScoreToolTrajectory(
		task.RequiredToolCalls, task.ExpectedToolCalls,
		result.ToolCalls, result.ToolResults, task.AllowToolErrors,
	)...)
	return failures
}

func hashSnapshot(snapshot trajectory.Snapshot) string {
	encoded, _ := json.Marshal(snapshot)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func cloneToolResults(results []trajectory.ToolResult) []trajectory.ToolResult {
	clone := append([]trajectory.ToolResult(nil), results...)
	for index := range clone {
		clone[index].Output = slices.Clone(clone[index].Output)
	}
	return clone
}

func normalize(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration.Microseconds()) / 1_000
}
