package realtimegateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/eventloop"
	protocol "github.com/bojieli/OpenRealtime/protocol/openai"
	"github.com/bojieli/OpenRealtime/trajectory"
	"github.com/coder/websocket"
)

type sessionSettings struct {
	Instruction      string
	Tools            []continuation.ToolDefinition
	InputFormat      audioFormat
	OutputFormat     audioFormat
	Voice            string
	OutputModalities []string
	VAD              vadConfig
}

type mediaCommand struct {
	ItemID     string
	PCM16      []byte
	SampleRate uint32
	Start      bool
	Final      bool
}

type pendingCall struct {
	InvocationID string
	Name         string
}

type pendingInvocation struct {
	Calls           []trajectory.ToolCall
	Results         map[string]trajectory.ToolResult
	ResumeRequested bool
	Dispatching     bool
}

type session struct {
	ctx            context.Context
	cancel         context.CancelCauseFunc
	connection     *websocket.Conn
	config         Config
	model          string
	id             string
	conversationID string
	origin         time.Time

	nextSequence atomic.Uint64
	sourceRev    atomic.Uint64
	sendChannel  chan []byte
	media        chan mediaCommand
	cognition    chan struct{}
	wait         sync.WaitGroup
	validator    *protocol.Validator

	settingsMu sync.RWMutex
	settings   sessionSettings
	detector   *energyVAD
	userItemID string

	store       *trajectory.Store
	cognitive   *cognitionRuntime
	coordinator *eventloop.Coordinator
	speech      *speechScheduler

	pendingMu          sync.Mutex
	pendingCalls       map[string]pendingCall
	pendingInvocations map[string]*pendingInvocation
	wireAssistantMu    sync.Mutex
	wireAssistantItems map[string][]string
}

func newSession(parent context.Context, connection *websocket.Conn, config Config, requestedModel string) (*session, error) {
	ctx, cancel := context.WithCancelCause(parent)
	origin := time.Now()
	model := strings.TrimSpace(requestedModel)
	if model == "" {
		model = config.Model
	}
	result := &session{
		ctx: ctx, cancel: cancel, connection: connection, config: config, model: model,
		origin: origin, sendChannel: make(chan []byte, 512), media: make(chan mediaCommand, 1_024),
		cognition: make(chan struct{}, 1), validator: protocol.NewValidator(),
		store: trajectory.NewStore(), pendingCalls: make(map[string]pendingCall),
		pendingInvocations: make(map[string]*pendingInvocation),
		wireAssistantItems: make(map[string][]string),
	}
	result.id = result.nextID("sess")
	result.conversationID = result.nextID("conv")
	result.settings = sessionSettings{
		InputFormat: audioFormat{Type: formatPCMU}, OutputFormat: audioFormat{Type: formatPCMU},
		Voice: "alloy", OutputModalities: []string{"audio"},
		VAD: vadConfig{Threshold: 0.5, PrefixPaddingMS: 300, SilenceDurationMS: 500},
	}
	var err error
	result.detector, err = newEnergyVAD(result.settings.VAD, 8_000)
	if err != nil {
		cancel(err)
		return nil, err
	}
	result.cognitive, err = newCognitionRuntime(cognitionConfig{
		Store: result.store, Fast: config.FastProvider, Slow: config.SlowProvider, Callbacks: result,
		PreparationFast: config.preparationFast, PreparationSlow: config.preparationSlow,
		FastTokens: config.FastMaxTokens, SlowTokens: config.SlowMaxTokens,
		MaxSlow: config.MaxSlowInvocations, SlowPace: config.SlowPreparationMin,
		SlowPolicy: config.SlowContextPolicy,
		Now:        func() uint64 { return uint64(time.Since(origin)) }, NextID: result.nextID,
	})
	if err != nil {
		cancel(err)
		return nil, err
	}
	reservedInterruptEvents := 8
	if reservedInterruptEvents >= config.MaxPendingEvents {
		reservedInterruptEvents = config.MaxPendingEvents - 1
	}
	result.coordinator, err = eventloop.New(eventloop.Config{
		Store: result.store, Processor: result.cognitive,
		MaxPendingEvents: config.MaxPendingEvents, ReservedInterruptEvents: reservedInterruptEvents,
		Now: func() uint64 { return uint64(time.Since(origin)) }, NextID: result.nextID,
	})
	if err != nil {
		cancel(err)
		return nil, err
	}
	result.speech = newSpeechScheduler(result, config.SpeechProvider)
	return result, nil
}

