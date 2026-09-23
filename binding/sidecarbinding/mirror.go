package sidecarbinding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/cognition"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/sidecar"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// mirror reads the sidecar and does two things with what it produces:
// forwards it to the client, and commits what matters to the canonical
// trajectory so the background reasoner sees the conversation the model is
// having. The second half is the entire point of the binding.
func (runtime *runtime) mirror() {
	for message := range runtime.model.Frames() {
		if err := runtime.mirrorMessage(message); err != nil {
			runtime.sink.Failed(runtime.ctx, binding.ErrorEvent{
				Code: "sidecar_error", Message: err.Error(),
			})
		}
	}
	if err := runtime.model.Err(); err != nil {
		runtime.sink.Failed(runtime.ctx, binding.ErrorEvent{
			Code: "sidecar_closed", Message: err.Error(),
		})
	}
}

func (runtime *runtime) mirrorMessage(message sidecar.Message) error {
	switch message.Type {
	case sidecar.TypeSpeechStarted:
		if runtime.spec.Ownership.Floor != binding.OwnerModel {
			return nil
		}
		utteranceID := runtime.nativeSpeechStarted()
		return runtime.sink.Activity(runtime.ctx, binding.ActivityEvent{Started: true, ItemID: utteranceID})
	case sidecar.TypeSpeechStopped:
		if runtime.spec.Ownership.Floor != binding.OwnerModel {
			return nil
		}
		utteranceID, err := runtime.nativeSpeechStopped(runtime.ctx)
		if err != nil {
			return err
		}
		return runtime.sink.Activity(runtime.ctx, binding.ActivityEvent{Stopped: true, ItemID: utteranceID})
	case sidecar.TypeTranscript:
		if !message.Final {
			return nil
		}
		if runtime.spec.Ownership.Interaction == binding.OwnerEngine &&
			runtime.policies.Interaction != nil && runtime.interactionAudio != nil {
			// The policy recogniser is the canonical evidence in this
			// composition. Committing the sidecar's second transcript first would
			// start cognition before the controller chose an act, and committing it
			// afterwards would make one utterance happen twice.
			return nil
		}
		return runtime.commitUserSpeech(message.Text)
	case sidecar.TypeTextDelta:
		return runtime.forwardText(message.Text)
	case sidecar.TypeTextDone:
		if err := runtime.forwardRemainingText(message.Text); err != nil {
			return err
		}
		return runtime.commitModelSpeech(message.Text)
	case sidecar.TypeOutputAudio:
		return runtime.forwardAudio(message)
	case sidecar.TypeTurnDone:
		return runtime.finishTurn(message.TurnStatus == sidecar.TurnInterrupted)
	case sidecar.TypeToolCall:
		return runtime.modelToolCall(message)
	case sidecar.TypeError:
		runtime.sink.Failed(runtime.ctx, binding.ErrorEvent{
			Code: "sidecar_" + firstNonEmpty(message.Code, "error"), Message: message.Message(),
		})
		return nil
	default:
		return nil
	}
}

// commitUserSpeech records what the model heard as a canonical observation.
func (runtime *runtime) commitUserSpeech(text string) error {
	_, err := runtime.commitUserSpeechAs(text, "sidecar", nil)
	return err
}

