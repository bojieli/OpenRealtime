package scenarioconversation

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	legacy "github.com/bojieli/OpenRealtime/binding"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	"github.com/bojieli/OpenRealtime/spoken"
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
	timing       spoken.TrackerConfig
	mu           sync.Mutex
	turns        map[string]playbackTurn
	released     map[string]playbackRelease
	releaseOrder []string
	closed       bool
}

type playbackTurn struct {
	utterance action.Utterance
	textOnly  bool
	begun     bool
	ended     bool
	outcome   action.Outcome
	turn      legacy.TurnOutcome
}

type playbackRelease struct {
	runID     string
	utterance action.Utterance
	outcome   action.Outcome
}

func newSessionPlaybackSink(
	ctx context.Context, sink legacy.Sink, descriptor v1.Descriptor,
	presentation *presentationState, timing ...spoken.TrackerConfig,
) *sessionPlaybackSink {
	descriptor.Capabilities = maps.Clone(descriptor.Capabilities)
	configured := spoken.TrackerConfig{}
	if len(timing) > 0 {
		configured = timing[0]
	}
	return &sessionPlaybackSink{
		ctx: ctx, sink: sink, descriptor: descriptor, presentation: presentation,
		timing: configured, turns: make(map[string]playbackTurn),
		released: make(map[string]playbackRelease),
	}
}

// PlaybackTiming gives speech.Playback the exact session-local aligner. The
// graph playback element owns the tracker because it sees both all synthesised
// audio and the frames that actually crossed the presentation boundary.
func (sink *sessionPlaybackSink) PlaybackTiming() spoken.TrackerConfig {
	if sink == nil {
		return spoken.TrackerConfig{}
	}
	return sink.timing
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
	valid := found && turn.begun && !turn.ended && samePlaybackUtterance(turn.utterance, utterance)
	if valid {
		turn.ended = true
		turn.outcome = outcome
		if !outcome.Completed {
			turn.turn = legacy.TurnOutcome{
				Incomplete: true, Detail: boundedAdapterReason(outcome.Reason),
			}
		}
		sink.turns[utterance.ID] = turn
	}
	sink.mu.Unlock()
	if !valid {
		return errors.New("scenario conversation playback end has no begun utterance")
	}
	speechErr := sink.sink.SpeechEnd(ctx, utterance, outcome)
	if speechErr == nil {
		// TurnEnd is intentionally withheld. The graph's barriered released
		// receipt is the externally visible permit for the next user turn.
		return nil
	}
	sink.mu.Lock()
	delete(sink.turns, utterance.ID)
	sink.rememberReleasedLocked(playbackRelease{utterance: clonePlaybackUtterance(utterance), outcome: outcome})
	sink.mu.Unlock()
	turnOutcome := legacy.TurnOutcome{Incomplete: true, Detail: boundedAdapterReason(speechErr.Error())}
	turnErr := sink.sink.TurnEnd(ctx, turnOutcome)
	return errors.Join(speechErr, turnErr)
}

// Release crosses the graph-authorized turn-completion boundary. Playback.End
// records the exact sink effect but cannot expose TurnEnd itself: doing so lets
// response.done overtake policy lifecycle retirement on independent graph
// lanes. The released receipt must match and consume that pending effect once.
func (sink *sessionPlaybackSink) Release(
	ctx context.Context, runID string, receipt speechelements.PlaybackReceipt,
) error {
	if sink == nil || sink.sink == nil {
		return errors.New("scenario conversation playback sink is unavailable")
	}
	if ctx == nil {
		return errors.New("scenario conversation playback release has nil context")
	}
	runID = strings.TrimSpace(runID)
	utteranceID := strings.TrimSpace(receipt.Utterance.ID)
	if !canonicalIdentity(runID) || !canonicalIdentity(utteranceID) {
		return errors.New("scenario conversation playback release requires exact run and utterance identities")
	}
	sink.mu.Lock()
	if previous, duplicate := sink.released[utteranceID]; duplicate {
		sink.mu.Unlock()
		if previous.runID != runID || !samePlaybackUtterance(previous.utterance, receipt.Utterance) ||
			previous.outcome != receipt.Outcome {
			return fmt.Errorf("scenario conversation playback release %q conflicts with its terminal receipt", utteranceID)
		}
		return fmt.Errorf("scenario conversation playback release %q was already consumed", utteranceID)
	}
	turn, found := sink.turns[utteranceID]
	if !found {
		sink.mu.Unlock()
		return fmt.Errorf("scenario conversation playback release %q has no pending sink outcome", utteranceID)
	}
	if !turn.begun || !turn.ended {
		sink.mu.Unlock()
		return fmt.Errorf("scenario conversation playback release %q arrived before sink completion", utteranceID)
	}
	if !samePlaybackUtterance(turn.utterance, receipt.Utterance) || turn.outcome != receipt.Outcome {
		sink.mu.Unlock()
		return fmt.Errorf("scenario conversation playback release %q changed its sink effect", utteranceID)
	}
	delete(sink.turns, utteranceID)
	sink.rememberReleasedLocked(playbackRelease{
		runID: runID, utterance: clonePlaybackUtterance(turn.utterance), outcome: turn.outcome,
	})
	turnOutcome := turn.turn
	sink.mu.Unlock()
	if err := sink.sink.TurnEnd(ctx, turnOutcome); err != nil {
		return fmt.Errorf("release scenario conversation playback turn %q: %w", utteranceID, err)
	}
	return nil
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
	if sink.closed {
		sink.mu.Unlock()
		return nil
	}
	sink.closed = true
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
		} else if !turn.ended {
			joined = errors.Join(joined, sink.sink.SpeechEnd(
				sink.ctx, turn.utterance, action.Outcome{Reason: "playback sink closed"},
			))
		}
		turnOutcome := turn.turn
		if !turnOutcome.Incomplete {
			turnOutcome = legacy.TurnOutcome{
				Incomplete: true, Detail: "playback release not observed before sink closed",
			}
		}
		joined = errors.Join(joined, sink.sink.TurnEnd(sink.ctx, turnOutcome))
	}
	return joined
}

func (sink *sessionPlaybackSink) rememberReleasedLocked(release playbackRelease) {
	if sink.released == nil {
		sink.released = make(map[string]playbackRelease)
	}
	id := release.utterance.ID
	if _, found := sink.released[id]; found {
		return
	}
	sink.released[id] = release
	sink.releaseOrder = append(sink.releaseOrder, id)
	if len(sink.releaseOrder) <= maximumAdapterMemory {
		return
	}
	oldest := sink.releaseOrder[0]
	sink.releaseOrder = sink.releaseOrder[1:]
	delete(sink.released, oldest)
}

func clonePlaybackUtterance(utterance action.Utterance) action.Utterance {
	utterance.AssistantItemIDs = slices.Clone(utterance.AssistantItemIDs)
	return utterance
}

func (sink *sessionPlaybackSink) deleteTurn(utteranceID string) {
	sink.mu.Lock()
	delete(sink.turns, utteranceID)
	sink.mu.Unlock()
}

var _ action.SpeechSink = (*sessionPlaybackSink)(nil)
var _ action.SpeechReservationSink = (*sessionPlaybackSink)(nil)
var _ speechelements.PlaybackTimingSource = (*sessionPlaybackSink)(nil)
