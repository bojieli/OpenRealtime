package continuation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bojieli/OpenRealtime/trajectory"
)

// StreamObserver receives validated deltas before the safe-point append. A
// speech planner can use assistant deltas for speculative TTS while the
// trajectory remains authoritative at completion.
type StreamObserver func(Event) error

// TrajectoryProjection derives model-visible experimental context from an
// immutable canonical prefix. It cannot change the store or the version used
// for the eventual compare-and-append commit. Production uses nil (the exact
// canonical prefix); non-nil projections exist only for registered controls.
type TrajectoryProjection func(trajectory.Snapshot) (trajectory.Snapshot, error)

// Runner coordinates one provider invocation and atomically appends its output
// at the next safe point.
type Runner struct {
	store           *trajectory.Store
	now             func() uint64
	nextID          func(string) string
	retainReasoning bool
	media           MediaResolver
}

// RunnerConfig supplies dependencies. Now must return monotonic nanoseconds.
// RetainReasoning controls plaintext reasoning only; opaque provider state may
// still be retained to preserve authenticated same-provider continuation.
type RunnerConfig struct {
	Store           *trajectory.Store
	Now             func() uint64
	NextID          func(prefix string) string
	RetainReasoning bool
	// Media resolves attachments for adapters that can use them.
	Media MediaResolver
}