// commitUserSpeechAs records one transcript source and optionally associates
// the interaction act that admitted it. The association is consumed by the
// event-loop processor, so "listen" can still enter the canonical history
// without quietly starting background work and speech.
func (runtime *runtime) commitUserSpeechAs(
	text, observer string, act *interaction.Act,
) (uint64, error) {
	if strings.TrimSpace(text) == "" {
		return 0, nil
	}
	runtime.audioMu.Lock()
	utteranceID := runtime.utteranceID
	runtime.audioMu.Unlock()
	runtime.stateMu.Lock()
	if utteranceID != "" && runtime.committedUtterance == utteranceID {
		// Two recognisers may describe the same audio in the text-policy
		// composition. The first final transcript is canonical; committing the
		// second as another user turn would make one utterance happen twice.
		runtime.stateMu.Unlock()
		return 0, nil
	}
	runtime.committedUtterance = utteranceID
	runtime.stateMu.Unlock()
	observation := perception.Observation{
		Text: text, Observer: observer, Source: "microphone",
		Authority: trajectory.AuthorityUser, Final: true,
	}
	if err := runtime.sink.Transcript(runtime.ctx, binding.TranscriptEvent{
		ItemID: utteranceID, Text: text, Final: true,
	}); err != nil {
		return 0, err
	}
	if err := runtime.sink.Observation(runtime.ctx, observation); err != nil {
		return 0, err
	}
	revision := runtime.nextRevision()
	if act != nil {
		runtime.stateMu.Lock()
		runtime.policyActions[revision] = *act
		runtime.stateMu.Unlock()
	}
	_, err := runtime.coordinator.Submit(eventloop.Event{
		Type: observer + ".transcript", Source: observer, Channel: "voice",
		Priority: eventloop.PriorityRoutine, Kind: trajectory.KindObservation,
		SourceRevision: revision, Producer: observation.Producer(), Content: text,
	})
	if err != nil && act != nil {
		runtime.stateMu.Lock()
		delete(runtime.policyActions, revision)
		runtime.stateMu.Unlock()
	}
	return revision, err
}

// commitModelSpeech records what the model said.
//
// It is committed with observer authority, not as an assistant item: the
// background reasoner did not produce this text and must not mistake it for
// its own prior reasoning, and a mirrored voice opening a turn would have the
// reasoner answering the model instead of the person.
func (runtime *runtime) commitModelSpeech(text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	observation := perception.Observation{
		Text: "The voice model said: " + text, Observer: "sidecar-voice", Source: "assistant",
		Authority: trajectory.AuthorityObserver, Final: true,
	}
	_, err := runtime.coordinator.Submit(eventloop.Event{
		Type: "sidecar.assistant", Source: "sidecar-voice", Channel: "voice",
		Priority: eventloop.PriorityRoutine, Kind: trajectory.KindObservation,
		SourceRevision: runtime.nextRevision(), Producer: observation.Producer(),
		Content: observation.Text, Observation: observation.Meta(),
	})
	return err
}

// forwardText delivers one transcript delta, opening the turn if the model
// spoke before it made a sound. Text and audio can arrive in either order, and
// whichever comes first is what starts the turn.
func (runtime *runtime) forwardText(delta string) error {
	if delta == "" {
		return nil
	}
	utterance, err := runtime.currentUtterance()
	if err != nil {
		return err
	}
	runtime.stateMu.Lock()
	runtime.spokenText += delta
	runtime.stateMu.Unlock()
	return runtime.sink.SpeechText(runtime.ctx, utterance, delta)
}

// forwardRemainingText sends whatever a terminal text frame carries that the
// deltas did not. A sidecar may stream deltas, send only the whole text at the
// end, or both, and the client should see the turn exactly once either way.
func (runtime *runtime) forwardRemainingText(whole string) error {
	runtime.stateMu.Lock()
	streamed := runtime.spokenText
	runtime.stateMu.Unlock()
	if strings.TrimSpace(whole) == "" || streamed == whole {
		return nil
	}
	if strings.HasPrefix(whole, streamed) {
		return runtime.forwardText(strings.TrimPrefix(whole, streamed))
	}
	return runtime.forwardText(whole)
}

// currentUtterance returns the turn in progress, opening one if needed.
func (runtime *runtime) currentUtterance() (action.Utterance, error) {
	runtime.stateMu.Lock()
	if runtime.utterance != nil {
		utterance := *runtime.utterance
		runtime.stateMu.Unlock()
		return utterance, nil
	}
	utterance := action.Utterance{
		ID: fmt.Sprintf("%s_speech_%d", runtime.spec.Name, runtime.sequence.Add(1)),
	}
	runtime.utterance = &utterance
	runtime.spokenText = ""
	runtime.stateMu.Unlock()
	return utterance, runtime.sink.SpeechBegin(runtime.ctx, utterance)
}

