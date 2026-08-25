package cognition

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	// FastInstruction and SlowInstruction override the
	// shipped phase instructions. Empty selects the shipped text.
	FastInstruction string
	SlowInstruction string
	FastMaxTokens   int
	SlowMaxTokens   int
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
	config Config
	runner *continuation.Runner
	// promptMu guards the phase prompts, which change when a session's
	// instruction does.
	promptMu   sync.RWMutex
	fastPrompt string
	slowPrompt string
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
	if config.Catalog != nil {
		if err := validateCapabilities(config.Catalog.Capabilities()); err != nil {
			return nil, err
		}
	}
	runner, err := continuation.NewRunner(continuation.RunnerConfig{
		Store: config.Store, Now: config.Now, NextID: config.NextID,
		RetainReasoning: config.RetainReasoning, Media: config.Media,
	})
	if err != nil {
		return nil, err
	}
	return &Engine{
		config: config, runner: runner,
		fastPrompt: Compose(config.AgentInstruction, config.FastInstruction),
		slowPrompt: Compose(config.AgentInstruction, config.SlowInstruction),
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
	// Holding says this spoken turn exists because the reasoner is taking a
	// while, not because anything new arrived. Without it the voice reads a
	// trajectory in which it has already spoken and nothing has changed, and
	// the most natural thing to write is what it wrote last time.
	Holding bool
	// Standing are the interaction policies people set out loud and have not
	// lifted.
	//
	// They reach cognition as well as the interaction model because some of
	// them govern what is said and not only when. Asked to count out loud as
	// somebody names animals, an interaction model that fires at exactly the
	// right moments still produces nonsense if the voice does not know what it
	// was called for: it reads a conversation, sees it is expected to say
	// something, and says "one, two, three, four, five" every time.
	Standing []string
	// Interjecting says the turn does not belong to the agent - it is speaking
	// while somebody else keeps the floor. What that calls for is the shortest
	// thing that serves, not a reply.
	Interjecting bool
	// Because names the act this turn exists to carry out.
	//
	// The decision layer knows why it called: a standing policy's condition
	// was met, or something said needs correcting now. The voice was told only
	// that it was interjecting and had to guess which - so, called to correct
	// a date it had been given, it said "got it", and called to count an
	// animal it had already counted one of, it said nothing at all. Both are
	// reasonable answers to "say something short"; neither is an answer to the
	// question that was actually asked.
	Because string
	// Heard is what the current speaker has said so far in an utterance that
	// has not been committed yet.
	//
	// Without it the three models do not see the same conversation, and the
	// difference is not a detail of resolution. Interaction decides on
	// partials; cognition reads the committed log. So a turn triggered by
	// something in a partial reaches the voice with that something missing -
	// it is asked to speak and cannot see the sentence that asked it to.
	// Told to count animals as they are mentioned, it counted from one to ten,
	// because the animal was in a partial and the instruction was all it had.
	Heard string
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
		Instruction: engine.instruction(engine.prompt(trajectory.PhaseFast), request), SourceRevision: request.SourceRevision,
		Capabilities:    engine.capabilityManifest(),
		MaxOutputTokens: engine.config.FastMaxTokens,
	}, observer, request.Heard)
}

// PrepareFast generates a fast continuation before the endpoint, against a
// provisional observation that is not in the log.
//
// The result appends nothing. It has no speech sink and no tool authority
// until Adopt commits it, which is what makes speculating on evidence that is
// still changing a latency decision rather than a correctness one.
func (engine *Engine) PrepareFast(
	ctx context.Context, request Request, provisional trajectory.Item,
) (*continuation.Prepared, error) {
	if engine.config.ExternalFast {
		return nil, ErrExternalFast
	}
	return engine.runner.Prepare(ctx, engine.config.Fast, continuation.Invocation{
		Instruction: engine.instruction(engine.prompt(trajectory.PhaseFast), request), SourceRevision: request.SourceRevision,
		Capabilities:    engine.capabilityManifest(),
		MaxOutputTokens: engine.config.FastMaxTokens,
	}, provisional, nil)
}

// PrepareSlow generates a slow continuation before the endpoint.
//
// Its tool calls are prepared and uncommitted like everything else, so a
// speculation that turns out to answer a sentence the user never finished
// dispatches nothing. Only adoption puts a call into the log, and only a call
// in the log has execution authority.
func (engine *Engine) PrepareSlow(
	ctx context.Context, request Request, provisional trajectory.Item,
) (*continuation.Prepared, error) {
	return engine.runner.Prepare(ctx, engine.config.Slow, continuation.Invocation{
		Instruction: engine.instruction(engine.prompt(trajectory.PhaseSlow), request), SourceRevision: request.SourceRevision,
		Capabilities: engine.capabilityManifest(), Tools: engine.executableTools(),
		MaxOutputTokens: engine.config.SlowMaxTokens,
	}, provisional, nil)
}

// Adopt commits prepared output at a real safe point.
func (engine *Engine) Adopt(prepared *continuation.Prepared) (continuation.RunResult, error) {
	return engine.runner.Adopt(prepared)
}

