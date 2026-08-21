package gateway

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

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/perception"
	protocol "github.com/bojieli/OpenRealtime/protocol/openai"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/bojieli/OpenRealtime/trajectory"
	"github.com/coder/websocket"
)

type settings struct {
	instruction  string
	tools        []action.ToolSpec
	inputFormat  audioFormat
	outputFormat audioFormat
	voice        string
	modalities   []string
	gate         perception.GateConfig
	// manualTurns records that the client turned server VAD off and will
	// declare its own turns.
	manualTurns bool
	extension   openrealtime.Response
	limits      openrealtime.Limits
	// observers is the perception this session selected. Empty selects the
	// binding's default set.
	observers []string
}

type videoSource struct {
	state  openrealtime.SourceState
	width  int
	height int
	index  uint64
	// lastAdmitted is when this source last passed the declared rate cap.
	lastAdmitted time.Time
}

// session is one client connection.
//
// It is a renderer, not a runtime. Everything conversational happens in the
// binding; this translates protocol events into runtime calls and the
// runtime's sink events back into protocol events. Keeping that boundary sharp
// is what lets a WebRTC adapter or a benchmark harness drive the same runtime
// without reimplementing any of it.
type session struct {
	ctx            context.Context
	cancel         context.CancelCauseFunc
	connection     *websocket.Conn
	config         Config
	model          string
	id             string
	conversationID string

	runtime   binding.Runtime
	validator *protocol.Validator

	sequence    atomic.Uint64
	sendChannel chan []byte
	// events carries decoded client events to the handler goroutine. The read
	// loop must never run a handler itself: the WebSocket library answers
	// pings from inside Read, so a handler that waits on a model keeps the
	// socket from answering, and a client with a keepalive drops a session
	// that is working perfectly well. The buffer is deep enough to ride out a
	// slow provider call without the read loop blocking on the send.
	events chan queuedEvent
	wait   sync.WaitGroup

	settingsMu sync.RWMutex
	settings   settings

	sourcesMu sync.Mutex
	sources   map[string]*videoSource

	itemsMu    sync.Mutex
	utterances map[string]*wireUtterance
	callNames  map[string]string
}

type wireUtterance struct {
	responseID string
	itemID     string
	format     audioFormat
	voice      string
	encoder    *outputEncoder
	buffer     []byte
	frameBytes int
	sourceRate uint32
	text       string
	// textOnly records which modality this turn was announced in, so its end
	// is rendered the same way its beginning was even if the session is
	// reconfigured mid-turn.
	textOnly bool
}

func newSession(parent context.Context, connection *websocket.Conn, config Config, requestedModel string) (*session, error) {
	ctx, cancel := context.WithCancelCause(parent)
	model := strings.TrimSpace(requestedModel)
	if model == "" {
		model = config.Model
	}
	result := &session{
		ctx: ctx, cancel: cancel, connection: connection, config: config, model: model,
		validator: protocol.NewValidator(), sendChannel: make(chan []byte, 512),
		events:  make(chan queuedEvent, 512),
		sources: make(map[string]*videoSource), utterances: make(map[string]*wireUtterance),
		callNames: make(map[string]string),
	}
	// The session's own identity comes from the server, not from its item
	// counter: every session's counter starts at zero, so deriving it here
	// would name every session in the process sess_000000000001. Two
	// concurrent sessions would then be indistinguishable in the logs, and
	// anything that keys on the identifier would confuse them outright.
	result.id = config.nextSessionID()
	result.conversationID = result.nextID("conv")
	result.settings = settings{
		inputFormat: audioFormat{Type: formatPCMU}, outputFormat: audioFormat{Type: formatPCMU},
		voice: "alloy", modalities: []string{"audio"},
		gate:   perception.DefaultGateConfig(),
		limits: config.VideoLimits,
	}
	runtime, err := config.Binding.Start(ctx, binding.Options{
		Sink: result, Settings: result.bindingSettings(), SessionID: result.id,
	})
	if err != nil {
		cancel(err)
		return nil, err
	}
	result.runtime = runtime
	return result, nil
}

func (session *session) nextID(prefix string) string {
	return fmt.Sprintf("%s_%012d", prefix, session.sequence.Add(1))
}