// NewRunner creates an experimental continuation runner.
func NewRunner(config RunnerConfig) (*Runner, error) {
	if config.Store == nil {
		return nil, errors.New("continuation runner requires a trajectory store")
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
	return &Runner{
		store: config.Store, now: config.Now, nextID: config.NextID,
		retainReasoning: config.RetainReasoning, media: config.Media,
	}, nil
}

// RunResult identifies the items appended for one continuation.
type RunResult struct {
	InvocationID   string                `json:"invocation_id"`
	SourceRevision uint64                `json:"source_revision,omitempty"`
	StartVersion   uint64                `json:"start_version"`
	EndVersion     uint64                `json:"end_version"`
	AppendedIDs    []string              `json:"appended_ids"`
	AssistantText  string                `json:"assistant_text,omitempty"`
	ToolProposals  []trajectory.ToolCall `json:"tool_proposals,omitempty"`
	ToolCalls      []trajectory.ToolCall `json:"tool_calls,omitempty"`
	Completion     Completion            `json:"completion"`
	Committed      bool                  `json:"committed"`
	Interrupted    bool                  `json:"interrupted,omitempty"`
	// Finished reports that this continuation declared the turn complete. The
	// marker that carried it is stripped before any item is built, so it is
	// visible here and nowhere else.
	Finished bool `json:"finished,omitempty"`
}

type bufferedSegment struct {
	kind     EventKind
	text     strings.Builder
	toolCall *trajectory.ToolCall
	// undeclared marks a call naming a tool this invocation did not offer. It
	// is recorded, never executed.
	undeclared bool
}

// Run invokes provider, forwards validated stream events to observer, and
// appends completed or interrupted output. If provider returns an error after
// producing partial output, the partial output is retained with Interrupted.
func (runner *Runner) Run(
	ctx context.Context,
	provider Provider,
	invocation Invocation,
	observer StreamObserver,
) (RunResult, error) {
	return runner.run(ctx, provider, invocation, nil, observer, nil, trajectory.Item{})
}

// RunLive invokes a provider over the canonical trajectory plus one utterance
// that is still being spoken, and commits its output like any other turn.
//
// Every act that speaks into somebody else's turn runs on a sentence that is
// not in the log yet, and putting that sentence only in the instruction leaves
// the conversation ending with whatever the agent last said. A provider asked
// to continue from its own last turn, with nothing new addressed to it, has
// nothing to continue: measured against Gemini 3.5 Flash, "count them as I
// mention them" with the animal in the instruction returned an empty string
// three times out of three, and the same call with the animal as a user turn
// returned "2" three times out of three. That is the whole of the second
// animal, the second sentence of an interpretation, and the dish that fits.
func (runner *Runner) RunLive(
	ctx context.Context,
	provider Provider,
	invocation Invocation,
	live string,
	observer StreamObserver,
) (RunResult, error) {
	if strings.TrimSpace(live) == "" {
		return runner.Run(ctx, provider, invocation, observer)
	}
	provisional := trajectory.Item{
		ID: runner.nextID("live"), Kind: trajectory.KindObservation,
		MonotonicNS: runner.now(), SourceRevision: invocation.SourceRevision,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: live,
	}
	return runner.run(ctx, provider, invocation, nil, observer, nil, provisional)
}

// RunProjected invokes a provider over an explicit experimental projection,
// while still committing against the exact canonical version captured before
// inference. A projection therefore cannot create an alternate memory owner.
func (runner *Runner) RunProjected(
	ctx context.Context,
	provider Provider,
	invocation Invocation,
	projection TrajectoryProjection,
	observer StreamObserver,
) (RunResult, error) {
	if projection == nil {
		return RunResult{}, errors.New("projected continuation requires a projection")
	}
	return runner.run(ctx, provider, invocation, projection, observer, nil, trajectory.Item{})
}

func (runner *Runner) run(
	ctx context.Context,
	provider Provider,
	invocation Invocation,
	projection TrajectoryProjection,
	observer StreamObserver,
	prepared *Prepared,
	provisional trajectory.Item,
) (RunResult, error) {
	if provider == nil {
		return RunResult{}, errors.New("continuation provider is required")
	}
	descriptor := provider.Descriptor()
	if err := ValidateDescriptor(descriptor); err != nil {
		return RunResult{}, err
	}
	invocation = cloneInvocation(invocation)
	if err := ValidateInvocation(invocation, descriptor); err != nil {
		return RunResult{}, err
	}

	invocationID := runner.nextID("invocation")
	before := runner.store.Snapshot()
	providerPrefix := before
	if projection != nil {
		projectionInput, err := runner.store.Prefix(before.Version)
		if err != nil {
			return RunResult{InvocationID: invocationID, StartVersion: before.Version}, fmt.Errorf("clone continuation prefix for projection: %w", err)
		}
		providerPrefix, err = projection(projectionInput)
		if err != nil {
			return RunResult{InvocationID: invocationID, StartVersion: before.Version}, fmt.Errorf("project continuation trajectory: %w", err)
		}
		if providerPrefix.Version != before.Version {
			return RunResult{InvocationID: invocationID, StartVersion: before.Version}, errors.New("trajectory projection changed the canonical version")
		}
		if err := validateProjection(before, providerPrefix); err != nil {
			return RunResult{InvocationID: invocationID, StartVersion: before.Version}, err
		}
		providerPrefix.Items = slices.Clone(providerPrefix.Items)
	}
	instruction := trajectory.Item{
		ID: runner.nextID("instruction"), Kind: trajectory.KindInstruction,
		MonotonicNS: runner.now(), Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime},
		SourceRevision: invocation.SourceRevision, InvocationID: invocationID,
		Content: invocation.Instruction,
	}
	if len(before.Items) > 0 {
		instruction.CausalParentIDs = []string{before.Items[len(before.Items)-1].ID}
	}
	// The instruction is visible to the provider as the next trajectory item,
	// but it is not published separately. Instruction and model output commit as
	// one version-checked safe-point transaction after generation completes.
	visibleInstruction := instruction
	if projection != nil {
		visibleInstruction.CausalParentIDs = nil
		if len(providerPrefix.Items) > 0 {
			visibleInstruction.CausalParentIDs = []string{providerPrefix.Items[len(providerPrefix.Items)-1].ID}
		}
	}
	prefix := providerPrefix
	if prepared != nil && prepared.provisional.ID != "" {
		provisional = prepared.provisional
	}
	if provisional.ID != "" {
		// The provisional observation is what makes preparation possible at
		// all: at this instant the user is still talking, so nothing about
		// this turn is in the canonical log yet. It is shown to the provider
		// and appended to nothing, and adoption later checks that the endpoint
		// said the same thing.
		//
		// A turn that speaks into somebody else's needs it for the same
		// reason and gets it the same way. Only the visible copy of the
		// instruction is reparented onto it, so nothing that commits refers to
		// an item that was never appended.
		prefix.Items = append(prefix.Items, provisional)
		prefix.Version++
		visibleInstruction.CausalParentIDs = []string{provisional.ID}
	}
	// One sentence is one thing somebody said, however many times the
	// recogniser committed it on the way - and the sentence still being spoken
	// is the fullest version of it, so the collapse runs after the provisional
	// is in place and lets it supersede the partials it continues.
	//
	// Applied here rather than in each adapter because it is true of every
	// provider and every phase, and projections are already permitted to drop
	// items: validateProjection forbids fabricating, duplicating and
	// reordering, and nothing else.
	prefix.Items = trajectory.WithoutSupersededPartials(prefix.Items)
	prefix.Items = append(prefix.Items, visibleInstruction)
	prefix.Version++
	request := Request{
		InvocationID: invocationID, Descriptor: descriptor,
		Trajectory: prefix, Invocation: invocation, Media: runner.media,
	}

	var segments []bufferedSegment
	declaredTools := make(map[string]struct{}, len(invocation.Tools))
	for _, tool := range invocation.Tools {
		declaredTools[tool.Name] = struct{}{}
	}
	emit := func(event Event) error {
		if err := ValidateEvent(event); err != nil {
			return err
		}
		if event.Kind == EventToolCall && descriptor.EffectiveToolAuthority() == ToolAuthorityNone {
			return errors.New("continuation emitted a tool call without proposal or execution authority")
		}
		// An undeclared name must never execute. It is still recorded, whatever
		// the provider's authority: the fast phase is offered no tools at all,
		// so every call it emits is undeclared by construction, and an
		// executing provider that hallucinates a name has made a mistake of
		// the same kind. Both become non-executable proposals, which the
		// dispatcher re-checks at the point of effect.
		//
		// Failing the invocation instead would end the session over a model's
		// spelling. Observed: a reasoner emitted "google_calendar.list_events?"
		// with the question mark attached, and another emitted a sentence from
		// its own instructions as a tool name. Neither could have executed, and
		// neither is a reason for the user to lose the conversation - a wrong
		// name is what a tool error is for, and the slow phase is told that a
		// tool error is authoritative.
		undeclared := false
		if event.Kind == EventToolCall {
			if _, declared := declaredTools[event.ToolCall.Name]; !declared {
				undeclared = true
			}
		}
		if observer != nil {
			if err := observer(event); err != nil {
				return err
			}
		}
		if event.Kind == EventToolCall {
			copy := *event.ToolCall
			copy.Arguments = append(json.RawMessage(nil), event.ToolCall.Arguments...)
			segments = append(segments, bufferedSegment{kind: event.Kind, toolCall: &copy, undeclared: undeclared})
			return nil
		}
		if len(segments) == 0 || segments[len(segments)-1].kind != event.Kind || segments[len(segments)-1].toolCall != nil {
			segments = append(segments, bufferedSegment{kind: event.Kind})
		}
		segments[len(segments)-1].text.WriteString(event.Text)
		return nil
	}

	completion, providerErr := provider.Continue(ctx, request, emit)
	if err := validateCompletion(completion, descriptor); err != nil && providerErr == nil {
		providerErr = err
	}
	interrupted := providerErr != nil || ctx.Err() != nil
	// The completion marker is control, not speech. It is removed here, before
	// items are built and before the assistant text is assembled, which is
	// what makes it impossible for it to reach the trajectory, the speech
	// commit boundary, or the user.
	finished := stripMarkers(segments)
	items := runner.buildItems(instruction, invocationID, descriptor, invocation, segments, completion, interrupted)
	result := RunResult{
		InvocationID: invocationID, SourceRevision: invocation.SourceRevision, StartVersion: before.Version,
		Completion: completion, Interrupted: interrupted, Finished: finished,
	}
	for _, segment := range segments {
		if segment.kind == EventAssistantDelta {
			result.AssistantText += segment.text.String()
		}
		if segment.toolCall != nil && !interrupted {
			// The same test the item kind uses. Reporting a proposal as a call
			// here would hand the caller something to execute that the log
			// says is not executable, which is the one disagreement this pair
			// must never have.
			if descriptor.EffectiveToolAuthority() == ToolAuthorityPropose || segment.undeclared {
				result.ToolProposals = append(result.ToolProposals, *segment.toolCall)
			} else {
				result.ToolCalls = append(result.ToolCalls, *segment.toolCall)
			}
		}
	}
	commitItems := make([]trajectory.Item, 0, len(items)+1)
	commitItems = append(commitItems, instruction)
	commitItems = append(commitItems, items...)
	if prepared != nil {
		// A prepared continuation stops here. Nothing has been appended, so it
		// has no speech sink and no tool authority until it is adopted at a
		// real safe point - which is exactly what makes it safe to be wrong
		// about, and what makes preparation a latency policy rather than a
		// correctness one.
		prepared.result = result
		prepared.items = commitItems
		prepared.baseVersion = before.Version
		return result, providerErr
	}
	if err := runner.commit(&result, before.Version, commitItems); err != nil {
		return result, errors.Join(providerErr, err)
	}
	return result, providerErr
}

