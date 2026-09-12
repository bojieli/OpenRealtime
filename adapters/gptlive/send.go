package gptlive

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/coder/websocket"
)

// clientEvent is the part of a Realtime client event this translator reads,
// plus the three OpenRealtime-internal events the upstream binding sends when
// it has something the base protocol cannot say.
type clientEvent struct {
	Type    string `json:"type"`
	Audio   string `json:"audio"`
	Session struct {
		Instructions string `json:"instructions"`
		Voice        string `json:"voice"`
		Audio        struct {
			Output struct {
				Voice string `json:"voice"`
			} `json:"output"`
		} `json:"audio"`
	} `json:"session"`
	Item struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		CallID  string `json:"call_id"`
		Output  string `json:"output"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"item"`
	// Text is the payload of an internal context or steer event.
	Text string `json:"text"`
	// Muted is the payload of an internal mute event.
	Muted bool `json:"muted"`
}

// Internal event types the binding may send. No Realtime endpoint ever sees
// them: the binding translates them itself before an ordinary client, and
// only a translator like this one receives them by name.
const (
	// EventContext carries evidence the voice should know without saying.
	EventContext = "openrealtime.upstream.context"
	// EventSteer carries an application instruction that changes behaviour
	// now, including interrupting speech in progress.
	EventSteer = "openrealtime.upstream.steer"
	// EventMute holds or resumes the endpoint's hearing.
	EventMute = "openrealtime.upstream.mute"
)

// stopInstruction is what a cancel becomes. Live has no response to cancel;
// what it has is an instruction channel that interrupts speech.
const stopInstruction = "Stop speaking now and wait for the user."

// Send translates one Realtime client event into Live protocol messages.
//
// An event with no Live equivalent is accepted and dropped rather than refused,
// for the reason the Gemini translator gives: the caller is speaking a protocol
// this endpoint does not implement, and failing its ordinary traffic would take
// down a session over a message that changes nothing. Here that covers the
// commit and truncate halves of a turn loop a full-duplex endpoint does not run.
func (client *Client) Send(ctx context.Context, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode Realtime event for GPT-Live: %w", err)
	}
	var event clientEvent
	if err := json.Unmarshal(encoded, &event); err != nil {
		return fmt.Errorf("decode Realtime event for GPT-Live: %w", err)
	}

	switch event.Type {
	case "session.update":
		if client.sideband {
			// The session was declared by whoever owns it.
			return nil
		}
		return client.configure(ctx, event)
	case "input_audio_buffer.append":
		if client.sideband {
			return errors.New("a GPT-Live sideband cannot carry audio; the primary connection does")
		}
		return client.appendAudio(ctx, event.Audio)
	case "conversation.item.create":
		return client.bufferHandoff(event)
	case "response.create":
		return client.speak(ctx)
	case "response.cancel":
		// Live cannot cancel a response, because it has no responses. It can
		// be told to stop, which is what the caller meant.
		return client.appendChannel(ctx, "session.instructions.append", stopInstruction, "", "openrealtime_stop")
	case EventContext:
		return client.appendChannel(ctx, "session.thinking.append", event.Text, "", "openrealtime_context")
	case EventSteer:
		return client.appendChannel(ctx, "session.instructions.append", event.Text, "", "openrealtime_steer")
	case EventMute:
		return client.mute(ctx, event.Muted)
	case "input_audio_buffer.commit", "conversation.item.truncate":
		// Both belong to a turn loop this endpoint does not run. Live streams
		// continuously and interrupts on new input, so there is nothing to
		// commit or truncate.
		return nil
	default:
		return nil
	}
}

