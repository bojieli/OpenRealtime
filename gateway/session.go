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
	effectauthority "github.com/bojieli/OpenRealtime/authority"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/perception"
	protocol "github.com/bojieli/OpenRealtime/protocol/openai"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/bojieli/OpenRealtime/trajectory"
	"github.com/coder/websocket"
)

// The two liveness causes a session can end with that are not the client's
// doing and not a fault in the conversation. They are named rather than
// anonymous because they arrive at an operator as a session that simply
// stopped, and "the peer stopped reading" and "the peer stopped answering" are
// different problems with different fixes.
var (
	errClientStoppedReading = errors.New("client stopped reading and the send did not complete")
	errPeerUnreachable      = errors.New("peer did not answer a keepalive ping")
)

type settings struct {
	instruction string
	tools       []action.ToolSpec
	// clientEffects commits host-owned effect declarations independently of
	// the generic action.ToolSpec surface. It is active only while the feature
	// is negotiated and never contains a receipt or key material.
	clientEffects map[string]openrealtime.ClientEffectDeclaration
	inputFormat   audioFormat
	outputFormat  audioFormat
	voice         string
	modalities    []string
	gate          perception.GateConfig
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
	// activeNS is the last time this connection demonstrably carried bytes in
	// either direction, as Unix nanoseconds. The keepalive reads it so that a
	// session in the middle of a conversation is never asked to prove it is
	// there: the audio it is exchanging already proved it.
	activeNS atomic.Int64

	settingsMu sync.RWMutex
	settings   settings

	inspectionMu      sync.Mutex
	inspectionRevoke  func()
	inspectionDispose func()
	inspectionClosed  bool

	sourcesMu sync.Mutex
	sources   map[string]*videoSource

	itemsMu sync.Mutex
	// lastItemID is the conversation item most recently added, which is what
	// the next one follows.
	//
	// conversation.item.created carries previous_item_id so a client can put
	// the conversation in order, and the field is null only for an item with
	// no predecessor. Sending null for every item tells a client that every
	// item is the first one, which an ordered view cannot recover from.
	lastItemID string
	// priorItemID is what lastItemID itself follows, so an item announced
	// twice reports the same predecessor both times.
	priorItemID string
	utterances  map[string]*wireUtterance
	// issuedCalls remembers what a tool call was, from the moment it is handed
	// to the client until the client returns its result. It is bounded because
	// nothing else bounds it: the protocol lets a client ignore a call, so the
	// entries are added by the model and removed by the client, and a client
	// that never answers leaves them for the life of the session. A meeting
	// that runs for hours is exactly where that accumulates and exactly where
	// nobody is watching.
	issuedCalls map[string]issuedCall

	responseMu sync.Mutex
	// response is the turn currently producing output. One response carries
	// the whole turn: text, audio, and function calls, indexed within it.
	response *wireResponse
	// planning is true while a rollout is deciding what this turn produces;
	// outstanding counts utterances it queued that have not yet ended.
	// reservations identifies the queued subset that has not reached
	// SpeechBegin yet. The response closes when planning and all accepted
	// asynchronous output are done.
	planning bool
	// incomplete carries why the turn stopped, for a turn that stopped for a
	// reason the client cannot infer from what it received.
	incomplete   *binding.TurnOutcome
	outstanding  int
	reservations map[string]struct{}
}

// issuedCall is a tool call the client has been handed and has not answered.
type issuedCall struct {
	name    string
	started time.Time
}

// maxOutstandingCalls bounds how many unanswered tool calls one session
// remembers. It is far above any turn's fan-out and far below a number that
// matters, which is the range a bound on something a client controls should
// sit in.
const maxOutstandingCalls = 256

// wireResponse is one turn as the protocol renders it.
type wireResponse struct {
	id        string
	nextIndex int
	output    []map[string]any
	usage     *continuation.Usage
	cancelled bool
}

