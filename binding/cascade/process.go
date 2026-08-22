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

// Process executes the interaction plane's rollout plan for one committed
// batch. It is the only place cognition runs.
//
// It plans once and returns. Everything a step produces - an escalation, a
// finished background result, a tool result - re-enters as its own event, and
// the loop decides when to act on it. A processor that chained its own steps
// would be a second scheduler for the same conversation, running without the
// gate that owns that decision, which is how a turn ends up spoken over
// another one or answered twice.
func (runtime *runtime) Process(ctx context.Context, batch eventloop.Batch) error {
	// An obligation the ledger is holding becomes visible to the model here,
	// at the first safe point after the evidence that created it committed.
	// Raising it earlier is not possible - the log refuses a repair whose
	// target is not yet recorded as played - and raising it later would let a
	// turn answer the user while still owing them a correction.
	if err := runtime.raiseRepairs(); err != nil {
		runtime.fail("repair_error", err)
	}
	revision := runtime.latestRevision(batch)
	plan := runtime.policies.Rollout.Plan(interaction.RolloutInput{
		Context: interaction.Context{
			NowNS: runtime.scheduler.NowNS(), Duplex: runtime.duplex.Snapshot(),
		},
		Cause: interaction.Cause{
			Observation:      batch.Contains(trajectory.KindObservation),
			Escalated:        batch.Signalled(interaction.SignalEscalated),
			ToolResult:       batch.Contains(trajectory.KindToolResult),
			PendingRepair:    len(trajectory.PendingRepairs(runtime.store.Snapshot())) > 0,
			BackgroundResult: batch.Signalled(interaction.SignalBackgroundResult),
			SlowInvocations:  runtime.engine.SlowInvocations(revision),
			Parallel:         batch.Triage == eventloop.TriageParallel,
		},
	})
	if len(plan) == 0 {
		return nil
	}

	// Everything this plan produces belongs to one turn, and the client is
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

	request := cognition.Request{
		SourceRevision: revision,
		PendingRepair:  len(trajectory.PendingRepairs(runtime.store.Snapshot())) > 0,
	}
	var failures []error
	for _, step := range plan {
		select {
		case <-ctx.Done():
			return errors.Join(append(failures, context.Cause(ctx))...)
		default:
		}
		if err := runtime.runStep(ctx, step, request, turn); err != nil {
			failures = append(failures, fmt.Errorf("%s step: %w", step.Kind, err))
			if ctx.Err() != nil {
				break
			}
		}
	}
	return errors.Join(failures...)
}

// signal opens a safe point because a cognition phase finished. It appends
// nothing: what it refers to is already in the trajectory.
func (runtime *runtime) signal(eventType string) error {
	_, err := runtime.coordinator.Submit(eventloop.Event{
		Type: eventType, Source: "cognition", Channel: "cognition",
		Priority: eventloop.PriorityRoutine, Kind: eventloop.KindSignal,
	})
	return err
}

// runStep executes one rollout step.
func (runtime *runtime) runStep(
	ctx context.Context, step interaction.Step, request cognition.Request, turn *turnReport,
) error {
	switch step.Kind {
	case interaction.StepFast:
		return runtime.runFast(ctx, request, turn, step.Reason == interaction.ReasonBackgroundResult)
	case interaction.StepSlow:
		return runtime.runSlow(ctx, request)
	default:
		return fmt.Errorf("unknown rollout step %q", step.Kind)
	}
}

// runFast speaks, and hands the turn on unless the voice declared it finished.
//
// A turn that is itself speaking a background result never hands on again. The
// reasoner has just answered; asking it to answer once more is a loop, and the
// next thing the user says opens the question again anyway.
func (runtime *runtime) runFast(
	ctx context.Context, request cognition.Request, turn *turnReport, speaksBackgroundResult bool,
) error {
	// A preparation that answered this exact sentence is adopted rather than
	// regenerated. It is the same continuation, produced earlier.
	result, adopted := runtime.adopt(trajectory.PhaseFast, canonicalText(
		runtime.store.Snapshot(), request.SourceRevision))
	var err error
	if !adopted {
		result, err = runtime.engine.RunFast(ctx, request, nil)
	}
	turn.record(result)
	publishErr := runtime.publishAssistant(ctx, result)
	var signalErr error
	// A turn the voice did not declare finished goes to the reasoner. So does
	// one carrying a proposal, which is the voice naming a capability it
	// cannot run. Silence from a small model means "not finished", because the
	// alternative reading loses every capability the agent has the moment the
	// marker is forgotten.
	if !speaksBackgroundResult && (!result.Finished || len(result.ToolProposals) > 0) {
		signalErr = runtime.signal(interaction.SignalEscalated)
	}
	return errors.Join(err, publishErr, signalErr)
}

// runSlow deliberates and acts. It never speaks: what it writes is recorded as
// background state, and the signal it raises is what causes the voice to read
// that state and say something of its own.
func (runtime *runtime) runSlow(ctx context.Context, request cognition.Request) error {
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
				return errors.Join(err, placeholderErr)
			}
		}
		return err
	}
	if len(result.ToolCalls) > 0 {
		return runtime.dispatch(ctx, result)
	}
	if strings.TrimSpace(result.AssistantText) != "" {
		// The correction, if one was owed, is this. Recording it here rather
		// than when it is spoken is deliberate: the slow provider is the one
		// that was instructed to correct, and the voice saying so afterwards
		// is a rendering of the correction rather than the correction itself.
		if err := runtime.resolveRepairs(result); err != nil {
			runtime.fail("repair_error", err)
		}
		return runtime.signal(interaction.SignalBackgroundResult)
	}
	return nil
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
func (runtime *runtime) dispatch(ctx context.Context, result continuation.RunResult) error {
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
			return err
		}
	}
	if len(local) == 0 {
		return nil
	}
	results, err := runtime.tools.DispatchAll(ctx, local)
	if err != nil {
		return err
	}
	if len(remote) > 0 {
		// A split batch cannot be appended atomically: the invocation's
		// outstanding set includes calls this process is not executing. The
		// local results wait for the client's, which the tool-result path
		// assembles.
		runtime.clientCalls.Hold(result.InvocationID, results)
		return nil
	}
	// Locally dispatched results rejoin exactly where a client's do. Appending
	// them here instead would resume the chain without the loop ever seeing
	// that a tool came back, which is the one thing every completion owes it.
	return runtime.commitToolResults(result.InvocationID, results)
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