func (runtime *runtime) forwardAudio(message sidecar.Message) error {
	rate := runtime.ready.OutputRate
	if rate <= 0 {
		rate = 24_000
	}
	utterance, err := runtime.currentUtterance()
	if err != nil {
		return err
	}
	frame := action.Frame{
		PCM16LE: message.Payload, SampleRateHz: uint32(rate),
		Duration: time.Duration(len(message.Payload)/2) * time.Second / time.Duration(rate),
	}
	if err := runtime.duplex.AgentAudioHandedOff(utterance.ID, frame.Duration); err != nil {
		return err
	}
	return runtime.sink.SpeechAudio(runtime.ctx, utterance, frame)
}

// finishTurn ends the model's utterance. An interrupted turn is not a
// completed one: clients see its response cancelled, which is what tells a
// player to stop and report where it stopped.
func (runtime *runtime) finishTurn(interrupted bool) error {
	runtime.stateMu.Lock()
	utterance := runtime.utterance
	runtime.utterance = nil
	runtime.spokenText = ""
	runtime.stateMu.Unlock()
	if utterance == nil {
		return nil
	}
	return runtime.sink.SpeechEnd(runtime.ctx, *utterance, action.Outcome{Completed: !interrupted})
}

// modelToolCall commits and dispatches the bounded fast-action subset when the
// deployment explicitly enabled it. Naming a call in another process grants
// no ambient authority: the canonical trajectory, declared tool schema,
// confirmation policy, target, ledger, and dispatcher are still engine-owned.
func (runtime *runtime) modelToolCall(message sidecar.Message) error {
	refuse := func(reason string) error {
		runtime.config.Logf("sidecar model call %q refused: %s", message.Name, reason)
		return runtime.model.Send(sidecar.Message{
			Type: sidecar.TypeToolResult, CallID: message.CallID, Error: reason,
		})
	}
	if !runtime.boundedComputerTool(message.Name) {
		return refuse("the fast provider has execution authority only for declared, bounded computer actions")
	}
	call := trajectory.ToolCall{
		CallID: message.CallID, Name: message.Name, Arguments: slices.Clone(message.Arguments),
	}
	if len(call.Arguments) == 0 || !json.Valid(call.Arguments) {
		return refuse("tool arguments are not valid JSON")
	}
	snapshot := runtime.store.Snapshot()
	invocationID := fmt.Sprintf("%s_foreground_%d", runtime.spec.Name, runtime.sequence.Add(1))
	item := trajectory.Item{
		ID:   fmt.Sprintf("%s_call_%d", runtime.spec.Name, runtime.sequence.Add(1)),
		Kind: trajectory.KindToolCall, MonotonicNS: runtime.scheduler.NowNS(),
		SourceRevision: runtime.revision.Load(), InvocationID: invocationID,
		Producer: trajectory.Producer{
			Phase: trajectory.PhaseFast, Provider: "sidecar", Model: runtime.ready.Model,
			SpeechAuthority: string(continuation.SpeechAuthorityVoice),
		},
		ToolCall: &call,
	}
	if len(snapshot.Items) > 0 {
		item.CausalParentIDs = []string{snapshot.Items[len(snapshot.Items)-1].ID}
	}
	if err := runtime.store.AppendBatchAt(snapshot.Version, []trajectory.Item{item}); err != nil {
		return refuse("the visual action was overtaken by newer evidence")
	}
	runtime.foregroundInvocations.Store(invocationID, struct{}{})
	spec, _ := runtime.registry.Lookup(call.Name)
	if spec.Dispatcher != nil {
		results, _ := runtime.tools.DispatchAll(runtime.ctx, []trajectory.ToolCall{call})
		return runtime.commitToolResults(invocationID, results)
	}
	if err := runtime.clientCalls.Track(invocationID, []trajectory.ToolCall{call}); err != nil {
		return runtime.commitToolResults(invocationID, []trajectory.ToolResult{{
			CallID: call.CallID, Name: call.Name, Error: err.Error(),
		}})
	}
	if err := runtime.tools.EmitRemote(runtime.ctx, call); err != nil {
		return runtime.clientCalls.Result(trajectory.ToolResult{
			CallID: call.CallID, Name: call.Name, Error: err.Error(),
		})
	}
	if err := runtime.sink.ToolCalls(runtime.ctx, binding.ToolCallEvent{
		InvocationID: invocationID, Calls: []trajectory.ToolCall{call},
	}); err != nil {
		return runtime.clientCalls.Result(trajectory.ToolResult{
			CallID: call.CallID, Name: call.Name, Error: err.Error(),
		})
	}
	return nil
}

