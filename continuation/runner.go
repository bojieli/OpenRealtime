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
}

type bufferedSegment struct {
	kind     EventKind
	text     strings.Builder
	toolCall *trajectory.ToolCall
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
	return runner.run(ctx, provider, invocation, nil, observer, nil)
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
	return runner.run(ctx, provider, invocation, projection, observer, nil)
}

func (runner *Runner) run(
	ctx context.Context,
	provider Provider,
	invocation Invocation,
	projection TrajectoryProjection,
	observer StreamObserver,
	prepared *Prepared,
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
		// The provisional observation is what makes preparation possible at
		// all: at this instant the user is still talking, so nothing about
		// this turn is in the canonical log yet. It is shown to the provider
		// and appended to nothing, and adoption later checks that the endpoint
		// said the same thing.
		prefix.Items = append(prefix.Items, prepared.provisional)
		prefix.Version++
		visibleInstruction.CausalParentIDs = []string{prepared.provisional.ID}
	}
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
		if event.Kind == EventToolCall {
			if _, declared := declaredTools[event.ToolCall.Name]; !declared {
				return fmt.Errorf("continuation emitted undeclared tool %q", event.ToolCall.Name)
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
			segments = append(segments, bufferedSegment{kind: event.Kind, toolCall: &copy})
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
	items := runner.buildItems(instruction, invocationID, descriptor, invocation, segments, completion, interrupted)
	result := RunResult{
		InvocationID: invocationID, SourceRevision: invocation.SourceRevision, StartVersion: before.Version,
		Completion: completion, Interrupted: interrupted,
	}
	for _, segment := range segments {
		if segment.kind == EventAssistantDelta {
			result.AssistantText += segment.text.String()
		}
		if segment.toolCall != nil && !interrupted {
			if descriptor.EffectiveToolAuthority() == ToolAuthorityPropose {
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

// commit appends one continuation's output as a version-checked transaction.
func (runner *Runner) commit(result *RunResult, expectedVersion uint64, items []trajectory.Item) error {
	if err := runner.store.AppendBatchAt(expectedVersion, items); err != nil {
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
	hasProposal := descriptor.EffectiveToolAuthority() == ToolAuthorityPropose && slices.ContainsFunc(segments, func(segment bufferedSegment) bool {
		return segment.toolCall != nil
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
			item.Kind = trajectory.KindAssistant
			item.Content = segment.text.String()
			item.Visibility = trajectory.VisibilityPrepared
		case EventToolCall:
			item.Kind = trajectory.KindToolCall
			if descriptor.EffectiveToolAuthority() == ToolAuthorityPropose {
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
	if _, err := runner.run(ctx, provider, invocation, nil, observer, prepared); err != nil {
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
