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
// Visibility is not authority. Both providers see the capability manifest,
// slow sees the complete executable schema set, and fast sees only the exact
// schemas an operator explicitly admitted to its bounded execution lane.
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
	// executable-tool authority unless FastToolFilter is configured.
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
	// FastToolFilter is the deployment-owned allowlist for tools the fast
	// provider may execute. Nil preserves proposal-only fast cognition. The
	// filter is evaluated against the live catalog for every invocation so
	// session-declared tools added after engine construction are handled without
	// weakening the boundary.
	//
	// This is one half of a two-key configuration: Fast must also declare
	// ToolAuthorityExecute. Each fast invocation must additionally set
	// Request.AllowFastTools; otherwise no executable schemas are attached and
	// any emitted call is downgraded to a proposal by the runner.
	FastToolFilter func(continuation.ToolDefinition) bool
	// SlowToolFilter narrows the executable catalog for the slow provider. Nil
	// preserves the default that slow owns every tool. A multimodal binding can
	// use it to keep visually grounded effectors on a direct-vision lane when
	// its slow reasoner cannot see; prompt text is not an authority boundary.
	SlowToolFilter func(continuation.ToolDefinition) bool
	// VisualReflex is an optional, silent visual action role. It is separate
	// from Fast so enabling vision never changes the voice model, its prompt,
	// or its authority. The controller gives it a compact current-frame
	// projection and one bounded act/wait/abstain decision.
	VisualReflex *VisualReflexConfig
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
	// reflexMu makes per-session modality updates able to add or remove the
	// optional controller without rebuilding voice cognition.
	reflexMu sync.RWMutex
	reflex   *visualReflex
	// promptMu guards the phase prompts, which change when a session's
	// instruction does.
	promptMu    sync.RWMutex
	agentPrompt string
	fastPrompt  string
	slowPrompt  string
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
	fastExecutes := config.FastToolFilter != nil
	if fast.EffectiveToolAuthority() == continuation.ToolAuthorityExecute && !fastExecutes {
		return nil, errors.New("fast provider execution authority requires an explicit tool filter")
	}
	if fastExecutes && fast.EffectiveToolAuthority() != continuation.ToolAuthorityExecute {
		return nil, errors.New("a fast executable-tool filter requires fast execution authority")
	}
	if fastExecutes && config.Catalog == nil {
		return nil, errors.New("a fast executable-tool filter requires a tool catalog")
	}
	if config.RequireSilentSlow && slow.EffectiveSpeechAuthority() != continuation.SpeechAuthoritySilent {
		return nil, errors.New("slow provider must be configured silent: its output is voiced by a fast continuation")
	}
	if config.Catalog != nil {
		if !config.ExternalFast && !fastExecutes && fast.EffectiveToolAuthority() != continuation.ToolAuthorityPropose {
			return nil, errors.New("fast provider needs proposal authority when tools are declared without a fast execution allowlist")
		}
		if slow.EffectiveToolAuthority() != continuation.ToolAuthorityExecute {
			return nil, errors.New("slow provider needs execution authority when tools are declared")
		}
	}
	if config.FastMaxTokens < 0 || config.SlowMaxTokens < 0 {
		return nil, errors.New("output token limits cannot be negative")
	}
	// Zero means no ceiling, and it is the default. A spoken turn is kept
	// short by the instruction that asks for a spoken turn, not by cutting one
	// off part-way: measured uncapped, the fast phase answers in eighteen to
	// thirty-two tokens on its own. A ceiling only ever produced a broken
	// utterance, and on a provider that thinks it produced silence, because
	// the thinking is spent against the same allowance before any speech.
	if config.SlowMaxTokens == 0 {
		config.SlowMaxTokens = 2048
	}
	if config.FastInstruction == "" {
		config.FastInstruction = FastInstruction
	}
	if fastExecutes {
		config.FastInstruction = Compose(config.FastInstruction, FastActionInstruction)
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
	reflex, err := newVisualReflex(config.VisualReflex, config.Catalog, runner)
	if err != nil {
		return nil, err
	}
	return &Engine{
		config: config, runner: runner, reflex: reflex,
		agentPrompt: config.AgentInstruction,
		fastPrompt:  Compose(config.AgentInstruction, config.FastInstruction),
		slowPrompt:  Compose(config.AgentInstruction, config.SlowInstruction),
	}, nil
}