type wireUtterance struct {
	responseID  string
	itemID      string
	format      audioFormat
	voice       string
	encoder     *outputEncoder
	buffer      []byte
	frameBytes  int
	sourceRate  uint32
	text        string
	outputIndex int
	startedAt   time.Time
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
		issuedCalls:  make(map[string]issuedCall),
		reservations: make(map[string]struct{}),
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
		// The voice is the binding's to state. A protocol default here would
		// name a voice from a hosted catalogue on a deployment synthesising
		// with something else entirely, which is a client told what it is
		// hearing and told wrong.
		voice: config.bindingContract.capabilities.Voice.InForce, modalities: []string{"audio"},
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
	dispose, _, err := config.management.register(result.id, runtime)
	if err != nil {
		_ = runtime.Close(context.Background(), err)
		cancel(err)
		return nil, fmt.Errorf("register session management runtime: %w", err)
	}
	result.inspectionDispose = dispose
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

// addItem records a new conversation item and reports the one it follows.
//
// The returned value is written straight into previous_item_id, so it is nil
// for the first item of a session and a string afterwards. An item announced
// twice - committed and then created - keeps the predecessor it was announced
// with rather than becoming its own.
func (session *session) addItem(itemID string) any {
	session.itemsMu.Lock()
	defer session.itemsMu.Unlock()
	if itemID == "" || itemID == session.lastItemID {
		return nullableItem(session.priorItemID)
	}
	previous := session.lastItemID
	session.priorItemID, session.lastItemID = previous, itemID
	return nullableItem(previous)
}

// predecessorOf reports what an item will follow without adding it, which is
// what input_audio_buffer.committed announces before the item exists.
func (session *session) predecessorOf(itemID string) any {
	session.itemsMu.Lock()
	defer session.itemsMu.Unlock()
	if itemID != "" && itemID == session.lastItemID {
		return nullableItem(session.priorItemID)
	}
	return nullableItem(session.lastItemID)
}

// nullableItem renders an absent predecessor as the protocol's null rather
// than as an empty string, which is a different thing on the wire.
func nullableItem(itemID string) any {
	if itemID == "" {
		return nil
	}
	return itemID
}

// Run drives the connection until it closes.
func (session *session) Run() error {
	defer session.closeInspection()
	session.markActive()
	session.wait.Add(2)
	go session.writerLoop()
	go session.handlerLoop()
	if session.config.KeepaliveInterval > 0 {
		session.wait.Add(1)
		go session.keepaliveLoop()
	}
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
	session.closeInspection()
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
			if err := session.write(message); err != nil {
				session.cancel(err)
				return
			}
		}
	}
}

// write sends one frame with a bound on how long the socket may take it.
//
// The bound is the whole point. websocket.Write returns when the peer's
// receive window has room or when its context ends, and the session context
// ends only when the session does - so a peer that stops reading blocks this
// goroutine with nothing left to end it. The failure that produces is not a
// slow session but a permanently wedged one, holding a binding runtime and its
// provider connections, invisible to every health check because the process is
// fine and it is one session that is gone.
//
// A write that times out closes the connection underneath, so the read loop
// returns and the session ends with a cause that names what happened rather
// than with an unexplained disconnect.
func (session *session) write(payload []byte) error {
	ctx := session.ctx
	if session.config.WriteTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, session.config.WriteTimeout)
		defer cancel()
	}
	if err := session.connection.Write(ctx, websocket.MessageText, payload); err != nil {
		if session.ctx.Err() == nil && ctx.Err() != nil {
			return fmt.Errorf("%w after %s", errClientStoppedReading, session.config.WriteTimeout)
		}
		return err
	}
	session.markActive()
	return nil
}

