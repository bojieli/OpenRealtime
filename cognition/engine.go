package cognition

import (
	"context"
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

// Catalog is the capability surface visible to both providers.
//
// Visibility is not authority: the fast provider sees the same tool schemas as
// the slow one so it can say which capability a request needs, and its
// descriptor is what makes any call it emits a non-executable proposal.
type Catalog interface {
	Capabilities() []continuation.Capability
	Tools() []continuation.ToolDefinition
}

// StreamEvent identifies which provider produced one streamed event.
type StreamEvent struct {
	Phase      trajectory.Phase
	Descriptor continuation.Descriptor
	Event      continuation.Event
}

// StreamObserver receives streamed provider output. Returning an error stops
// the continuation at its next safe point.
type StreamObserver func(StreamEvent) error

// Config supplies the engine's immutable dependencies.
type Config struct {
	Store *trajectory.Store
	// Fast is the voice. It must declare the fast phase and must not hold
	// executable-tool authority.
	Fast continuation.Provider
	// Slow is the brain. It must declare the slow phase and, when the
	// arrangement pairs the two, must be silent.
	Slow continuation.Provider
	// Catalog supplies capabilities and tool definitions. Tool execution
	// happens in the action plane, never here.
	Catalog Catalog
	// AgentInstruction is the deployment's own instruction, composed ahead of
	// each phase instruction.
	AgentInstruction string
	// FastInstruction, SlowInstruction, and VoiceInstruction override the
	// shipped phase instructions. Empty selects the shipped text.
	FastInstruction  string
	SlowInstruction  string
	VoiceInstruction string
	FastMaxTokens    int
	SlowMaxTokens    int
	// RequireSilentSlow enforces the second cognition boundary at
	// construction. A single-provider arrangement sets it false.
	RequireSilentSlow bool
	// ExternalFast declares that the fast provider lives outside this engine -
	// a remote Realtime endpoint, or a model that owns its own voice. The
	// engine then refuses to run fast or voicing continuations rather than
	// producing a second voice, and it does not check a fast tool authority
	// that no continuation here will ever exercise.
	ExternalFast    bool
	RetainReasoning bool
	// Media resolves attachments an observer retained, for providers that can
	// see. Without it a model gets the narration and nothing else, which is
	// enough to reason about a screen and not enough to click on one.
	Media  continuation.MediaResolver
	Now    func() uint64
	NextID func(prefix string) string
}

// Engine runs continuations over one canonical trajectory.
type Engine struct {
	config       Config
	runner       *continuation.Runner
	capabilities []continuation.Capability
	fastPrompt   string
	slowPrompt   string
	voicePrompt  string
}

// New validates the provider arrangement and creates an engine.
//
// The two boundaries are checked here, once, rather than at every call site.
// A misconfigured arrangement fails to start instead of running with a
// silently corrected provider, because a runtime that quietly muted a provider
// would be running something other than what it was configured with.
func New(config Config) (*Engine, error) {
	if config.Store == nil {
		return nil, errors.New("cognition requires a trajectory store")
	}
	if config.Fast == nil || config.Slow == nil {
		return nil, errors.New("cognition requires fast and slow providers")
	}
	fast, slow := config.Fast.Descriptor(), config.Slow.Descriptor()
	if err := continuation.ValidateDescriptor(fast); err != nil {
		return nil, fmt.Errorf("invalid fast provider: %w", err)
	}
	if err := continuation.ValidateDescriptor(slow); err != nil {
		return nil, fmt.Errorf("invalid slow provider: %w", err)
	}
	if fast.Phase != trajectory.PhaseFast {
		return nil, errors.New("fast provider must declare the fast phase")
	}
	if slow.Phase != trajectory.PhaseSlow {
		return nil, errors.New("slow provider must declare the slow phase")
	}
	if fast.EffectiveToolAuthority() == continuation.ToolAuthorityExecute {
		return nil, errors.New("fast provider must not have executable-tool authority")
	}
	if config.RequireSilentSlow && slow.EffectiveSpeechAuthority() != continuation.SpeechAuthoritySilent {
		return nil, errors.New("slow provider must be configured silent: its output is voiced by a fast continuation")
	}
	if config.Catalog != nil {
		if !config.ExternalFast && fast.EffectiveToolAuthority() != continuation.ToolAuthorityPropose {
			return nil, errors.New("fast provider needs proposal authority when tools are declared")
		}
		if slow.EffectiveToolAuthority() != continuation.ToolAuthorityExecute {
			return nil, errors.New("slow provider needs execution authority when tools are declared")
		}
	}
	if config.FastMaxTokens < 0 || config.SlowMaxTokens < 0 {
		return nil, errors.New("output token limits cannot be negative")
	}
	if config.FastMaxTokens == 0 {
		config.FastMaxTokens = 96
	}
	if config.SlowMaxTokens == 0 {
		config.SlowMaxTokens = 2048
	}
	if config.FastInstruction == "" {
		config.FastInstruction = FastInstruction
	}
	if config.SlowInstruction == "" {
		config.SlowInstruction = SlowInstruction
	}
	if config.VoiceInstruction == "" {
		config.VoiceInstruction = VoiceInstruction
	}
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
	capabilities := []continuation.Capability(nil)
	if config.Catalog != nil {
		capabilities = config.Catalog.Capabilities()
	}
	if err := validateCapabilities(capabilities); err != nil {
		return nil, err
	}
	runner, err := continuation.NewRunner(continuation.RunnerConfig{
		Store: config.Store, Now: config.Now, NextID: config.NextID,
		RetainReasoning: config.RetainReasoning, Media: config.Media,
	})
	if err != nil {
		return nil, err
	}
	return &Engine{
		config: config, runner: runner, capabilities: capabilities,
		fastPrompt:  Compose(config.AgentInstruction, config.FastInstruction),
		slowPrompt:  Compose(config.AgentInstruction, config.SlowInstruction),
		voicePrompt: Compose(config.AgentInstruction, config.VoiceInstruction),
	}, nil
}

// ErrExternalFast means the caller asked this engine to run a continuation
// whose provider lives outside it. The binding that declared the fast provider
// external owns the voice, and running one here would produce a second.
var ErrExternalFast = errors.New("the fast provider is external to this engine")

// Request binds one continuation to the perception revision it answers.
type Request struct {
	SourceRevision uint64
	// PendingRepair injects the repair obligation instruction. It is runtime
	// policy derived from typed trajectory state, never from text.
	PendingRepair bool
}

// Descriptors reports the configured providers, for evidence and health.
func (engine *Engine) Descriptors() (fast, slow continuation.Descriptor) {
	return engine.config.Fast.Descriptor(), engine.config.Slow.Descriptor()
}

// RunFast appends one low-latency continuation. The fast provider sees the
// complete capability and tool definitions; its descriptor is what makes any
// call it emits a non-executable proposal.
func (engine *Engine) RunFast(ctx context.Context, request Request, observer StreamObserver) (continuation.RunResult, error) {
	if engine.config.ExternalFast {
		return continuation.RunResult{}, ErrExternalFast
	}
	return engine.run(ctx, engine.config.Fast, trajectory.PhaseFast, continuation.Invocation{
		Instruction: engine.instruction(engine.fastPrompt, request), SourceRevision: request.SourceRevision,
		Capabilities: slices.Clone(engine.capabilities), Tools: engine.proposalTools(),
		MaxOutputTokens: engine.config.FastMaxTokens,
	}, observer)
}

// RunVoice runs the fast provider over what slow has already committed. It is
// the step that exists because slow cannot speak.
func (engine *Engine) RunVoice(ctx context.Context, request Request, observer StreamObserver) (continuation.RunResult, error) {
	if engine.config.ExternalFast {
		return continuation.RunResult{}, ErrExternalFast
	}
	return engine.run(ctx, engine.config.Fast, trajectory.PhaseFast, continuation.Invocation{
		Instruction: engine.instruction(engine.voicePrompt, request), SourceRevision: request.SourceRevision,
		Capabilities: slices.Clone(engine.capabilities),
		// The voicing step gets no tool definitions at all. It is not deciding
		// anything; it is saying what was decided.
		MaxOutputTokens: engine.config.FastMaxTokens,
	}, observer)
}

// RunSlow performs exactly one higher-reasoning continuation and stops at its
// terminal safe point. If it emitted calls, the action plane executes them and
// their results are appended before the next call.
func (engine *Engine) RunSlow(ctx context.Context, request Request, observer StreamObserver) (continuation.RunResult, error) {
	return engine.run(ctx, engine.config.Slow, trajectory.PhaseSlow, continuation.Invocation{
		Instruction: engine.instruction(engine.slowPrompt, request), SourceRevision: request.SourceRevision,
		Capabilities: slices.Clone(engine.capabilities), Tools: engine.executableTools(),
		MaxOutputTokens: engine.config.SlowMaxTokens,
	}, observer)
}

func (engine *Engine) instruction(prompt string, request Request) string {
	if !request.PendingRepair {
		return prompt
	}
	return prompt + "\n\n" + RepairInstruction
}

func (engine *Engine) run(
	ctx context.Context,
	provider continuation.Provider,
	phase trajectory.Phase,
	invocation continuation.Invocation,
	observer StreamObserver,
) (continuation.RunResult, error) {
	if len(engine.config.Store.Snapshot().Items) == 0 {
		return continuation.RunResult{}, errors.New("a continuation requires an observation or prior trajectory item")
	}
	descriptor := provider.Descriptor()
	var observerErr error
	var stream continuation.StreamObserver
	if observer != nil {
		stream = func(event continuation.Event) error {
			if observerErr != nil {
				return observerErr
			}
			observerErr = observer(StreamEvent{Phase: phase, Descriptor: descriptor, Event: event})
			return observerErr
		}
	}
	result, err := engine.runner.Run(ctx, provider, invocation, stream)
	return result, errors.Join(err, observerErr)
}

// AppendToolResults commits one invocation's complete outstanding call set as
// a single version-checked transaction. Results may arrive in any order;
// canonical order follows the model's call order.
func (engine *Engine) AppendToolResults(invocationID string, results []trajectory.ToolResult) error {
	snapshot := engine.config.Store.Snapshot()
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
			ID: engine.config.NextID("tool-result"), Kind: trajectory.KindToolResult,
			MonotonicNS: engine.config.Now(), CausalParentIDs: parents,
			SourceRevision: pair.Pending.SourceRevision, InvocationID: invocationID,
			Producer: trajectory.Producer{Phase: trajectory.PhaseTool}, ToolResult: &result,
		}
		items = append(items, item)
		parentID = item.ID
	}
	if err := engine.config.Store.AppendBatchAt(snapshot.Version, items); err != nil {
		return fmt.Errorf("commit tool result batch for invocation %q: %w", invocationID, err)
	}
	return nil
}