// ErrExternalFast means the caller asked this engine to run a continuation
// whose provider lives outside it. The binding that declared the fast provider
// external owns the voice, and running one here would produce a second.
var ErrExternalFast = errors.New("the fast provider is external to this engine")

// Request binds one continuation to the perception revision it answers.
type Request struct {
	SourceRevision uint64
	// ToolResult says this continuation was opened by an authoritative action
	// result. The slow lane may legitimately add no prose after consuming that
	// result; the runtime still needs this typed cause so it can wake the voice
	// to render the result already present in the canonical trajectory.
	ToolResult bool
	// VisualIntentID identifies the user utterance that armed a visual action.
	// It is runtime control state, not prompt content. The action plane uses it
	// to join decisions made from a live ASR partial, its committed final text,
	// and later retained frames without suppressing a genuinely new request to
	// revisit the same coordinate.
	VisualIntentID string
	// VisualTask is the current user task reconstructed across adjacent ASR
	// fragments. The visual projection remains direct pixels plus compact typed
	// state; this text prevents a recognizer-created tail such as "and begin
	// presenting" from being treated as independent coordinate authority.
	VisualTask string
	// NextVisualAction is the controller-parsed current action chunk. After one
	// or more ordered actions succeed, the visual provider receives this clause
	// as its temporary user turn instead of the full sequence, while VisualTask
	// remains in the instruction for ordering and continuation state. This keeps
	// an unchanged screen from drawing the actor back to the first visible
	// control in a request such as "open the review, then share your screen".
	NextVisualAction string
	// CurrentVisualCallIDs names computer calls dispatched for VisualIntentID.
	// The visual projection uses it to retain only same-intent effect memory.
	// Calls from an earlier correction target, and rejected model candidates
	// that never crossed the effect boundary, must not become few-shot examples
	// whose stripped private fields override the current action schema.
	CurrentVisualCallIDs []string
	// VisualUnstable is the recognizer's provisional tail for a live visual
	// decision. It lets the direct-pixel actor distinguish a complete named
	// target from an ASR prefix such as "over" that may become "Overview" on
	// the next 200 ms micro-turn, without reducing the image to narration.
	VisualUnstable string
	// CompletedVisualActions is the number of successful bounded action chunks
	// already carried out for this visual intent. It is supplied only on a
	// fresh observer-frame replan, so it cannot authorize a second action from
	// the stale image that selected the first one.
	CompletedVisualActions int
	// AllowFastTools opens the configured fast-tool allowlist for this one
	// invocation. The binding sets it only for a committed observation (or a
	// preparation that can be adopted only by that same observation), never
	// for holding speech, interjections, or narration of background results.
	AllowFastTools bool
	// FastBackgroundToolsOnly narrows an otherwise eligible fast invocation to
	// background-safe tools. A separate visual controller uses this when it
	// owns the current explicit screen task, preventing the speaking provider
	// from becoming a second coordinate controller while still allowing the
	// semantic half of a composite request to start asynchronous work.
	FastBackgroundToolsOnly bool
	// PendingRepair injects the repair obligation instruction. It is runtime
	// policy derived from typed trajectory state, never from text.
	PendingRepair bool
	// Holding says this spoken turn exists because the reasoner is taking a
	// while, not because anything new arrived. Without it the voice reads a
	// trajectory in which it has already spoken and nothing has changed, and
	// the most natural thing to write is what it wrote last time.
	Holding bool
	// Silent says this request is for a phase that is never heard, which
	// changes how the policies people set out loud are introduced to it.
	Silent bool
	// Counting says a policy in force asks for a running count, which is the
	// one kind of policy the voice needs different guidance for. It is read
	// from the policy when it is pinned, by the pass that reads the policy.
	Counting bool
	// Setting says the speech this turn is answering is the speech a policy
	// was read out of. A policy is not in force for the sentence that set it:
	// "count the animals out loud as I mention them" mentions no animal, and
	// carrying it out there leaves the count wrong by one for the rest of the
	// conversation.
	Setting bool
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
	// ImmediateNonvisual is a high-confidence semantic clause that the
	// interaction controller separated from a future visual condition. It is
	// typed decomposition state, not generated screen narration: pixels remain
	// on the visual actor path, while the voice learns exactly which independent
	// transcript clause is due now.
	ImmediateNonvisual string
	// Observed is something the runtime noticed that nobody said - so far, a
	// stretch of silence long enough to meet a policy that was waiting for
	// one. It reaches the provider the same way a live utterance does, because
	// a turn with nothing new in front of it produces nothing.
	Observed string
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
	// Answered is how much of what they are saying the agent has already
	// spoken for.
	//
	// Without it the agent answers one stretch of speech twice. It speaks into
	// a sentence while the person is still saying it, the sentence commits a
	// moment later, and to the turn that runs on the commit it is a line in
	// the conversation like any other with nothing to say it has been dealt
	// with. Measured under a counting policy, every line of a story got two
	// numbers - "one two" at a sentence with no animal in it, "three four" at
	// the one with the capybara.
	//
	// The runtime is what knows this, and knowing it is not the same as being
	// able to act on it: whether what remains contains another occurrence is a
	// judgement about content, and a sentence that commits whole can carry a
	// second animal after the point the agent spoke. So the fact is handed
	// over and the judgement is left where it belongs.
	Answered string
	// InFlight names authoritative tool work that has started and has no result
	// yet. It prevents another cognition lane from approximating or repeating
	// the same request while preserving independent obligations in a composite
	// turn.
	InFlight []string
}