func (session *session) nextID(prefix string) string {
	return fmt.Sprintf("%s_%012d", prefix, session.nextSequence.Add(1))
}

func (session *session) Run() error {
	session.wait.Add(4)
	go session.writerLoop()
	go session.mediaLoop()
	go session.cognitionLoop()
	go session.speech.Run()
	if err := session.send(session.sessionEvent("session.created")); err != nil {
		session.cancel(err)
		session.wait.Wait()
		return err
	}
	err := session.readLoop()
	if websocket.CloseStatus(err) == websocket.StatusNormalClosure || websocket.CloseStatus(err) == websocket.StatusGoingAway || errors.Is(err, io.EOF) {
		err = nil
	}
	session.cancel(err)
	session.wait.Wait()
	return err
}

func (session *session) writerLoop() {
	defer session.wait.Done()
	for {
		select {
		case <-session.ctx.Done():
			return
		case message := <-session.sendChannel:
			if err := session.connection.Write(session.ctx, websocket.MessageText, message); err != nil {
				session.cancel(err)
				return
			}
		}
	}
}

func (session *session) readLoop() error {
	for {
		messageType, input, err := session.connection.Read(session.ctx)
		if err != nil {
			return err
		}
		if messageType != websocket.MessageText {
			session.sendError("invalid_event", "Realtime client events must be JSON text messages")
			continue
		}
		message, err := protocol.Decode(input)
		if err != nil {
			session.sendError("invalid_event", err.Error())
			continue
		}
		if session.config.ValidateWire {
			if err := session.validator.Validate(protocol.ProfileRealtime, protocol.DirectionClient, message); err != nil {
				session.sendError("invalid_event", err.Error())
				continue
			}
		}
		if err := session.handleClientEvent(message); err != nil {
			session.sendError("invalid_request_error", err.Error())
		}
	}
}

func (session *session) handleClientEvent(message protocol.Message) error {
	switch message.Type() {
	case protocol.EventSessionUpdate:
		var update sessionUpdateEvent
		if err := message.Unmarshal(&update); err != nil {
			return err
		}
		return session.update(update.Session)
	case protocol.EventInputAudioBufferAppend:
		var appendEvent audioAppendEvent
		if err := message.Unmarshal(&appendEvent); err != nil {
			return err
		}
		audio, err := base64.StdEncoding.DecodeString(appendEvent.Audio)
		if err != nil {
			return errors.New("input audio is not valid base64")
		}
		if len(audio) == 0 || len(audio) > session.config.MaxAudioFrameBytes {
			return fmt.Errorf("input audio frame must contain 1..%d bytes", session.config.MaxAudioFrameBytes)
		}
		return session.onAudio(audio)
	case protocol.EventConversationItemTruncate:
		var truncate truncateEvent
		if err := message.Unmarshal(&truncate); err != nil {
			return err
		}
		return session.truncate(truncate)
	case protocol.EventConversationItemCreate:
		var create conversationItemCreateEvent
		if err := message.Unmarshal(&create); err != nil {
			return err
		}
		return session.acceptToolResult(create)
	case protocol.EventResponseCreate:
		return session.requestResume()
	case protocol.EventResponseCancel:
		return session.interrupt("client response cancellation")
	case protocol.EventInputAudioBufferClear:
		if session.detector.speaking {
			return errors.New("cannot clear the audio buffer during active speech")
		}
		rate, _ := session.settings.InputFormat.sampleRate()
		session.detector, _ = newEnergyVAD(session.settings.VAD, rate)
		return session.send(event("input_audio_buffer.cleared", session.nextID("event"), nil))
	default:
		return fmt.Errorf("unsupported Realtime client event %q", message.Type())
	}
}