// PlaceholderForInterrupted records a placeholder for every executable call
// left unresolved, so an interrupted prefix stays well-formed.
func (engine *Engine) PlaceholderForInterrupted(reason string) ([]trajectory.ToolPlaceholder, error) {
	snapshot := engine.config.Store.Snapshot()
	pending := trajectory.UnresolvedToolCalls(snapshot)
	if len(pending) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(reason) == "" {
		reason = "interrupted"
	}
	items := make([]trajectory.Item, 0, len(pending))
	placeholders := make([]trajectory.ToolPlaceholder, 0, len(pending))
	parentID := snapshot.Items[len(snapshot.Items)-1].ID
	for _, call := range pending {
		placeholder := trajectory.ToolPlaceholder{
			CallID: call.Call.CallID, Name: call.Call.Name, Reason: reason,
		}
		parents := []string{parentID}
		if call.ItemID != parentID {
			parents = append(parents, call.ItemID)
		}
		item := trajectory.Item{
			ID: engine.config.NextID("tool-placeholder"), Kind: trajectory.KindToolPlaceholder,
			MonotonicNS: engine.config.Now(), CausalParentIDs: parents,
			SourceRevision: call.SourceRevision, InvocationID: call.InvocationID,
			Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, ToolPlaceholder: &placeholder,
		}
		items = append(items, item)
		placeholders = append(placeholders, placeholder)
		parentID = item.ID
	}
	if err := engine.config.Store.AppendBatchAt(snapshot.Version, items); err != nil {
		return nil, fmt.Errorf("commit tool placeholders: %w", err)
	}
	return placeholders, nil
}