// Descriptors reports the configured providers, for evidence and health.
func (engine *Engine) Descriptors() (fast, slow continuation.Descriptor) {
	return engine.config.Fast.Descriptor(), engine.config.Slow.Descriptor()
}

// VisualReflexDescriptor reports the optional visual action provider.
func (engine *Engine) VisualReflexDescriptor() (continuation.Descriptor, bool) {
	engine.reflexMu.RLock()
	defer engine.reflexMu.RUnlock()
	if engine.reflex == nil {
		return continuation.Descriptor{}, false
	}
	return engine.reflex.provider.Descriptor(), true
}

// ConfigureVisualReflex adds, replaces, or removes the optional controller.
// It changes no voice/slow engine configuration and is safe between session
// observer updates and an in-flight decision.
func (engine *Engine) ConfigureVisualReflex(config *VisualReflexConfig) error {
	reflex, err := newVisualReflex(config, engine.config.Catalog, engine.runner)
	if err != nil {
		return err
	}
	engine.reflexMu.Lock()
	engine.reflex = reflex
	engine.reflexMu.Unlock()
	return nil
}

// RunFast appends one low-latency continuation. The fast provider sees the
// complete capability manifest and, only at an eligible safe point, schemas
// carrying either its proposal-only lane or its exact executable allowlist.
func (engine *Engine) RunFast(ctx context.Context, request Request, observer StreamObserver) (continuation.RunResult, error) {
	if engine.config.ExternalFast {
		return continuation.RunResult{}, ErrExternalFast
	}
	return engine.run(
		ctx, engine.config.Fast, trajectory.PhaseFast, engine.fastInvocation(request), observer, request.live(),
	)
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
	return engine.runner.Prepare(ctx, engine.config.Fast, engine.fastInvocation(request), provisional, nil)
}

