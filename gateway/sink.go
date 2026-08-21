package gateway

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
)

// The session implements binding.Sink: it renders what the runtime produced
// into the wire events a Realtime client expects. Nothing conversational is
// decided here.

func (session *session) Activity(_ context.Context, activity binding.ActivityEvent) error {
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

func (session *session) SpeechBegin(_ context.Context, utterance action.Utterance) error {
	session.settingsMu.RLock()
	format, voice := session.settings.outputFormat, session.settings.voice
	session.settingsMu.RUnlock()

	responseID := session.nextID("resp")
	itemID := session.nextID("item")
	session.itemsMu.Lock()
	session.utterances[utterance.ID] = &wireUtterance{
		responseID: responseID, itemID: itemID, format: format, voice: voice, text: utterance.Text,
	}
	session.itemsMu.Unlock()

	if err := session.send(event("response.created", session.nextID("event"), map[string]any{
		"response": responseObject(responseID, "in_progress", session.conversationID, nil, nil, format, voice),
	})); err != nil {
		return err
	}
	if err := session.send(event("response.output_item.added", session.nextID("event"), map[string]any{
		"response_id": responseID, "output_index": 0, "item": assistantItem(itemID, "in_progress", ""),
	})); err != nil {
		return err
	}
	if err := session.send(event("response.content_part.added", session.nextID("event"), map[string]any{
		"response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "audio", "transcript": ""},
	})); err != nil {
		return err
	}
	return session.send(event("response.output_audio_transcript.delta", session.nextID("event"), map[string]any{
		"response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0,
		"delta": utterance.Text,
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
		"response_id": wire.responseID, "item_id": wire.itemID, "output_index": 0, "content_index": 0,
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
	status, itemStatus := "completed", "completed"
	if !outcome.Completed {
		status, itemStatus = "cancelled", "incomplete"
	}
	terminal := assistantItem(wire.itemID, itemStatus, wire.text)
	for _, message := range []map[string]any{
		event("response.output_audio.done", session.nextID("event"), map[string]any{
			"response_id": wire.responseID, "item_id": wire.itemID, "output_index": 0, "content_index": 0,
		}),
		event("response.output_audio_transcript.done", session.nextID("event"), map[string]any{
			"response_id": wire.responseID, "item_id": wire.itemID, "output_index": 0, "content_index": 0,
			"transcript": wire.text,
		}),
		event("response.content_part.done", session.nextID("event"), map[string]any{
			"response_id": wire.responseID, "item_id": wire.itemID, "output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "audio", "transcript": wire.text},
		}),
		event("response.output_item.done", session.nextID("event"), map[string]any{
			"response_id": wire.responseID, "output_index": 0, "item": terminal,
		}),
	} {
		if err := session.send(message); err != nil {
			return err
		}
	}
	done := responseObject(wire.responseID, status, session.conversationID, []map[string]any{terminal}, nil, wire.format, wire.voice)
	if status == "cancelled" {
		done["status_details"] = map[string]any{"type": "cancelled", "reason": "turn_detected"}
	}
	return session.send(event("response.done", session.nextID("event"), map[string]any{"response": done}))
}

// ToolCalls hands authoritative calls to the client as ordinary function
// calling. Computer-use actions travel this same path and add no protocol.
func (session *session) ToolCalls(_ context.Context, calls binding.ToolCallEvent) error {
	session.config.Metrics.toolCallsOut.Add(uint64(len(calls.Calls)))
	session.recordCallNames(calls.Calls)
	session.settingsMu.RLock()
	format, voice := session.settings.outputFormat, session.settings.voice
	session.settingsMu.RUnlock()

	responseID := session.nextID("resp")
	if err := session.send(event("response.created", session.nextID("event"), map[string]any{
		"response": responseObject(responseID, "in_progress", session.conversationID, nil, nil, format, voice),
	})); err != nil {
		return err
	}
	output := make([]map[string]any, 0, len(calls.Calls))
	for index, call := range calls.Calls {
		itemID := session.nextID("item")
		if err := session.send(event("response.output_item.added", session.nextID("event"), map[string]any{
			"response_id": responseID, "output_index": index, "item": functionCallItem(itemID, "in_progress", call),
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
		output = append(output, completed)
	}
	return session.send(event("response.done", session.nextID("event"), map[string]any{
		"response": responseObject(responseID, "completed", session.conversationID, output, calls.Usage, format, voice),
	}))
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
