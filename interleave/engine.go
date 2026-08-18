// Package interleave runs a fast model and a slow model as successive compute
// phases over one canonical trajectory.
//
// The reference policy is intentionally small: invoke fast once, invoke slow
// unconditionally, and continue the slow phase only when it emits tool calls.
// There is no difficulty classifier, keyword router, or separate slow-advice
// channel.
package interleave

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	defaultMaxSlowInvocations = 8
	// DefaultFastInstruction is exported so pre-endpoint preparation can hash
	// and invoke exactly the same model-visible policy as canonical RunFast.
	DefaultFastInstruction = "Produce one self-contained spoken micro-turn that makes truthful immediate progress as the low-latency first continuation of this agent. Keep assistant content to one short sentence of at most twelve words; do not enumerate questions. You know the complete tool schemas but only have proposal authority: a proposed call is working state for the slow continuation and cannot execute. If the request depends on information or an action not already evidenced in the trajectory, emit the appropriate tool proposal. Any assistant content before it may acknowledge what the agent is checking, but must not state an unknown result or claim completion."
	// DefaultSlowInstruction is the ordinary continuation policy used after the
	// fast safe point and after every authoritative tool result.
	DefaultSlowInstruction = "Continue the same agent trajectory with careful reasoning and independently resolve the latest user request. Treat fast assistant content and tool proposals as provisional working state, not as proof that the task is complete or correct. Do not repeat an already adequate fast segment; append only the missing answer, action, or explicit correction. Only your tool calls have execution authority. Preserve user-supplied literal identifiers exactly; a tool error is authoritative, so do not guess spelling variants. Use available tools when needed and incorporate their results. For spoken output, give the shortest complete answer that preserves every required fact, confirmation, and correction."
)

// ToolCatalog is the capability surface visible to both continuations. Tool
// execution may live in this process or in an external asynchronous
// orchestrator; visibility never implies execution authority.
type ToolCatalog interface {
	Capabilities() []continuation.Capability
	Tools() []continuation.ToolDefinition
}

// ToolSet adds an in-process executor to a ToolCatalog. The fast continuation
// receives the same definitions with proposal-only authority; only slow output
// reaches Execute.
type ToolSet interface {
	ToolCatalog
	Execute(context.Context, trajectory.ToolCall) trajectory.ToolResult
}

// StreamEvent identifies the phase that produced one ordinary continuation
// event. It is internal runtime data, not an OpenAI Realtime wire event.
type StreamEvent struct {
	Phase      trajectory.Phase
	Descriptor continuation.Descriptor
	Event      continuation.Event
}

// StreamObserver may send assistant deltas to speculative TTS and record
// provider timing. Returning an error stops the rollout.
type StreamObserver func(StreamEvent) error

// Config supplies immutable engine dependencies and safety bounds.
type Config struct {
	Store               *trajectory.Store
	FastProvider        continuation.Provider
	SlowProvider        continuation.Provider
	Tools               ToolSet
	ToolCatalog         ToolCatalog
	Capabilities        []continuation.Capability
	AgentInstruction    string
	FastInstruction     string
	SlowInstruction     string
	FastMaxOutputTokens int
	SlowMaxOutputTokens int
	MaxSlowInvocations  int
	RetainReasoning     bool
	SlowContextPolicy   SlowContextPolicy
	Now                 func() uint64
	NextID              func(prefix string) string
}

// Engine owns one canonical fast/slow rollout policy.
type Engine struct {
	store        *trajectory.Store
	fast         continuation.Provider
	slow         continuation.Provider
	tools        ToolSet
	catalog      ToolCatalog
	capabilities []continuation.Capability
	fastPrompt   string
	slowPrompt   string
	fastTokens   int
	slowTokens   int
	maxSlow      int
	now          func() uint64
	nextID       func(string) string
	runner       *continuation.Runner
	slowProject  continuation.TrajectoryProjection
}

