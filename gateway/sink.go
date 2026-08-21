package gateway

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
)

// The session implements binding.Sink: it renders what the runtime produced
// into the wire events a Realtime client expects. Nothing conversational is
// decided here.

func (session *session) Activity(_ context.Context, activity binding.ActivityEvent) error {
	if activity.Committed {
		return session.send(event("input_audio_buffer.committed", session.nextID("event"), map[string]any{
			"item_id": activity.ItemID, "previous_item_id": nil,
		}))
	}
	if activity.Started {
		return session.send(event("input_audio_buffer.speech_started", session.nextID("event"), map[string]any{
			"audio_start_ms": activity.AudioStartMS, "item_id": activity.ItemID,
		}))
	}
	if activity.Stopped {
		return session.send(event("input_audio_buffer.speech_stopped", session.nextID("event"), map[string]any{
			"audio_end_ms": activity.AudioEndMS, "item_id": activity.ItemID,
		}))
	}
	return nil
}

func (session *session) Transcript(_ context.Context, transcript binding.TranscriptEvent) error {
	if !transcript.Final {
		// Partial transcripts are runtime evidence. The base protocol has no
		// event for them, so they are not invented onto the wire: a client
		// sees the committed transcript, which is what it can rely on.
		return nil
	}
	if err := session.send(event("conversation.item.created", session.nextID("event"), map[string]any{
		"previous_item_id": nil, "item": userAudioItem(transcript.ItemID, transcript.Text),
	})); err != nil {
		return err
	}
	return session.send(event("conversation.item.input_audio_transcription.completed", session.nextID("event"), map[string]any{
		"item_id": transcript.ItemID, "content_index": 0, "transcript": transcript.Text,
		"usage": map[string]any{"type": "duration", "seconds": transcript.DurationSec},
	}))
}

// Observation reports what an observer perceived, if the client negotiated it.
func (session *session) Observation(_ context.Context, observation perception.Observation) error {
	session.settingsMu.RLock()
	enabled := hasFeature(session.settings.extension, openrealtime.FeatureObservations)
	session.settingsMu.RUnlock()
	if !enabled || observation.Provisional {
		return nil
	}
	return session.send(map[string]any{
		"type": openrealtime.EventObservationAdded, "event_id": session.nextID("event"),
		"observation_id": session.nextID("obs"), "observer": observation.Observer,
		"source": observation.Source, "text": observation.Text,
		"authority": string(observation.Authority), "timestamp_ms": int64(observation.OccurredNS / 1_000_000),
	})
}

// SpeechBegin adds this turn's spoken output item to the response.
//
// The response itself is opened by whatever crosses into the world first,
// which may be this or may be a function call: one response carries the whole
// turn, and the client is told it is done once.
func (session *session) SpeechBegin(ctx context.Context, utterance action.Utterance) error {
	session.settingsMu.RLock()
	format, voice := session.settings.outputFormat, session.settings.voice
	session.settingsMu.RUnlock()
	text := session.textOnly()

	// Claimed before the response is opened, not after. Opening it publishes
	// the response to every other goroutine, and a rollout finishing in that
	// window would find nothing outstanding and close a turn whose first
	// output had not been announced yet.
	session.startedSpeaking()
	responseID, index, _, err := session.openOutput()
	if err != nil {
		session.finishedSpeaking()
		return err
	}
	itemID := session.nextID("item")
	session.itemsMu.Lock()
	session.utterances[utterance.ID] = &wireUtterance{
		responseID: responseID, itemID: itemID, format: format, voice: voice,
		text: "", textOnly: text, outputIndex: index,
	}
	session.itemsMu.Unlock()

	if err := session.send(event("response.output_item.added", session.nextID("event"), map[string]any{
		"response_id": responseID, "output_index": index,
		"item": assistantItem(itemID, "in_progress", "", text),
	})); err != nil {
		return err
	}
	part := map[string]any{"type": "audio", "transcript": ""}
	if text {
		part = map[string]any{"type": "text", "text": ""}
	}
	if err := session.send(event("response.content_part.added", session.nextID("event"), map[string]any{
		"response_id": responseID, "item_id": itemID, "output_index": index, "content_index": 0,
		"part": part,
	})); err != nil {
		return err
	}
	_ = ctx
	return nil
}

