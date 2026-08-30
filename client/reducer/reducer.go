// Package reducer implements the framework-neutral state machine shared by
// realtime clients. It deliberately owns protocol state, not transports,
// devices, effects, or views. Those adapters feed operations in and execute
// the returned OpenAI Realtime commands.
package reducer

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	FormatVersion  = 1
	MaxCorpusBytes = 1 << 20
)

// Limits are part of the corpus format so every language applies the same
// resource ceilings before reducing attacker-controlled protocol messages.
type Limits struct {
	MaxVectors            int   `json:"max_vectors"`
	MaxStepsPerVector     int   `json:"max_steps_per_vector"`
	MaxEventBytes         int   `json:"max_event_bytes"`
	MaxStringBytes        int   `json:"max_string_bytes"`
	MaxConversationItems  int   `json:"max_conversation_items"`
	MaxToolCalls          int   `json:"max_tool_calls"`
	MaxOutboundEvents     int   `json:"max_outbound_events"`
	MaxProtocolLogEntries int   `json:"max_protocol_log_entries"`
	MaxErrorsPerVector    int   `json:"max_errors_per_vector"`
	MaxReconnectAttempts  int   `json:"max_reconnect_attempts"`
	MaxVirtualTimeMS      int64 `json:"max_virtual_time_ms"`
}

func DefaultLimits() Limits {
	return Limits{
		MaxVectors:            64,
		MaxStepsPerVector:     512,
		MaxEventBytes:         64 << 10,
		MaxStringBytes:        16 << 10,
		MaxConversationItems:  1_024,
		MaxToolCalls:          256,
		MaxOutboundEvents:     2_048,
		MaxProtocolLogEntries: 4_096,
		MaxErrorsPerVector:    128,
		MaxReconnectAttempts:  3,
		MaxVirtualTimeMS:      86_400_000,
	}
}

// Operation is the portable adapter boundary. Fields not used by Kind are
// rejected; this prevents a malformed vector from being interpreted
// differently by permissive decoders in another language.
type Operation struct {
	Kind      string          `json:"kind"`
	Transport string          `json:"transport,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	Event     json.RawMessage `json:"event,omitempty"`
	Session   json.RawMessage `json:"session,omitempty"`
	ItemID    string          `json:"item_id,omitempty"`
	Text      string          `json:"text,omitempty"`
	Speaking  bool            `json:"speaking,omitempty"`
	PlayedMS  int64           `json:"played_ms,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Status    string          `json:"status,omitempty"`
	Output    string          `json:"output,omitempty"`
	Error     string          `json:"error,omitempty"`
	present   map[string]bool
}

func (operation *Operation) UnmarshalJSON(data []byte) error {
	if err := strictjson.Validate(data); err != nil {
		return err
	}
	type operationAlias Operation
	var decoded operationAlias
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*operation = Operation(decoded)
	operation.present = make(map[string]bool, len(fields))
	for name := range fields {
		operation.present[name] = true
	}
	return nil
}

type ConnectionSnapshot struct {
	Phase       string `json:"phase"`
	Transport   string `json:"transport"`
	Reason      string `json:"reason"`
	Attempt     int    `json:"attempt"`
	NextRetryMS int64  `json:"next_retry_ms"`
}

type VideoSnapshot struct {
	Format        string `json:"format"`
	FPSCap        int    `json:"fps_cap"`
	MaxDimension  int    `json:"max_dimension"`
	MaxFrameBytes int    `json:"max_frame_bytes"`
}

type NegotiationSnapshot struct {
	Present            bool          `json:"present"`
	Version            int           `json:"version"`
	Enabled            []string      `json:"enabled"`
	Observers          []string      `json:"observers"`
	AvailableObservers []string      `json:"available_observers"`
	DebugEnabled       bool          `json:"debug_enabled"`
	Video              VideoSnapshot `json:"video"`
}

type SessionSnapshot struct {
	ID           string              `json:"id"`
	ManualTurns  bool                `json:"manual_turns"`
	OpenRealtime NegotiationSnapshot `json:"openrealtime"`
}

