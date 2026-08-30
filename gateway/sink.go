package gateway

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

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
	name := "vad.activity"
	phase := "instant"
	if activity.Started {
		name, phase = "vad.speech_started", "start"
	} else if activity.Stopped {
		name, phase = "vad.speech_stopped", "end"
	} else if activity.Committed {
		name, phase = "vad.client_committed", "end"
	}
	_ = session.Debug(session.ctx, binding.DebugEvent{
		Category: string(openrealtime.DebugVAD), Name: name, Phase: phase,
		CorrelationID: activity.ItemID, Attributes: map[string]any{
			"audio_start_ms": activity.AudioStartMS, "audio_end_ms": activity.AudioEndMS,
		},
	})
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
	_ = session.Debug(session.ctx, binding.DebugEvent{
		Category: string(openrealtime.DebugASR), Name: "asr.transcript", Phase: map[bool]string{true: "end", false: "update"}[transcript.Final],
		CorrelationID: transcript.ItemID, Attributes: map[string]any{
			"final": transcript.Final, "duration_seconds": transcript.DurationSec,
		}, Payload: map[string]any{"text": transcript.Text},
	})
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

// Debug renders an optional binding trace into the negotiated developer event
// stream. No negotiation means no event and no observable change for ordinary
// clients.
func (session *session) Debug(_ context.Context, entry binding.DebugEvent) error {
	session.settingsMu.RLock()
	config := session.settings.extension.Debug
	session.settingsMu.RUnlock()
	if config == nil || !config.Enabled {
		return nil
	}
	category := openrealtime.DebugCategory(entry.Category)
	if !slices.Contains(config.Categories, category) {
		return nil
	}
	debug := openrealtime.DebugEvent{
		Type: openrealtime.EventDebug, EventID: session.nextID("event"),
		TimestampMS: time.Now().UnixMilli(), Category: category,
		Name: entry.Name, Phase: entry.Phase, DurationMS: entry.DurationMS,
		CorrelationID: entry.CorrelationID, Message: entry.Message,
		Attributes: entry.Attributes,
	}
	if config.IncludePayloads {
		debug.Payload = entry.Payload
	} else if len(entry.Payload) > 0 {
		if debug.Attributes == nil {
			debug.Attributes = map[string]any{}
		}
		debug.Attributes["payloads_redacted"] = true
	}
	return session.send(map[string]any{
		"type": debug.Type, "event_id": debug.EventID, "timestamp_ms": debug.TimestampMS,
		"category": debug.Category, "name": debug.Name, "phase": debug.Phase,
		"duration_ms": debug.DurationMS, "correlation_id": debug.CorrelationID,
		"message": debug.Message, "attributes": debug.Attributes, "payload": debug.Payload,
	})
}

