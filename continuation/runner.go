package continuation

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

	"github.com/bojieli/OpenRealtime/trajectory"
)

// StreamObserver receives validated deltas before the safe-point append. A
// speech planner can use assistant deltas for speculative TTS while the
// trajectory remains authoritative at completion.
type StreamObserver func(Event) error

// Runner coordinates one provider invocation and atomically appends its output
// at the next safe point.
type Runner struct {
	store           *trajectory.Store
	now             func() uint64
	nextID          func(string) string
	retainReasoning bool
}

// RunnerConfig supplies dependencies. Now must return monotonic nanoseconds.
// RetainReasoning controls plaintext reasoning only; opaque provider state may
// still be retained to preserve authenticated same-provider continuation.
type RunnerConfig struct {
	Store           *trajectory.Store
	Now             func() uint64
	NextID          func(prefix string) string
	RetainReasoning bool
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
		retainReasoning: config.RetainReasoning,
	}, nil
}

// RunResult identifies the items appended for one continuation.
type RunResult struct {
	InvocationID  string                `json:"invocation_id"`
	StartVersion  uint64                `json:"start_version"`
	EndVersion    uint64                `json:"end_version"`
	AppendedIDs   []string              `json:"appended_ids"`
	AssistantText string                `json:"assistant_text,omitempty"`
	ToolProposals []trajectory.ToolCall `json:"tool_proposals,omitempty"`
	ToolCalls     []trajectory.ToolCall `json:"tool_calls,omitempty"`
	Completion    Completion            `json:"completion"`
	Committed     bool                  `json:"committed"`
	Interrupted   bool                  `json:"interrupted,omitempty"`
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
	prefix := before
	prefix.Items = append(prefix.Items, instruction)
	prefix.Version++
	request := Request{
		InvocationID: invocationID, Descriptor: descriptor,
		Trajectory: prefix, Invocation: invocation,
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
		InvocationID: invocationID, StartVersion: before.Version,
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
	if err := runner.store.AppendBatchAt(before.Version, commitItems); err != nil {
		result.Interrupted = true
		result.ToolProposals = nil
		result.ToolCalls = nil
		result.EndVersion = runner.store.Snapshot().Version
		if errors.Is(err, trajectory.ErrVersionConflict) {
			err = errors.Join(ErrStalePrefix, err)
		}
		return result, errors.Join(providerErr, fmt.Errorf("commit continuation safe point: %w", err))
	}
	result.Committed = true
	for _, item := range commitItems {
		result.AppendedIDs = append(result.AppendedIDs, item.ID)
	}
	result.EndVersion = runner.store.Snapshot().Version
	return result, providerErr
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