func (session *session) update(update sessionUpdateBody) error {
	session.settingsMu.RLock()
	settings := session.settings
	session.settingsMu.RUnlock()
	if update.Instructions != nil {
		settings.Instruction = *update.Instructions
	}
	if update.OutputModalities != nil {
		if len(update.OutputModalities) != 1 || update.OutputModalities[0] != "audio" {
			return errors.New("local Fish speech requires output_modalities [\"audio\"]")
		}
		settings.OutputModalities = slices.Clone(update.OutputModalities)
	}
	if update.Tools != nil {
		definitions := make([]continuation.ToolDefinition, 0, len(update.Tools))
		seen := make(map[string]struct{}, len(update.Tools))
		for _, tool := range update.Tools {
			definition, err := tool.definition()
			if err != nil {
				return err
			}
			if _, duplicate := seen[definition.Name]; duplicate {
				return fmt.Errorf("duplicate Realtime tool %q", definition.Name)
			}
			seen[definition.Name] = struct{}{}
			definitions = append(definitions, definition)
		}
		settings.Tools = definitions
	}
	if update.Audio.Input.Format.Type != "" {
		settings.InputFormat = update.Audio.Input.Format.audioFormat
	}
	if update.Audio.Output.Format.Type != "" {
		settings.OutputFormat = update.Audio.Output.Format.audioFormat
	}
	if update.Audio.Output.Voice != "" {
		settings.Voice = update.Audio.Output.Voice
	}
	if update.Audio.Input.TurnDetection != nil {
		turn := update.Audio.Input.TurnDetection
		if turn.Type != "server_vad" {
			return errors.New("gateway currently requires server_vad turn detection")
		}
		settings.VAD = vadConfig{
			Threshold: turn.Threshold, PrefixPaddingMS: turn.PrefixPaddingMS,
			SilenceDurationMS: turn.SilenceDurationMS,
		}
	}
	inputRate, err := settings.InputFormat.sampleRate()
	if err != nil {
		return err
	}
	if _, err := settings.OutputFormat.sampleRate(); err != nil {
		return err
	}
	detector, err := newEnergyVAD(settings.VAD, inputRate)
	if err != nil {
		return err
	}
	if session.detector.speaking {
		return errors.New("cannot change session audio configuration during active speech")
	}
	session.settingsMu.Lock()
	session.settings = settings
	session.detector = detector
	session.settingsMu.Unlock()
	return session.send(session.sessionEvent("session.updated"))
}

func (session *session) Semantics() sessionSemantics {
	session.settingsMu.RLock()
	defer session.settingsMu.RUnlock()
	return sessionSemantics{Instruction: session.settings.Instruction, Tools: slices.Clone(session.settings.Tools)}.clone()
}

func (session *session) onAudio(input []byte) error {
	session.settingsMu.RLock()
	format := session.settings.InputFormat
	session.settingsMu.RUnlock()
	pcm16, rate, err := decodeAudio(format, input)
	if err != nil {
		return err
	}
	result, err := session.detector.Push(pcm16)
	if err != nil {
		return err
	}
	if result.Started {
		session.userItemID = session.nextID("item")
		if err := session.interrupt("server VAD detected user speech"); err != nil {
			return err
		}
		if err := session.send(event("input_audio_buffer.speech_started", session.nextID("event"), map[string]any{
			"audio_start_ms": result.AudioStartMS, "item_id": session.userItemID,
		})); err != nil {
			return err
		}
	}
	if len(result.Audio) != 0 {
		command := mediaCommand{
			ItemID: session.userItemID, PCM16: result.Audio, SampleRate: rate,
			Start: result.Started, Final: result.Stopped,
		}
		select {
		case session.media <- command:
		case <-session.ctx.Done():
			return context.Cause(session.ctx)
		default:
			return errors.New("ASR media queue is full")
		}
	}
	if result.Stopped {
		if err := session.send(event("input_audio_buffer.speech_stopped", session.nextID("event"), map[string]any{
			"audio_end_ms": result.AudioEndMS, "item_id": session.userItemID,
		})); err != nil {
			return err
		}
		session.userItemID = ""
	}
	return nil
}