// New creates an engine and rejects phase or tool-authority configurations
// that would undermine the fast/slow contract.
func New(config Config) (*Engine, error) {
	if config.Store == nil {
		return nil, errors.New("interleave engine requires a trajectory store")
	}
	if config.FastProvider == nil || config.SlowProvider == nil {
		return nil, errors.New("interleave engine requires fast and slow providers")
	}
	if config.Tools != nil && config.ToolCatalog != nil {
		return nil, errors.New("configure either an in-process tool set or an external tool catalog, not both")
	}
	catalog := config.ToolCatalog
	if catalog == nil {
		catalog = config.Tools
	}
	fastDescriptor := config.FastProvider.Descriptor()
	slowDescriptor := config.SlowProvider.Descriptor()
	if err := continuation.ValidateDescriptor(fastDescriptor); err != nil {
		return nil, fmt.Errorf("invalid fast provider: %w", err)
	}
	if err := continuation.ValidateDescriptor(slowDescriptor); err != nil {
		return nil, fmt.Errorf("invalid slow provider: %w", err)
	}
	if fastDescriptor.Phase != trajectory.PhaseFast {
		return nil, errors.New("fast provider must declare the fast phase")
	}
	if fastDescriptor.EffectiveToolAuthority() == continuation.ToolAuthorityExecute {
		return nil, errors.New("fast provider must not have executable-tool authority")
	}
	if slowDescriptor.Phase != trajectory.PhaseSlow {
		return nil, errors.New("slow provider must declare the slow phase")
	}
	if catalog != nil && fastDescriptor.EffectiveToolAuthority() != continuation.ToolAuthorityPropose {
		return nil, errors.New("fast provider must have proposal authority when a tool set is configured")
	}
	if catalog != nil && slowDescriptor.EffectiveToolAuthority() != continuation.ToolAuthorityExecute {
		return nil, errors.New("slow provider must permit executable tools when a tool set is configured")
	}
	if config.FastMaxOutputTokens < 0 || config.SlowMaxOutputTokens < 0 {
		return nil, errors.New("output token limits cannot be negative")
	}
	if config.MaxSlowInvocations < 0 {
		return nil, errors.New("maximum slow invocations cannot be negative")
	}
	if config.MaxSlowInvocations == 0 {
		config.MaxSlowInvocations = defaultMaxSlowInvocations
	}
	if config.FastInstruction == "" {
		config.FastInstruction = DefaultFastInstruction
	}
	if config.SlowInstruction == "" {
		config.SlowInstruction = DefaultSlowInstruction
	}
	if config.SlowContextPolicy == "" {
		config.SlowContextPolicy = SlowContextCanonical
	}
	fastPrompt := composeInstruction(config.AgentInstruction, config.FastInstruction)
	slowPrompt := composeInstruction(config.AgentInstruction, config.SlowInstruction)
	if config.Now == nil {
		origin := time.Now()
		config.Now = func() uint64 { return uint64(time.Since(origin)) }
	}
	if config.NextID == nil {
		var counter atomic.Uint64
		config.NextID = func(prefix string) string {
			return prefix + "-" + strconv.FormatUint(counter.Add(1), 10)
		}
	}
	capabilities := append([]continuation.Capability(nil), config.Capabilities...)
	if catalog != nil {
		capabilities = append(capabilities, catalog.Capabilities()...)
	}
	if err := validateCapabilities(capabilities); err != nil {
		return nil, err
	}
	runner, err := continuation.NewRunner(continuation.RunnerConfig{
		Store: config.Store, Now: config.Now, NextID: config.NextID,
		RetainReasoning: config.RetainReasoning,
	})
	if err != nil {
		return nil, err
	}
	slowProject, err := SlowContextProjection(config.SlowContextPolicy)
	if err != nil {
		return nil, err
	}
	return &Engine{
		store: config.Store, fast: config.FastProvider, slow: config.SlowProvider,
		tools: config.Tools, catalog: catalog, capabilities: capabilities,
		fastPrompt: fastPrompt, slowPrompt: slowPrompt,
		fastTokens: config.FastMaxOutputTokens, slowTokens: config.SlowMaxOutputTokens,
		maxSlow: config.MaxSlowInvocations, now: config.Now, nextID: config.NextID,
		runner: runner, slowProject: slowProject,
	}, nil
}

func composeInstruction(agentInstruction, phaseInstruction string) string {
	if strings.TrimSpace(agentInstruction) == "" {
		return phaseInstruction
	}
	return agentInstruction + "\n\n" + phaseInstruction
}

