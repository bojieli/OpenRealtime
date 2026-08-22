package upstream

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/cognition"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// Process runs the background reasoner over the mirrored conversation.
//
// It plans once and returns; what a step produces re-enters as its own event.
// The remote owns the voice here, so the step that speaks is the hand-off
// rather than a local fast turn - running one would be a second voice.
func (runtime *runtime) Process(ctx context.Context, batch eventloop.Batch) error {
	cause := interaction.Cause{
		Observation:      batch.Contains(trajectory.KindObservation),
		ToolResult:       batch.Contains(trajectory.KindToolResult),
		BackgroundResult: batch.Signalled(interaction.SignalBackgroundResult),
		Parallel:         batch.Triage == eventloop.TriageParallel,
	}
	if !cause.Observation && !cause.ToolResult && !cause.BackgroundResult {
		return nil
	}
	// Only the user's own speech opens a turn. The remote's voice is mirrored
	// as evidence, and treating it as a new request would make the reasoner
	// answer the voice model instead of the person.
	if cause.Observation && !batchHasUserSpeech(batch) && !cause.ToolResult {
		return nil
	}
	revision := runtime.latestRevision(batch)
	cause.SlowInvocations = runtime.engine.SlowInvocations(revision)
	plan := runtime.policies.Rollout.Plan(interaction.RolloutInput{
		Context: interaction.Context{
			NowNS: runtime.scheduler.NowNS(), Duplex: runtime.duplex.Snapshot(),
		},
		Cause: cause,
	})
	request := cognition.Request{SourceRevision: revision}
	var failures []error
	for _, step := range plan {
		var err error
		switch step.Kind {
		case interaction.StepSlow:
			err = runtime.runSlow(ctx, request)
		case interaction.StepFast:
			// The remote is the voice. Giving it the answer is what speaking
			// means in this binding.
			err = runtime.handOff(ctx)
		default:
			err = fmt.Errorf("upstream cannot run a %s step", step.Kind)
		}
		if err != nil {
			failures = append(failures, err)
			break
		}
	}
	return errors.Join(failures...)
}

// runSlow deliberates and acts. Its answer is remembered for the hand-off and
// announced as a signal, so the loop decides when the remote is told.
func (runtime *runtime) runSlow(ctx context.Context, request cognition.Request) error {
	result, err := runtime.engine.RunSlow(ctx, request, nil)
	if err != nil {
		return err
	}
	if len(result.ToolCalls) > 0 {
		return runtime.dispatch(ctx, result)
	}
	if strings.TrimSpace(result.AssistantText) != "" {
		runtime.rememberAnswer(result.AssistantText)
		return runtime.signal(interaction.SignalBackgroundResult)
	}
	return nil
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

func batchHasUserSpeech(batch eventloop.Batch) bool {
	for _, item := range batch.Items {
		if item.Kind == trajectory.KindObservation && trajectory.AuthorityOf(item) == trajectory.AuthorityUser {
			return true
		}
	}
	return false
}

func (runtime *runtime) rememberAnswer(text string) {
	runtime.stateMu.Lock()
	defer runtime.stateMu.Unlock()
	runtime.answer = strings.TrimSpace(text)
}

// handoffDirective is what the remote is told to do with a completed answer.
// It is identical whichever channel carries it, so the wording lives in one
// place and a vendor difference stays a transport difference.
const handoffDirective = "The background reasoner has completed the answer. Say this, briefly and " +
	"naturally, preserving every fact and identifier exactly, and add nothing:\n\n"

// handOff gives the remote the completed answer to say.
//
// This is the explicit hand-off, and which channel carries it is a declared
// property of the endpoint rather than an assumption. A conversation item is
// the portable form and what every endpoint modelled on OpenAI's own accepts;
// the session instruction is the fallback for an endpoint whose conversation
// items are reserved for tool results.
func (runtime *runtime) handOff(ctx context.Context) error {
	runtime.stateMu.Lock()
	answer := runtime.answer
	runtime.answer = ""
	runtime.stateMu.Unlock()
	if answer == "" {
		return nil
	}
	handoff, cancel := context.WithTimeout(ctx, runtime.config.HandoffTimeout)
	defer cancel()
	if runtime.config.Handoff == HandoffSessionInstruction {
		return runtime.handOffBySessionInstruction(handoff, answer)
	}
	if err := runtime.remote.Send(handoff, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": "system",
			"content": []map[string]any{{
				"type": "input_text", "text": handoffDirective + answer,
			}},
		},
	}); err != nil {
		return err
	}
	return runtime.remote.Send(handoff, map[string]any{"type": "response.create"})
}