// Process runs the background reasoner over the mirrored conversation.
func (runtime *runtime) Process(ctx context.Context, batch eventloop.Batch) error {
	backgroundToolResult := false
	foregroundToolResult := false
	for _, event := range batch.Events {
		if event.Kind != trajectory.KindToolResult {
			continue
		}
		if _, foreground := runtime.foregroundInvocations.LoadAndDelete(event.InvocationID); !foreground {
			backgroundToolResult = true
			continue
		}
		foregroundToolResult = true
		for _, result := range event.ToolResults {
			runtime.tools.Complete(result.CallID)
			message := sidecar.Message{
				Type: sidecar.TypeToolResult, CallID: result.CallID,
			}
			if result.Error != "" {
				message.Error = result.Error
			} else {
				message.Output = slices.Clone(result.Output)
			}
			if err := runtime.model.Send(message); err != nil {
				return err
			}
		}
	}
	cause := interaction.Cause{
		Observation:      batch.Contains(trajectory.KindObservation),
		ToolResult:       backgroundToolResult,
		BackgroundResult: batch.Signalled(interaction.SignalBackgroundResult),
		Parallel:         batch.Triage == eventloop.TriageParallel,
	}
	// Mirrored model speech is observer evidence, not a new request. It can be
	// committed in the same safe-point batch as a background result or tool
	// result, though, and suppressing the whole batch would consume that other
	// cause without acting on it. Remove only the observer-only cause so the
	// signal that shared its commit still reaches the rollout policy.
	if cause.Observation && !batchHasUserSpeech(batch) {
		cause.Observation = false
	}
	if foregroundToolResult && !cause.Observation && !cause.ToolResult && !cause.BackgroundResult {
		return nil
	}
	if !cause.Observation && !cause.ToolResult && !cause.BackgroundResult {
		return nil
	}
	revision := runtime.latestRevision(batch)
	if cause.Observation {
		if act, controlled := runtime.policyAction(batch); controlled {
			switch act {
			case interaction.ActStaySilent, interaction.ActKeepSpeaking, interaction.ActStopSpeaking:
				// Listening still commits the utterance to history. It does not
				// secretly start a background turn through the event loop.
				return nil
			case interaction.ActActSilently:
				// The slow phase may execute but must not hand prose back to the
				// voice. The flag remains across tool results until the slow phase
				// reaches a terminal non-tool answer.
				runtime.silentToolWork.Store(true)
			}
		}
	}
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
			// The model is the voice. Giving it the answer is what speaking
			// means in this binding.
			err = runtime.handOff()
		default:
			err = fmt.Errorf("%s cannot run a %s step: the model owns the voice", runtime.spec.Name, step.Kind)
		}
		if err != nil {
			failures = append(failures, err)
			break
		}
	}
	return errors.Join(failures...)
}