// supersedesContinuation reports whether an item invalidates output derived
// from a prefix that predates it.
//
// New evidence does: the person said something else, or a repair obligation was
// raised, or a tool came back. A continuation that answered the previous
// question is answering a question nobody is asking any more, and committing it
// would put a stale answer in the log as though it were current.
//
// The agent's own output does not. A voice turn, its playback state, reasoning
// text, a non-executable proposal - none of them change what the reasoner
// relied on, and refusing its work because the voice filled a silence is
// exactly backwards: the arrangement exists so the voice can talk while the
// reasoner reasons, and a rule that punishes it for doing so removes the
// concurrency the whole design is for.
//
// One case this does not yet get right, and cannot reach today. The loop can
// classify an arriving question as a parallel branch, meaning it "can be
// handled without disturbing the work in flight" - and then the observation it
// carries supersedes that very work here. Both statements cannot be true. The
// loop's classification is the better authority, because it is the one that
// looked at whether the branch disturbs anything, so the predicate should take
// it rather than infer from the kind alone.
//
// It is unreachable while the loop has a single driver: a parallel branch
// cannot run beside a continuation, so no observation can arrive during one.
// Whoever makes that concurrent has to thread the classification through to
// here, or an unrelated question asked mid-deliberation will silently discard
// the deliberation.
func supersedesContinuation(item trajectory.Item) bool {
	switch item.Kind {
	case trajectory.KindObservation, trajectory.KindRepair, trajectory.KindToolResult:
		return true
	default:
		return false
	}
}