// handOffBySessionInstruction carries the answer in the session instruction.
//
// The instruction is session state rather than a turn, so it has to be put
// back: leaving it in place would have the remote repeat a stale answer on
// every later turn. finishRemoteResponse does the restoring, once the response
// this handoff asked for has completed.
func (runtime *runtime) handOffBySessionInstruction(ctx context.Context, answer string) error {
	settings := runtime.Settings()
	base := remoteInstruction(settings.Instruction)
	// The declaration travels with every session.update, not just the first.
	// This one is borrowing the instruction to carry an answer; leaving the
	// detector out would hand the floor back to the remote as a side effect of
	// saying something.
	if err := runtime.remote.Send(ctx, sessionUpdate(
		base+"\n\n"+handoffDirective+answer, settings.ManualTurns, settings.Modalities)); err != nil {
		return err
	}
	runtime.stateMu.Lock()
	runtime.restoreInstruction = true
	runtime.stateMu.Unlock()
	return runtime.remote.Send(ctx, map[string]any{"type": "response.create"})
}

// dispatch splits the slow provider's calls between local execution and the
// client, exactly as the cascade does. The remote is never asked to execute a
// call: it has no authority over these tools.
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
		if err := runtime.clientCalls.Track(result.InvocationID, result.ToolCalls); err != nil {
			return err
		}
		if err := runtime.sink.ToolCalls(ctx, binding.ToolCallEvent{
			InvocationID: result.InvocationID, Calls: remote,
		}); err != nil {
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
		runtime.clientCalls.Hold(result.InvocationID, results)
		return nil
	}
	// Locally dispatched results rejoin exactly where a client's do.
	return runtime.commitToolResults(result.InvocationID, results)
}

// ToolResult accepts a client-executed result for a call the background
// reasoner issued.
func (runtime *runtime) ToolResult(_ context.Context, result trajectory.ToolResult) error {
	return runtime.clientCalls.Result(result)
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

// reportUnanswered tells the client what its own silence cost. The model
// already knows - it has results saying the calls failed - and the client is
// the one part of the system that would otherwise never find out that the work
// it was asked for never came back.
func (runtime *runtime) reportUnanswered(invocationID string, unanswered []trajectory.ToolCall) {
	names := make([]string, 0, len(unanswered))
	for _, call := range unanswered {
		names = append(names, call.Name)
	}
	runtime.sink.Failed(runtime.ctx, binding.ErrorEvent{
		Code: "tool_result_timeout",
		Message: fmt.Sprintf("invocation %s: no result for %s, and the call has been failed",
			invocationID, strings.Join(names, ", ")),
	})
}

func (runtime *runtime) latestRevision(batch eventloop.Batch) uint64 {
	revision := batch.SourceRevision()
	for _, item := range runtime.store.Snapshot().Items {
		if item.Kind == trajectory.KindObservation {
			revision = max(revision, item.SourceRevision)
		}
	}
	return revision
}

func pcmDuration(bytes int, sampleRate uint32) time.Duration {
	if sampleRate == 0 || bytes <= 0 {
		return 0
	}
	return time.Duration(int64(bytes/2)) * time.Second / time.Duration(sampleRate)
}

var _ eventloop.Processor = (*runtime)(nil)