// SpeechText renders one text delta: a transcript of the audio in an audio
// turn, and the answer itself in a text one.
func (session *session) SpeechText(_ context.Context, utterance action.Utterance, delta string) error {
	if delta == "" {
		return nil
	}
	session.itemsMu.Lock()
	wire := session.utterances[utterance.ID]
	if wire != nil {
		wire.text += delta
	}
	session.itemsMu.Unlock()
	if wire == nil {
		return fmt.Errorf("transcript for an unannounced utterance %q", utterance.ID)
	}
	eventType := "response.output_audio_transcript.delta"
	if wire.textOnly {
		eventType = "response.output_text.delta"
	}
	return session.send(event(eventType, session.nextID("event"), map[string]any{
		"response_id": wire.responseID, "item_id": wire.itemID,
		"output_index": wire.outputIndex, "content_index": 0, "delta": delta,
	}))
}

// SpeechAudio encodes one paced frame for the wire.
//
// Pacing already happened in the action plane, in duration. All that is left
// here is resampling and encoding into whatever format the client asked for,
// which is exactly the split that lets a WebRTC adapter use the same paced
// frames without going through mu-law.
func (session *session) SpeechAudio(_ context.Context, utterance action.Utterance, frame action.Frame) error {
	session.itemsMu.Lock()
	wire := session.utterances[utterance.ID]
	session.itemsMu.Unlock()
	if wire == nil {
		return fmt.Errorf("audio for an unannounced utterance %q", utterance.ID)
	}
	if wire.encoder == nil {
		encoder, err := newOutputEncoder(wire.format, frame.SampleRateHz)
		if err != nil {
			return err
		}
		wire.encoder, wire.sourceRate = encoder, frame.SampleRateHz
	}
	if frame.SampleRateHz != wire.sourceRate {
		return errors.New("speech sample rate changed within one response")
	}
	encoded, err := wire.encoder.Push(frame.PCM16LE, frame.Final)
	if err != nil {
		return err
	}
	if len(encoded) == 0 {
		return nil
	}
	session.config.Metrics.audioFramesOut.Add(1)
	return session.send(event("response.output_audio.delta", session.nextID("event"), map[string]any{
		"response_id": wire.responseID, "item_id": wire.itemID,
		"output_index": wire.outputIndex, "content_index": 0,
		"delta": base64.StdEncoding.EncodeToString(encoded),
	}))
}

func (session *session) SpeechEnd(_ context.Context, utterance action.Utterance, outcome action.Outcome) error {
	session.itemsMu.Lock()
	wire := session.utterances[utterance.ID]
	delete(session.utterances, utterance.ID)
	session.itemsMu.Unlock()
	if wire == nil {
		return nil
	}
	itemStatus := "completed"
	if !outcome.Completed {
		itemStatus = "incomplete"
	}
	terminal := assistantItem(wire.itemID, itemStatus, wire.text, wire.textOnly)
	messages := []map[string]any{
		event("response.output_audio.done", session.nextID("event"), map[string]any{
			"response_id": wire.responseID, "item_id": wire.itemID,
			"output_index": wire.outputIndex, "content_index": 0,
		}),
		event("response.output_audio_transcript.done", session.nextID("event"), map[string]any{
			"response_id": wire.responseID, "item_id": wire.itemID,
			"output_index": wire.outputIndex, "content_index": 0, "transcript": wire.text,
		}),
		event("response.content_part.done", session.nextID("event"), map[string]any{
			"response_id": wire.responseID, "item_id": wire.itemID,
			"output_index": wire.outputIndex, "content_index": 0,
			"part": map[string]any{"type": "audio", "transcript": wire.text},
		}),
	}
	if wire.textOnly {
		// No audio was produced, so nothing announces the end of audio. A
		// client that saw response.output_audio.done on a text turn would be
		// told about a stream it never received.
		messages = []map[string]any{
			event("response.output_text.done", session.nextID("event"), map[string]any{
				"response_id": wire.responseID, "item_id": wire.itemID,
				"output_index": wire.outputIndex, "content_index": 0, "text": wire.text,
			}),
			event("response.content_part.done", session.nextID("event"), map[string]any{
				"response_id": wire.responseID, "item_id": wire.itemID,
				"output_index": wire.outputIndex, "content_index": 0,
				"part": map[string]any{"type": "text", "text": wire.text},
			}),
		}
	}
	messages = append(messages, event("response.output_item.done", session.nextID("event"), map[string]any{
		"response_id": wire.responseID, "output_index": wire.outputIndex, "item": terminal,
	}))
	for _, message := range messages {
		if err := session.send(message); err != nil {
			return err
		}
	}
	session.recordOutput(terminal, outcome.Completed)
	session.finishedSpeaking()
	return session.closeIfComplete(context.Background())
}