// SlowInvocations counts the slow continuations already run for a revision. It
// is derived from canonical state rather than a process-local counter, so a
// freshly constructed engine enforces the same bound after a restart.
func (engine *Engine) SlowInvocations(sourceRevision uint64) int {
	snapshot := engine.config.Store.Snapshot()
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

func (engine *Engine) executableTools() []continuation.ToolDefinition {
	if engine.config.Catalog == nil {
		return nil
	}
	tools := engine.config.Catalog.Tools()
	for index := range tools {
		tools[index].Parameters = slices.Clone(tools[index].Parameters)
	}
	return tools
}

func (engine *Engine) proposalTools() []continuation.ToolDefinition {
	if engine.config.Fast.Descriptor().EffectiveToolAuthority() != continuation.ToolAuthorityPropose {
		return nil
	}
	return engine.executableTools()
}

func validateCapabilities(capabilities []continuation.Capability) error {
	seen := make(map[string]struct{}, len(capabilities))
	for index, capability := range capabilities {
		if capability.Name == "" || capability.Description == "" {
			return fmt.Errorf("capability %d requires a name and description", index)
		}
		if _, duplicate := seen[capability.Name]; duplicate {
			return fmt.Errorf("duplicate capability %q", capability.Name)
		}
		seen[capability.Name] = struct{}{}
	}
	return nil
}