// configure performs the handshake on the first session.update and reduces
// every later one to what actually changed.
//
// Everything that matters about a Live session - model, instruction, voice,
// audio format, and the delegation mode - is fixed in session.start and
// settable nowhere else, so the handshake waits for the caller's first
// session.update rather than guessing. A later update cannot change those
// fields; what it can do is add to the instruction, through a channel capped at
// 500 tokens. So the difference is sent when it fits, and the caller is told
// when it does not, rather than the update being dropped in silence.
func (client *Client) configure(ctx context.Context, event clientEvent) error {
	instruction := strings.TrimSpace(event.Session.Instructions)
	client.writeMu.Lock()
	startSent := client.startSent
	previous := client.startInstruction
	forking := client.forking
	client.writeMu.Unlock()
	if startSent {
		if forking {
			// A fork inherits its instruction. Whatever the caller's later
			// session.update carries is what it declared at the start, and
			// the recording already has it.
			return nil
		}
		if instruction == "" || instruction == previous {
			return nil
		}
		if len([]rune(instruction)) > commentaryChunkRunes {
			return client.emit(ctx, "error", map[string]any{
				"type": "error",
				"error": map[string]any{
					"code": "instruction_update_rejected",
					"message": "GPT-Live fixes the instruction at session start; a later change " +
						"can only be appended, and this one exceeds the 500-token append cap",
				},
			})
		}
		client.writeMu.Lock()
		client.startInstruction = instruction
		client.writeMu.Unlock()
		return client.appendChannel(ctx, "session.instructions.append", instruction, "", "openrealtime_instruction")
	}
	return client.start(ctx, event, instruction)
}

// start sends session.start.
func (client *Client) start(ctx context.Context, event clientEvent, instruction string) error {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	if client.startSent {
		return nil
	}
	voice := strings.TrimSpace(event.Session.Audio.Output.Voice)
	if voice == "" {
		voice = strings.TrimSpace(event.Session.Voice)
	}
	if voice == "" {
		voice = client.config.Voice
	}
	// Remembered so a restart - after the project refuses storage - opens
	// the session with the voice the caller chose, not the default.
	client.config.Voice = voice
	format := map[string]any{"type": client.config.SessionFormat, "rate": client.config.SessionSampleRateHz}
	var session map[string]any
	if client.forking {
		// A fork inherits everything from the stored session - model,
		// instruction, voice, delegation, history. What it does not inherit
		// is the WebSocket audio format, so that is all the start carries.
		session = map[string]any{"audio": map[string]any{"format": format}}
	} else {
		session = map[string]any{
			"model": client.config.Model,
			"audio": map[string]any{
				"format": format,
				"output": map[string]any{"voice": voice},
			},
			"delegation": client.delegation(),
		}
		if instruction != "" {
			session["instructions"] = instruction
		}
	}
	if client.config.Store {
		session["store"] = true
	}
	if err := client.write(ctx, map[string]any{
		"type": "session.start", "event_id": "openrealtime_start", "session": session,
	}); err != nil {
		return err
	}
	client.startSent = true
	client.startInstruction = instruction
	return nil
}

// delegation declares where backend work goes.
//
// Client delegation is what this binding is for: the background reasoner is
// the backend, and handing the work to a Responses model as well would put two
// reasoners on one conversation. A deployment that wants the vendor's managed
// loop instead says so by naming a Responses model, and then this side stops
// reasoning and keeps mirroring, observing, and holding the floor.
func (client *Client) delegation() map[string]any {
	if client.config.ResponsesModel == "" {
		return map[string]any{"type": "client"}
	}
	responses := map[string]any{"model": client.config.ResponsesModel}
	if instructions := strings.TrimSpace(client.config.ResponsesInstructions); instructions != "" {
		responses["instructions"] = instructions
	}
	if len(client.config.ResponsesTools) > 0 {
		responses["tools"] = client.config.ResponsesTools
		responses["tool_choice"] = "auto"
	}
	return map[string]any{"type": "responses", "responses": responses}
}

// appendAudio converts one frame to the session's rate and streams it.
func (client *Client) appendAudio(ctx context.Context, encoded string) error {
	if encoded == "" {
		return nil
	}
	payload, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("decode audio for GPT-Live: %w", err)
	}
	if len(payload) == 0 {
		return nil
	}
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	if client.toLive != nil {
		if payload, err = client.toLive.Push(payload); err != nil {
			return fmt.Errorf("resample audio for GPT-Live: %w", err)
		}
		if len(payload) == 0 {
			return nil
		}
	}
	if client.compand != nil {
		payload = client.compand(payload)
	}
	client.lastCallerNS = client.config.Scheduler.NowNS()
	return client.write(ctx, map[string]any{
		"type":  "session.input_audio.append",
		"audio": base64.StdEncoding.EncodeToString(payload),
	})
}

