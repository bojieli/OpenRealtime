package cascade

import (
	"context"
	"errors"
	"strings"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/eventloop"
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
	if err := runtime.sink.SpeechEnd(ctx, utterance, outcome); err != nil {
		return err
	}
	visibility := trajectory.VisibilityCancelled
	playedMS := uint64(0)
	if outcome.PlayedMS > 0 {
		visibility, playedMS = trajectory.VisibilityPlayed, outcome.PlayedMS
	}
	events := make([]eventloop.Event, 0, len(utterance.AssistantItemIDs))
	for _, id := range utterance.AssistantItemIDs {
		events = append(events, eventloop.Event{
			Type: "speech.completed", Source: "action", Channel: "voice",
			Priority: eventloop.PriorityRoutine, Kind: trajectory.KindAssistantState,
			AssistantState: &trajectory.AssistantState{
				AssistantItemID: id, Visibility: visibility, PlayedAudioMS: playedMS,
			},
		})
	}
	if len(events) == 0 {
		return nil
	}
	if _, err := runtime.coordinator.SubmitBatch(events); err != nil {
		return err
	}
	return nil
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
	events := make([]eventloop.Event, 0, len(commitment.AssistantItemIDs)+1)
	for _, id := range commitment.AssistantItemIDs {
		events = append(events, eventloop.Event{
			Type: "playback.truncated", Source: "client", Channel: "voice",
			Priority: eventloop.PriorityRoutine, Kind: trajectory.KindAssistantState,
			AssistantState: &trajectory.AssistantState{
				AssistantItemID: id, Visibility: visibility, PlayedAudioMS: playedMS,
			},
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