// keepaliveLoop proves the peer is still there when nothing else does.
//
// It runs on its own goroutine rather than inside the writer because a ping
// waits for its pong, and making outbound audio queue behind that wait would
// trade a rare failure for a routine one. Every Conn method except Read may be
// called concurrently, so the ping and the writer share the connection safely.
//
// The pong is read by the read loop, which is the property that makes this
// worth having in a system whose read loop is deliberately kept free of
// conversational work: if a stalled handler has backed events up far enough to
// block the reader, the pong is not collected either, and the session that
// cannot answer is the session that should end.
func (session *session) keepaliveLoop() {
	defer session.wait.Done()
	interval := session.config.KeepaliveInterval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-session.ctx.Done():
			return
		case <-ticker.C:
			if time.Since(session.lastActive()) < interval {
				continue
			}
			ctx, cancel := context.WithTimeout(session.ctx, interval)
			err := session.connection.Ping(ctx)
			cancel()
			if err != nil {
				if session.ctx.Err() != nil {
					return
				}
				session.cancel(fmt.Errorf("%w: %w", errPeerUnreachable, err))
				return
			}
			session.markActive()
		}
	}
}

func (session *session) markActive() {
	session.activeNS.Store(time.Now().UnixNano())
}

func (session *session) lastActive() time.Time {
	return time.Unix(0, session.activeNS.Load())
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
		session.markActive()
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
				causedBy, _ := event.message.EventID()
				session.sendClientError(clientError{
					code: "invalid_request_error", message: err.Error(), causedBy: causedBy,
				})
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
		causedBy, _ := message.EventID()
		return session.update(update.Session, causedBy)
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

func (session *session) update(update sessionUpdateBody, causedBy string) error {
	session.settingsMu.RLock()
	current := session.settings
	session.settingsMu.RUnlock()
	current.clientEffects = cloneClientEffectDeclarations(current.clientEffects)

	// Fields this deployment cannot honour. They are collected rather than
	// returned because they are not failures of the event: everything else in
	// it applies, and the client hears about each one by name afterwards.
	var refused []clientError
	var negotiatedExtension *openrealtime.Response

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
		clientEffects := make(map[string]openrealtime.ClientEffectDeclaration)
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
			if tool.OpenRealtime != nil && tool.OpenRealtime.ClientEffect != nil {
				clientEffects[spec.Name] = *tool.OpenRealtime.ClientEffect
			}
		}
		current.tools = specs
		current.clientEffects = clientEffects
	}
	if update.Audio.Input.Format.Type != "" {
		current.inputFormat = update.Audio.Input.Format.audioFormat
	}
	if update.Audio.Output.Format.Type != "" {
		current.outputFormat = update.Audio.Output.Format.audioFormat
	}
	if update.Audio.Output.Voice != "" {
		if voice := session.config.bindingContract.capabilities.Voice; voice.Selectable {
			current.voice = update.Audio.Output.Voice
		} else if voice.InForce != "" && update.Audio.Output.Voice == voice.InForce {
			// A client generated from the base Realtime API normally repeats a
			// voice in every session.update. A fixed-voice binding cannot honor a
			// change, but asking for the voice it already uses is not a change and
			// must not turn an otherwise compatible update into an error.
			current.voice = voice.InForce
		} else {
			refused = append(refused, clientError{
				code:  "unsupported_value",
				param: "session.audio.output.voice",
				message: fmt.Sprintf(
					"voice %q is not supported: this binding's voice is fixed when its speech "+
						"provider is created%s. The field was not applied and the rest of the "+
						"session.update was.",
					update.Audio.Output.Voice, inForce(voice.InForce)),
			})
		}
	}

	refused = append(refused, unappliedFields(update, session.config.TranscriptionModel)...)
	if update.Audio.Input.TurnDetectionSet {
		turn := update.Audio.Input.TurnDetection
		switch {
		case turn == nil:
			// Explicit null. The client is taking the floor: it will commit
			// the input buffer and ask for responses itself, and the server
			// stops ending turns on silence.
			//
			// Only where there is a floor to give. A binding whose model owns
			// the floor cannot hand over what it does not hold, and accepting
			// the declaration anyway would leave the client waiting to be
			// asked while the model answered on its own schedule.
			if !session.config.bindingContract.capabilities.ManualTurns {
				refused = append(refused, clientError{
					code:  "unsupported_value",
					param: "session.audio.input.turn_detection",
					message: "turn detection cannot be switched off on this binding: its model " +
						"owns the floor and decides for itself when a turn ended. The field " +
						"was not applied and the rest of the session.update was.",
				})
				break
			}
			current.manualTurns = true
		case turn.Type == "server_vad":
			current.manualTurns = false
			current.gate = turn.gate()
		default:
			// A detector this deployment does not have.
			//
			// The field is not applied and the client is told, by name, that
			// it was not. What is deliberately not done is either of the two
			// obvious alternatives. Refusing the whole event discards the
			// instructions, the tools, and the audio formats that arrived in
			// the same session.update - OpenAI's own SDK defaults to
			// semantic_vad, so that left every unmodified official client
			// unable to configure a session at all. Quietly substituting the
			// detector this server does have is worse in a different way: the
			// client asked for particular endpointing behaviour, did not get
			// it, and a difference it could only discover by reading a field
			// back and noticing it had changed is a difference most clients
			// will not discover.
			//
			// So: everything else applies, this does not, and an error names
			// the field. Turn detection stays whatever it already was, which
			// for a new session is this deployment's default.
			refused = append(refused, clientError{
				code:  "unsupported_value",
				param: "session.audio.input.turn_detection.type",
				message: fmt.Sprintf(
					"turn detection %q is not supported: this deployment ends turns with server_vad. "+
						"The field was not applied and the rest of the session.update was; "+
						"session.updated reports the detection actually in force.", turn.Type),
			})
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
			session.config.bindingContract.capabilities.Observers)
		if err != nil {
			return err
		}
		if inspectionDebugEnabled(response.Debug) {
			access, err := session.inspectionAccess()
			if err != nil {
				return err
			}
			response.Debug.Inspection = access
		} else {
			session.disableInspection()
		}
		// The inspection bearer is a one-time negotiation result, not durable
		// session configuration. Persist the negotiated debug policy without the
		// secret so later session events and in-memory settings cannot replay it.
		persistent := response
		if response.Debug != nil {
			debug := *response.Debug
			debug.Inspection = nil
			persistent.Debug = &debug
		}
		current.extension = persistent
		negotiatedExtension = &response
		current.observers = slices.Clone(response.Observers)
		if response.Video != nil {
			current.limits = *response.Video
		}
	}
	if update.Tools != nil && len(current.clientEffects) > 0 &&
		!hasFeature(current.extension, openrealtime.FeatureClientEffects) {
		return errors.New("tool openrealtime.client_effect requires negotiated client.effects and a server authority issuer")
	}

	session.settingsMu.Lock()
	session.settings = current
	session.settingsMu.Unlock()
	if err := session.runtime.Update(session.ctx, session.bindingSettings()); err != nil {
		return err
	}
	// The confirmation goes first. A client that sees an error before it has
	// been told the update applied has every reason to read the error as the
	// update failing, which is the misunderstanding this ordering exists to
	// prevent: session.updated says what the session now is, and the errors
	// that follow say which parts of the request did not contribute to it.
	updated := session.sessionEvent("session.updated")
	if negotiatedExtension != nil {
		object, ok := updated["session"].(map[string]any)
		if !ok {
			return errors.New("build session.updated: missing session object")
		}
		object["openrealtime"] = *negotiatedExtension
	}
	if err := session.send(updated); err != nil {
		return err
	}
	_ = session.Debug(session.ctx, binding.DebugEvent{
		Category: string(openrealtime.DebugSession), Name: "session.updated", Phase: "instant",
		Attributes: map[string]any{
			"manual_turns": current.manualTurns, "modalities": slices.Clone(current.modalities),
			"observers": slices.Clone(current.observers), "tool_count": len(current.tools),
			// This is the live session status, after sidecar handshake and session
			// policy resolution. Architecture benchmarks retain it separately from
			// their intended manifest so a preset label cannot stand in for what
			// actually ran.
			"runtime": session.runtime.Status(),
		},
		Payload: map[string]any{"instructions": current.instruction},
	})
	for _, failure := range refused {
		failure.causedBy = causedBy
		session.sendClientError(failure)
	}
	return nil
}