// runSlow deliberates and acts. Its answer is remembered for the hand-off and
// announced as a signal, so the loop decides when the model is told.
func (runtime *runtime) runSlow(ctx context.Context, request cognition.Request) error {
	result, err := runtime.engine.RunSlow(ctx, request, nil)
	if err != nil {
		runtime.silentToolWork.Store(false)
		return err
	}
	if len(result.ToolCalls) > 0 {
		if err := runtime.dispatch(ctx, result); err != nil {
			runtime.silentToolWork.Store(false)
			return err
		}
		return nil
	}
	if runtime.silentToolWork.Swap(false) {
		// act-silently authorises engagement and explicitly withholds speech, including
		// an empty terminal result.
		return nil
	}
	if strings.TrimSpace(result.AssistantText) != "" {
		runtime.stateMu.Lock()
		runtime.answer = strings.TrimSpace(result.AssistantText)
		runtime.stateMu.Unlock()
		return runtime.signal(interaction.SignalBackgroundResult)
	}
	return nil
}

func (runtime *runtime) policyAction(batch eventloop.Batch) (interaction.Act, bool) {
	runtime.stateMu.Lock()
	defer runtime.stateMu.Unlock()
	var selected interaction.Act
	var selectedRevision uint64
	for _, item := range batch.Items {
		if item.Kind != trajectory.KindObservation ||
			trajectory.AuthorityOf(item) != trajectory.AuthorityUser {
			continue
		}
		act, controlled := runtime.policyActions[item.SourceRevision]
		if !controlled {
			continue
		}
		delete(runtime.policyActions, item.SourceRevision)
		if item.SourceRevision >= selectedRevision {
			selected, selectedRevision = act, item.SourceRevision
		}
	}
	return selected, selectedRevision != 0
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

// handOff gives the model what the reasoner found, and lets it speak.
//
// Where the sidecar declares text injection, the result becomes context and
// the model speaks on its next turn - which is the closest thing to splicing a
// second model into a stack that owns its own voice. Where it does not, the
// same text is sent followed by a respond request. The second path always
// works, which is why the binding ships whatever the research into the first
// one concludes.
//
// What it does not do is ask the model to recite. The cascade shipped that
// design and withdrew it: a phase told to say what another provider wrote
// treats the text as a script, and when it cannot find the referent it reads
// back the last thing it said instead - once, word for word, including the
// truncation from its own token limit. The result is handed over as something
// now known rather than something to perform, which is the same contract the
// cascade voice runs under.
func (runtime *runtime) handOff() error {
	runtime.stateMu.Lock()
	answer := runtime.answer
	runtime.answer = ""
	runtime.stateMu.Unlock()
	if answer == "" {
		return nil
	}
	text := HandOffText(answer)
	if err := runtime.model.Send(sidecar.Message{
		Type: sidecar.TypeText, Role: "system", Text: text,
	}); err != nil {
		return err
	}
	if runtime.ready.Has(sidecar.CapabilityTextInjection) &&
		runtime.spec.Ownership.Interaction == binding.OwnerModel {
		// A model selected as interaction owner decides for itself when to say
		// what it now knows. Demanding a turn would override that selection;
		// floor ownership is independent.
		return nil
	}
	return runtime.model.Send(sidecar.Message{Type: sidecar.TypeRespond})
}

func batchHasUserSpeech(batch eventloop.Batch) bool {
	for _, item := range batch.Items {
		if item.Kind == trajectory.KindObservation && trajectory.AuthorityOf(item) == trajectory.AuthorityUser {
			return true
		}
	}
	return false
}

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

// ToolResult accepts a client-executed result for a call the reasoner issued.
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

var _ eventloop.Processor = (*runtime)(nil)

// HandOffText is what the model is told when a background result arrives. It
// is a separate function so the wording can be asserted directly: this is the
// one place a second model's writing meets a voice, and the difference between
// stating and dictating is the difference between speech and recitation.
func HandOffText(answer string) string {
	return "The background reasoner has finished, and this is what it found. You now know it:\n\n" +
		answer + "\n\nTell the user what it means in your own words, briefly, as speech, and never " +
		"by reading it out. Keep every fact and identifier exactly as they are above. Do not " +
		"mention the reasoner or that anything arrived."
}
