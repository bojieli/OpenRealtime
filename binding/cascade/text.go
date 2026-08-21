package cascade

import (
	"context"
	"strings"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// A text session is the same conversation with a different output boundary.
//
// Everything above action is unchanged: the same observations, the same two
// cognition providers, the same rollout, the same commitment policy deciding
// what may be emitted. What changes is that nothing is synthesised and nothing
// is paced, because there is no audio to pace - a turn crosses into the world
// the moment it is written.
//
// That is why this is not "audio with the synthesiser switched off". Pacing
// exists so that "the agent is speaking" means something a barge-in decision
// can be built on, and a text turn is heard by nobody: it takes no time, it
// cannot be talked over, and it never touches the duplex state. Running it
// through the speech planner would make the session report an agent speaking
// for as long as a synthesiser would have taken, which is a fiction.

// textOnly reports whether this session's output modality is text.
func (runtime *runtime) textOnly() bool {
	runtime.settingsMu.RLock()
	defer runtime.settingsMu.RUnlock()
	modalities := runtime.settings.Modalities
	return len(modalities) == 1 && strings.EqualFold(modalities[0], "text")
}

// emitText delivers one turn as text and records that it reached the world.
//
// The commit boundary is the same one speech crosses, and it is crossed here
// for the same reason: text a client has received cannot be unreceived, so it
// goes through the ledger, and the trajectory learns that the content was
// delivered rather than merely decided.
func (runtime *runtime) emitText(
	ctx context.Context, utterance action.Utterance, speechAuthority string,
) error {
	if speechAuthority == "silent" {
		// The second cognition boundary holds whatever the modality is. Slow
		// cannot speak, and it cannot write to the client either: its output
		// reaches the world through a fast continuation or not at all.
		return nil
	}
	if err := runtime.ledger.Prepare(action.Commitment{
		ID: utterance.ID, Kind: action.KindText, AssistantItemIDs: utterance.AssistantItemIDs,
		SourceRevision: utterance.SourceRevision, Phase: utterance.Phase,
		SpeechAuthority: speechAuthority,
	}); err != nil {
		return err
	}
	if err := runtime.ledger.Queue(utterance.ID); err != nil {
		return err
	}
	if err := runtime.sink.SpeechBegin(ctx, utterance); err != nil {
		return runtime.abandonText(utterance, err)
	}
	// The crossing is recorded before the bytes move, exactly as the speech
	// path does it: a cancellation racing this must never be able to classify
	// something already on its way as safely droppable.
	if err := runtime.ledger.Emit(utterance.ID); err != nil {
		return err
	}
	if err := runtime.sink.SpeechText(ctx, utterance, utterance.Text); err != nil {
		return runtime.abandonText(utterance, err)
	}
	if err := runtime.ledger.Complete(utterance.ID, 0); err != nil {
		return err
	}
	if err := runtime.sink.SpeechEnd(ctx, utterance, action.Outcome{Completed: true}); err != nil {
		return err
	}
	return runtime.recordDelivered(utterance.AssistantItemIDs)
}

// abandonText records that a turn decided as text never reached the client.
func (runtime *runtime) abandonText(utterance action.Utterance, cause error) error {
	if _, err := runtime.ledger.Cancel(utterance.ID, cause.Error()); err != nil {
		return cause
	}
	if recordErr := runtime.recordCancellations([]action.Commitment{{
		ID: utterance.ID, AssistantItemIDs: utterance.AssistantItemIDs,
	}}, "text-sink", eventloop.PriorityRoutine); recordErr != nil {
		return recordErr
	}
	return cause
}

// recordDelivered commits the visibility transitions for text that reached the
// client, so a provider compiling the prefix sees it as conversational history
// rather than as something still pending.
func (runtime *runtime) recordDelivered(assistantItemIDs []string) error {
	events := make([]eventloop.Event, 0, len(assistantItemIDs)*2)
	for _, id := range assistantItemIDs {
		events = append(events,
			eventloop.Event{
				Type: "text.queued", Source: "action", Channel: "text",
				Priority: eventloop.PriorityRoutine, Kind: trajectory.KindAssistantState,
				AssistantState: &trajectory.AssistantState{
					AssistantItemID: id, Visibility: trajectory.VisibilityQueued,
				},
			},
			eventloop.Event{
				Type: "text.delivered", Source: "action", Channel: "text",
				Priority: eventloop.PriorityRoutine, Kind: trajectory.KindAssistantState,
				AssistantState: &trajectory.AssistantState{
					AssistantItemID: id, Visibility: trajectory.VisibilityPlayed,
				},
			})
	}
	if len(events) == 0 {
		return nil
	}
	_, err := runtime.coordinator.SubmitBatch(events)
	return err
}