// Request binds one rollout to the latest perception revision already present
// in the trajectory.
type Request struct {
	SourceRevision uint64
}

// Result records every phase and tool outcome appended during one rollout.
type Result struct {
	Fast         continuation.RunResult   `json:"fast"`
	Slow         []continuation.RunResult `json:"slow"`
	ToolResults  []trajectory.ToolResult  `json:"tool_results,omitempty"`
	FastFailed   bool                     `json:"fast_failed,omitempty"`
	StartVersion uint64                   `json:"start_version"`
	EndVersion   uint64                   `json:"end_version"`
}

// SlowResult contains the higher-reasoning continuation and any tool outcomes
// appended after a fast phase. It exists so an audio runtime can begin
// synthesizing completed fast content while slow reasoning proceeds.
type SlowResult struct {
	Runs        []continuation.RunResult `json:"runs"`
	ToolResults []trajectory.ToolResult  `json:"tool_results,omitempty"`
}

// RunFast appends exactly one low-latency continuation. The fast provider sees
// the complete capability and tool definitions, but its descriptor ensures any
// emitted call is appended as a non-executable proposal.
func (engine *Engine) RunFast(ctx context.Context, request Request, observer StreamObserver) (continuation.RunResult, error) {
	if err := engine.requireTrajectory(); err != nil {
		return continuation.RunResult{}, err
	}
	result, observerErr, providerErr := engine.runFast(ctx, request, observer)
	if observerErr != nil {
		return result, observerErr
	}
	return result, providerErr
}

// RunSlow always starts the higher-reasoning continuation from the trajectory
// as it exists at call time. Tool calls execute at terminal model safe points,
// their results are appended, and slow continuation resumes until no tool call
// remains or the configured safety bound is reached.
func (engine *Engine) RunSlow(ctx context.Context, request Request, observer StreamObserver) (SlowResult, error) {
	if err := engine.requireTrajectory(); err != nil {
		return SlowResult{}, err
	}
	result, observerErr, runErr := engine.runSlow(ctx, request, observer)
	return result, errors.Join(runErr, observerErr)
}

// RunSlowStep performs exactly one higher-reasoning continuation and stops at
// its terminal safe point. If it emits calls, an external orchestrator can
// execute them asynchronously, append one complete result batch with
// AppendToolResults, and invoke RunSlowStep again. No placeholder or second
// agent state is introduced.
func (engine *Engine) RunSlowStep(ctx context.Context, request Request, observer StreamObserver) (continuation.RunResult, error) {
	if err := engine.requireTrajectory(); err != nil {
		return continuation.RunResult{}, err
	}
	if count := slowInvocationCount(engine.store.Snapshot(), request.SourceRevision); count >= engine.maxSlow {
		return continuation.RunResult{}, fmt.Errorf("slow continuation exceeded %d invocation safety limit", engine.maxSlow)
	}
	result, observerErr, runErr := engine.runSlowOnce(ctx, request, observer)
	return result, errors.Join(runErr, observerErr)
}

// slowInvocationCount derives the external-resumption budget from canonical
// state rather than a process-local workflow flag. A freshly constructed
// processor therefore enforces the same bound after an exact result batch.
func slowInvocationCount(snapshot trajectory.Snapshot, sourceRevision uint64) int {
	start := 0
	for index, item := range snapshot.Items {
		if item.Kind == trajectory.KindObservation && item.SourceRevision == sourceRevision {
			start = index + 1
		}
	}
	invocations := make(map[string]struct{})
	for _, item := range snapshot.Items[start:] {
		if item.SourceRevision != sourceRevision || item.Producer.Phase != trajectory.PhaseSlow || item.InvocationID == "" {
			continue
		}
		invocations[item.InvocationID] = struct{}{}
	}
	return len(invocations)
}