// commit appends one continuation's output as a transaction that is checked
// against what arrived while it was thinking, rather than against a version
// number that moves for reasons it does not care about.
func (runner *Runner) commit(result *RunResult, expectedVersion uint64, items []trajectory.Item) error {
	// Stamped as they enter, not as they were produced.
	//
	// A continuation is asked at one moment and commits at another, and with a
	// second producer running beside it those can interleave: the instruction
	// item of a reasoner that started six seconds ago carries a time from
	// before everything the voice has said since. The log is append-only and
	// its times exist to agree with its order, so entry time is the honest
	// one - production order is already recorded by the order of the items
	// themselves.
	entered := runner.now()
	for index := range items {
		if items[index].MonotonicNS < entered {
			items[index].MonotonicNS = entered
		}
	}
	if err := runner.store.AppendBatchAfter(expectedVersion, supersedesContinuation, items); err != nil {
		result.Interrupted = true
		result.ToolProposals = nil
		result.ToolCalls = nil
		result.EndVersion = runner.store.Snapshot().Version
		if errors.Is(err, trajectory.ErrVersionConflict) {
			err = errors.Join(ErrStalePrefix, err)
		}
		return fmt.Errorf("commit continuation safe point: %w", err)
	}
	result.Committed = true
	result.AppendedIDs = nil
	for _, item := range items {
		result.AppendedIDs = append(result.AppendedIDs, item.ID)
	}
	result.EndVersion = runner.store.Snapshot().Version
	return nil
}

func validateProjection(canonical, projected trajectory.Snapshot) error {
	canonicalByID := make(map[string]trajectory.Item, len(canonical.Items))
	canonicalOrder := make(map[string]int, len(canonical.Items))
	for index, item := range canonical.Items {
		canonicalByID[item.ID] = item
		canonicalOrder[item.ID] = index
	}
	retained := make(map[string]struct{}, len(projected.Items))
	previous := -1
	for _, item := range projected.Items {
		original, exists := canonicalByID[item.ID]
		if !exists {
			return fmt.Errorf("trajectory projection fabricated item %q", item.ID)
		}
		if _, duplicate := retained[item.ID]; duplicate {
			return fmt.Errorf("trajectory projection duplicated item %q", item.ID)
		}
		if canonicalOrder[item.ID] <= previous {
			return errors.New("trajectory projection changed canonical item order")
		}
		previous = canonicalOrder[item.ID]
		retained[item.ID] = struct{}{}
		if len(item.ProviderState) > 0 && !slices.Equal(item.ProviderState, original.ProviderState) {
			return fmt.Errorf("trajectory projection replaced provider state on item %q", item.ID)
		}
		if item.ProviderStateType != "" && item.ProviderStateType != original.ProviderStateType {
			return fmt.Errorf("trajectory projection replaced provider state type on item %q", item.ID)
		}
		original.CausalParentIDs = slices.Clone(item.CausalParentIDs)
		original.ProviderStateType = item.ProviderStateType
		original.ProviderState = slices.Clone(item.ProviderState)
		if !reflect.DeepEqual(original, item) {
			return fmt.Errorf("trajectory projection changed semantic item %q", item.ID)
		}
	}
	for _, item := range projected.Items {
		for _, parent := range item.CausalParentIDs {
			if _, exists := retained[parent]; !exists {
				return fmt.Errorf("trajectory projection left dangling parent %q on item %q", parent, item.ID)
			}
		}
		if item.Kind == trajectory.KindAssistantState && item.AssistantState != nil {
			if _, exists := retained[item.AssistantState.AssistantItemID]; !exists {
				return fmt.Errorf("trajectory projection left dangling assistant state on item %q", item.ID)
			}
		}
	}
	return nil
}