func (session *session) interrupt(reason string) error {
	cause := fmt.Errorf("%s: %w", reason, eventloop.ErrInterrupted)
	session.coordinator.Interrupt(cause)
	return session.cancelAssistantSpeech(session.speech.Interrupt(cause), "realtime-vad", eventloop.PriorityInterrupt)
}

func (session *session) cancelAssistantSpeech(assistantIDs []string, source string, priority eventloop.Priority) error {
	var failures []error
	for _, assistantID := range assistantIDs {
		_, err := session.coordinator.Submit(eventloop.Event{
			Type: "speech.cancelled", Source: source, Channel: "voice",
			Priority: priority, Kind: trajectory.KindAssistantState,
			Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime},
			AssistantState: &trajectory.AssistantState{
				AssistantItemID: assistantID, Visibility: trajectory.VisibilityCancelled,
			},
		})
		if err == nil {
			session.signalCognition()
		} else {
			failures = append(failures, fmt.Errorf("record cancellation for assistant %q: %w", assistantID, err))
		}
	}
	return errors.Join(failures...)
}

func (session *session) truncate(truncate truncateEvent) error {
	if strings.TrimSpace(truncate.ItemID) == "" || truncate.ContentIndex != 0 || truncate.AudioEndMS < 0 {
		return errors.New("conversation.item.truncate requires an item, content index zero, and non-negative audio_end_ms")
	}
	session.wireAssistantMu.Lock()
	assistantIDs := slices.Clone(session.wireAssistantItems[truncate.ItemID])
	session.wireAssistantMu.Unlock()
	for _, assistantID := range assistantIDs {
		if _, err := session.coordinator.Submit(eventloop.Event{
			Type: "conversation.item.truncated", Source: "realtime-client", Channel: "voice",
			Priority: eventloop.PriorityRoutine, Kind: trajectory.KindAssistantState,
			AssistantState: &trajectory.AssistantState{
				AssistantItemID: assistantID, Visibility: trajectory.VisibilityCancelled,
			},
		}); err != nil {
			return err
		}
		session.signalCognition()
	}
	return session.send(event("conversation.item.truncated", session.nextID("event"), map[string]any{
		"item_id": truncate.ItemID, "content_index": 0, "audio_end_ms": truncate.AudioEndMS,
	}))
}

func (session *session) PublishAssistant(phase trajectory.Phase, result continuation.RunResult) error {
	if strings.TrimSpace(result.AssistantText) == "" {
		return nil
	}
	if phase == trajectory.PhaseSlow {
		if err := session.cancelAssistantSpeech(session.speech.SupersedeFast(), "slow-continuation", eventloop.PriorityRoutine); err != nil {
			return err
		}
	}
	assistantIDs := resultAssistantIDs(session.store, result)
	job := speechJob{
		Phase: phase, Text: result.AssistantText, AssistantIDs: assistantIDs,
		Usage: result.Completion.Usage,
	}
	if err := session.speech.Enqueue(job); err != nil {
		return err
	}
	for _, assistantID := range assistantIDs {
		if _, err := session.coordinator.Submit(eventloop.Event{
			Type: "speech.queued", Source: "fish-audio", Channel: "voice",
			Priority: eventloop.PriorityRoutine, Kind: trajectory.KindAssistantState,
			AssistantState: &trajectory.AssistantState{
				AssistantItemID: assistantID, Visibility: trajectory.VisibilityQueued,
			},
		}); err != nil {
			return err
		}
		session.signalCognition()
	}
	return nil
}