// ToolCalls hands authoritative calls to the client as ordinary function
// calling. Computer-use actions travel this same path and add no protocol.
//
// They are output items of the turn that produced them, not a turn of their
// own: a client that was told the response was done before the calls arrived
// would have stopped reading exactly where the work was.
func (session *session) ToolCalls(_ context.Context, calls binding.ToolCallEvent) error {
	session.config.Metrics.toolCallsOut.Add(uint64(len(calls.Calls)))
	session.recordCallNames(calls.Calls)
	responseID, _, _, err := session.openOutput()
	if err != nil {
		return err
	}
	// The first call took the index openOutput handed out; the rest claim
	// their own. Every one of them is an output item of this turn.
	session.rewindOutputIndex()
	for _, call := range calls.Calls {
		index := session.claimOutputIndex()
		itemID := session.nextID("item")
		if err := session.send(event("response.output_item.added", session.nextID("event"), map[string]any{
			"response_id": responseID, "output_index": index,
			"item": functionCallItem(itemID, "in_progress", call),
		})); err != nil {
			return err
		}
		if err := session.send(event("response.function_call_arguments.delta", session.nextID("event"), map[string]any{
			"response_id": responseID, "item_id": itemID, "output_index": index,
			"call_id": call.CallID, "delta": string(call.Arguments),
		})); err != nil {
			return err
		}
		if err := session.send(event("response.function_call_arguments.done", session.nextID("event"), map[string]any{
			"response_id": responseID, "item_id": itemID, "output_index": index,
			"call_id": call.CallID, "name": call.Name, "arguments": string(call.Arguments),
		})); err != nil {
			return err
		}
		completed := functionCallItem(itemID, "completed", call)
		if err := session.send(event("response.output_item.done", session.nextID("event"), map[string]any{
			"response_id": responseID, "output_index": index, "item": completed,
		})); err != nil {
			return err
		}
		session.recordOutput(completed, true)
	}
	session.recordUsage(calls.Usage)
	return nil
}

func (session *session) Failed(_ context.Context, failure binding.ErrorEvent) {
	session.sendError(failure.Code, failure.Message)
}

func hasFeature(response openrealtime.Response, feature openrealtime.Feature) bool {
	for _, enabled := range response.Enabled {
		if enabled == feature {
			return true
		}
	}
	return false
}

var _ binding.Sink = (*session)(nil)

// A response is a turn.
//
// The protocol's contract is one response per response.create, carrying every
// output item the turn produced, indexed within it. A client that has been
// told a response is done stops reading it, so rendering each output kind as
// its own response ends the turn - from the client's point of view - at
// whichever kind happened to come first. An agent that spoke and then called a
// tool would have its calls arrive after the client had already moved on.
//
// When the turn is over is not when the rollout returns. Speech is
// deliberately asynchronous: the rollout decides what to say, hands it to the
// planner, and returns while the audio is still being paced out over seconds.
// So a response closes when both are done - the rollout has finished planning
// and every utterance it opened has finished playing - which is what the
// outstanding count is for. Closing at the rollout's return would tell a client
// the turn was complete while it was still receiving the audio.
//
// The response is opened by whatever crosses into the world first, so a turn
// that produced nothing announces nothing: a deferred batch with no plan is
// not a response with no output.