func cloneInvocation(invocation Invocation) Invocation {
	invocation.Capabilities = append([]Capability(nil), invocation.Capabilities...)
	invocation.Tools = append([]ToolDefinition(nil), invocation.Tools...)
	for index := range invocation.Tools {
		invocation.Tools[index].Parameters = append(json.RawMessage(nil), invocation.Tools[index].Parameters...)
	}
	return invocation
}

// stripMarkers removes the completion marker from every assistant segment and
// reports whether any carried it. It applies to any provider rather than only
// the one expected to emit it, so the marker is unspeakable by construction
// instead of by the phase happening to be right.
func stripMarkers(segments []bufferedSegment) bool {
	finished := false
	for index := range segments {
		if segments[index].kind != EventAssistantDelta {
			continue
		}
		stripped, found := StripMarkers(segments[index].text.String())
		if !found {
			continue
		}
		finished = true
		segments[index].text.Reset()
		segments[index].text.WriteString(stripped)
	}
	return finished
}

func (runner *Runner) buildItems(
	instruction trajectory.Item,
	invocationID string,
	descriptor Descriptor,
	invocation Invocation,
	segments []bufferedSegment,
	completion Completion,
	interrupted bool,
) []trajectory.Item {
	producer := trajectory.Producer{
		Phase: descriptor.Phase, Provider: descriptor.Provider, Model: descriptor.Model,
		ReasoningEffort: string(descriptor.Effort),
		SpeechAuthority: string(descriptor.EffectiveSpeechAuthority()),
	}
	parentID := instruction.ID
	var items []trajectory.Item
	providerStateAttached := false
	// Native assistant state containing proposal-only function calls must not be
	// replayed as an executable pending call by a same-provider continuation.
	// The portable tool_proposal item below is the authoritative handoff.
	hasProposal := slices.ContainsFunc(segments, func(segment bufferedSegment) bool {
		if segment.toolCall == nil {
			return false
		}
		return descriptor.EffectiveToolAuthority() == ToolAuthorityPropose || segment.undeclared
	})
	for _, segment := range segments {
		// A tool call becomes executable only when the provider invocation
		// reaches its terminal safe point. Interrupted reasoning and assistant
		// text remain useful history, but an interrupted action request must not
		// escape as a side effect.
		if interrupted && segment.kind == EventToolCall {
			continue
		}
		item := trajectory.Item{
			ID: runner.nextID("trajectory"), MonotonicNS: runner.now(),
			CausalParentIDs: []string{parentID}, SourceRevision: invocation.SourceRevision,
			InvocationID: invocationID, Producer: producer, Interrupted: interrupted,
		}
		switch segment.kind {
		case EventReasoningDelta:
			if !runner.retainReasoning {
				continue
			}
			item.Kind = trajectory.KindReasoning
			item.Content = segment.text.String()
		case EventAssistantDelta:
			// A turn whose whole content was the completion marker has nothing
			// left once the marker is removed. The marker is control, so what
			// remains is not an empty utterance to record - it is no utterance.
			if strings.TrimSpace(segment.text.String()) == "" {
				continue
			}
			item.Kind = trajectory.KindAssistant
			item.Content = segment.text.String()
			item.Visibility = trajectory.VisibilityPrepared
		case EventToolCall:
			item.Kind = trajectory.KindToolCall
			if descriptor.EffectiveToolAuthority() == ToolAuthorityPropose || segment.undeclared {
				item.Kind = trajectory.KindToolProposal
			}
			item.ToolCall = segment.toolCall
		default:
			continue
		}
		if !hasProposal && !providerStateAttached && len(completion.ProviderState) > 0 {
			item.ProviderStateType = completion.ProviderStateType
			item.ProviderState = append(json.RawMessage(nil), completion.ProviderState...)
			providerStateAttached = true
		}
		items = append(items, item)
		parentID = item.ID
	}
	if !hasProposal && !providerStateAttached && len(completion.ProviderState) > 0 {
		items = append(items, trajectory.Item{
			ID: runner.nextID("trajectory"), Kind: trajectory.KindReasoning,
			MonotonicNS: runner.now(), CausalParentIDs: []string{parentID},
			SourceRevision: invocation.SourceRevision, InvocationID: invocationID,
			Producer: producer, Interrupted: interrupted,
			ProviderStateType: completion.ProviderStateType,
			ProviderState:     append(json.RawMessage(nil), completion.ProviderState...),
		})
	}
	return items
}

