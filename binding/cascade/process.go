package cascade

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/cognition"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// actionUtterance is a local alias so the audio path can name the predicate
// type without importing action for one signature.
type actionUtterance = action.Utterance

// maxRolloutIterations bounds re-planning within one safe point. A rollout
// that keeps asking for work is a bug in the policy, and a runaway loop in a
// live session is worse than a truncated turn.
const maxRolloutIterations = 16

// Process executes the interaction plane's rollout plan for one committed
// batch. It is the only place cognition runs.
func (runtime *runtime) Process(ctx context.Context, batch eventloop.Batch) error {
	cause := interaction.Cause{
		Observation:   batch.Contains(trajectory.KindObservation),
		ToolResult:    batch.Contains(trajectory.KindToolResult),
		PendingRepair: len(trajectory.PendingRepairs(runtime.store.Snapshot())) > 0,
		Parallel:      batch.Triage == eventloop.TriageParallel,
	}
	if !cause.Observation && !cause.ToolResult && !cause.PendingRepair {
		return nil
	}
	revision := runtime.latestRevision(batch)
	var failures []error

	for iteration := 0; iteration < maxRolloutIterations; iteration++ {
		cause.SlowInvocations = runtime.engine.SlowInvocations(revision)
		plan := runtime.policies.Rollout.Plan(interaction.RolloutInput{
			Context: interaction.Context{
				NowNS: runtime.scheduler.NowNS(), Duplex: runtime.duplex.Snapshot(),
			},
			Cause: cause,
		})
		if len(plan) == 0 {
			break
		}
		request := cognition.Request{SourceRevision: revision, PendingRepair: cause.PendingRepair}
		// Each iteration answers what the previous one produced, so the causes
		// that opened this one are consumed here rather than replanned into a
		// second answer.
		cause.Observation, cause.ToolResult, cause.SlowCommitted = false, false, false

		for _, step := range plan {
			select {
			case <-ctx.Done():
				return errors.Join(append(failures, context.Cause(ctx))...)
			default:
			}
			next, err := runtime.runStep(ctx, step, request, &cause)
			if err != nil {
				failures = append(failures, fmt.Errorf("%s step: %w", step.Kind, err))
				if ctx.Err() != nil {
					return errors.Join(failures...)
				}
			}
			if !next {
				return errors.Join(failures...)
			}
		}
		if !cause.SlowCommitted && !cause.ToolResult {
			break
		}
	}
	return errors.Join(failures...)
}

// runStep executes one rollout step and reports whether the rollout continues.
func (runtime *runtime) runStep(
	ctx context.Context, step interaction.Step, request cognition.Request, cause *interaction.Cause,
) (bool, error) {
	switch step.Kind {
	case interaction.StepFast:
		result, err := runtime.engine.RunFast(ctx, request, nil)
		if publishErr := runtime.publishAssistant(ctx, result); publishErr != nil {
			return false, errors.Join(err, publishErr)
		}
		return err == nil, err
	case interaction.StepVoice:
		result, err := runtime.engine.RunVoice(ctx, request, nil)
		if publishErr := runtime.publishAssistant(ctx, result); publishErr != nil {
			return false, errors.Join(err, publishErr)
		}
		return err == nil, err
	case interaction.StepSlow:
		return runtime.runSlow(ctx, request, cause)
	default:
		return false, fmt.Errorf("unknown rollout step %q", step.Kind)
	}
}

func (runtime *runtime) runSlow(
	ctx context.Context, request cognition.Request, cause *interaction.Cause,
) (bool, error) {
	result, err := runtime.engine.RunSlow(ctx, request, nil)
	if err != nil {
		if ctx.Err() != nil {
			// Work in flight was interrupted. Any executable call it had
			// already committed must be closed out so the prefix stays
			// well-formed rather than dangling.
			if _, placeholderErr := runtime.engine.PlaceholderForInterrupted("interrupted"); placeholderErr != nil {
				return false, errors.Join(err, placeholderErr)
			}
		}
		return false, err
	}
	if len(result.ToolCalls) > 0 {
		dispatched, dispatchErr := runtime.dispatch(ctx, result)
		if dispatchErr != nil {
			return false, dispatchErr
		}
		// A locally dispatched batch appended its results, so the rollout
		// continues from them. A client-executed batch stops here: the results
		// arrive later as their own event and open their own safe point.
		cause.ToolResult = dispatched
		return dispatched, nil
	}
	if strings.TrimSpace(result.AssistantText) != "" {
		cause.SlowCommitted = true
		return true, nil
	}
	return false, nil
}