// textOnly reports whether this session's output is text rather than audio.
//
// It is read where a turn is rendered rather than where it is decided,
// because the binding produces a turn either way: what changes is whether
// anything synthesises it and which content part carries it on the wire.
func (session *session) textOnly() bool {
	session.settingsMu.RLock()
	defer session.settingsMu.RUnlock()
	return len(session.settings.modalities) == 1 && session.settings.modalities[0] == "text"
}

// outputModalities is what a response object declares it produced.
func (session *session) outputModalities() []string {
	session.settingsMu.RLock()
	defer session.settingsMu.RUnlock()
	return slices.Clone(session.settings.modalities)
}

func (session *session) bindingSettings() binding.Settings {
	session.settingsMu.RLock()
	defer session.settingsMu.RUnlock()
	return binding.Settings{
		Instruction: session.settings.instruction, Tools: slices.Clone(session.settings.tools),
		Voice: session.settings.voice, Modalities: slices.Clone(session.settings.modalities),
		Gate: session.settings.gate, Observers: slices.Clone(session.settings.observers),
		ManualTurns: session.settings.manualTurns,
	}
}

// Run drives the connection until it closes.
func (session *session) Run() error {
	session.wait.Add(2)
	go session.writerLoop()
	go session.handlerLoop()
	if err := session.send(session.sessionEvent("session.created")); err != nil {
		session.cancel(err)
		session.wait.Wait()
		return err
	}
	err := session.readLoop()
	if websocket.CloseStatus(err) == websocket.StatusNormalClosure ||
		websocket.CloseStatus(err) == websocket.StatusGoingAway || errors.Is(err, io.EOF) {
		err = nil
	}
	_ = session.runtime.Close(context.Background(), err)
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

// queuedEvent is one decoded client event on its way to the handler.
//
// Extension events carry their raw bytes because they are routed by a
// different decoder; base events carry the decoded message, so validation
// stays on the read goroutine where its errors are cheap and ordered.
type queuedEvent struct {
	extension []byte
	message   protocol.Message
}

// readLoop decodes client events and hands them to the handler.
//
// It does no conversational work itself, and that is the point. The WebSocket
// library answers pings from inside Read, so any handler that waits on a model
// - a recogniser under GPU contention, a reasoner mid-turn - would keep this
// loop out of Read for as long as the wait lasts. A client with a twenty
// second keepalive then drops a session that is working perfectly well, which
// is a fault the system under test gets blamed for.
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
		// Extension events are routed before the base protocol sees them. The
		// pinned registry cannot know about them by construction, so handing
		// them to it first would make every extension event an invalid one.
		if isExtensionEvent(input) {
			if err := session.enqueue(queuedEvent{extension: input}); err != nil {
				return err
			}
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
		if err := session.enqueue(queuedEvent{message: message}); err != nil {
			return err
		}
	}
}

// enqueue hands one event to the handler goroutine.
//
// The send can block when the handler is behind, which is deliberate: dropping
// audio frames would silently corrupt every measurement taken through this
// server, and an unbounded queue would turn a slow provider into an
// out-of-memory. Blocking is the honest option, and the buffer is deep enough
// that a single slow call does not reach it.
func (session *session) enqueue(event queuedEvent) error {
	select {
	case session.events <- event:
		return nil
	case <-session.ctx.Done():
		return context.Cause(session.ctx)
	}
}