// fastInvocation adds proposal guidance only when this exact safe point
// carries proposal schemas. Session tools arrive after engine construction,
// while tool-free voice sessions must not pay for an irrelevant action prompt
// on every turn, so neither a static prompt nor catalog presence alone is the
// right condition.
func (engine *Engine) fastInvocation(request Request) continuation.Invocation {
	tools := engine.fastTools(request.AllowFastTools, request.FastBackgroundToolsOnly)
	capabilities := engine.capabilityManifest()
	instruction := engine.instruction(engine.prompt(trajectory.PhaseFast), request)
	if len(tools) > 0 && engine.config.Fast.Descriptor().EffectiveToolAuthority() == continuation.ToolAuthorityPropose {
		instruction = Compose(instruction, FastProposalInstruction)
	}
	if len(capabilities) > 0 {
		// The ownership rule governs both ordinary clarification and tool
		// proposals, so it is the most specific instruction and comes last.
		instruction = Compose(instruction, FastClarificationInstruction)
	}
	return continuation.Invocation{
		Instruction: instruction, SourceRevision: request.SourceRevision,
		Capabilities: capabilities, Tools: tools,
		MaxOutputTokens: engine.config.FastMaxTokens,
	}
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
	request.Silent = true
	return engine.runner.Prepare(ctx, engine.config.Slow, engine.slowInvocation(request), provisional, nil)
}

// Adopt commits prepared output at a real safe point.
func (engine *Engine) Adopt(prepared *continuation.Prepared) (continuation.RunResult, error) {
	return engine.runner.Adopt(prepared)
}

// RunSlow performs exactly one higher-reasoning continuation and stops at its
// terminal safe point. If it emitted calls, the action plane executes them and
// their results are appended before the next call.
func (engine *Engine) RunSlow(ctx context.Context, request Request, observer StreamObserver) (continuation.RunResult, error) {
	// The reasoner is never heard, and the policies people set out loud read
	// as instructions to whoever is speaking.
	request.Silent = true
	return engine.run(
		ctx, engine.config.Slow, trajectory.PhaseSlow, engine.slowInvocation(request), observer, request.live(),
	)
}