func (session *session) PublishToolCalls(invocation string, calls []trajectory.ToolCall, usage *continuation.Usage) error {
	if len(calls) == 0 {
		return nil
	}
	if err := session.cancelAssistantSpeech(session.speech.SupersedeFast(), "slow-tool-call", eventloop.PriorityRoutine); err != nil {
		return err
	}
	cloned := make([]trajectory.ToolCall, len(calls))
	for index, call := range calls {
		call.Arguments = slices.Clone(call.Arguments)
		cloned[index] = call
	}
	session.pendingMu.Lock()
	if _, duplicate := session.pendingInvocations[invocation]; duplicate {
		session.pendingMu.Unlock()
		return fmt.Errorf("duplicate tool invocation %q", invocation)
	}
	seen := make(map[string]struct{}, len(cloned))
	for _, call := range cloned {
		if _, duplicate := seen[call.CallID]; duplicate {
			session.pendingMu.Unlock()
			return fmt.Errorf("duplicate call ID %q in tool invocation %q", call.CallID, invocation)
		}
		seen[call.CallID] = struct{}{}
		if _, duplicate := session.pendingCalls[call.CallID]; duplicate {
			session.pendingMu.Unlock()
			return fmt.Errorf("duplicate pending call ID %q", call.CallID)
		}
	}
	pending := &pendingInvocation{Calls: cloned, Results: make(map[string]trajectory.ToolResult)}
	for _, call := range cloned {
		session.pendingCalls[call.CallID] = pendingCall{InvocationID: invocation, Name: call.Name}
	}
	session.pendingInvocations[invocation] = pending
	session.pendingMu.Unlock()
	if err := session.publishToolResponse(cloned, usage); err != nil {
		session.pendingMu.Lock()
		delete(session.pendingInvocations, invocation)
		for _, call := range cloned {
			delete(session.pendingCalls, call.CallID)
		}
		session.pendingMu.Unlock()
		return err
	}
	return nil
}

func (session *session) acceptToolResult(create conversationItemCreateEvent) error {
	if create.Item.Type != "function_call_output" || strings.TrimSpace(create.Item.CallID) == "" {
		return errors.New("conversation.item.create currently accepts function_call_output items")
	}
	session.pendingMu.Lock()
	call, exists := session.pendingCalls[create.Item.CallID]
	if !exists {
		session.pendingMu.Unlock()
		return fmt.Errorf("tool result references unknown call %q", create.Item.CallID)
	}
	pending := session.pendingInvocations[call.InvocationID]
	if _, duplicate := pending.Results[create.Item.CallID]; duplicate {
		session.pendingMu.Unlock()
		return fmt.Errorf("duplicate tool result for call %q", create.Item.CallID)
	}
	pending.Results[create.Item.CallID] = encodeToolResult(create.Item.CallID, call.Name, create.Item.Output)
	ready := pending.ResumeRequested && !pending.Dispatching && len(pending.Results) == len(pending.Calls)
	session.pendingMu.Unlock()
	itemID := create.Item.ID
	if itemID == "" {
		itemID = session.nextID("item")
	}
	if err := session.send(event("conversation.item.created", session.nextID("event"), map[string]any{
		"previous_item_id": nil, "item": functionOutputItem(itemID, create.Item.CallID, create.Item.Output),
	})); err != nil {
		return err
	}
	if ready {
		return session.resumeInvocation(call.InvocationID)
	}
	return nil
}

func (session *session) requestResume() error {
	var ready []string
	session.pendingMu.Lock()
	for invocation, pending := range session.pendingInvocations {
		pending.ResumeRequested = true
		if !pending.Dispatching && len(pending.Results) == len(pending.Calls) {
			ready = append(ready, invocation)
		}
	}
	session.pendingMu.Unlock()
	for _, invocation := range ready {
		if err := session.resumeInvocation(invocation); err != nil {
			return err
		}
	}
	return nil
}