// AppendToolResults commits the complete outstanding call set from one slow
// invocation as a single version-checked transaction. Results may arrive in
// any order; canonical order follows the model's call order. Partial batches,
// duplicates, identity changes, and stale-prefix commits are rejected.
func (engine *Engine) AppendToolResults(invocationID string, results []trajectory.ToolResult) error {
	snapshot := engine.store.Snapshot()
	matched, err := trajectory.MatchToolResultBatch(snapshot, invocationID, results)
	if err != nil {
		return err
	}

	items := make([]trajectory.Item, 0, len(matched))
	parentID := snapshot.Items[len(snapshot.Items)-1].ID
	for _, pair := range matched {
		parents := []string{parentID}
		if pair.Pending.ItemID != parentID {
			parents = append(parents, pair.Pending.ItemID)
		}
		result := pair.Result
		item := trajectory.Item{
			ID: engine.nextID("tool-result"), Kind: trajectory.KindToolResult,
			MonotonicNS: engine.now(), CausalParentIDs: parents,
			SourceRevision: pair.Pending.SourceRevision, InvocationID: invocationID,
			Producer: trajectory.Producer{Phase: trajectory.PhaseTool}, ToolResult: &result,
		}
		items = append(items, item)
		parentID = item.ID
	}
	if err := engine.store.AppendBatchAt(snapshot.Version, items); err != nil {
		return fmt.Errorf("commit tool result batch for invocation %q: %w", invocationID, err)
	}
	return nil
}

// Run executes one fast continuation, then always starts slow continuation.
// A fast provider failure does not suppress the slow fallback unless the
// context or observer cancelled the rollout. Slow tool calls are executed and
// appended before the next slow continuation.
func (engine *Engine) Run(ctx context.Context, request Request, observer StreamObserver) (Result, error) {
	start := engine.store.Snapshot()
	if len(start.Items) == 0 {
		return Result{}, errors.New("interleave rollout requires an observation or prior trajectory item")
	}
	result := Result{StartVersion: start.Version}
	var failures []error

	fast, fastObserverErr, fastErr := engine.runFast(ctx, request, observer)
	result.Fast = fast
	if fastObserverErr != nil {
		result.EndVersion = engine.store.Snapshot().Version
		return result, fastObserverErr
	}
	if fastErr != nil {
		result.FastFailed = true
		failures = append(failures, fmt.Errorf("fast continuation: %w", fastErr))
		if ctx.Err() != nil {
			result.EndVersion = engine.store.Snapshot().Version
			return result, errors.Join(failures...)
		}
	}

	slow, slowObserverErr, slowErr := engine.runSlow(ctx, request, observer)
	result.Slow = slow.Runs
	result.ToolResults = slow.ToolResults
	if slowErr != nil {
		failures = append(failures, slowErr)
	}
	if slowObserverErr != nil {
		failures = append(failures, slowObserverErr)
	}
	result.EndVersion = engine.store.Snapshot().Version
	return result, errors.Join(failures...)
}

func (engine *Engine) runFast(ctx context.Context, request Request, observer StreamObserver) (continuation.RunResult, error, error) {
	fastObserver, fastObserverError := phaseObserver(trajectory.PhaseFast, engine.fast.Descriptor(), observer)
	result, providerErr := engine.runner.Run(ctx, engine.fast, continuation.Invocation{
		Instruction: engine.fastPrompt, SourceRevision: request.SourceRevision,
		Capabilities: cloneCapabilities(engine.capabilities), Tools: engine.fastToolDefinitions(),
		MaxOutputTokens: engine.fastTokens,
	}, fastObserver)
	return result, fastObserverError(), providerErr
}

