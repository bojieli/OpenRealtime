package scenarioconversation

import (
	"context"
	"errors"
	"maps"
	"sync"

	"github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	legacy "github.com/bojieli/OpenRealtime/binding"
)

const (
	scenarioPlaybackName    = "openrealtime.scenario-conversation.gateway-playback"
	scenarioPlaybackVersion = "1.0.0"
)

// presentationState is the session-owned projection of client output
// modalities. It is not UI policy: browser, macOS, benchmark, or another
// client still implements the same legacy.Sink API. This state only prevents
// PCM from being handed to a client that selected text-only output.
type presentationState struct {
	mu       sync.RWMutex
	settings legacy.Settings
}

func newPresentationState(settings legacy.Settings) *presentationState {
	return &presentationState{settings: legacy.CloneSettings(settings)}
}

func (state *presentationState) Update(settings legacy.Settings) {
	if state == nil {
		return
	}
	state.mu.Lock()
	state.settings = legacy.CloneSettings(settings)
	state.mu.Unlock()
}

func (state *presentationState) textOnly() bool {
	if state == nil {
		return false
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	return len(state.settings.Modalities) == 1 && state.settings.Modalities[0] == "text"
}

func scenarioPlaybackDescriptor() v1.Descriptor {
	return v1.Descriptor{
		Name: scenarioPlaybackName, Version: scenarioPlaybackVersion,
		Capabilities: v1.Capabilities{v1.CapabilityStreamingInput: true},
	}
}

// sessionPlaybackSink is an explicit plugin-owned adapter from graph-paced
// action.SpeechSink frames to the stable session presentation API. It does
// not own or close the client sink supplied by the server/benchmark host.
type sessionPlaybackSink struct {
	ctx          context.Context
	sink         legacy.Sink
	descriptor   v1.Descriptor
	presentation *presentationState
	mu           sync.Mutex
	turns        map[string]playbackTurn
}

type playbackTurn struct {
	utterance action.Utterance
	textOnly  bool
	begun     bool
}

func newSessionPlaybackSink(
	ctx context.Context, sink legacy.Sink, descriptor v1.Descriptor,
	presentation *presentationState,
) *sessionPlaybackSink {
	descriptor.Capabilities = maps.Clone(descriptor.Capabilities)
	return &sessionPlaybackSink{
		ctx: ctx, sink: sink, descriptor: descriptor, presentation: presentation,
		turns: make(map[string]playbackTurn),
	}
}

func (sink *sessionPlaybackSink) Descriptor() v1.Descriptor {
	if sink == nil {
		return v1.Descriptor{}
	}
	descriptor := sink.descriptor
	descriptor.Capabilities = maps.Clone(sink.descriptor.Capabilities)
	return descriptor
}

func (sink *sessionPlaybackSink) Begin(ctx context.Context, utterance action.Utterance) error {
	if sink == nil || sink.sink == nil {
		return errors.New("scenario conversation playback sink is unavailable")
	}
	sink.mu.Lock()
	turn, reserved := sink.turns[utterance.ID]
	if reserved {
		if turn.begun {
			sink.mu.Unlock()
			return errors.New("scenario conversation playback utterance began twice")
		}
		turn.begun = true
		turn.utterance = utterance
		sink.turns[utterance.ID] = turn
	}
	sink.mu.Unlock()
	if !reserved {
		if err := sink.sink.TurnBegin(ctx); err != nil {
			return err
		}
		turn = playbackTurn{utterance: utterance, textOnly: sink.presentation.textOnly(), begun: true}
		sink.mu.Lock()
		if _, duplicate := sink.turns[utterance.ID]; duplicate {
			sink.mu.Unlock()
			endErr := sink.sink.TurnEnd(ctx, legacy.TurnOutcome{
				Incomplete: true, Detail: "playback utterance identity raced with another turn",
			})
			return errors.Join(errors.New("scenario conversation playback utterance identity is duplicated"), endErr)
		}
		sink.turns[utterance.ID] = turn
		sink.mu.Unlock()
	}
	if err := sink.sink.SpeechBegin(ctx, utterance); err != nil {
		sink.deleteTurn(utterance.ID)
		endErr := sink.sink.TurnEnd(ctx, legacy.TurnOutcome{
			Incomplete: true, Detail: err.Error(),
		})
		return errors.Join(err, endErr)
	}
	if err := sink.sink.SpeechText(ctx, utterance, utterance.Text); err != nil {
		sink.deleteTurn(utterance.ID)
		endErr := sink.sink.SpeechEnd(ctx, utterance, action.Outcome{Reason: err.Error()})
		turnErr := sink.sink.TurnEnd(ctx, legacy.TurnOutcome{
			Incomplete: true, Detail: err.Error(),
		})
		return errors.Join(err, endErr, turnErr)
	}
	return nil
}

func (sink *sessionPlaybackSink) Audio(
	ctx context.Context, utterance action.Utterance, frame action.Frame,
) error {
	if sink == nil || sink.sink == nil {
		return errors.New("scenario conversation playback sink is unavailable")
	}
	sink.mu.Lock()
	turn, found := sink.turns[utterance.ID]
	sink.mu.Unlock()
	if !found || !turn.begun {
		return errors.New("scenario conversation playback audio has no begun utterance")
	}
	if turn.textOnly {
		return nil
	}
	return sink.sink.SpeechAudio(ctx, utterance, frame)
}

func (sink *sessionPlaybackSink) End(
	ctx context.Context, utterance action.Utterance, outcome action.Outcome,
) error {
	if sink == nil || sink.sink == nil {
		return errors.New("scenario conversation playback sink is unavailable")
	}
	sink.mu.Lock()
	turn, found := sink.turns[utterance.ID]
	if found {
		delete(sink.turns, utterance.ID)
	}
	sink.mu.Unlock()
	if !found || !turn.begun {
		return errors.New("scenario conversation playback end has no begun utterance")
	}
	speechErr := sink.sink.SpeechEnd(ctx, utterance, outcome)
	turnOutcome := legacy.TurnOutcome{}
	if !outcome.Completed {
		turnOutcome.Incomplete = true
		turnOutcome.Detail = outcome.Reason
	}
	turnErr := sink.sink.TurnEnd(ctx, turnOutcome)
	return errors.Join(speechErr, turnErr)
}

func (sink *sessionPlaybackSink) Reserve(utterance action.Utterance) error {
	if sink == nil || sink.sink == nil {
		return errors.New("scenario conversation playback sink is unavailable")
	}
	if utterance.ID == "" {
		return errors.New("scenario conversation playback reservation requires an utterance ID")
	}
	sink.mu.Lock()
	if _, duplicate := sink.turns[utterance.ID]; duplicate {
		sink.mu.Unlock()
		return errors.New("scenario conversation playback utterance is already reserved")
	}
	sink.mu.Unlock()
	if err := sink.sink.TurnBegin(sink.ctx); err != nil {
		return err
	}
	if reserving, ok := sink.sink.(legacy.SpeechReservationSink); ok {
		if err := reserving.SpeechReserved(sink.ctx, utterance); err != nil {
			endErr := sink.sink.TurnEnd(sink.ctx, legacy.TurnOutcome{
				Incomplete: true, Detail: err.Error(),
			})
			return errors.Join(err, endErr)
		}
	}
	sink.mu.Lock()
	if _, duplicate := sink.turns[utterance.ID]; duplicate {
		sink.mu.Unlock()
		if reserving, ok := sink.sink.(legacy.SpeechReservationSink); ok {
			reserving.SpeechReservationCancelled(sink.ctx, utterance)
		}
		endErr := sink.sink.TurnEnd(sink.ctx, legacy.TurnOutcome{
			Incomplete: true, Detail: "playback reservation identity raced with another turn",
		})
		return errors.Join(errors.New("scenario conversation playback reservation is duplicated"), endErr)
	}
	sink.turns[utterance.ID] = playbackTurn{
		utterance: utterance, textOnly: sink.presentation.textOnly(),
	}
	sink.mu.Unlock()
	return nil
}

func (sink *sessionPlaybackSink) CancelReservation(utterance action.Utterance) {
	if sink == nil || sink.sink == nil {
		return
	}
	sink.mu.Lock()
	turn, found := sink.turns[utterance.ID]
	if found && !turn.begun {
		delete(sink.turns, utterance.ID)
	}
	sink.mu.Unlock()
	if !found || turn.begun {
		return
	}
	if reserving, ok := sink.sink.(legacy.SpeechReservationSink); ok {
		reserving.SpeechReservationCancelled(sink.ctx, utterance)
	}
	_ = sink.sink.TurnEnd(sink.ctx, legacy.TurnOutcome{
		Incomplete: true, Detail: "speech reservation canceled before playback began",
	})
}

func (sink *sessionPlaybackSink) Close() error {
	if sink == nil || sink.sink == nil {
		return nil
	}
	sink.mu.Lock()
	turns := make([]playbackTurn, 0, len(sink.turns))
	for id, turn := range sink.turns {
		delete(sink.turns, id)
		turns = append(turns, turn)
	}
	sink.mu.Unlock()
	var joined error
	for _, turn := range turns {
		if !turn.begun {
			if reserving, ok := sink.sink.(legacy.SpeechReservationSink); ok {
				reserving.SpeechReservationCancelled(sink.ctx, turn.utterance)
			}
		} else {
			joined = errors.Join(joined, sink.sink.SpeechEnd(
				sink.ctx, turn.utterance, action.Outcome{Reason: "playback sink closed"},
			))
		}
		joined = errors.Join(joined, sink.sink.TurnEnd(sink.ctx, legacy.TurnOutcome{
			Incomplete: true, Detail: "playback sink closed",
		}))
	}
	return joined
}

func (sink *sessionPlaybackSink) deleteTurn(utteranceID string) {
	sink.mu.Lock()
	delete(sink.turns, utteranceID)
	sink.mu.Unlock()
}

var _ action.SpeechSink = (*sessionPlaybackSink)(nil)
var _ action.SpeechReservationSink = (*sessionPlaybackSink)(nil)