// slowInvocation attaches action-planning guidance only when this invocation
// has an action surface. Tool-free help and factual turns keep the exact base
// reasoning prompt; a live tool declaration gets both its schemas and the
// rules for resolving their prerequisites in the same provider request.
func (engine *Engine) slowInvocation(request Request) continuation.Invocation {
	tools := engine.executableTools()
	instruction := engine.instruction(engine.prompt(trajectory.PhaseSlow), request)
	if len(tools) > 0 {
		instruction = Compose(instruction, SlowToolPrerequisiteInstruction)
	}
	return continuation.Invocation{
		Instruction: instruction, SourceRevision: request.SourceRevision,
		Capabilities: engine.capabilityManifest(), Tools: tools,
		MaxOutputTokens: engine.config.SlowMaxTokens,
	}
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
	//
	// A phase that is never heard is told so here, because the policies are
	// written as instructions to a speaker and a phase told to say something
	// says it. Measured: with "count the animals out loud" in force the
	// reasoner wrote "0", which reached the conversation and became the number
	// every later count was measured from.
	if len(request.Standing) > 0 {
		preamble := StandingInstruction
		if request.Silent {
			preamble = SilentStandingInstruction
		}
		prompt += "\n\n" + preamble + "\n- " + strings.Join(request.Standing, "\n- ")
		// How to carry one out goes with the policies rather than with the
		// act, because the agent speaks on ordinary turns too and the rules
		// are the same ones there. A phase that is never heard needs none of
		// it: it is not the one carrying them out.
		// Setting suppresses the carrying-out instruction on the speech that
		// set a policy. It must not suppress a turn that is answering
		// something else: the visual case sets its policy in one line and then
		// the person goes quiet, so "they have not said anything past it" is
		// true for the rest of the conversation, and the build finished with
		// the agent never told there was anything to report. What ends the
		// setting moment there is not more speech, it is the thing the policy
		// was watching for happening.
		if !request.Silent && !(request.Setting && strings.TrimSpace(request.Observed) == "") {
			prompt += "\n\n" + CarryingOutInstruction
			if request.Counting {
				prompt += "\n\n" + CountingInstruction
			}
		}
	}
	if strings.TrimSpace(request.Heard) != "" {
		prompt += "\n\n" + HeardInstruction + " \"" + strings.TrimSpace(request.Heard) + "\""
	}
	if answered := strings.TrimSpace(request.Answered); answered != "" {
		prompt += "\n\n" + AnsweredInstruction + " \"" + answered + "\""
	}
	if len(request.InFlight) > 0 {
		prompt += "\n\nWork already in flight: " + strings.Join(request.InFlight, ", ") +
			". Do not repeat or approximate that work with a different tool or screen control. " +
			"A separate explicit obligation in the latest request may still be handled."
	}
	if task := strings.TrimSpace(request.VisualTask); task != "" {
		prompt += "\n\nCurrent user task across recognition fragments, oldest to newest: \"" + task +
			"\". Later corrections override earlier directions. This reconstruction supplies intent only; ground every effect in the current images."
	}
	if unstable := strings.TrimSpace(request.VisualUnstable); unstable != "" {
		prompt += "\n\nThe trailing recognition text \"" + unstable +
			"\" is provisional ASR output. It may be an unfinished word. Never autocomplete it into a visible label or destination. " +
			"It grants action authority only if the provisional words already exactly name the visible requested control; otherwise WAIT for the next micro-turn."
	}
	if request.CompletedVisualActions > 0 {
		prompt += fmt.Sprintf(
			"\n\nCompleted visual action chunks for this user request: %d. Each succeeded. Do not repeat or restart them; "+
				"on this fresh frame, advance to the next explicitly requested control in the user's order. "+
				"Earlier coordinate arguments are intentionally omitted: ground new x/y values from the current pixels instead of copying the prior call.",
			request.CompletedVisualActions,
		)
	}
	if observed := strings.TrimSpace(request.Observed); observed != "" {
		prompt += "\n\n" + ObservedInstruction + " " + observed
	}
	if request.Interjecting {
		prompt += "\n\n" + InterjectingInstruction
	}
	if reason := becauseInstruction(request.Because, request.Counting); reason != "" {
		prompt += "\n\n" + reason
	}
	if immediate := strings.TrimSpace(request.ImmediateNonvisual); immediate != "" {
		prompt += "\n\nThe interaction controller has separated the immediate nonvisual clause due now: \"" +
			immediate + "\". Speak the actual content requested by that clause now. This turn is not future-only, so " +
			WaitToken + " is not a valid response. Do not announce the monitor or the screen action."
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
	return engine.placeholderCalls(snapshot, pending, reason)
}

// PlaceholderCalls closes only the named committed calls that did not cross
// the action boundary. Visual target validation uses it after rejecting a
// model-authored coordinate: leaving that call unresolved would make a later,
// correctly grounded retry at the same coordinate look already in flight.
// Selecting by call ID avoids disturbing independent slow/background work.
func (engine *Engine) PlaceholderCalls(
	calls []trajectory.ToolCall, reason string,
) ([]trajectory.ToolPlaceholder, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	wanted := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		if id := strings.TrimSpace(call.CallID); id != "" {
			wanted[id] = struct{}{}
		}
	}
	snapshot := engine.config.Store.Snapshot()
	pending := trajectory.UnresolvedToolCalls(snapshot)
	pending = slices.DeleteFunc(pending, func(call trajectory.PendingToolCall) bool {
		_, keep := wanted[call.Call.CallID]
		return !keep
	})
	return engine.placeholderCalls(snapshot, pending, reason)
}

func (engine *Engine) placeholderCalls(
	snapshot trajectory.Snapshot, pending []trajectory.PendingToolCall, reason string,
) ([]trajectory.ToolPlaceholder, error) {
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
	var tools []continuation.ToolDefinition
	for _, tool := range engine.config.Catalog.Tools() {
		if engine.config.SlowToolFilter != nil && !engine.config.SlowToolFilter(tool) {
			continue
		}
		tool.Parameters = slices.Clone(tool.Parameters)
		tools = append(tools, tool)
	}
	return tools
}