// handlerLoop runs client events in the order they arrived.
//
// One goroutine, not a pool: the protocol is a sequence, and two audio frames
// applied concurrently are two frames applied in an arbitrary order.
func (session *session) handlerLoop() {
	defer session.wait.Done()
	for {
		select {
		case <-session.ctx.Done():
			return
		case event := <-session.events:
			if event.extension != nil {
				if _, err := session.handleExtension(event.extension); err != nil {
					session.sendError("invalid_request_error", err.Error())
				}
				continue
			}
			if err := session.handleClientEvent(event.message); err != nil {
				session.sendError("invalid_request_error", err.Error())
			}
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
		return session.onTruncate(truncate)
	case protocol.EventConversationItemCreate:
		var create conversationItemCreateEvent
		if err := message.Unmarshal(&create); err != nil {
			return err
		}
		return session.onItemCreate(create)
	case protocol.EventResponseCreate:
		return session.runtime.CreateResponse(session.ctx)
	case protocol.EventResponseCancel:
		return session.runtime.Cancel(session.ctx, "client response cancellation")
	case protocol.EventInputAudioBufferClear:
		return session.send(event("input_audio_buffer.cleared", session.nextID("event"), nil))
	case protocol.EventInputAudioBufferCommit:
		return session.onAudioCommit()
	case protocol.EventOutputAudioBufferClear:
		// A client asking to stop hearing the agent is a cancellation, which
		// the runtime already has a name for. It matters most over WebRTC,
		// where audio the client has buffered keeps playing after the events
		// stop, so a client that wants silence has to be able to ask for it.
		return session.onOutputBufferClear()
	default:
		return session.unsupported(message.Type())
	}
}

// unsupported explains a standard event this deployment does not implement.
//
// The explanation is the point. "Unsupported event" tells a client author
// nothing they can act on, and two of these are refused for a structural
// reason rather than because nobody got to them: an append-only trajectory has
// no delete, and server VAD owning commitment is what makes an explicit commit
// meaningless here. A client that knows which of those it hit can do something
// about it.
func (session *session) unsupported(eventType protocol.EventType) error {
	switch eventType {
	case protocol.EventConversationItemDelete:
		return errors.New("the conversation is an append-only trajectory and has no delete: " +
			"content that reached the world cannot be un-reached, so it is superseded rather than removed")
	case protocol.EventConversationItemRetrieve:
		return errors.New("this deployment does not serve item retrieval; " +
			"a client that negotiated observations receives what the agent perceived as it happens")
	default:
		return fmt.Errorf("unsupported Realtime client event %q", eventType)
	}
}

// onAudioCommit closes the input buffer at the client's request.
//
// It is the other half of turn detection being off: a client that took the
// floor has to be able to say where a turn ended, and the acknowledgement
// names the item so the transcript that follows can be correlated with it.
func (session *session) onAudioCommit() error {
	return session.runtime.CommitAudio(session.ctx)
}

// onOutputBufferClear stops agent audio at the client's request.
//
// The acknowledgement names the response it cleared, because that is what the
// event carries and a client tracking responses needs to know which one
// stopped. With nothing playing there is no response to name, and refusing is
// the honest answer: a client that asked to stop hearing something it was not
// hearing has a bug worth seeing.
func (session *session) onOutputBufferClear() error {
	session.itemsMu.Lock()
	responseID := ""
	for _, utterance := range session.utterances {
		responseID = utterance.responseID
	}
	session.itemsMu.Unlock()
	if responseID == "" {
		return errors.New("there is no agent audio to clear: no response is in progress")
	}
	if err := session.runtime.Cancel(session.ctx, "client cleared the output audio buffer"); err != nil {
		return err
	}
	return session.send(event("output_audio_buffer.cleared", session.nextID("event"), map[string]any{
		"response_id": responseID,
	}))
}

func (session *session) update(update sessionUpdateBody) error {
	session.settingsMu.RLock()
	current := session.settings
	session.settingsMu.RUnlock()

	if update.Instructions != nil {
		current.instruction = *update.Instructions
	}
	if update.OutputModalities != nil {
		// Audio or text, one of them. A client that wants text is not asking
		// for less of the same thing: a computer-use agent wants the function
		// calls and the reasoning, and synthesising an answer nobody listens
		// to spends a GPU on nothing. Refusing it made this server unusable
		// for exactly the clients the extension exists for.
		if len(update.OutputModalities) != 1 {
			return errors.New("output_modalities must name exactly one of audio or text")
		}
		switch update.OutputModalities[0] {
		case "audio", "text":
		default:
			return fmt.Errorf("output modality must be audio or text, got %q", update.OutputModalities[0])
		}
		current.modalities = slices.Clone(update.OutputModalities)
	}
	if update.Tools != nil {
		specs := make([]action.ToolSpec, 0, len(update.Tools))
		seen := make(map[string]struct{}, len(update.Tools))
		for _, tool := range update.Tools {
			spec, err := tool.spec()
			if err != nil {
				return err
			}
			if _, duplicate := seen[spec.Name]; duplicate {
				return fmt.Errorf("duplicate Realtime tool %q", spec.Name)
			}
			seen[spec.Name] = struct{}{}
			specs = append(specs, spec)
		}
		current.tools = specs
	}
	if update.Audio.Input.Format.Type != "" {
		current.inputFormat = update.Audio.Input.Format.audioFormat
	}
	if update.Audio.Output.Format.Type != "" {
		current.outputFormat = update.Audio.Output.Format.audioFormat
	}
	if update.Audio.Output.Voice != "" {
		current.voice = update.Audio.Output.Voice
	}
	if update.Audio.Input.TurnDetectionSet {
		turn := update.Audio.Input.TurnDetection
		switch {
		case turn == nil:
			// Explicit null. The client is taking the floor: it will commit
			// the input buffer and ask for responses itself, and the server
			// stops ending turns on silence.
			current.manualTurns = true
		case turn.Type == "server_vad":
			current.manualTurns = false
			current.gate = perception.GateConfig{
				Threshold: turn.Threshold, PrefixPaddingMS: turn.PrefixPaddingMS,
				SilenceDurationMS: turn.SilenceDurationMS,
			}
		default:
			return fmt.Errorf(
				"turn detection must be server_vad or null, got %q", turn.Type)
		}
	}
	if _, err := current.inputFormat.sampleRate(); err != nil {
		return err
	}
	if _, err := current.outputFormat.sampleRate(); err != nil {
		return err
	}
	if update.OpenRealtime != nil {
		response, err := openrealtime.NegotiateSession(
			*update.OpenRealtime, session.supportedFeatures(), session.config.VideoLimits,
			session.config.Binding.Capabilities().Observers)
		if err != nil {
			return err
		}
		current.extension = response
		current.observers = slices.Clone(response.Observers)
		if response.Video != nil {
			current.limits = *response.Video
		}
	}

	session.settingsMu.Lock()
	session.settings = current
	session.settingsMu.Unlock()
	if err := session.runtime.Update(session.ctx, session.bindingSettings()); err != nil {
		return err
	}
	return session.send(session.sessionEvent("session.updated"))
}

// supportedFeatures is what this deployment can actually offer, which is the
// binding's capability set rather than a static list. Negotiating a feature
// the binding cannot provide would be promising and then failing.
func (session *session) supportedFeatures() []openrealtime.Feature {
	capabilities := session.config.Binding.Capabilities()
	var supported []openrealtime.Feature
	if capabilities.Video {
		supported = append(supported, openrealtime.FeatureVideoInput)
	}
	if capabilities.Observations {
		supported = append(supported, openrealtime.FeatureObservations)
	}
	if capabilities.ComputerUse {
		supported = append(supported, openrealtime.FeatureComputerUse)
	}
	return supported
}

func (session *session) onAudio(input []byte) error {
	session.settingsMu.RLock()
	format := session.settings.inputFormat
	session.settingsMu.RUnlock()
	pcm16, rate, err := decodeAudio(format, input)
	if err != nil {
		return err
	}
	session.config.Metrics.audioFramesIn.Add(1)
	return session.runtime.Audio(session.ctx, perception.Frame{
		Kind: perception.FrameAudio, Source: "microphone",
		CapturedNS: uint64(time.Now().UnixNano()), PCM16LE: pcm16, SampleRateHz: rate,
	})
}

func (session *session) onTruncate(truncate truncateEvent) error {
	if strings.TrimSpace(truncate.ItemID) == "" || truncate.ContentIndex != 0 || truncate.AudioEndMS < 0 {
		return errors.New("conversation.item.truncate requires an item, content index zero, and non-negative audio_end_ms")
	}
	session.itemsMu.Lock()
	utteranceID := ""
	for id, utterance := range session.utterances {
		if utterance.itemID == truncate.ItemID {
			utteranceID = id
		}
	}
	session.itemsMu.Unlock()
	if utteranceID != "" {
		if err := session.runtime.Truncate(session.ctx, binding.Truncation{
			ItemID: utteranceID, AudioEndMS: truncate.AudioEndMS,
		}); err != nil {
			return err
		}
	}
	return session.send(event("conversation.item.truncated", session.nextID("event"), map[string]any{
		"item_id": truncate.ItemID, "content_index": 0, "audio_end_ms": truncate.AudioEndMS,
	}))
}

// onItemCreate accepts what a client can add to the conversation directly.
//
// Two shapes matter: a tool result, and a typed message. The second is how a
// client that is not speaking - a text client, a harness, a computer-use
// driver - says something, and refusing it would make the protocol
// voice-only in a way the base protocol is not.
func (session *session) onItemCreate(create conversationItemCreateEvent) error {
	if create.Item.Type == "message" {
		return session.onTextMessage(create)
	}
	if create.Item.Type != "function_call_output" || strings.TrimSpace(create.Item.CallID) == "" {
		return errors.New("conversation.item.create accepts message and function_call_output items")
	}
	session.itemsMu.Lock()
	name := session.callNames[create.Item.CallID]
	session.itemsMu.Unlock()
	itemID := create.Item.ID
	if itemID == "" {
		itemID = session.nextID("item")
	}
	if err := session.send(event("conversation.item.created", session.nextID("event"), map[string]any{
		"previous_item_id": nil,
		"item":             functionOutputItem(itemID, create.Item.CallID, create.Item.Output),
	})); err != nil {
		return err
	}
	return session.runtime.ToolResult(session.ctx, encodeToolResult(create.Item.CallID, name, create.Item.Output))
}

// onTextMessage commits a typed message as an observation.
//
// It carries user authority, because it is the user talking: a client typing
// is the same participant as a client speaking, and the difference is the
// transport rather than the provenance.
func (session *session) onTextMessage(create conversationItemCreateEvent) error {
	text := create.text()
	if strings.TrimSpace(text) == "" {
		return errors.New("a message item requires text content")
	}
	role := strings.TrimSpace(create.Item.Role)
	if role == "" {
		role = "user"
	}
	if role != "user" && role != "system" {
		return fmt.Errorf("a client may add user or system messages, not %q", role)
	}
	itemID := create.Item.ID
	if itemID == "" {
		itemID = session.nextID("item")
	}
	if err := session.send(event("conversation.item.created", session.nextID("event"), map[string]any{
		"previous_item_id": nil, "item": userAudioItem(itemID, text),
	})); err != nil {
		return err
	}
	return session.runtime.Text(session.ctx, binding.TextInput{
		ItemID: itemID, Role: role, Text: text,
	})
}

func (session *session) sessionEvent(eventType string) map[string]any {
	session.settingsMu.RLock()
	current := session.settings
	session.settingsMu.RUnlock()
	tools := make([]map[string]any, 0, len(current.tools))
	for _, tool := range current.tools {
		var parameters any
		_ = json.Unmarshal(tool.Parameters, &parameters)
		definition := map[string]any{
			"type": "function", "name": tool.Name, "description": tool.Description,
			"parameters": parameters,
		}
		if tool.Confirm != action.ConfirmNever || tool.Target != "" {
			definition["openrealtime"] = openrealtime.ToolExtension{
				Confirm: string(tool.Confirm), Target: tool.Target,
			}
		}
		tools = append(tools, definition)
	}
	object := map[string]any{
		"type": "realtime", "id": session.id, "object": "realtime.session", "model": session.model,
		"output_modalities": current.modalities, "instructions": current.instruction,
		"tools": tools, "tool_choice": "auto", "max_output_tokens": "inf",
		"audio": map[string]any{
			"input": map[string]any{
				"format":         current.inputFormat,
				"transcription":  map[string]any{"model": session.config.TranscriptionModel},
				"turn_detection": turnDetection(current),
			},
			"output": map[string]any{"format": current.outputFormat, "voice": current.voice, "speed": 1},
		},
	}
	if current.extension.Version != 0 {
		object["openrealtime"] = current.extension
	}
	return event(eventType, session.nextID("event"), map[string]any{"session": object})
}

func (session *session) send(value map[string]any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if session.config.ValidateWire && !strings.HasPrefix(fmt.Sprint(value["type"]), "openrealtime.") {
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
	session.config.Logger.Warn("session error",
		"session", session.id, "code", code, "message", message)
	_ = session.send(event("error", session.nextID("event"), map[string]any{
		"error": map[string]any{"type": "invalid_request_error", "code": code, "message": message},
	}))
}

func (session *session) recordCallNames(calls []trajectory.ToolCall) {
	session.itemsMu.Lock()
	defer session.itemsMu.Unlock()
	for _, call := range calls {
		session.callNames[call.CallID] = call.Name
	}
}