func (engine *Engine) runSlow(ctx context.Context, request Request, observer StreamObserver) (SlowResult, error, error) {
	var result SlowResult
	var failures []error
	for invocation := 0; invocation < engine.maxSlow; invocation++ {
		slow, slowObserverErr, slowErr := engine.runSlowOnce(ctx, request, observer)
		result.Runs = append(result.Runs, slow)
		if slowObserverErr != nil {
			return result, slowObserverErr, errors.Join(failures...)
		}
		if slowErr != nil {
			if errors.Is(slowErr, continuation.ErrPreempted) && ctx.Err() == nil {
				if invocation == engine.maxSlow-1 {
					failures = append(failures, fmt.Errorf("slow continuation exceeded %d invocation safety limit after resource preemption", engine.maxSlow))
				}
				continue
			}
			failures = append(failures, fmt.Errorf("slow continuation %d: %w", invocation+1, slowErr))
			break
		}
		if len(slow.ToolCalls) == 0 {
			break
		}
		if engine.tools == nil {
			// An external orchestrator owns these calls. Stop at the safe point;
			// AppendToolResults followed by RunSlowStep resumes the same trajectory.
			break
		}
		toolResults := make([]trajectory.ToolResult, 0, len(slow.ToolCalls))
		for _, call := range slow.ToolCalls {
			toolResult := engine.tools.Execute(ctx, call)
			if toolResult.CallID != call.CallID || toolResult.Name != call.Name {
				failures = append(failures, fmt.Errorf("tool %q returned mismatched identity", call.Name))
				return result, nil, errors.Join(failures...)
			}
			toolResults = append(toolResults, cloneToolResult(toolResult))
		}
		if err := engine.AppendToolResults(slow.InvocationID, toolResults); err != nil {
			failures = append(failures, err)
			break
		}
		result.ToolResults = append(result.ToolResults, toolResults...)
		if ctx.Err() != nil {
			failures = append(failures, ctx.Err())
			break
		}
		if invocation == engine.maxSlow-1 {
			failures = append(failures, fmt.Errorf("slow continuation exceeded %d invocation safety limit", engine.maxSlow))
		}
	}
	return result, nil, errors.Join(failures...)
}

func (engine *Engine) runSlowOnce(ctx context.Context, request Request, observer StreamObserver) (continuation.RunResult, error, error) {
	slowObserver, slowObserverError := phaseObserver(trajectory.PhaseSlow, engine.slow.Descriptor(), observer)
	invocation := continuation.Invocation{
		Instruction: engine.slowPrompt, SourceRevision: request.SourceRevision,
		Capabilities: cloneCapabilities(engine.capabilities), Tools: engine.toolDefinitions(),
		MaxOutputTokens: engine.slowTokens,
	}
	var result continuation.RunResult
	var runErr error
	if engine.slowProject == nil {
		result, runErr = engine.runner.Run(ctx, engine.slow, invocation, slowObserver)
	} else {
		result, runErr = engine.runner.RunProjected(ctx, engine.slow, invocation, engine.slowProject, slowObserver)
	}
	return result, slowObserverError(), runErr
}

func (engine *Engine) requireTrajectory() error {
	if len(engine.store.Snapshot().Items) == 0 {
		return errors.New("interleave rollout requires an observation or prior trajectory item")
	}
	return nil
}

func (engine *Engine) toolDefinitions() []continuation.ToolDefinition {
	if engine.catalog == nil {
		return nil
	}
	tools := engine.catalog.Tools()
	for index := range tools {
		tools[index].Parameters = slices.Clone(tools[index].Parameters)
	}
	return tools
}

func (engine *Engine) fastToolDefinitions() []continuation.ToolDefinition {
	if engine.fast.Descriptor().EffectiveToolAuthority() != continuation.ToolAuthorityPropose {
		return nil
	}
	return engine.toolDefinitions()
}

func phaseObserver(phase trajectory.Phase, descriptor continuation.Descriptor, observer StreamObserver) (continuation.StreamObserver, func() error) {
	var observedError error
	if observer == nil {
		return nil, func() error { return nil }
	}
	return func(event continuation.Event) error {
		if observedError != nil {
			return observedError
		}
		observedError = observer(StreamEvent{Phase: phase, Descriptor: descriptor, Event: event})
		return observedError
	}, func() error { return observedError }
}

func validateCapabilities(capabilities []continuation.Capability) error {
	seen := make(map[string]struct{}, len(capabilities))
	for index, capability := range capabilities {
		if capability.Name == "" || capability.Description == "" {
			return fmt.Errorf("interleave capability %d requires name and description", index)
		}
		if _, duplicate := seen[capability.Name]; duplicate {
			return fmt.Errorf("duplicate interleave capability %q", capability.Name)
		}
		seen[capability.Name] = struct{}{}
	}
	return nil
}

func cloneCapabilities(capabilities []continuation.Capability) []continuation.Capability {
	return append([]continuation.Capability(nil), capabilities...)
}

func cloneToolResult(result trajectory.ToolResult) trajectory.ToolResult {
	result.Output = append(json.RawMessage(nil), result.Output...)
	return result
}