// publishAssistant applies the commitment policy to one continuation's output
// and, when it commits, queues speech and records the queued transition.
func (runtime *runtime) publishAssistant(ctx context.Context, result continuation.RunResult) error {
	if strings.TrimSpace(result.AssistantText) == "" || !result.Committed {
		return nil
	}
	items := runtime.assistantItems(result)
	if len(items) == 0 {
		return nil
	}
	authority := items[0].Producer.SpeechAuthority
	decision := runtime.policies.Commitment.Decide(interaction.CommitmentInput{
		Context: interaction.Context{
			NowNS: runtime.scheduler.NowNS(), Duplex: runtime.duplex.Snapshot(),
			Phase: items[0].Producer.Phase,
		},
		Text: result.AssistantText, Complete: !result.Interrupted, SpeechAuthority: authority,
	})
	if !decision.Committed() {
		return nil
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	utterance := action.Utterance{
		ID: idFor("speech", runtime.sequence.Add(1)), Text: decision.Emit,
		Phase: items[0].Producer.Phase, SourceRevision: result.SourceRevision,
		AssistantItemIDs: ids,
	}
	// The queued transition is committed before audio can be emitted, so the
	// log's account of what the world heard never runs ahead of the world.
	events := make([]eventloop.Event, 0, len(ids))
	for _, id := range ids {
		events = append(events, eventloop.Event{
			Type: "speech.queued", Source: "action", Channel: "voice",
			Priority: eventloop.PriorityRoutine, Kind: trajectory.KindAssistantState,
			AssistantState: &trajectory.AssistantState{
				AssistantItemID: id, Visibility: trajectory.VisibilityQueued,
			},
		})
	}
	if _, err := runtime.coordinator.SubmitBatch(events); err != nil {
		return err
	}
	if err := runtime.speech.Enqueue(utterance, authority); err != nil {
		if errors.Is(err, action.ErrSilentProducer) {
			// The commitment policy should have held this. Enforcing it again
			// here is the point of having the boundary at the commit site.
			return nil
		}
		cancelErr := runtime.recordCancellations([]action.Commitment{{
			ID: utterance.ID, AssistantItemIDs: ids,
		}}, "speech-enqueue", eventloop.PriorityRoutine)
		return errors.Join(err, cancelErr)
	}
	_ = ctx
	return nil
}

func (runtime *runtime) assistantItems(result continuation.RunResult) []trajectory.Item {
	appended := make(map[string]struct{}, len(result.AppendedIDs))
	for _, id := range result.AppendedIDs {
		appended[id] = struct{}{}
	}
	var items []trajectory.Item
	for _, item := range runtime.store.Snapshot().Items {
		if item.Kind != trajectory.KindAssistant {
			continue
		}
		if _, ok := appended[item.ID]; ok {
			items = append(items, item)
		}
	}
	return items
}

// recordCancellations commits the visibility transitions for output that never
// reached the world, so a provider compiling the prefix does not see cancelled
// content as conversational history.
func (runtime *runtime) recordCancellations(
	cancelled []action.Commitment, source string, priority eventloop.Priority,
) error {
	seen := make(map[string]struct{})
	events := make([]eventloop.Event, 0, len(cancelled))
	for _, commitment := range cancelled {
		for _, id := range commitment.AssistantItemIDs {
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			events = append(events, eventloop.Event{
				Type: "speech.cancelled", Source: source, Channel: "voice",
				Priority: priority, Kind: trajectory.KindAssistantState,
				AssistantState: &trajectory.AssistantState{
					AssistantItemID: id, Visibility: trajectory.VisibilityCancelled,
				},
			})
		}
	}
	if len(events) == 0 {
		return nil
	}
	if _, err := runtime.coordinator.SubmitBatch(events); err != nil {
		return fmt.Errorf("record %d cancellations: %w", len(events), err)
	}
	return nil
}

// dispatch splits an authoritative call batch between the tools this process
// executes and the tools the client executes, and reports whether results were
// appended here.
func (runtime *runtime) dispatch(ctx context.Context, result continuation.RunResult) (bool, error) {
	var local, remote []trajectory.ToolCall
	for _, call := range result.ToolCalls {
		call.Arguments = slices.Clone(call.Arguments)
		spec, declared := runtime.registry.Lookup(call.Name)
		if declared && spec.Dispatcher != nil {
			local = append(local, call)
			continue
		}
		remote = append(remote, call)
	}
	if len(remote) > 0 {
		if err := runtime.sendToClient(ctx, result, remote); err != nil {
			return false, err
		}
	}
	if len(local) == 0 {
		return false, nil
	}
	results, err := runtime.tools.DispatchAll(ctx, local)
	if err != nil {
		return false, err
	}
	if len(remote) > 0 {
		// A split batch cannot be appended atomically: the invocation's
		// outstanding set includes calls this process is not executing. The
		// local results wait for the client's, which the tool-result path
		// assembles.
		runtime.holdLocalResults(result.InvocationID, results)
		return false, nil
	}
	if err := runtime.engine.AppendToolResults(result.InvocationID, results); err != nil {
		return false, err
	}
	return true, nil
}

func (runtime *runtime) sendToClient(ctx context.Context, result continuation.RunResult, calls []trajectory.ToolCall) error {
	runtime.callsMu.Lock()
	if _, duplicate := runtime.invocations[result.InvocationID]; duplicate {
		runtime.callsMu.Unlock()
		return fmt.Errorf("duplicate tool invocation %q", result.InvocationID)
	}
	pending := &clientInvocation{results: make(map[string]trajectory.ToolResult)}
	for _, call := range result.ToolCalls {
		pending.calls = append(pending.calls, call)
		runtime.callOwner[call.CallID] = result.InvocationID
	}
	runtime.invocations[result.InvocationID] = pending
	runtime.callsMu.Unlock()

	var usage *continuation.Usage
	if strings.TrimSpace(result.AssistantText) == "" {
		usage = &result.Completion.Usage
	}
	return runtime.sink.ToolCalls(ctx, binding.ToolCallEvent{
		InvocationID: result.InvocationID, Calls: calls, Usage: usage,
	})
}

func (runtime *runtime) holdLocalResults(invocationID string, results []trajectory.ToolResult) {
	runtime.callsMu.Lock()
	defer runtime.callsMu.Unlock()
	pending, exists := runtime.invocations[invocationID]
	if !exists {
		return
	}
	for _, result := range results {
		pending.results[result.CallID] = result
	}
}

// ToolResult accepts a client-executed function result. The batch is committed
// only when every outstanding call in the invocation has one, because a
// partial batch would leave the model looking at a call with no result and no
// way to tell whether one is coming.
func (runtime *runtime) ToolResult(_ context.Context, result trajectory.ToolResult) error {
	if strings.TrimSpace(result.CallID) == "" {
		return errors.New("a tool result requires a call ID")
	}
	runtime.callsMu.Lock()
	invocationID, known := runtime.callOwner[result.CallID]
	if !known {
		runtime.callsMu.Unlock()
		return fmt.Errorf("tool result references unknown call %q", result.CallID)
	}
	pending := runtime.invocations[invocationID]
	if pending == nil {
		runtime.callsMu.Unlock()
		return fmt.Errorf("tool result references a completed invocation %q", invocationID)
	}
	if _, duplicate := pending.results[result.CallID]; duplicate {
		runtime.callsMu.Unlock()
		return fmt.Errorf("duplicate tool result for call %q", result.CallID)
	}
	for _, call := range pending.calls {
		if call.CallID == result.CallID {
			result.Name = call.Name
		}
	}
	pending.results[result.CallID] = result
	complete := len(pending.results) == len(pending.calls) && !pending.dispatched
	if complete {
		pending.dispatched = true
	}
	ordered := make([]trajectory.ToolResult, 0, len(pending.calls))
	if complete {
		for _, call := range pending.calls {
			ordered = append(ordered, pending.results[call.CallID])
		}
	}
	runtime.callsMu.Unlock()
	if !complete {
		return nil
	}
	_, err := runtime.coordinator.Submit(eventloop.Event{
		Type: "tool.results", Source: "client", Channel: "tool",
		Priority: eventloop.PriorityRoutine, Kind: trajectory.KindToolResult,
		InvocationID: invocationID, ToolResults: ordered,
	})
	if err != nil {
		runtime.callsMu.Lock()
		if current := runtime.invocations[invocationID]; current == pending {
			current.dispatched = false
		}
		runtime.callsMu.Unlock()
		return err
	}
	runtime.callsMu.Lock()
	delete(runtime.invocations, invocationID)
	for _, call := range pending.calls {
		delete(runtime.callOwner, call.CallID)
	}
	runtime.callsMu.Unlock()
	return nil
}

// CreateResponse asks for a response now. It exists for clients that drive
// turns explicitly rather than relying on the engine's floor.
func (runtime *runtime) CreateResponse(context.Context) error {
	runtime.coordinator.Wake("client requested a response")
	return nil
}

// Cancel cancels response generation in flight.
func (runtime *runtime) Cancel(_ context.Context, reason string) error {
	if strings.TrimSpace(reason) == "" {
		reason = "client cancellation"
	}
	runtime.coordinator.Interrupt(fmt.Errorf("%s: %w", reason, eventloop.ErrInterrupted))
	cancelled, heard := runtime.speech.Cancel(reason)
	if heard {
		runtime.duplex.AgentAudioStopped(runtime.scheduler.NowNS())
	}
	return runtime.recordCancellations(cancelled, "client", eventloop.PriorityInterrupt)
}

func (runtime *runtime) latestRevision(batch eventloop.Batch) uint64 {
	revision := batch.SourceRevision()
	// A late tool result or repair resumes from the newest canonical
	// observation. Its own older source stays on the result item, but new
	// output must not be labelled as an old branch.
	for _, item := range runtime.store.Snapshot().Items {
		if item.Kind == trajectory.KindObservation {
			revision = max(revision, item.SourceRevision)
		}
	}
	return revision
}

var _ eventloop.Processor = (*runtime)(nil)