// mute holds or resumes the endpoint's hearing. It does not stop billing,
// delegated work, or speech already generating; it stops the model hearing.
func (client *Client) mute(ctx context.Context, muted bool) error {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	eventType := "session.input_audio.unmute"
	if muted {
		eventType = "session.input_audio.mute"
	}
	return client.write(ctx, map[string]any{"type": eventType, "event_id": "openrealtime_" + eventType})
}

// bufferHandoff holds the text of a conversation item until the caller asks for
// a response.
//
// The binding hands a completed answer over as an item followed by
// response.create, which is the portable form and what every endpoint modelled
// on the Realtime API accepts. Live has no conversation items, so the pair is
// buffered here and becomes one commentary append - the channel the vendor
// built for returning delegated results to the voice.
func (client *Client) bufferHandoff(event clientEvent) error {
	if event.Item.Type == "function_call_output" {
		// Under client delegation the remote has no tool authority, so a
		// result meant for it is a result the reasoner already owns. Under
		// Responses delegation the managed backend asked for it, and this is
		// how it is returned.
		if client.ResponsesDelegation() {
			return client.returnToolResult(event)
		}
		return nil
	}
	var text strings.Builder
	for _, part := range event.Item.Content {
		if part.Text != "" {
			text.WriteString(part.Text)
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		return nil
	}
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	if client.handoff.Len() > 0 {
		client.handoff.WriteString("\n\n")
	}
	client.handoff.WriteString(text.String())
	return nil
}

// returnToolResult hands one function result to the managed backend.
func (client *Client) returnToolResult(event clientEvent) error {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	return client.write(context.Background(), map[string]any{
		"type": "response.item.create", "event_id": "openrealtime_tool_result",
		"item": map[string]any{
			"type": "function_call_output", "call_id": event.Item.CallID,
			"output": event.Item.Output,
		},
	})
}

// speak gives the buffered answer to the Live voice as commentary.
//
// With nothing buffered there is nothing to say: response.create in Live starts
// or continues *backend* work, and this binding's backend is the engine, which
// has not been asked for anything. Dropping it is what keeps a stray
// response.create from talking to the vendor's Responses API instead.
func (client *Client) speak(ctx context.Context) error {
	client.writeMu.Lock()
	answer := strings.TrimSpace(client.handoff.String())
	client.handoff.Reset()
	client.writeMu.Unlock()
	if client.ResponsesDelegation() {
		// response.create means "continue the backend" here, which is exactly
		// what the caller asked for after returning a result. Nothing is
		// spoken from this side: the managed backend's answer reaches the
		// voice by itself.
		client.writeMu.Lock()
		defer client.writeMu.Unlock()
		return client.write(ctx, map[string]any{
			"type": "response.create", "event_id": "openrealtime_continue",
		})
	}
	if answer == "" {
		return nil
	}
	// A delegation the endpoint opened is the right home for this result: it
	// is what the endpoint is waiting on, and correlating the two is what lets
	// it keep talking until the answer lands. When it asked for nothing, the
	// answer is still session context, which is what a null ID means.
	return client.appendChannel(ctx, "session.commentary.append", answer, client.openDelegation(), "openrealtime_handoff")
}

// appendChannel sends text down one of the three append channels, split into
// pieces the endpoint will accept.
//
// The delegation ID is required and nullable, so the field is always present:
// empty means null, which is session-wide context rather than a reply.
func (client *Client) appendChannel(ctx context.Context, channel, text, delegation, prefix string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	for index, chunk := range splitForAppend(text) {
		message := map[string]any{
			"type":          channel,
			"event_id":      fmt.Sprintf("%s_%d", prefix, index+1),
			"content":       chunk,
			"delegation_id": nil,
		}
		if delegation != "" {
			message["delegation_id"] = delegation
		}
		if err := client.write(ctx, message); err != nil {
			return err
		}
	}
	return nil
}

// splitForAppend divides text into appends the endpoint will accept.
//
// One append is capped at 500 tokens, and this side can only count characters,
// so the bound is conservative. Splitting rather than truncating is deliberate:
// the vendor documents repeated appends as the way to continue one delegation,
// and a hand-off that arrives in two pieces is a hand-off, while one rejected
// for length is the binding's entire contribution silently lost.
func splitForAppend(answer string) []string {
	runes := []rune(answer)
	if len(runes) <= commentaryChunkRunes {
		return []string{answer}
	}
	var chunks []string
	for len(runes) > 0 {
		if len(runes) <= commentaryChunkRunes {
			chunks = append(chunks, strings.TrimSpace(string(runes)))
			break
		}
		// Prefer a sentence end, then any space, so a split lands between
		// thoughts rather than inside an identifier the answer must preserve.
		cut := lastBreak(runes[:commentaryChunkRunes])
		chunks = append(chunks, strings.TrimSpace(string(runes[:cut])))
		runes = runes[cut:]
	}
	return chunks
}

// lastBreak finds where to cut a chunk, preferring the end of a sentence and
// falling back to whitespace, then to the hard bound.
func lastBreak(runes []rune) int {
	// Only the final quarter is searched: a break found much earlier would
	// trade one oversized append for many tiny ones.
	floor := len(runes) - len(runes)/4
	for index := len(runes) - 1; index >= floor; index-- {
		switch runes[index] {
		case '.', '!', '?', '\n':
			return index + 1
		}
	}
	for index := len(runes) - 1; index >= floor; index-- {
		if unicode.IsSpace(runes[index]) {
			return index + 1
		}
	}
	return len(runes)
}

// write encodes and sends, holding traffic that precedes the handshake.
//
// The gate is session.started rather than session.start, because the vendor
// requires it: nothing but the start command may go out until the endpoint has
// confirmed the session. Holding rather than dropping keeps the opening audio,
// and the held messages are released here - on the caller's next send - rather
// than by the read goroutine, so that reading, which is what answers the
// endpoint's pings, is never waiting on a write.
func (client *Client) write(ctx context.Context, message map[string]any) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode GPT-Live message: %w", err)
	}
	if message["type"] != "session.start" && !client.started {
		if len(client.pending) >= pendingLimit {
			return errors.New("GPT-Live handshake did not complete before the audio buffer filled")
		}
		client.pending = append(client.pending, encoded)
		return nil
	}
	if len(client.pending) > 0 {
		held := client.pending
		client.pending = nil
		for _, earlier := range held {
			if err := client.writeRaw(ctx, earlier); err != nil {
				return err
			}
		}
	}
	return client.writeRaw(ctx, encoded)
}

// release sends everything held for the handshake.
//
// It runs on its own goroutine rather than on the read loop, so that reading -
// which is what answers the endpoint's pings - never waits on a write. A caller
// that keeps sending would drain the queue through write anyway; this is for
// the one that does not. A probe sends three messages and then listens, and
// without this its session would open and never say anything.
func (client *Client) release(ctx context.Context) error {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	if !client.started || len(client.pending) == 0 {
		return nil
	}
	held := client.pending
	client.pending = nil
	for _, encoded := range held {
		if err := client.writeRaw(ctx, encoded); err != nil {
			return err
		}
	}
	return nil
}

// writeRaw sends one encoded message. Callers hold writeMu.
func (client *Client) writeRaw(ctx context.Context, encoded []byte) error {
	select {
	case <-client.closed:
		return errors.New("GPT-Live connection is closed")
	default:
	}
	if client.config.WriteTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, client.config.WriteTimeout)
		defer cancel()
	}
	if err := client.connection.Write(ctx, websocket.MessageText, encoded); err != nil {
		return fmt.Errorf("send to GPT-Live: %w", err)
	}
	return nil
}