type ResponseSnapshot struct {
	ID     string `json:"id"`
	Open   bool   `json:"open"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

type ConversationItem struct {
	ItemID      string `json:"item_id"`
	Role        string `json:"role"`
	Channel     string `json:"channel"`
	Text        string `json:"text"`
	AudioDeltas int    `json:"audio_deltas"`
	AudioDone   bool   `json:"audio_done"`
	TruncatedMS int64  `json:"truncated_ms"`
}

type PlayoutSnapshot struct {
	Speaking bool   `json:"speaking"`
	ItemID   string `json:"item_id"`
	PlayedMS int64  `json:"played_ms"`
}

type ToolCall struct {
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
	Output    string `json:"output"`
	Error     string `json:"error"`
}

type TruncationSnapshot struct {
	ItemID     string `json:"item_id"`
	AudioEndMS int64  `json:"audio_end_ms"`
}

type ProtocolEntry struct {
	AtMS      int64  `json:"at_ms"`
	Direction string `json:"direction"`
	Type      string `json:"type"`
}

type Snapshot struct {
	NowMS          int64              `json:"now_ms"`
	Connection     ConnectionSnapshot `json:"connection"`
	Session        SessionSnapshot    `json:"session"`
	Response       ResponseSnapshot   `json:"response"`
	Conversation   []ConversationItem `json:"conversation"`
	Playout        PlayoutSnapshot    `json:"playout"`
	Tools          []ToolCall         `json:"tools"`
	LastTruncation TruncationSnapshot `json:"last_truncation"`
	LastError      string             `json:"last_error"`
	ProtocolLog    []ProtocolEntry    `json:"protocol_log"`
}

type Reducer struct {
	limits         Limits
	nowMS          int64
	connection     ConnectionSnapshot
	session        SessionSnapshot
	response       ResponseSnapshot
	conversation   []ConversationItem
	playout        PlayoutSnapshot
	tools          []ToolCall
	lastTruncation TruncationSnapshot
	lastError      string
	protocolLog    []ProtocolEntry
	outbound       []json.RawMessage
}

func New(limits Limits) (*Reducer, error) {
	if err := validateLimits(limits); err != nil {
		return nil, err
	}
	return &Reducer{
		limits:       limits,
		connection:   ConnectionSnapshot{Phase: "disconnected"},
		session:      emptySession(),
		conversation: []ConversationItem{},
		tools:        []ToolCall{},
		protocolLog:  []ProtocolEntry{},
		outbound:     []json.RawMessage{},
	}, nil
}

func validateLimits(limits Limits) error {
	if limits.MaxVectors <= 0 || limits.MaxStepsPerVector <= 0 ||
		limits.MaxEventBytes <= 0 || limits.MaxStringBytes <= 0 ||
		limits.MaxConversationItems <= 0 || limits.MaxToolCalls <= 0 ||
		limits.MaxOutboundEvents <= 0 || limits.MaxProtocolLogEntries <= 0 ||
		limits.MaxErrorsPerVector <= 0 || limits.MaxReconnectAttempts <= 0 ||
		limits.MaxVirtualTimeMS <= 0 {
		return errors.New("all reducer limits must be positive")
	}
	if limits.MaxEventBytes > MaxCorpusBytes || limits.MaxStringBytes > limits.MaxEventBytes {
		return errors.New("reducer byte limits exceed the corpus envelope")
	}
	return nil
}

func emptySession() SessionSnapshot {
	return SessionSnapshot{OpenRealtime: NegotiationSnapshot{
		Enabled: []string{}, Observers: []string{}, AvailableObservers: []string{},
	}}
}

func (reducer *Reducer) Apply(atMS int64, operation Operation) error {
	if reducer == nil {
		return errors.New("reducer is nil")
	}
	if atMS < reducer.nowMS {
		return fmt.Errorf("virtual time moved backwards from %d to %d", reducer.nowMS, atMS)
	}
	if atMS > reducer.limits.MaxVirtualTimeMS {
		return fmt.Errorf("virtual time %d exceeds limit %d", atMS, reducer.limits.MaxVirtualTimeMS)
	}
	if err := reducer.validateOperation(operation); err != nil {
		return err
	}
	next := reducer.clone()
	next.nowMS = atMS
	if err := next.apply(operation); err != nil {
		return err
	}
	*reducer = *next
	return nil
}

func (reducer *Reducer) apply(operation Operation) error {
	switch operation.Kind {
	case "connect":
		return reducer.connect(operation.Transport)
	case "connected":
		return reducer.connected()
	case "transport_lost":
		return reducer.transportLost(operation.Reason)
	case "retry":
		return reducer.retry()
	case "disconnect":
		return reducer.disconnect(operation.Reason)
	case "inbound":
		return reducer.inbound(operation.Event)
	case "session_update":
		return reducer.sessionUpdate(operation.Session)
	case "typed_text":
		return reducer.typedText(operation.ItemID, operation.Text)
	case "end_turn":
		return reducer.endTurn()
	case "playout":
		reducer.playout = PlayoutSnapshot{
			Speaking: operation.Speaking, ItemID: operation.ItemID, PlayedMS: operation.PlayedMS,
		}
		return nil
	case "cancel_response":
		return reducer.cancelResponse()
	case "tool_result":
		return reducer.toolResult(operation)
	default:
		return fmt.Errorf("unknown operation kind %q", operation.Kind)
	}
}

func (reducer *Reducer) validateOperation(operation Operation) error {
	if err := boundedString("operation kind", operation.Kind, reducer.limits.MaxStringBytes, true); err != nil {
		return err
	}
	stringsToCheck := []struct {
		name  string
		value string
	}{
		{name: "transport", value: operation.Transport},
		{name: "reason", value: operation.Reason},
		{name: "item_id", value: operation.ItemID},
		{name: "text", value: operation.Text},
		{name: "call_id", value: operation.CallID},
		{name: "status", value: operation.Status},
		{name: "output", value: operation.Output},
		{name: "error", value: operation.Error},
	}
	for _, field := range stringsToCheck {
		if err := boundedString(field.name, field.value, reducer.limits.MaxStringBytes, false); err != nil {
			return err
		}
	}
	if len(operation.Event) > reducer.limits.MaxEventBytes || len(operation.Session) > reducer.limits.MaxEventBytes {
		return fmt.Errorf("operation JSON exceeds %d bytes", reducer.limits.MaxEventBytes)
	}
	if operation.PlayedMS < 0 {
		return errors.New("played_ms must not be negative")
	}
	used := func(names ...string) map[string]bool {
		result := map[string]bool{}
		for _, name := range names {
			result[name] = true
		}
		return result
	}
	var allowed map[string]bool
	switch operation.Kind {
	case "connect":
		allowed = used("transport")
	case "connected", "retry", "end_turn", "cancel_response":
		allowed = used()
	case "transport_lost", "disconnect":
		allowed = used("reason")
	case "inbound":
		allowed = used("event")
	case "session_update":
		allowed = used("session")
	case "typed_text":
		allowed = used("item_id", "text")
	case "playout":
		allowed = used("item_id", "speaking", "played_ms")
	case "tool_result":
		allowed = used("call_id", "status", "output", "error")
	default:
		return fmt.Errorf("unknown operation kind %q", operation.Kind)
	}
	present := []struct {
		name     string
		inferred bool
	}{
		{name: "transport", inferred: operation.Transport != ""},
		{name: "reason", inferred: operation.Reason != ""},
		{name: "event", inferred: len(operation.Event) != 0},
		{name: "session", inferred: len(operation.Session) != 0},
		{name: "item_id", inferred: operation.ItemID != ""},
		{name: "text", inferred: operation.Text != ""},
		{name: "speaking", inferred: operation.Speaking},
		{name: "played_ms", inferred: operation.PlayedMS != 0},
		{name: "call_id", inferred: operation.CallID != ""},
		{name: "status", inferred: operation.Status != ""},
		{name: "output", inferred: operation.Output != ""},
		{name: "error", inferred: operation.Error != ""},
	}
	for _, field := range present {
		exists := field.inferred
		if operation.present != nil {
			exists = operation.present[field.name]
		}
		if exists && !allowed[field.name] {
			return fmt.Errorf("operation %q does not allow field %q", operation.Kind, field.name)
		}
	}
	return nil
}

func (reducer *Reducer) connect(transport string) error {
	if reducer.connection.Phase != "disconnected" && reducer.connection.Phase != "failed" {
		return fmt.Errorf("cannot connect while connection is %s", reducer.connection.Phase)
	}
	if transport != "websocket" && transport != "webrtc" {
		return fmt.Errorf("unsupported transport %q", transport)
	}
	reducer.connection = ConnectionSnapshot{Phase: "connecting", Transport: transport}
	reducer.lastError = ""
	return nil
}

func (reducer *Reducer) connected() error {
	if reducer.connection.Phase != "connecting" {
		return fmt.Errorf("cannot become connected while connection is %s", reducer.connection.Phase)
	}
	reducer.connection.Phase = "connected"
	reducer.connection.Reason = ""
	reducer.connection.Attempt = 0
	reducer.connection.NextRetryMS = 0
	reducer.lastError = ""
	return nil
}

func (reducer *Reducer) transportLost(reason string) error {
	if reducer.connection.Phase != "connected" && reducer.connection.Phase != "connecting" {
		return fmt.Errorf("transport cannot be lost while connection is %s", reducer.connection.Phase)
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("transport loss requires a reason")
	}
	reducer.cleanupSession(reason)
	attempt := reducer.connection.Attempt
	if attempt == 0 {
		attempt = 1
	} else if reducer.connection.Phase == "connecting" {
		if attempt >= reducer.limits.MaxReconnectAttempts {
			reducer.connection.Phase = "failed"
			reducer.connection.Reason = reason
			reducer.connection.NextRetryMS = 0
			reducer.lastError = reason
			return nil
		}
		attempt++
	}
	reducer.connection.Phase = "reconnecting"
	reducer.connection.Reason = reason
	reducer.connection.Attempt = attempt
	reducer.connection.NextRetryMS = reducer.nowMS + retryDelay(attempt)
	return nil
}

func retryDelay(attempt int) int64 {
	delays := [...]int64{250, 1_000, 4_000}
	if attempt <= 0 {
		return delays[0]
	}
	if attempt > len(delays) {
		return delays[len(delays)-1]
	}
	return delays[attempt-1]
}

func (reducer *Reducer) retry() error {
	if reducer.connection.Phase != "reconnecting" {
		return fmt.Errorf("cannot retry while connection is %s", reducer.connection.Phase)
	}
	if reducer.nowMS < reducer.connection.NextRetryMS {
		return fmt.Errorf("retry at %d precedes deadline %d", reducer.nowMS, reducer.connection.NextRetryMS)
	}
	reducer.connection.Phase = "connecting"
	reducer.connection.NextRetryMS = 0
	return nil
}

func (reducer *Reducer) disconnect(reason string) error {
	if reason == "" {
		reason = "disconnected"
	}
	reducer.cleanupSession(reason)
	reducer.connection = ConnectionSnapshot{Phase: "disconnected", Reason: reason}
	return nil
}

func (reducer *Reducer) cleanupSession(reason string) {
	if reducer.response.Open {
		reducer.response.Open = false
		reducer.response.Status = "abandoned"
		reducer.response.Reason = reason
	}
	reducer.playout = PlayoutSnapshot{}
	for index := range reducer.tools {
		if reducer.tools[index].Status == "pending" {
			reducer.tools[index].Status = "failed"
			reducer.tools[index].Error = reason
		}
	}
	reducer.session = emptySession()
}

func (reducer *Reducer) sessionUpdate(session json.RawMessage) error {
	if err := reducer.requireConnected(); err != nil {
		return err
	}
	object, err := strictObject(session, reducer.limits.MaxEventBytes, "session update")
	if err != nil {
		return err
	}
	if raw, ok := object["type"]; ok {
		value, err := decodeString(raw, "session.type")
		if err != nil {
			return err
		}
		if value != "realtime" {
			return fmt.Errorf("session.type must be %q", "realtime")
		}
	}
	return reducer.emit(map[string]any{"type": "session.update", "session": json.RawMessage(session)})
}

func (reducer *Reducer) typedText(itemID, text string) error {
	if err := reducer.requireConnected(); err != nil {
		return err
	}
	itemID = strings.TrimSpace(itemID)
	text = strings.TrimSpace(text)
	if itemID == "" || text == "" {
		return errors.New("typed_text requires non-empty item_id and text")
	}
	if reducer.findConversation(itemID, "input_text") >= 0 {
		return fmt.Errorf("conversation item %q already exists", itemID)
	}
	if len(reducer.conversation) >= reducer.limits.MaxConversationItems {
		return errors.New("conversation item limit reached")
	}
	commands := []any{
		map[string]any{
			"type": "conversation.item.create",
			"item": map[string]any{
				"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": text}},
			},
		},
		map[string]any{"type": "response.create"},
	}
	if err := reducer.emitMany(commands...); err != nil {
		return err
	}
	reducer.conversation = append(reducer.conversation, ConversationItem{
		ItemID: itemID, Role: "user", Channel: "input_text", Text: text,
	})
	return nil
}

func (reducer *Reducer) endTurn() error {
	if err := reducer.requireConnected(); err != nil {
		return err
	}
	if !reducer.session.ManualTurns {
		return errors.New("end_turn requires negotiated manual turn detection")
	}
	return reducer.emitMany(
		map[string]any{"type": "input_audio_buffer.commit"},
		map[string]any{"type": "response.create"},
	)
}

func (reducer *Reducer) cancelResponse() error {
	if err := reducer.requireConnected(); err != nil {
		return err
	}
	if !reducer.response.Open {
		return errors.New("no response is open")
	}
	if reducer.response.Status == "cancelling" {
		return errors.New("response cancellation is already pending")
	}
	if err := reducer.emit(responseCancel(reducer.response.ID)); err != nil {
		return err
	}
	reducer.response.Status = "cancelling"
	return nil
}

func responseCancel(responseID string) map[string]any {
	event := map[string]any{"type": "response.cancel"}
	if responseID != "" {
		event["response_id"] = responseID
	}
	return event
}

func (reducer *Reducer) inbound(raw json.RawMessage) error {
	if err := reducer.requireConnected(); err != nil {
		return err
	}
	object, err := strictObject(raw, reducer.limits.MaxEventBytes, "inbound event")
	if err != nil {
		return err
	}
	eventType, err := requiredString(object, "type", reducer.limits.MaxStringBytes)
	if err != nil {
		return fmt.Errorf("inbound event: %w", err)
	}
	if len(reducer.protocolLog) >= reducer.limits.MaxProtocolLogEntries {
		return errors.New("protocol log limit reached")
	}
	reducer.protocolLog = append(reducer.protocolLog, ProtocolEntry{
		AtMS: reducer.nowMS, Direction: "in", Type: eventType,
	})
	switch eventType {
	case "session.created":
		return reducer.sessionCreated(object)
	case "session.updated":
		return reducer.sessionUpdated(object)
	case "input_audio_buffer.speech_started":
		return reducer.speechStarted()
	case "input_audio_buffer.speech_stopped":
		return nil
	case "conversation.item.input_audio_transcription.completed":
		return reducer.inputTranscription(object)
	case "response.created":
		return reducer.responseCreated(object)
	case "response.output_audio_transcript.delta":
		return reducer.responseTextDelta(object, "output_audio_transcript")
	case "response.output_text.delta":
		return reducer.responseTextDelta(object, "output_text")
	case "response.output_audio.delta":
		return reducer.responseAudioDelta(object)
	case "response.output_audio.done":
		return reducer.responseAudioDone(object)
	case "response.function_call_arguments.done":
		return reducer.functionCall(object)
	case "response.done":
		return reducer.responseDone(object)
	case "conversation.item.truncated":
		return reducer.itemTruncated(object)
	case "openrealtime.observation.added":
		return reducer.observationAdded(object)
	case "openrealtime.debug.event":
		return nil
	case "error":
		return reducer.serverError(object)
	default:
		// OpenAI and OpenRealtime both require forward-compatible clients to
		// ignore event types they do not understand. The ordered log retains
		// the type without retaining potentially sensitive payloads.
		return nil
	}
}

func (reducer *Reducer) sessionCreated(event map[string]json.RawMessage) error {
	session, err := requiredObject(event, "session", reducer.limits.MaxEventBytes)
	if err != nil {
		return err
	}
	id, err := requiredString(session, "id", reducer.limits.MaxStringBytes)
	if err != nil {
		return fmt.Errorf("session.created: %w", err)
	}
	reducer.session.ID = id
	return nil
}

func (reducer *Reducer) sessionUpdated(event map[string]json.RawMessage) error {
	session, err := requiredObject(event, "session", reducer.limits.MaxEventBytes)
	if err != nil {
		return fmt.Errorf("session.updated: %w", err)
	}
	if rawID, ok := session["id"]; ok {
		id, err := decodeString(rawID, "session.id")
		if err != nil {
			return err
		}
		if err := boundedString("session.id", id, reducer.limits.MaxStringBytes, false); err != nil {
			return err
		}
		reducer.session.ID = id
	}
	reducer.session.ManualTurns = false
	if audioRaw, ok := session["audio"]; ok {
		audio, err := decodeObject(audioRaw, "session.audio")
		if err != nil {
			return err
		}
		if inputRaw, ok := audio["input"]; ok {
			input, err := decodeObject(inputRaw, "session.audio.input")
			if err != nil {
				return err
			}
			if detection, ok := input["turn_detection"]; ok && bytes.Equal(bytes.TrimSpace(detection), []byte("null")) {
				reducer.session.ManualTurns = true
			}
		}
	}
	negotiation := NegotiationSnapshot{
		Enabled: []string{}, Observers: []string{}, AvailableObservers: []string{},
	}
	if extensionRaw, ok := session["openrealtime"]; ok && !bytes.Equal(bytes.TrimSpace(extensionRaw), []byte("null")) {
		extension, err := decodeObject(extensionRaw, "session.openrealtime")
		if err != nil {
			return err
		}
		negotiation.Present = true
		if raw, ok := extension["version"]; ok {
			negotiation.Version, err = decodeInt(raw, "session.openrealtime.version", 0)
			if err != nil {
				return err
			}
		}
		if negotiation.Enabled, err = optionalStrings(extension, "enabled", reducer.limits); err != nil {
			return err
		}
		if negotiation.Observers, err = optionalStrings(extension, "observers", reducer.limits); err != nil {
			return err
		}
		if negotiation.AvailableObservers, err = optionalStrings(extension, "available_observers", reducer.limits); err != nil {
			return err
		}
		if debugRaw, ok := extension["debug"]; ok && !bytes.Equal(bytes.TrimSpace(debugRaw), []byte("null")) {
			debug, err := decodeObject(debugRaw, "session.openrealtime.debug")
			if err != nil {
				return err
			}
			if enabledRaw, ok := debug["enabled"]; ok {
				if err := json.Unmarshal(enabledRaw, &negotiation.DebugEnabled); err != nil {
					return fmt.Errorf("session.openrealtime.debug.enabled: %w", err)
				}
			}
		}
		if videoRaw, ok := extension["video"]; ok && !bytes.Equal(bytes.TrimSpace(videoRaw), []byte("null")) {
			video, err := decodeObject(videoRaw, "session.openrealtime.video")
			if err != nil {
				return err
			}
			if raw, ok := video["format"]; ok {
				negotiation.Video.Format, err = decodeString(raw, "session.openrealtime.video.format")
				if err != nil {
					return err
				}
			}
			integerFields := []struct {
				name   string
				target *int
			}{
				{name: "fps_cap", target: &negotiation.Video.FPSCap},
				{name: "max_dimension", target: &negotiation.Video.MaxDimension},
				{name: "max_frame_bytes", target: &negotiation.Video.MaxFrameBytes},
			}
			for _, field := range integerFields {
				if raw, ok := video[field.name]; ok {
					*field.target, err = decodeInt(raw, "session.openrealtime.video."+field.name, 0)
					if err != nil {
						return err
					}
				}
			}
		}
	}
	reducer.session.OpenRealtime = negotiation
	return nil
}

func (reducer *Reducer) speechStarted() error {
	if !reducer.playout.Speaking {
		return nil
	}
	if reducer.playout.ItemID == "" {
		return errors.New("active playout has no item_id")
	}
	commands := []any{}
	if reducer.response.Open && reducer.response.Status != "cancelling" {
		commands = append(commands, responseCancel(reducer.response.ID))
	}
	if reducer.response.Open && reducer.connection.Transport == "webrtc" {
		commands = append(commands, map[string]any{"type": "output_audio_buffer.clear"})
	}
	commands = append(commands, map[string]any{
		"type": "conversation.item.truncate", "item_id": reducer.playout.ItemID,
		"content_index": 0, "audio_end_ms": reducer.playout.PlayedMS,
	})
	if err := reducer.emitMany(commands...); err != nil {
		return err
	}
	if reducer.response.Open {
		reducer.response.Status = "cancelling"
	}
	reducer.playout = PlayoutSnapshot{}
	return nil
}

func (reducer *Reducer) inputTranscription(event map[string]json.RawMessage) error {
	itemID, err := requiredString(event, "item_id", reducer.limits.MaxStringBytes)
	if err != nil {
		return err
	}
	transcript, err := requiredString(event, "transcript", reducer.limits.MaxStringBytes)
	if err != nil {
		return err
	}
	return reducer.setConversation(itemID, "user", "input_audio_transcript", transcript, false)
}

func (reducer *Reducer) responseCreated(event map[string]json.RawMessage) error {
	if reducer.response.Open {
		return fmt.Errorf("response %q is already open", reducer.response.ID)
	}
	response, err := requiredObject(event, "response", reducer.limits.MaxEventBytes)
	if err != nil {
		return err
	}
	id, err := requiredString(response, "id", reducer.limits.MaxStringBytes)
	if err != nil {
		return fmt.Errorf("response.created: %w", err)
	}
	status := "in_progress"
	if raw, ok := response["status"]; ok {
		status, err = decodeString(raw, "response.status")
		if err != nil {
			return err
		}
	}
	reducer.response = ResponseSnapshot{ID: id, Open: true, Status: status}
	return nil
}

func (reducer *Reducer) responseTextDelta(event map[string]json.RawMessage, channel string) error {
	if !reducer.response.Open {
		return errors.New("response delta arrived without an open response")
	}
	itemID, err := requiredString(event, "item_id", reducer.limits.MaxStringBytes)
	if err != nil {
		return err
	}
	delta, err := requiredStringAllowEmpty(event, "delta", reducer.limits.MaxStringBytes)
	if err != nil {
		return err
	}
	return reducer.appendConversation(itemID, "assistant", channel, delta)
}

func (reducer *Reducer) responseAudioDelta(event map[string]json.RawMessage) error {
	if !reducer.response.Open {
		return errors.New("audio delta arrived without an open response")
	}
	itemID, err := requiredString(event, "item_id", reducer.limits.MaxStringBytes)
	if err != nil {
		return err
	}
	delta, err := requiredString(event, "delta", reducer.limits.MaxStringBytes)
	if err != nil {
		return err
	}
	if _, err := base64.StdEncoding.DecodeString(delta); err != nil {
		return fmt.Errorf("response audio delta is not base64: %w", err)
	}
	index := reducer.findConversation(itemID, "output_audio_transcript")
	if index < 0 {
		if len(reducer.conversation) >= reducer.limits.MaxConversationItems {
			return errors.New("conversation item limit reached")
		}
		reducer.conversation = append(reducer.conversation, ConversationItem{
			ItemID: itemID, Role: "assistant", Channel: "output_audio_transcript", AudioDeltas: 1,
		})
		return nil
	}
	reducer.conversation[index].AudioDeltas++
	return nil
}

func (reducer *Reducer) responseAudioDone(event map[string]json.RawMessage) error {
	itemID, err := requiredString(event, "item_id", reducer.limits.MaxStringBytes)
	if err != nil {
		return err
	}
	index := reducer.findConversation(itemID, "output_audio_transcript")
	if index < 0 {
		if len(reducer.conversation) >= reducer.limits.MaxConversationItems {
			return errors.New("conversation item limit reached")
		}
		reducer.conversation = append(reducer.conversation, ConversationItem{
			ItemID: itemID, Role: "assistant", Channel: "output_audio_transcript", AudioDone: true,
		})
		return nil
	}
	reducer.conversation[index].AudioDone = true
	return nil
}

func (reducer *Reducer) functionCall(event map[string]json.RawMessage) error {
	callID, err := requiredString(event, "call_id", reducer.limits.MaxStringBytes)
	if err != nil {
		return err
	}
	name, err := requiredString(event, "name", reducer.limits.MaxStringBytes)
	if err != nil {
		return err
	}
	arguments, err := requiredStringAllowEmpty(event, "arguments", reducer.limits.MaxStringBytes)
	if err != nil {
		return err
	}
	if reducer.findTool(callID) >= 0 {
		return fmt.Errorf("tool call %q already exists", callID)
	}
	if len(reducer.tools) >= reducer.limits.MaxToolCalls {
		return errors.New("tool call limit reached")
	}
	reducer.tools = append(reducer.tools, ToolCall{
		CallID: callID, Name: name, Arguments: arguments, Status: "pending",
	})
	return nil
}

func (reducer *Reducer) toolResult(operation Operation) error {
	if err := reducer.requireConnected(); err != nil {
		return err
	}
	index := reducer.findTool(operation.CallID)
	if index < 0 {
		return fmt.Errorf("unknown tool call %q", operation.CallID)
	}
	if reducer.tools[index].Status != "pending" {
		return fmt.Errorf("tool call %q is already %s", operation.CallID, reducer.tools[index].Status)
	}
	wireOutput := operation.Output
	switch operation.Status {
	case "done":
		if operation.Error != "" {
			return errors.New("a completed tool result cannot contain error")
		}
	case "failed", "declined":
		if operation.Error == "" {
			return fmt.Errorf("%s tool result requires error", operation.Status)
		}
		encoded, err := json.Marshal(map[string]string{"error": operation.Error})
		if err != nil {
			return err
		}
		wireOutput = string(encoded)
	default:
		return fmt.Errorf("unknown tool result status %q", operation.Status)
	}
	command := map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "function_call_output", "call_id": operation.CallID, "output": wireOutput,
		},
	}
	// OpenAI commits the function output as a conversation item and resumes
	// inference with a separate response.create. Emit the pair atomically so a
	// transport cannot leave the model waiting after recording the result.
	if err := reducer.emitMany(command, map[string]any{"type": "response.create"}); err != nil {
		return err
	}
	reducer.tools[index].Status = operation.Status
	reducer.tools[index].Output = operation.Output
	reducer.tools[index].Error = operation.Error
	return nil
}

func (reducer *Reducer) responseDone(event map[string]json.RawMessage) error {
	response, err := requiredObject(event, "response", reducer.limits.MaxEventBytes)
	if err != nil {
		return err
	}
	id, err := requiredString(response, "id", reducer.limits.MaxStringBytes)
	if err != nil {
		return err
	}
	status, err := requiredString(response, "status", reducer.limits.MaxStringBytes)
	if err != nil {
		return err
	}
	if reducer.response.Open && reducer.response.ID != "" && reducer.response.ID != id {
		return fmt.Errorf("response.done for %q while %q is open", id, reducer.response.ID)
	}
	reason := ""
	if detailsRaw, ok := response["status_details"]; ok && !bytes.Equal(bytes.TrimSpace(detailsRaw), []byte("null")) {
		details, err := decodeObject(detailsRaw, "response.status_details")
		if err != nil {
			return err
		}
		if raw, ok := details["reason"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			reason, err = decodeString(raw, "response.status_details.reason")
			if err != nil {
				return err
			}
		}
	}
	reducer.response = ResponseSnapshot{ID: id, Status: status, Reason: reason}
	return nil
}

func (reducer *Reducer) itemTruncated(event map[string]json.RawMessage) error {
	itemID, err := requiredString(event, "item_id", reducer.limits.MaxStringBytes)
	if err != nil {
		return err
	}
	audioEndMS, err := requiredInt64(event, "audio_end_ms", 0)
	if err != nil {
		return err
	}
	reducer.lastTruncation = TruncationSnapshot{ItemID: itemID, AudioEndMS: audioEndMS}
	for index := range reducer.conversation {
		if reducer.conversation[index].ItemID == itemID {
			reducer.conversation[index].TruncatedMS = audioEndMS
		}
	}
	return nil
}

func (reducer *Reducer) observationAdded(event map[string]json.RawMessage) error {
	itemID, err := requiredString(event, "observation_id", reducer.limits.MaxStringBytes)
	if err != nil {
		return err
	}
	observer, err := requiredString(event, "observer", reducer.limits.MaxStringBytes)
	if err != nil {
		return err
	}
	text, err := requiredStringAllowEmpty(event, "text", reducer.limits.MaxStringBytes)
	if err != nil {
		return err
	}
	channel := "observation." + observer
	if raw, ok := event["source"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		source, err := decodeString(raw, "source")
		if err != nil {
			return err
		}
		if source != "" {
			channel = "observation." + source
		}
	}
	return reducer.setConversation(itemID, "observation", channel, text, true)
}

func (reducer *Reducer) serverError(event map[string]json.RawMessage) error {
	errorObject, err := requiredObject(event, "error", reducer.limits.MaxEventBytes)
	if err != nil {
		return err
	}
	message, err := requiredString(errorObject, "message", reducer.limits.MaxStringBytes)
	if err != nil {
		return err
	}
	reducer.lastError = message
	return nil
}

func (reducer *Reducer) setConversation(itemID, role, channel, text string, replace bool) error {
	index := reducer.findConversation(itemID, channel)
	if index >= 0 {
		if !replace {
			return fmt.Errorf("conversation item %q channel %q already exists", itemID, channel)
		}
		reducer.conversation[index].Text = text
		return nil
	}
	if len(reducer.conversation) >= reducer.limits.MaxConversationItems {
		return errors.New("conversation item limit reached")
	}
	reducer.conversation = append(reducer.conversation, ConversationItem{
		ItemID: itemID, Role: role, Channel: channel, Text: text,
	})
	return nil
}

func (reducer *Reducer) appendConversation(itemID, role, channel, delta string) error {
	index := reducer.findConversation(itemID, channel)
	if index < 0 {
		if len(reducer.conversation) >= reducer.limits.MaxConversationItems {
			return errors.New("conversation item limit reached")
		}
		reducer.conversation = append(reducer.conversation, ConversationItem{
			ItemID: itemID, Role: role, Channel: channel, Text: delta,
		})
		return nil
	}
	if len(reducer.conversation[index].Text)+len(delta) > reducer.limits.MaxStringBytes {
		return fmt.Errorf("conversation text exceeds %d bytes", reducer.limits.MaxStringBytes)
	}
	reducer.conversation[index].Text += delta
	return nil
}

func (reducer *Reducer) findConversation(itemID, channel string) int {
	return slices.IndexFunc(reducer.conversation, func(item ConversationItem) bool {
		return item.ItemID == itemID && item.Channel == channel
	})
}

func (reducer *Reducer) findTool(callID string) int {
	return slices.IndexFunc(reducer.tools, func(call ToolCall) bool { return call.CallID == callID })
}

func (reducer *Reducer) requireConnected() error {
	if reducer.connection.Phase != "connected" {
		return fmt.Errorf("operation requires connected state, got %s", reducer.connection.Phase)
	}
	return nil
}

func (reducer *Reducer) emit(events any) error {
	return reducer.emitMany(events)
}

func (reducer *Reducer) emitMany(events ...any) error {
	if len(reducer.outbound)+len(events) > reducer.limits.MaxOutboundEvents {
		return errors.New("outbound event limit reached")
	}
	if len(reducer.protocolLog)+len(events) > reducer.limits.MaxProtocolLogEntries {
		return errors.New("protocol log limit reached")
	}
	encoded := make([]json.RawMessage, 0, len(events))
	entries := make([]ProtocolEntry, 0, len(events))
	for _, event := range events {
		raw, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("encode outbound event: %w", err)
		}
		if len(raw) > reducer.limits.MaxEventBytes {
			return fmt.Errorf("outbound event exceeds %d bytes", reducer.limits.MaxEventBytes)
		}
		object, err := strictObject(raw, reducer.limits.MaxEventBytes, "outbound event")
		if err != nil {
			return err
		}
		eventType, err := requiredString(object, "type", reducer.limits.MaxStringBytes)
		if err != nil {
			return err
		}
		encoded = append(encoded, raw)
		entries = append(entries, ProtocolEntry{AtMS: reducer.nowMS, Direction: "out", Type: eventType})
	}
	reducer.outbound = append(reducer.outbound, encoded...)
	reducer.protocolLog = append(reducer.protocolLog, entries...)
	return nil
}

func (reducer *Reducer) Snapshot() Snapshot {
	if reducer == nil {
		return Snapshot{}
	}
	result := Snapshot{
		NowMS: reducer.nowMS, Connection: reducer.connection, Session: reducer.session,
		Response: reducer.response, Playout: reducer.playout,
		LastTruncation: reducer.lastTruncation, LastError: reducer.lastError,
	}
	result.Session.OpenRealtime.Enabled = slices.Clone(reducer.session.OpenRealtime.Enabled)
	result.Session.OpenRealtime.Observers = slices.Clone(reducer.session.OpenRealtime.Observers)
	result.Session.OpenRealtime.AvailableObservers = slices.Clone(reducer.session.OpenRealtime.AvailableObservers)
	result.Conversation = slices.Clone(reducer.conversation)
	result.Tools = slices.Clone(reducer.tools)
	result.ProtocolLog = slices.Clone(reducer.protocolLog)
	return result
}

func (reducer *Reducer) Outbound() []json.RawMessage {
	if reducer == nil {
		return nil
	}
	result := make([]json.RawMessage, len(reducer.outbound))
	for index := range reducer.outbound {
		result[index] = bytes.Clone(reducer.outbound[index])
	}
	return result
}

func (reducer *Reducer) clone() *Reducer {
	copy := *reducer
	copy.session.OpenRealtime.Enabled = slices.Clone(reducer.session.OpenRealtime.Enabled)
	copy.session.OpenRealtime.Observers = slices.Clone(reducer.session.OpenRealtime.Observers)
	copy.session.OpenRealtime.AvailableObservers = slices.Clone(reducer.session.OpenRealtime.AvailableObservers)
	copy.conversation = slices.Clone(reducer.conversation)
	copy.tools = slices.Clone(reducer.tools)
	copy.protocolLog = slices.Clone(reducer.protocolLog)
	// Previously encoded commands are immutable. Clone only the slice header
	// for transactional append; deep-copying every payload on every operation
	// would turn the published outbound bound into quadratic byte copying.
	copy.outbound = slices.Clone(reducer.outbound)
	return &copy
}

func strictObject(raw []byte, maximum int, name string) (map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%s is empty", name)
	}
	if len(raw) > maximum {
		return nil, fmt.Errorf("%s exceeds %d bytes", name, maximum)
	}
	if err := strictjson.Validate(raw); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return decodeObject(raw, name)
}

func decodeObject(raw []byte, name string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil {
		return nil, fmt.Errorf("%s must be a JSON object: %w", name, err)
	}
	if object == nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, fmt.Errorf("%s must be a JSON object", name)
	}
	return object, nil
}

func requiredObject(object map[string]json.RawMessage, key string, maximum int) (map[string]json.RawMessage, error) {
	raw, ok := object[key]
	if !ok {
		return nil, fmt.Errorf("missing %s", key)
	}
	if len(raw) > maximum {
		return nil, fmt.Errorf("%s exceeds %d bytes", key, maximum)
	}
	return decodeObject(raw, key)
}

func requiredString(object map[string]json.RawMessage, key string, maximum int) (string, error) {
	value, err := requiredStringAllowEmpty(object, key, maximum)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", fmt.Errorf("%s must not be empty", key)
	}
	return value, nil
}

func requiredStringAllowEmpty(object map[string]json.RawMessage, key string, maximum int) (string, error) {
	raw, ok := object[key]
	if !ok {
		return "", fmt.Errorf("missing %s", key)
	}
	value, err := decodeString(raw, key)
	if err != nil {
		return "", err
	}
	if err := boundedString(key, value, maximum, false); err != nil {
		return "", err
	}
	return value, nil
}

func decodeString(raw []byte, name string) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%s must be a string: %w", name, err)
	}
	return value, nil
}

func boundedString(name, value string, maximum int, required bool) error {
	if required && value == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	if len(value) > maximum {
		return fmt.Errorf("%s exceeds %d bytes", name, maximum)
	}
	return nil
}

func optionalStrings(object map[string]json.RawMessage, key string, limits Limits) ([]string, error) {
	raw, ok := object[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return []string{}, nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("%s must be an array of strings: %w", key, err)
	}
	if len(values) > limits.MaxToolCalls {
		return nil, fmt.Errorf("%s has too many values", key)
	}
	for _, value := range values {
		if err := boundedString(key+" value", value, limits.MaxStringBytes, true); err != nil {
			return nil, err
		}
	}
	return values, nil
}

func decodeInt(raw []byte, name string, minimum int) (int, error) {
	value, err := decodeInt64(raw, name, int64(minimum))
	if err != nil {
		return 0, err
	}
	maximum := int64(^uint(0) >> 1)
	if value > maximum {
		return 0, fmt.Errorf("%s exceeds integer range", name)
	}
	return int(value), nil
}

func requiredInt64(object map[string]json.RawMessage, key string, minimum int64) (int64, error) {
	raw, ok := object[key]
	if !ok {
		return 0, fmt.Errorf("missing %s", key)
	}
	return decodeInt64(raw, key, minimum)
}

func decodeInt64(raw []byte, name string, minimum int64) (int64, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value json.Number
	if err := decoder.Decode(&value); err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	integer, err := value.Int64()
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	if integer < minimum {
		return 0, fmt.Errorf("%s must be at least %d", name, minimum)
	}
	return integer, nil
}