func validateCompletion(completion Completion, descriptor Descriptor) error {
	if len(completion.ProviderState) == 0 && completion.ProviderStateType == "" {
		return nil
	}
	if completion.ProviderStateType == "" || len(completion.ProviderState) == 0 || !json.Valid(completion.ProviderState) {
		return errors.New("completion provider state type and valid JSON must be supplied together")
	}
	if descriptor.NativeStateType != "" && completion.ProviderStateType != descriptor.NativeStateType {
		return fmt.Errorf("completion state type %q does not match descriptor %q", completion.ProviderStateType, descriptor.NativeStateType)
	}
	return nil
}

// Prepared is a continuation that has been generated and not committed.
//
// Speculative preparation is a latency policy, and it is only safe to be a
// latency policy because prepared work is private by construction: it has no
// speech sink and no tool authority until it is adopted at a real safe point.
// Being wrong therefore costs tokens and nothing else - which is the property
// that lets the decision to prepare be made on evidence that is still changing.
type Prepared struct {
	provisional trajectory.Item
	result      RunResult
	items       []trajectory.Item
	baseVersion uint64
	adopted     bool
}

// ProvisionalText is what the preparation was generated against. Adoption is
// only sound when the canonical record of the turn says the same thing.
func (prepared *Prepared) ProvisionalText() string {
	if prepared == nil {
		return ""
	}
	return prepared.provisional.Content
}

// Ready reports whether the preparation produced output worth adopting.
func (prepared *Prepared) Ready() bool {
	return prepared != nil && !prepared.adopted && len(prepared.items) > 1 && !prepared.result.Interrupted
}

// Prepare generates a continuation against the canonical prefix plus one
// uncommitted observation, and appends nothing.
//
// The provisional observation carries what perception has heard so far. It is
// not committed, cannot be committed by this call, and never becomes part of
// the log: only the model's answer does, and only if Adopt accepts it.
func (runner *Runner) Prepare(
	ctx context.Context,
	provider Provider,
	invocation Invocation,
	provisional trajectory.Item,
	observer StreamObserver,
) (*Prepared, error) {
	if strings.TrimSpace(provisional.ID) == "" || provisional.Kind != trajectory.KindObservation {
		return nil, errors.New("preparation requires a provisional observation item")
	}
	if strings.TrimSpace(provisional.Content) == "" {
		return nil, errors.New("a provisional observation requires text")
	}
	prepared := &Prepared{provisional: provisional}
	if _, err := runner.run(ctx, provider, invocation, nil, observer, prepared, trajectory.Item{}); err != nil {
		return nil, err
	}
	if !prepared.Ready() {
		return nil, errors.New("preparation produced nothing to adopt")
	}
	return prepared, nil
}

// Adopt commits prepared output at the current safe point.
//
// It restamps commit time, because commit time is when something entered the
// log rather than when it was generated, and a prepared item still carrying
// its generation time would make the log's clock run backwards. Everything
// else - the instruction, the output, the provenance - is exactly what the
// provider produced, so an adopted continuation is indistinguishable from one
// that was run at this instant except in having been faster.
func (runner *Runner) Adopt(prepared *Prepared) (RunResult, error) {
	if !prepared.Ready() {
		return RunResult{}, errors.New("this preparation cannot be adopted")
	}
	snapshot := runner.store.Snapshot()
	if snapshot.Version < prepared.baseVersion {
		return RunResult{}, errors.New("the trajectory is shorter than the prefix this preparation was generated against")
	}
	items := slices.Clone(prepared.items)
	now := runner.now()
	for index := range items {
		items[index].MonotonicNS = now
	}
	if len(snapshot.Items) > 0 {
		items[0].CausalParentIDs = []string{snapshot.Items[len(snapshot.Items)-1].ID}
	}
	result := prepared.result
	result.StartVersion = snapshot.Version
	if err := runner.commit(&result, snapshot.Version, items); err != nil {
		return result, err
	}
	prepared.adopted = true
	return result, nil
}