// TurnBegin declares that a rollout is about to run.
func (session *session) TurnBegin(context.Context) error {
	session.responseMu.Lock()
	defer session.responseMu.Unlock()
	session.planning = true
	return nil
}

// TurnEnd reports that the rollout finished planning. The response closes here
// only if nothing it started is still playing.
func (session *session) TurnEnd(ctx context.Context) error {
	session.responseMu.Lock()
	session.planning = false
	session.responseMu.Unlock()
	return session.closeIfComplete(ctx)
}

// closeIfComplete ends the response once the rollout has finished planning and
// every utterance it opened has finished.
func (session *session) closeIfComplete(context.Context) error {
	session.responseMu.Lock()
	current := session.response
	if current == nil || session.planning || session.outstanding > 0 {
		session.responseMu.Unlock()
		return nil
	}
	session.response = nil
	session.responseMu.Unlock()

	session.settingsMu.RLock()
	format, voice := session.settings.outputFormat, session.settings.voice
	session.settingsMu.RUnlock()
	status := "completed"
	if current.cancelled {
		status = "cancelled"
	}
	done := responseObject(current.id, status, session.conversationID, current.output,
		current.usage, format, voice, session.outputModalities())
	if current.cancelled {
		done["status_details"] = map[string]any{"type": "cancelled", "reason": "turn_detected"}
	}
	return session.send(event("response.done", session.nextID("event"), map[string]any{"response": done}))
}

// openOutput returns the response this turn's output belongs to, creating it
// on the first thing that crosses into the world.
func (session *session) openOutput() (string, int, bool, error) {
	session.responseMu.Lock()
	created := false
	if session.response == nil {
		session.response = &wireResponse{id: session.nextID("resp")}
		created = true
	}
	current := session.response
	index := current.nextIndex
	current.nextIndex++
	id := current.id
	session.responseMu.Unlock()

	if !created {
		return id, index, false, nil
	}
	session.settingsMu.RLock()
	format, voice := session.settings.outputFormat, session.settings.voice
	session.settingsMu.RUnlock()
	if err := session.send(event("response.created", session.nextID("event"), map[string]any{
		"response": responseObject(id, "in_progress", session.conversationID, nil, nil,
			format, voice, session.outputModalities()),
	})); err != nil {
		return "", 0, true, err
	}
	return id, index, true, nil
}

// claimOutputIndex takes the next output slot in the open response.
// rewindOutputIndex gives back the slot openOutput reserved, for a caller that
// numbers its own items.
func (session *session) rewindOutputIndex() {
	session.responseMu.Lock()
	defer session.responseMu.Unlock()
	if session.response != nil && session.response.nextIndex > 0 {
		session.response.nextIndex--
	}
}

func (session *session) claimOutputIndex() int {
	session.responseMu.Lock()
	defer session.responseMu.Unlock()
	if session.response == nil {
		return 0
	}
	index := session.response.nextIndex
	session.response.nextIndex++
	return index
}

// recordOutput remembers a completed item so response.done can list it.
func (session *session) recordOutput(item map[string]any, completed bool) {
	session.responseMu.Lock()
	defer session.responseMu.Unlock()
	if session.response == nil {
		return
	}
	session.response.output = append(session.response.output, item)
	if !completed {
		session.response.cancelled = true
	}
}

func (session *session) recordUsage(usage *continuation.Usage) {
	if usage == nil {
		return
	}
	session.responseMu.Lock()
	defer session.responseMu.Unlock()
	if session.response != nil {
		session.response.usage = usage
	}
}

// startedSpeaking and finishedSpeaking track output the turn is still
// producing after the rollout that planned it returned.
func (session *session) startedSpeaking() {
	session.responseMu.Lock()
	defer session.responseMu.Unlock()
	session.outstanding++
}

func (session *session) finishedSpeaking() {
	session.responseMu.Lock()
	if session.outstanding > 0 {
		session.outstanding--
	}
	session.responseMu.Unlock()
}