// SpeechBegin adds this turn's spoken output item to the response.
//
// The response itself is opened by whatever crosses into the world first,
// which may be this or may be a function call: one response carries the whole
// turn, and the client is told it is done once.
func (session *session) SpeechBegin(ctx context.Context, utterance action.Utterance) error {
	startedAt := time.Now()
	_ = session.Debug(ctx, binding.DebugEvent{
		Category: string(openrealtime.DebugTTS), Name: "tts.utterance_started", Phase: "start",
		CorrelationID: utterance.ID, Payload: map[string]any{"text": utterance.Text},
	})
	session.settingsMu.RLock()
	format, voice := session.settings.outputFormat, session.settings.voice
	session.settingsMu.RUnlock()
	text := session.textOnly()

	// Consume the queue reservation before the response is opened. Bindings
	// that produce speech synchronously may not reserve, so this also claims an
	// ordinary outstanding slot for them. Either way the count exists before
	// opening publishes the response to another goroutine.
	session.startedSpeaking(utterance.ID)
	responseID, index, _, err := session.openOutput()
	if err != nil {
		session.finishedSpeaking()
		return err
	}
	itemID := session.nextID("item")
	session.itemsMu.Lock()
	session.utterances[utterance.ID] = &wireUtterance{
		responseID: responseID, itemID: itemID, format: format, voice: voice,
		text: "", textOnly: text, outputIndex: index, startedAt: startedAt,
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

// SpeechReserved keeps a response open for speech the action plane accepted
// even before its serial worker begins synthesis. It opens the response now so
// the client can distinguish queued work from conversational quiet, but it
// announces no output item until SpeechBegin: a reservation is lifecycle
// state, not content that crossed into the world.
func (session *session) SpeechReserved(_ context.Context, utterance action.Utterance) error {
	if strings.TrimSpace(utterance.ID) == "" {
		return errors.New("speech reservation requires an utterance ID")
	}
	session.responseMu.Lock()
	if _, duplicate := session.reservations[utterance.ID]; duplicate {
		session.responseMu.Unlock()
		return fmt.Errorf("speech %q is already reserved", utterance.ID)
	}
	session.reservations[utterance.ID] = struct{}{}
	session.outstanding++
	if session.response != nil {
		session.responseMu.Unlock()
		return nil
	}

	// response.created is the protocol's only durable indication that work is
	// still owed. A slow synthesiser can otherwise leave the wire quiet long
	// enough for a meeting client to end the session before SpeechBegin. Keep
	// the announcement under the response lock so it remains the first event
	// even when a tool call and the speech worker race to publish output.
	current := &wireResponse{id: session.nextID("resp")}
	session.response = current
	session.settingsMu.RLock()
	format, voice := session.settings.outputFormat, session.settings.voice
	session.settingsMu.RUnlock()
	err := session.send(event("response.created", session.nextID("event"), map[string]any{
		"response": responseObject(current.id, "in_progress", session.conversationID, nil, nil,
			format, voice, session.outputModalities()),
	}))
	if err != nil {
		session.response = nil
		delete(session.reservations, utterance.ID)
		if session.outstanding > 0 {
			session.outstanding--
		}
	}
	session.responseMu.Unlock()
	return err
}

// SpeechReservationCancelled releases speech discarded before SpeechBegin.
// Once Begin consumes the typed reservation, SpeechEnd owns the release and a
// late cancellation notification is harmless.
func (session *session) SpeechReservationCancelled(_ context.Context, utterance action.Utterance) {
	session.responseMu.Lock()
	if _, reserved := session.reservations[utterance.ID]; reserved {
		delete(session.reservations, utterance.ID)
		if session.outstanding > 0 {
			session.outstanding--
		}
	}
	session.responseMu.Unlock()
	_ = session.closeIfComplete(context.Background())
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
func (session *session) SpeechAudio(ctx context.Context, utterance action.Utterance, frame action.Frame) error {
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
	first := wire.frameBytes == 0
	wire.frameBytes += len(encoded)
	if first {
		_ = session.Debug(ctx, binding.DebugEvent{
			Category: string(openrealtime.DebugTTS), Name: "tts.first_audio", Phase: "update",
			DurationMS:    float64(time.Since(wire.startedAt)) / float64(time.Millisecond),
			CorrelationID: utterance.ID, Attributes: map[string]any{
				"sample_rate_hz": frame.SampleRateHz, "encoded_bytes": len(encoded),
			},
		})
	}
	session.config.Metrics.audioFramesOut.Add(1)
	return session.send(event("response.output_audio.delta", session.nextID("event"), map[string]any{
		"response_id": wire.responseID, "item_id": wire.itemID,
		"output_index": wire.outputIndex, "content_index": 0,
		"delta": base64.StdEncoding.EncodeToString(encoded),
	}))
}

func (session *session) SpeechEnd(ctx context.Context, utterance action.Utterance, outcome action.Outcome) error {
	session.itemsMu.Lock()
	wire := session.utterances[utterance.ID]
	delete(session.utterances, utterance.ID)
	session.itemsMu.Unlock()
	if wire == nil {
		return nil
	}
	_ = session.Debug(ctx, binding.DebugEvent{
		Category: string(openrealtime.DebugTTS), Name: "tts.utterance_completed", Phase: "end",
		DurationMS:    float64(time.Since(wire.startedAt)) / float64(time.Millisecond),
		CorrelationID: utterance.ID, Attributes: map[string]any{
			"completed": outcome.Completed, "played_ms": outcome.PlayedMS,
			"encoded_bytes": wire.frameBytes, "text_only": wire.textOnly,
		},
	})
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
func (session *session) ToolCalls(ctx context.Context, calls binding.ToolCallEvent) error {
	prepared, err := session.prepareClientEffectCalls(ctx, calls.Calls)
	if err != nil {
		return err
	}
	session.config.Metrics.toolCallsOut.Add(uint64(len(calls.Calls)))
	session.recordCallNames(calls.Calls)
	for _, emission := range prepared {
		call := emission.call
		_ = session.Debug(ctx, binding.DebugEvent{
			Category: string(openrealtime.DebugTool), Name: "tool.call.emitted", Phase: "start",
			CorrelationID: call.CallID, Attributes: map[string]any{
				"name": call.Name, "invocation_id": calls.InvocationID,
			}, Payload: map[string]any{"arguments": string(call.Arguments)},
		})
	}
	responseID, first, _, err := session.openOutput()
	if err != nil {
		return err
	}
	// The first call takes the index openOutput already handed out and the
	// rest claim their own. Giving it back and re-claiming would be a race:
	// a concurrent utterance opening the same response could take the slot in
	// between, and two output items would carry the same index.
	for offset, emission := range prepared {
		call := emission.call
		index := first
		if offset > 0 {
			index = session.claimOutputIndex()
		}
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
		done := map[string]any{
			"response_id": responseID, "item_id": itemID, "output_index": index,
			"call_id": call.CallID, "name": call.Name, "arguments": string(call.Arguments),
		}
		if emission.extension != nil {
			done["openrealtime"] = map[string]any{"client_effect": *emission.extension}
		}
		if err := session.send(event("response.function_call_arguments.done", session.nextID("event"), done)); err != nil {
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

var _ binding.DebugSink = (*session)(nil)

func hasFeature(response openrealtime.Response, feature openrealtime.Feature) bool {
	for _, enabled := range response.Enabled {
		if enabled == feature {
			return true
		}
	}
	return false
}

var _ binding.Sink = (*session)(nil)

// A response is one thing the agent did, not the whole turn.
//
// One rollout produces one response, carrying every output item that rollout
// produced - spoken or written content and function calls, indexed within it.
// A turn can span several, and normally does: the voice answers in one, the
// background reasoner's tool calls arrive in another, and what it found is
// spoken in a third once the gate lets anything be heard.
//
// This comment used to argue the opposite - that a turn had to be a single
// response, because a client told a response is done stops reading it. That
// reasoning does not hold. Audio reaches a client on the audio channel rather
// than inside a response envelope, and a client executing a tool reads
// function_call items as they arrive; a response being done means that
// response has no more items, not that the session has stopped producing them.
// Held to, the rule would keep a response open across an unbounded
// deliberation, so the first answer could not complete until the last one did.
//
// When a response is over is still not when the rollout returns. Speech is
// deliberately asynchronous: the rollout decides what to say, hands it to the
// planner, and returns while the audio is still being paced out over seconds.
// So a response closes when both are done - planning has finished and every
// utterance it opened has finished playing - which is what the outstanding
// count is for. Closing at the rollout's return would tell a client the
// response was complete while it was still receiving the audio.
//
// The response is opened by whatever crosses into the world first, so a turn
// that produced nothing announces nothing: a deferred batch with no plan is
// not a response with no output.
//
// The exception is a turn that produced nothing because something went wrong.
// That one opens a response in order to close it as incomplete, because the
// two silences are not the same event: having nothing to add is the runtime
// working, and being cut off mid-thought is a client left waiting for a turn
// that is never coming.

// TurnBegin declares that a rollout is about to run.
func (session *session) TurnBegin(context.Context) error {
	session.responseMu.Lock()
	defer session.responseMu.Unlock()
	session.planning = true
	return nil
}

// TurnEnd reports that the rollout finished planning, and why it stopped when
// that is not evident from what it produced. The response closes here only if
// nothing it started is still playing.
func (session *session) TurnEnd(ctx context.Context, outcome binding.TurnOutcome) error {
	session.responseMu.Lock()
	session.planning = false
	if outcome.Incomplete {
		session.incomplete = &outcome
	}
	session.responseMu.Unlock()
	if outcome.Detail != "" {
		// The operator is who can act on this. A fast provider that spent its
		// whole output budget deliberating produces a session that opens, says
		// nothing, and closes - a configuration mistake with no symptom, which
		// is the failure this line exists to remove.
		session.config.Logger.Warn("turn produced no speech",
			"session", session.id, "reason", outcome.Reason, "detail", outcome.Detail)
	}
	return session.closeIfComplete(ctx)
}

// closeIfComplete ends the response once the rollout has finished planning and
// every utterance it opened has finished.
func (session *session) closeIfComplete(context.Context) error {
	session.responseMu.Lock()
	current := session.response
	if session.planning || session.outstanding > 0 {
		session.responseMu.Unlock()
		return nil
	}
	incomplete := session.incomplete
	if current == nil && incomplete == nil {
		session.responseMu.Unlock()
		return nil
	}
	session.incomplete = nil
	opened := false
	if current == nil {
		// Nothing crossed into the world, so no response was ever opened. One
		// is opened now for the sole purpose of reporting that the turn was
		// cut short: announcing it and completing it in the same breath is
		// still the whole story, and it is more than silence was telling.
		current = &wireResponse{id: session.nextID("resp")}
		opened = true
	}
	session.response = nil
	session.responseMu.Unlock()

	session.settingsMu.RLock()
	format, voice := session.settings.outputFormat, session.settings.voice
	session.settingsMu.RUnlock()
	status := "completed"
	if incomplete != nil {
		status = "incomplete"
	}
	if current.cancelled {
		status = "cancelled"
	}
	if opened {
		if err := session.send(event("response.created", session.nextID("event"), map[string]any{
			"response": responseObject(current.id, "in_progress", session.conversationID, nil, nil,
				format, voice, session.outputModalities()),
		})); err != nil {
			return err
		}
	}
	done := responseObject(current.id, status, session.conversationID, current.output,
		current.usage, format, voice, session.outputModalities())
	switch {
	case current.cancelled:
		done["status_details"] = map[string]any{"type": "cancelled", "reason": "turn_detected"}
	case incomplete != nil:
		details := map[string]any{"type": "incomplete"}
		if incomplete.Reason != "" {
			details["reason"] = incomplete.Reason
		}
		done["status_details"] = details
	}
	return session.send(event("response.done", session.nextID("event"), map[string]any{"response": done}))
}

// openOutput returns the response this turn's output belongs to, creating it
// on the first thing that crosses into the world.
func (session *session) openOutput() (string, int, bool, error) {
	// The announcement happens under the same lock that installs the response,
	// because response.created has to be the first event of the response it
	// announces. Two output paths can open one turn - a spoken answer and the
	// calls that follow it, on different goroutines - and if the lock were
	// released between installing and announcing, the second path could put
	// its first item on the wire ahead of the response it belongs to.
	session.responseMu.Lock()
	defer session.responseMu.Unlock()
	if session.response == nil {
		session.response = &wireResponse{id: session.nextID("resp")}
		session.settingsMu.RLock()
		format, voice := session.settings.outputFormat, session.settings.voice
		session.settingsMu.RUnlock()
		id := session.response.id
		if err := session.send(event("response.created", session.nextID("event"), map[string]any{
			"response": responseObject(id, "in_progress", session.conversationID, nil, nil,
				format, voice, session.outputModalities()),
		})); err != nil {
			session.response = nil
			return "", 0, true, err
		}
		index := session.response.nextIndex
		session.response.nextIndex++
		return id, index, true, nil
	}
	current := session.response
	index := current.nextIndex
	current.nextIndex++
	return current.id, index, false, nil
}

// claimOutputIndex takes the next output slot in the open response.
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

// startedSpeaking consumes a queued reservation or, for a binding that emits
// synchronously, claims a fresh outstanding slot. finishedSpeaking releases
// that slot after the utterance ends.
func (session *session) startedSpeaking(utteranceID string) {
	session.responseMu.Lock()
	defer session.responseMu.Unlock()
	if _, reserved := session.reservations[utteranceID]; reserved {
		delete(session.reservations, utteranceID)
		return
	}
	session.outstanding++
}

func (session *session) finishedSpeaking() {
	session.responseMu.Lock()
	if session.outstanding > 0 {
		session.outstanding--
	}
	session.responseMu.Unlock()
}