func (session *session) resumeInvocation(invocation string) error {
	session.pendingMu.Lock()
	pending := session.pendingInvocations[invocation]
	if pending == nil || pending.Dispatching || !pending.ResumeRequested || len(pending.Results) != len(pending.Calls) {
		session.pendingMu.Unlock()
		return nil
	}
	pending.Dispatching = true
	results := make([]trajectory.ToolResult, 0, len(pending.Calls))
	for _, call := range pending.Calls {
		results = append(results, pending.Results[call.CallID])
	}
	session.pendingMu.Unlock()
	if _, err := session.coordinator.Submit(eventloop.Event{
		Type: "tool.results", Source: "tau-environment", Channel: "tool",
		Priority: eventloop.PriorityRoutine, Kind: trajectory.KindToolResult,
		InvocationID: invocation, ToolResults: results,
	}); err != nil {
		session.pendingMu.Lock()
		if current := session.pendingInvocations[invocation]; current == pending {
			current.Dispatching = false
		}
		session.pendingMu.Unlock()
		return err
	}
	session.pendingMu.Lock()
	if current := session.pendingInvocations[invocation]; current == pending {
		delete(session.pendingInvocations, invocation)
		for _, call := range pending.Calls {
			delete(session.pendingCalls, call.CallID)
		}
	}
	session.pendingMu.Unlock()
	session.signalCognition()
	return nil
}

func (session *session) signalCognition() {
	select {
	case session.cognition <- struct{}{}:
	default:
	}
}

func (session *session) cognitionLoop() {
	defer session.wait.Done()
	for {
		select {
		case <-session.ctx.Done():
			return
		case <-session.cognition:
			for session.coordinator.Pending() > 0 {
				_, err := session.coordinator.RunNext(session.ctx)
				if err == nil || errors.Is(err, eventloop.ErrInterrupted) || errors.Is(err, context.Canceled) {
					continue
				}
				if errors.Is(err, eventloop.ErrBusy) {
					break
				}
				session.sendError("provider_error", err.Error())
			}
		}
	}
}

func (session *session) sessionEvent(eventType string) map[string]any {
	session.settingsMu.RLock()
	settings := session.settings
	session.settingsMu.RUnlock()
	tools := make([]map[string]any, 0, len(settings.Tools))
	for _, tool := range settings.Tools {
		var parameters any
		_ = json.Unmarshal(tool.Parameters, &parameters)
		tools = append(tools, map[string]any{
			"type": "function", "name": tool.Name, "description": tool.Description,
			"parameters": parameters,
		})
	}
	object := map[string]any{
		"type": "realtime", "id": session.id, "object": "realtime.session", "model": session.model,
		"output_modalities": settings.OutputModalities, "instructions": settings.Instruction,
		"tools": tools, "tool_choice": "auto", "max_output_tokens": "inf",
		"audio": map[string]any{
			"input": map[string]any{
				"format": settings.InputFormat, "transcription": map[string]any{"model": "Qwen/Qwen3-ASR-0.6B"},
				"turn_detection": map[string]any{
					"type": "server_vad", "threshold": settings.VAD.Threshold,
					"prefix_padding_ms":   settings.VAD.PrefixPaddingMS,
					"silence_duration_ms": settings.VAD.SilenceDurationMS,
					"create_response":     true, "interrupt_response": true,
				},
			},
			"output": map[string]any{"format": settings.OutputFormat, "voice": settings.Voice, "speed": 1},
		},
	}
	return event(eventType, session.nextID("event"), map[string]any{"session": object})
}

func (session *session) send(value map[string]any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if session.config.ValidateWire {
		message, decodeErr := protocol.Decode(encoded)
		if decodeErr != nil {
			return decodeErr
		}
		if validateErr := session.validator.Validate(protocol.ProfileRealtime, protocol.DirectionServer, message); validateErr != nil {
			return validateErr
		}
	}
	select {
	case session.sendChannel <- encoded:
		return nil
	case <-session.ctx.Done():
		return context.Cause(session.ctx)
	}
}

func (session *session) sendError(code, message string) {
	_ = session.send(event("error", session.nextID("event"), map[string]any{
		"error": map[string]any{"type": "invalid_request_error", "code": code, "message": message},
	}))
}