// supportedFeatures is what this deployment can actually offer, which is the
// validated session contract's capability set rather than a static list.
// Negotiating a feature the mounted graph cannot provide would be promising
// and then failing.
func (session *session) supportedFeatures() []openrealtime.Feature {
	capabilities := session.config.bindingContract.capabilities
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
	if session.config.ClientEffectIssuer != nil && session.config.ClientEffectIssuer.Available() {
		supported = append(supported, openrealtime.FeatureClientEffects)
	}
	return supported
}

func cloneClientEffectDeclarations(
	source map[string]openrealtime.ClientEffectDeclaration,
) map[string]openrealtime.ClientEffectDeclaration {
	if len(source) == 0 {
		return nil
	}
	cloned := make(map[string]openrealtime.ClientEffectDeclaration, len(source))
	for name, declaration := range source {
		cloned[name] = declaration
	}
	return cloned
}

type preparedClientEffectCall struct {
	call      trajectory.ToolCall
	extension *openrealtime.ClientEffectCall
}

// prepareClientEffectCalls seals every effect call before any item in the
// batch reaches the client. An issuer failure therefore cannot partially
// authorize a multi-call batch.
func (session *session) prepareClientEffectCalls(
	ctx context.Context, calls []trajectory.ToolCall,
) ([]preparedClientEffectCall, error) {
	session.settingsMu.RLock()
	negotiated := hasFeature(session.settings.extension, openrealtime.FeatureClientEffects)
	declarations := cloneClientEffectDeclarations(session.settings.clientEffects)
	tools := slices.Clone(session.settings.tools)
	session.settingsMu.RUnlock()

	prepared := make([]preparedClientEffectCall, len(calls))
	issuedAuthority := false
	for index, call := range calls {
		prepared[index].call = call
		declaration, effect := declarations[call.Name]
		if !negotiated || !effect {
			continue
		}
		if session.config.ClientEffectIssuer == nil || !session.config.ClientEffectIssuer.Available() {
			return nil, errors.New("client-effect call has no server authority issuer")
		}
		var target string
		declared := false
		for _, tool := range tools {
			if tool.Name == call.Name {
				target, declared = tool.Target, true
				break
			}
		}
		if !declared {
			return nil, fmt.Errorf("client-effect tool %q declaration drifted before authority issuance", call.Name)
		}
		_, argumentsDigest, err := effectauthority.CanonicalEffectArguments(call.Arguments)
		if err != nil {
			return nil, fmt.Errorf("client-effect tool %q arguments: %w", call.Name, err)
		}
		claims := effectauthority.EffectReceiptClaims{
			SessionID: session.id, CallID: call.CallID, Name: call.Name,
			ArgumentsDigest: argumentsDigest, DeclarationDigest: declaration.DeclarationDigest,
			Target: target,
		}
		receipt, err := session.config.ClientEffectIssuer.IssueEffectReceipt(ctx, claims)
		if err != nil {
			if ctx != nil && ctx.Err() != nil {
				return nil, context.Cause(ctx)
			}
			return nil, errors.New("client-effect server authority issuer refused the exact call")
		}
		emitted := &openrealtime.ClientEffectCall{
			Version: openrealtime.Version, DeclarationDigest: declaration.DeclarationDigest,
			Authority: receipt,
		}
		if err := emitted.Validate(); err != nil {
			return nil, fmt.Errorf("client-effect issuer returned an invalid receipt: %w", err)
		}
		prepared[index].extension = emitted
		issuedAuthority = true
	}
	if issuedAuthority && session.config.ClientEffectIssuer != nil &&
		!session.config.ClientEffectIssuer.Available() {
		return nil, errors.New("client-effect server authority issuer became unavailable")
	}

	// Recheck only the authority-relevant settings after potentially remote
	// issuer calls. Voice or debug updates do not invalidate a call; declaration
	// or target changes do.
	session.settingsMu.RLock()
	defer session.settingsMu.RUnlock()
	for _, emission := range prepared {
		if emission.extension == nil {
			continue
		}
		if !hasFeature(session.settings.extension, openrealtime.FeatureClientEffects) ||
			session.settings.clientEffects[emission.call.Name].DeclarationDigest !=
				emission.extension.DeclarationDigest {
			return nil, errors.New("client-effect declaration drifted during authority issuance")
		}
		matched := false
		for _, tool := range session.settings.tools {
			if tool.Name == emission.call.Name {
				matched = true
				for _, snapshot := range tools {
					if snapshot.Name == tool.Name && snapshot.Target != tool.Target {
						return nil, errors.New("client-effect target drifted during authority issuance")
					}
				}
				break
			}
		}
		if !matched {
			return nil, errors.New("client-effect tool disappeared during authority issuance")
		}
	}
	return prepared, nil
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
	issued := session.issuedCalls[create.Item.CallID]
	delete(session.issuedCalls, create.Item.CallID)
	session.itemsMu.Unlock()
	name := issued.name
	started := issued.started
	itemID := create.Item.ID
	if itemID == "" {
		itemID = session.nextID("item")
	}
	if err := session.send(event("conversation.item.created", session.nextID("event"), map[string]any{
		"previous_item_id": session.addItem(itemID),
		"item":             functionOutputItem(itemID, create.Item.CallID, create.Item.Output),
	})); err != nil {
		return err
	}
	durationMS := float64(0)
	if !started.IsZero() {
		durationMS = float64(time.Since(started)) / float64(time.Millisecond)
	}
	_ = session.Debug(session.ctx, binding.DebugEvent{
		Category: string(openrealtime.DebugTool), Name: "tool.result.received", Phase: "end",
		DurationMS:    durationMS,
		CorrelationID: create.Item.CallID, Attributes: map[string]any{"name": name},
		Payload: map[string]any{"output": create.Item.Output},
	})
	return session.runtime.ToolResult(session.ctx, encodeToolResult(create.Item.CallID, name, create.Item.Output))
}

