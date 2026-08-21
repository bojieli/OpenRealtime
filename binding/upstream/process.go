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

const maxRolloutIterations = 16

// Process runs the background reasoner over the mirrored conversation.
func (runtime *runtime) Process(ctx context.Context, batch eventloop.Batch) error {
	cause := interaction.Cause{
		Observation: batch.Contains(trajectory.KindObservation),
		ToolResult:  batch.Contains(trajectory.KindToolResult),
		Parallel:    batch.Triage == eventloop.TriageParallel,
	}
	if !cause.Observation && !cause.ToolResult {
		return nil
	}
	// Only the user's own speech opens a turn. The remote's voice is mirrored
	// as evidence, and treating it as a new request would make the reasoner
	// answer the voice model instead of the person.
	if cause.Observation && !batchHasUserSpeech(batch) && !cause.ToolResult {
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
			return errors.Join(failures...)
		}
		request := cognition.Request{SourceRevision: revision}
		previous := cause
		cause.Observation, cause.ToolResult, cause.SlowCommitted = false, false, false

		for _, step := range plan {
			switch step.Kind {
			case interaction.StepSlow:
				result, err := runtime.engine.RunSlow(ctx, request, nil)
				if err != nil {
					failures = append(failures, err)
					return errors.Join(failures...)
				}
				if len(result.ToolCalls) > 0 {
					dispatched, dispatchErr := runtime.dispatch(ctx, result)
					if dispatchErr != nil {
						failures = append(failures, dispatchErr)
						return errors.Join(failures...)
					}
					cause.ToolResult = dispatched
					if !dispatched {
						return errors.Join(failures...)
					}
					continue
				}
				if strings.TrimSpace(result.AssistantText) != "" {
					cause.SlowCommitted = true
					runtime.rememberAnswer(result.AssistantText)
				}
			case interaction.StepVoice:
				if err := runtime.handOff(ctx); err != nil {
					failures = append(failures, err)
					return errors.Join(failures...)
				}
			default:
				// A fast step would be a second voice. The stand-in provider
				// refuses to run, so this is defensive rather than reachable.
				failures = append(failures, fmt.Errorf("upstream cannot run a %s step: the remote owns the voice", step.Kind))
				return errors.Join(failures...)
			}
		}
		if !cause.SlowCommitted && !cause.ToolResult {
			return errors.Join(failures...)
		}
		_ = previous
	}
	return errors.Join(failures...)
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

// handOff gives the remote the completed answer to say.
//
// This is the explicit hand-off: a conversation item the remote treats as
// context, followed by a request to respond. It works against any
// Realtime-compatible endpoint because it uses nothing but the base protocol,
// which is what makes the upstream binding portable rather than tied to one
// vendor's internals.
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
	if err := runtime.remote.Send(handoff, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": "system",
			"content": []map[string]any{{
				"type": "input_text",
				"text": "The background reasoner has completed the answer. Say this, briefly and naturally, " +
					"preserving every fact and identifier exactly, and add nothing:\n\n" + answer,
			}},
		},
	}); err != nil {
		return err
	}
	return runtime.remote.Send(handoff, map[string]any{"type": "response.create"})
}

// dispatch splits the slow provider's calls between local execution and the
// client, exactly as the cascade does. The remote is never asked to execute a
// call: it has no authority over these tools.
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
		runtime.stateMu.Lock()
		pending := &pendingInvocation{results: make(map[string]trajectory.ToolResult)}
		for _, call := range result.ToolCalls {
			pending.calls = append(pending.calls, call)
			runtime.callOwner[call.CallID] = result.InvocationID
			runtime.callNames[call.CallID] = call.Name
		}
		runtime.pending[result.InvocationID] = pending
		runtime.stateMu.Unlock()
		if err := runtime.sink.ToolCalls(ctx, binding.ToolCallEvent{
			InvocationID: result.InvocationID, Calls: remote,
		}); err != nil {
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
		runtime.stateMu.Lock()
		if pending := runtime.pending[result.InvocationID]; pending != nil {
			for _, one := range results {
				pending.results[one.CallID] = one
			}
		}
		runtime.stateMu.Unlock()
		return false, nil
	}
	if err := runtime.engine.AppendToolResults(result.InvocationID, results); err != nil {
		return false, err
	}
	return true, nil
}

// ToolResult accepts a client-executed result for a call the background
// reasoner issued.
func (runtime *runtime) ToolResult(_ context.Context, result trajectory.ToolResult) error {
	if strings.TrimSpace(result.CallID) == "" {
		return errors.New("a tool result requires a call ID")
	}
	runtime.stateMu.Lock()
	invocationID, known := runtime.callOwner[result.CallID]
	if !known {
		runtime.stateMu.Unlock()
		return fmt.Errorf("tool result references unknown call %q", result.CallID)
	}
	pending := runtime.pending[invocationID]
	if pending == nil {
		runtime.stateMu.Unlock()
		return fmt.Errorf("tool result references a completed invocation %q", invocationID)
	}
	if _, duplicate := pending.results[result.CallID]; duplicate {
		runtime.stateMu.Unlock()
		return fmt.Errorf("duplicate tool result for call %q", result.CallID)
	}
	result.Name = runtime.callNames[result.CallID]
	pending.results[result.CallID] = result
	complete := len(pending.results) == len(pending.calls) && !pending.dispatched
	var ordered []trajectory.ToolResult
	if complete {
		pending.dispatched = true
		for _, call := range pending.calls {
			ordered = append(ordered, pending.results[call.CallID])
		}
	}
	runtime.stateMu.Unlock()
	if !complete {
		return nil
	}
	_, err := runtime.coordinator.Submit(eventloop.Event{
		Type: "tool.results", Source: "client", Channel: "tool",
		Priority: eventloop.PriorityRoutine, Kind: trajectory.KindToolResult,
		InvocationID: invocationID, ToolResults: ordered,
	})
	if err != nil {
		return err
	}
	runtime.stateMu.Lock()
	delete(runtime.pending, invocationID)
	for _, call := range pending.calls {
		delete(runtime.callOwner, call.CallID)
	}
	runtime.stateMu.Unlock()
	return nil
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