// RunSlow performs exactly one higher-reasoning continuation and stops at its
// terminal safe point. If it emitted calls, the action plane executes them and
// their results are appended before the next call.
func (engine *Engine) RunSlow(ctx context.Context, request Request, observer StreamObserver) (continuation.RunResult, error) {
	return engine.run(ctx, engine.config.Slow, trajectory.PhaseSlow, continuation.Invocation{
		Instruction: engine.instruction(engine.prompt(trajectory.PhaseSlow), request), SourceRevision: request.SourceRevision,
		Capabilities: engine.capabilityManifest(), Tools: engine.executableTools(),
		MaxOutputTokens: engine.config.SlowMaxTokens,
	}, observer, request.Heard)
}

func (engine *Engine) instruction(prompt string, request Request) string {
	return Instruct(prompt, request)
}

// Instruct renders what a request adds to a phase prompt.
//
// It is a function rather than a method because it uses nothing from the
// engine, and because what the voice is told is the thing most worth asserting
// about and the least visible from outside: a policy that fails to reach it
// looks exactly like a model choosing to ignore one.
func Instruct(prompt string, request Request) string {
	// Standing policies come first among the injected ones because they
	// outrank the rest: somebody asked for this out loud, and the others are
	// the runtime describing its own state.
	if len(request.Standing) > 0 {
		prompt += "\n\n" + StandingInstruction + "\n- " + strings.Join(request.Standing, "\n- ")
	}
	if strings.TrimSpace(request.Heard) != "" {
		prompt += "\n\n" + HeardInstruction + " \"" + strings.TrimSpace(request.Heard) + "\""
	}
	if request.Interjecting {
		prompt += "\n\n" + InterjectingInstruction
	}
	if reason := becauseInstruction(request.Because); reason != "" {
		prompt += "\n\n" + reason
	}
	if request.PendingRepair {
		prompt += "\n\n" + RepairInstruction
	}
	if request.Holding {
		prompt += "\n\n" + HoldingInstruction
	}
	return prompt
}

func (engine *Engine) run(
	ctx context.Context,
	provider continuation.Provider,
	phase trajectory.Phase,
	invocation continuation.Invocation,
	observer StreamObserver,
	live string,
) (continuation.RunResult, error) {
	// Something to speak from. An empty log used to be the whole test, which
	// was right while every turn began with a committed observation - and
	// wrong once a turn could begin with an utterance still in progress. An
	// interjection fires on a partial, so early in a session the log is
	// genuinely empty and the sentence that triggered it is in the request
	// rather than in the store. Refusing that is refusing the only turn that
	// had anything to say.
	if len(engine.config.Store.Snapshot().Items) == 0 && strings.TrimSpace(live) == "" {
		return continuation.RunResult{}, errors.New("a continuation requires an observation, a prior trajectory item, or an utterance in progress")
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
	// The live utterance is shown to the provider as an uncommitted
	// observation rather than kept in the instruction alone. It was the guard
	// above and nothing else, which left every interjection asking a provider
	// to continue from the agent's own last turn with nothing new addressed to
	// it - and a provider asked that says nothing at all.
	result, err := engine.runner.RunLive(ctx, provider, invocation, live, stream)
	return result, errors.Join(err, observerErr)
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

// capabilityManifest is what the agent can do, read when it is asked rather
// than when the engine was built.
//
// A client declares its tools in session.update, which arrives after the
// runtime for that session exists. Capturing the manifest at construction
// therefore froze it empty for every client-driven session, while the tool
// definitions beside it were always read live - so the phase that executes
// knew about the tools and the phase that talks to the user did not. That is
// the exact case the manifest's own wording exists to prevent: denying a
// capability the agent has because another phase is the one that runs it.
func (engine *Engine) capabilityManifest() []continuation.Capability {
	if engine.config.Catalog == nil {
		return nil
	}
	return engine.config.Catalog.Capabilities()
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

// becauseInstruction says what the turn was called for.
//
// It is a small vocabulary on purpose. The acts are the runtime's own and
// there are seven of them; only the two that mean "say something into a turn
// that is not yours" need explaining, because those are the two where the
// voice cannot work out from the conversation alone what it is for.
func becauseInstruction(act string) string {
	switch act {
	case "speak-through":
		return "You are speaking because something the person asked to be told about has just happened. Do that thing now, for the occurrence in front of you: if they asked for a count, say the next number; if they asked for a translation, give the English; if they asked to be told when something lands, say it has landed. Say only that."
	case "interrupt":
		return "You are cutting into their sentence because what they are saying needs correcting now, and waiting until they finish would make the correction useless. Say the correction itself - the right date, the right figure, the right name - not that you are listening and not a question about it."
	}
	return ""
}

// prompt returns the phase prompt as it stands now.
func (engine *Engine) prompt(phase trajectory.Phase) string {
	engine.promptMu.RLock()
	defer engine.promptMu.RUnlock()
	if phase == trajectory.PhaseSlow {
		return engine.slowPrompt
	}
	return engine.fastPrompt
}

// SetAgentInstruction replaces the deployment's own instruction.
//
// A session's instruction arrives after the session exists - every client
// sends it in session.update, including the official ones - and the prompts
// were composed once at construction from whatever was known then. So the
// voice never saw it. The interaction model did, because it reads the current
// settings on every decision, which is precisely the split that showed up in
// measurement: told a deadline was the third, the decision layer correctly cut
// in to correct a wrong date and the voice invented one, having never been
// told what the right one was.
func (engine *Engine) SetAgentInstruction(instruction string) {
	engine.promptMu.Lock()
	defer engine.promptMu.Unlock()
	engine.fastPrompt = Compose(instruction, engine.config.FastInstruction)
	engine.slowPrompt = Compose(instruction, engine.config.SlowInstruction)
}