// onTextMessage commits a typed message as an observation.
//
// It carries user authority, because it is the user talking: a client typing
// is the same participant as a client speaking, and the difference is the
// transport rather than the provenance.
func (session *session) onTextMessage(create conversationItemCreateEvent) error {
	session.settingsMu.RLock()
	limit := session.settings.limits.MaxFrameBytes
	session.settingsMu.RUnlock()
	images, err := create.images(limit)
	if err != nil {
		return err
	}
	text := create.text()
	if strings.TrimSpace(text) == "" && len(images) == 0 {
		return errors.New("a message item requires text or image content")
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
		"previous_item_id": session.addItem(itemID), "item": userAudioItem(itemID, text),
	})); err != nil {
		return err
	}
	return session.runtime.Text(session.ctx, binding.TextInput{
		ItemID: itemID, Role: role, Text: text, Images: images,
		// Gateway dispatch time shares the absolute Unix clock used by video
		// arrival timestamps. It is not claimed to be socket receipt time.
		OccurredNS: uint64(time.Now().UnixNano()),
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
		extension := openrealtime.ToolExtension{Target: tool.Target, Background: tool.Background}
		if tool.Confirm != action.ConfirmNever {
			extension.Confirm = string(tool.Confirm)
		}
		if hasFeature(current.extension, openrealtime.FeatureClientEffects) {
			if declaration, found := current.clientEffects[tool.Name]; found {
				copy := declaration
				extension.ClientEffect = &copy
			}
		}
		if extension.Confirm != "" || extension.Target != "" || extension.Background ||
			extension.ClientEffect != nil {
			definition["openrealtime"] = extension
		}
		tools = append(tools, definition)
	}
	object := map[string]any{
		"type": "realtime", "id": session.id, "object": "realtime.session", "model": session.model,
		"output_modalities": current.modalities, "instructions": current.instruction,
		"tools": tools, "tool_choice": "auto",
		"max_output_tokens": maxOutputTokens(session.config.bindingContract.capabilities.MaxOutputTokens),
		"audio": map[string]any{
			"input": map[string]any{
				"format":         current.inputFormat,
				"transcription":  map[string]any{"model": session.config.TranscriptionModel},
				"turn_detection": turnDetection(current),
			},
			"output": outputAudioObject(current.outputFormat, current.voice),
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
	// The extension has its own vocabulary, and the pinned registry cannot
	// know about it by construction, so asking the base validator about an
	// openrealtime.* event would fail every one of them.
	//
	// The type is read by assertion rather than by fmt.Sprint because this
	// runs on every outbound event, including one audio frame every twenty
	// milliseconds, and formatting a value that is already a string to compare
	// its prefix allocates for nothing.
	eventType, _ := value["type"].(string)
	if session.config.ValidateWire && !strings.HasPrefix(eventType, "openrealtime.") {
		// ValidateEncoded rather than Decode-then-Validate: the latter parsed
		// the envelope, copied the payload, and then parsed the whole document
		// again, which on an audio delta meant copying and re-parsing the
		// audio for no result the single pass does not produce.
		if validateErr := session.validator.ValidateEncoded(
			protocol.ProfileRealtime, protocol.DirectionServer, encoded,
		); validateErr != nil {
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

// clientError is something the client should know about that does not end the
// session. The base protocol's own description of the error event says as
// much: most errors are recoverable and the session stays open.
type clientError struct {
	code    string
	message string
	// param names the field the problem is about, when it is about one. It is
	// the difference between a client being told its request was wrong and
	// being told which part of it was.
	param string
	// causedBy is the event_id the client put on the request, echoed so it can
	// match the answer to the question. Clients are not required to send one.
	causedBy string
}

func (session *session) sendError(code, message string) {
	session.sendClientError(clientError{code: code, message: message})
}

func (session *session) sendClientError(failure clientError) {
	session.config.Logger.Warn("session error",
		"session", session.id, "code", failure.code,
		"param", failure.param, "message", failure.message)
	body := map[string]any{
		"type": "invalid_request_error", "code": failure.code, "message": failure.message,
	}
	_ = session.Debug(session.ctx, binding.DebugEvent{
		Category: string(openrealtime.DebugError), Name: "session.error", Phase: "error",
		Message: failure.message, Attributes: map[string]any{"code": failure.code, "param": failure.param},
	})
	if failure.param != "" {
		body["param"] = failure.param
	}
	if failure.causedBy != "" {
		body["event_id"] = failure.causedBy
	}
	_ = session.send(event("error", session.nextID("event"), map[string]any{"error": body}))
}

func (session *session) recordCallNames(calls []trajectory.ToolCall) {
	session.itemsMu.Lock()
	defer session.itemsMu.Unlock()
	now := time.Now()
	for _, call := range calls {
		session.issuedCalls[call.CallID] = issuedCall{name: call.Name, started: now}
	}
	session.forgetOldestCalls()
}

// forgetOldestCalls keeps the outstanding-call record bounded.
//
// A client is allowed to ignore a tool call, so an entry is added when the
// model issues one and removed when the client answers it, and the two are not
// the same rate. Nothing in the protocol closes the gap and nothing else in
// the session does either: these entries outlive the response that produced
// them, because a client may legitimately answer a call several turns later.
//
// The oldest go first because the record only feeds the name and the elapsed
// time on a tool-result debug event. An answer that arrives after this many
// unanswered calls loses its name and its duration, and still reaches the
// runtime intact - which is the right thing to give up, and far better than a
// map that only grows in the sessions that run longest.
func (session *session) forgetOldestCalls() {
	for len(session.issuedCalls) > maxOutstandingCalls {
		oldestID, oldest := "", time.Time{}
		for callID, call := range session.issuedCalls {
			if oldestID == "" || call.started.Before(oldest) {
				oldestID, oldest = callID, call.started
			}
		}
		delete(session.issuedCalls, oldestID)
	}
}
