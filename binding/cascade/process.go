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
	// An obligation the ledger is holding becomes visible to the model here,
	// at the first safe point after the evidence that created it committed.
	// Raising it earlier is not possible - the log refuses a repair whose
	// target is not yet recorded as played - and raising it later would let a
	// turn answer the user while still owing them a correction.
	if err := runtime.raiseRepairs(); err != nil {
		runtime.fail("repair_error", err)
	}
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

	// Everything this rollout produces belongs to one turn, and the client is
	// told the turn is done once. Bracketing here rather than around each
	// output kind is what makes that true: a turn that speaks and then calls a
	// tool is one response with two output items, not two responses of which
	// the client stops reading after the first.
	if err := runtime.sink.TurnBegin(ctx); err != nil {
		return err
	}
	turn := &turnReport{}
	defer func() {
		if err := runtime.sink.TurnEnd(ctx, turn.outcome()); err != nil {
			runtime.fail("sink_error", err)
		}
	}()

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
			next, err := runtime.runStep(ctx, step, request, &cause, turn)
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
	turn *turnReport,
) (bool, error) {
	switch step.Kind {
	case interaction.StepFast:
		// A preparation that answered this exact sentence is adopted rather
		// than regenerated. It is the same continuation, produced earlier.
		if result, adopted := runtime.adopt(trajectory.PhaseFast, canonicalText(
			runtime.store.Snapshot(), request.SourceRevision)); adopted {
			turn.record(result)
			return true, runtime.publishAssistant(ctx, result)
		}
		result, err := runtime.engine.RunFast(ctx, request, nil)
		turn.record(result)
		if publishErr := runtime.publishAssistant(ctx, result); publishErr != nil {
			return false, errors.Join(err, publishErr)
		}
		return err == nil, err
	case interaction.StepVoice:
		result, err := runtime.engine.RunVoice(ctx, request, nil)
		turn.record(result)
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
	result, adopted := runtime.adopt(trajectory.PhaseSlow, canonicalText(
		runtime.store.Snapshot(), request.SourceRevision))
	var err error
	if !adopted {
		result, err = runtime.engine.RunSlow(ctx, request, nil)
	}
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
		// The correction, if one was owed, is this. Recording that here rather
		// than when it is spoken is deliberate: the slow provider is the one
		// that was instructed to correct, and a fast utterance voicing it is a
		// rendering of the correction rather than the correction itself.
		if err := runtime.resolveRepairs(result); err != nil {
			runtime.fail("repair_error", err)
		}
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
	if runtime.textOnly() {
		// No synthesiser, no pacing, no duplex state: a text turn is delivered
		// the moment it is written. It crosses the same commit boundary, which
		// is the whole reason the boundary is one boundary for every output.
		return runtime.emitText(ctx, utterance, authority)
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
		runtime.clientCalls.Hold(result.InvocationID, results)
		return false, nil
	}
	if err := runtime.engine.AppendToolResults(result.InvocationID, results); err != nil {
		return false, err
	}
	return true, nil
}

func (runtime *runtime) sendToClient(ctx context.Context, result continuation.RunResult, calls []trajectory.ToolCall) error {
	if err := runtime.clientCalls.Track(result.InvocationID, result.ToolCalls); err != nil {
		return err
	}
	var usage *continuation.Usage
	if strings.TrimSpace(result.AssistantText) == "" {
		usage = &result.Completion.Usage
	}
	return runtime.sink.ToolCalls(ctx, binding.ToolCallEvent{
		InvocationID: result.InvocationID, Calls: calls, Usage: usage,
	})
}

// commitToolResults appends a batch every call in which now has a result,
// whether the client answered them or the deadline did.
func (runtime *runtime) commitToolResults(invocationID string, results []trajectory.ToolResult) error {
	_, err := runtime.coordinator.Submit(eventloop.Event{
		Type: "tool.results", Source: "client", Channel: "tool",
		Priority: eventloop.PriorityRoutine, Kind: trajectory.KindToolResult,
		InvocationID: invocationID, ToolResults: results,
	})
	return err
}

// reportUnanswered tells the client what its own silence cost.
//
// The model already knows - it has results saying the calls failed - and the
// client is the one part of the system that would otherwise have no way to
// find out that the work it was asked for never came back.
func (runtime *runtime) reportUnanswered(invocationID string, unanswered []trajectory.ToolCall) {
	names := make([]string, 0, len(unanswered))
	for _, call := range unanswered {
		names = append(names, call.Name)
	}
	runtime.fail("tool_result_timeout", fmt.Errorf(
		"invocation %s: no result for %s, and the call has been failed",
		invocationID, strings.Join(names, ", ")))
}

// ToolResult accepts a client-executed function result. The batch is committed
// only when every outstanding call in the invocation has one, because a
// partial batch would leave the model looking at a call with no result and no
// way to tell whether one is coming.
func (runtime *runtime) ToolResult(_ context.Context, result trajectory.ToolResult) error {
	return runtime.clientCalls.Result(result)
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
