package cascade

import (
	"context"
	"errors"
	"strings"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/spoken"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// speechSink connects the action plane's paced output to the session's sink,
// and records in the trajectory what actually reached the world.
//
// The two must not drift. The ledger knows what crossed the boundary and the
// trajectory is what a provider sees as conversational history, so the played
// and cancelled transitions are committed from here - the one place that knows
// both.
type speechSink struct{ runtime *runtime }

func (sink speechSink) Reserve(utterance action.Utterance) error {
	reserved, ok := sink.runtime.sink.(binding.SpeechReservationSink)
	if !ok {
		return nil
	}
	return reserved.SpeechReserved(sink.runtime.ctx, utterance)
}

func (sink speechSink) CancelReservation(utterance action.Utterance) {
	reserved, ok := sink.runtime.sink.(binding.SpeechReservationSink)
	if !ok {
		return
	}
	reserved.SpeechReservationCancelled(sink.runtime.ctx, utterance)
}

func (sink speechSink) Begin(ctx context.Context, utterance action.Utterance) error {
	// A planned utterance knows its whole text before the first frame, so the
	// transcript is delivered at the announcement. A binding whose model
	// streams text and audio together delivers it as it arrives instead.
	if err := sink.runtime.sink.SpeechBegin(ctx, utterance); err != nil {
		return err
	}
	return sink.runtime.sink.SpeechText(ctx, utterance, utterance.Text)
}

func (sink speechSink) Audio(ctx context.Context, utterance action.Utterance, frame action.Frame) error {
	return sink.runtime.sink.SpeechAudio(ctx, utterance, frame)
}

func (sink speechSink) End(ctx context.Context, utterance action.Utterance, outcome action.Outcome) error {
	runtime := sink.runtime
	visibility := trajectory.VisibilityCancelled
	playedMS := uint64(0)
	if outcome.PlayedMS > 0 {
		visibility, playedMS = trajectory.VisibilityPlayed, outcome.PlayedMS
	}
	heard := runtime.heardPerItem(utterance.AssistantItemIDs, outcome.Mark)
	events := make([]eventloop.Event, 0, len(utterance.AssistantItemIDs))
	for index, id := range utterance.AssistantItemIDs {
		state := &trajectory.AssistantState{
			AssistantItemID: id, Visibility: visibility, PlayedAudioMS: playedMS,
		}
		if index < len(heard) {
			mark := heard[index]
			state.Heard = &mark
		}
		events = append(events, eventloop.Event{
			Type: "speech.completed", Source: "action", Channel: "voice",
			Priority: eventloop.PriorityRoutine, Kind: trajectory.KindAssistantState,
			AssistantState: state,
		})
	}
	if len(events) > 0 {
		// Queue the canonical heard boundary before publishing SpeechEnd to the
		// client. The client may begin its next utterance as soon as it observes
		// that callback; if the callback won the race, the next user observation
		// could be committed and sent to cognition before this boundary even
		// entered the event queue. Ordering the internal fact first means both
		// events may still commit together, but the next model snapshot can never
		// run ahead of what the user had already heard.
		if _, err := runtime.coordinator.SubmitBatch(events); err != nil {
			return err
		}
	}
	return runtime.sink.SpeechEnd(ctx, utterance, outcome)
}

// heardPerItem splits one utterance's boundary back across the assistant items
// it covered.
//
// One utterance is one thing to say out loud and may be several items in the
// log. The cut is a fact about the audio, so it is discovered once and then
// recorded against each item it touches - an item that was fully spoken and an
// item that was never reached must not both be marked with whatever happened to
// the utterance as a whole.
//
// An utterance whose layout was never established reports nothing rather than
// reporting that nothing was heard: absence means "not measured here", and a
// caller falls back to the item's own text, while a recorded empty boundary
// would erase a turn the user did hear.
func (runtime *runtime) heardPerItem(ids []string, mark spoken.Mark) []spoken.Mark {
	if len(ids) == 0 || (!mark.Started() && strings.TrimSpace(mark.Pending) == "") {
		return nil
	}
	snapshot := runtime.store.Snapshot()
	contents := make([]string, 0, len(ids))
	for _, id := range ids {
		content := ""
		for _, item := range snapshot.Items {
			if item.Kind == trajectory.KindAssistant && item.ID == id {
				content = item.Content
				break
			}
		}
		contents = append(contents, content)
	}
	return spoken.Distribute(mark, contents)
}

// Truncate reports client-side playback truncation.
//
// The client is the authority on what was actually heard: the server knows
// what it sent and when, but only the client knows where playback stopped. A
// truncation that reveals the user heard content later evidence invalidated is
// what creates a repair obligation.
func (runtime *runtime) Truncate(ctx context.Context, truncation binding.Truncation) error {
	if strings.TrimSpace(truncation.ItemID) == "" || truncation.AudioEndMS < 0 {
		return errors.New("truncation requires an item and a non-negative audio end")
	}
	commitment, exists := runtime.ledger.Lookup(truncation.ItemID)
	if !exists {
		return nil
	}
	visibility := trajectory.VisibilityCancelled
	playedMS := uint64(0)
	if truncation.AudioEndMS > 0 {
		visibility, playedMS = trajectory.VisibilityPlayed, uint64(truncation.AudioEndMS)
	}
	// The client's number replaces the server's, so the boundary derived from
	// the server's number has to move with it. The layout worked out for this
	// utterance is exactly what makes moving it possible: without one, a
	// truncation could only shorten a duration and would leave the words the
	// user is now known not to have heard still recorded as heard.
	var heard []spoken.Mark
	if mark, known := runtime.timing.MarkAt(truncation.ItemID, playedMS); known {
		heard = runtime.heardPerItem(commitment.AssistantItemIDs, mark)
	}
	events := make([]eventloop.Event, 0, len(commitment.AssistantItemIDs)+1)
	for index, id := range commitment.AssistantItemIDs {
		state := &trajectory.AssistantState{
			AssistantItemID: id, Visibility: visibility, PlayedAudioMS: playedMS,
		}
		if index < len(heard) {
			mark := heard[index]
			state.Heard = &mark
		}
		events = append(events, eventloop.Event{
			Type: "playback.truncated", Source: "client", Channel: "voice",
			Priority: eventloop.PriorityRoutine, Kind: trajectory.KindAssistantState,
			AssistantState: state,
		})
	}
	if len(events) > 0 {
		if _, err := runtime.coordinator.SubmitBatch(events); err != nil {
			return err
		}
	}
	// The client is authoritative on how much was heard, so its number
	// replaces the server's. It is not a repair on its own - stopping playback
	// invalidates nothing - but it is the figure the repair policy will decide
	// on if later evidence does invalidate this content, and deciding on the
	// server's optimistic duration would over-report what the user was told.
	runtime.ledger.Truncated(truncation.ItemID, playedMS)
	_ = ctx
	return nil
}