// fastTools opens one of two typed lanes at an eligible user-observation safe
// point. Proposal authority sees the live catalog so action intent can be
// emitted structurally, but the runner records every such call as
// non-executable. Execute authority sees only the immutable exact allowlist.
//
// Reading the catalog live matters because client session updates can replace
// declarations after the engine was built. A removed or renamed declaration
// disappears from the invocation and therefore cannot be proposed or execute.
func (engine *Engine) fastTools(
	allowedAtSafePoint, backgroundOnly bool,
) []continuation.ToolDefinition {
	if !allowedAtSafePoint || engine.config.Catalog == nil {
		return nil
	}
	var result []continuation.ToolDefinition
	for _, tool := range engine.config.Catalog.Tools() {
		if engine.config.FastToolFilter != nil && !engine.config.FastToolFilter(tool) {
			continue
		}
		if backgroundOnly && !tool.Background {
			continue
		}
		tool.Parameters = slices.Clone(tool.Parameters)
		result = append(result, tool)
	}
	return result
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
func becauseInstruction(act string, counting bool) string {
	switch act {
	case "speak-through":
		// Only what this act is, now. How to carry a policy out lives with the
		// policies, because the agent speaks on ordinary turns too and the
		// rules do not change with the act.
		return "You are speaking because something the person asked to be told about has just happened. " +
			"Do exactly what their standing policy asks, for the occurrence in front of you, and say " +
			"only that. They have not finished talking and are not handing you the floor."
	case "act-silently":
		// Measured on a phone menu: the key was pressed correctly and then
		// announced out loud - "I have pressed two to select the order status
		// option" - to a recording, which cannot hear it and is still talking
		// over the announcement. The act is the answer; saying it as well
		// spends a turn describing what was already done.
		return "Nothing you write on this turn is heard by anybody. Do whatever the situation actually calls for - call a tool if there is one that applies, and nothing if there is not - and reply with " + WaitToken + ". Whoever is talking is still talking, and words aimed at them would be wasted. At a recorded menu, a key applies only when the current words contain both its digit and an option label matching the user's earlier goal. A greeting, a digit alone, or a different option means call no tool and reply " + WaitToken + "; never press the first key merely because it was named first."
	case "resume nonvisual work after silent visual branch":
		return "A silent visual worker has completed or deferred its bounded screen-action branch. Separate the latest request clause by clause, using the deployment instruction as authoritative when speech recognition has removed commas or sentence boundaries. An imperative outside a future condition is due now even when a later clause begins with if, when, or once: in 'Present the overview, and if an alert appears acknowledge it', begin the actual overview now; only acknowledging the alert is conditional. A deployment instruction to present while watching or monitoring establishes exactly that decomposition even if the transcript arrives as 'present the overview if an alert appears acknowledge it'. Handle the independent nonvisual obligation now while the visual worker continues monitoring. Speak its actual content, not a promise to do it and not a statement about monitoring. Do not repeat, extend, or describe the screen action. If every requested act is inside the future condition, or the request only asked for silent monitoring or a silent screen action, reply with " + WaitToken + " and nothing else. Never announce that you are monitoring, waiting, or about to click."
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

// visualPrompt composes the live deployment/session contract with the narrow
// visual role. The visual actor owns the pixels and the effect, so omitting the
// session contract here gives the least informed cognition lane the final
// authority. Session instructions arrive after construction and are updated
// under the same lock as the voice and slow prompts.
func (engine *Engine) visualPrompt(role string) string {
	engine.promptMu.RLock()
	defer engine.promptMu.RUnlock()
	return Compose(engine.agentPrompt, role)
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
	engine.agentPrompt = instruction
	engine.fastPrompt = Compose(instruction, engine.config.FastInstruction)
	engine.slowPrompt = Compose(instruction, engine.config.SlowInstruction)
}

// live is what the provider is given to continue from when the trajectory
// ends with the agent's own last words: an utterance still being spoken, or
// something the runtime noticed that nobody said.
func (request Request) live() string {
	if heard := strings.TrimSpace(request.Heard); heard != "" {
		return heard
	}
	return strings.TrimSpace(request.Observed)
}
