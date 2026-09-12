package gptlive

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/bojieli/OpenRealtime/realtimeclient"
	"github.com/coder/websocket"
)

// serverEvent is the part of a Live server event this translator reads. Unlike
// Gemini's, this protocol does have a type field; what it lacks is any event
// that ends something.
type serverEvent struct {
	Type     string `json:"type"`
	Delta    string `json:"delta"`
	Audio    string `json:"audio"`
	StartMS  int64  `json:"start_ms"`
	EndMS    int64  `json:"end_ms"`
	OffsetMS int64  `json:"offset_ms"`
	Session  struct {
		ID        string `json:"id"`
		ExpiresAt int64  `json:"expires_at"`
	} `json:"session"`
	Delegation struct {
		ID     string `json:"id"`
		Target string `json:"target"`
	} `json:"delegation"`
	Usage struct {
		Seconds float64 `json:"seconds"`
	} `json:"usage"`
	ContextWindow struct {
		UsageRatio float64 `json:"usage_ratio"`
	} `json:"context_window"`
	// ClientEventID correlates an acknowledgement with the command it answers.
	ClientEventID string `json:"client_event_id"`
	// Code and Message are an info notice's fields; Reason is why a session
	// closed.
	Code    string `json:"code"`
	Message string `json:"message"`
	Reason  string `json:"reason"`
	Error   struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Param   string `json:"param"`
	} `json:"error"`
}

// read is the only goroutine touching the connection's reader.
func (client *Client) read(ctx context.Context) {
	defer close(client.events)
	defer close(client.done)
	for {
		client.writeMu.Lock()
		connection := client.connection
		client.writeMu.Unlock()
		kind, payload, err := connection.Read(ctx)
		if err != nil {
			if client.fork(ctx, err) {
				continue
			}
			// A session that ends while someone was mid-sentence still has a
			// turn to commit: the words were heard, and dropping them would
			// lose the last thing said on every disconnection.
			client.finishTurns(ctx)
			if !isCleanClose(err) {
				client.fail(fmt.Errorf("read GPT-Live stream: %w", err))
			}
			return
		}
		if kind != websocket.MessageText && kind != websocket.MessageBinary {
			continue
		}
		var event serverEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			client.fail(fmt.Errorf("decode GPT-Live event: %w", err))
			return
		}
		client.armStallWatch(ctx)
		if err := client.translate(ctx, event); err != nil {
			client.fail(err)
			return
		}
	}
}

// translate turns one Live event into the Realtime events the mirror reads.
func (client *Client) translate(ctx context.Context, event serverEvent) error {
	switch event.Type {
	case "session.started":
		// This is the gate on everything the caller has been holding. Only the
		// flag is set here; the held messages go out on the caller's next send
		// so that reading is never blocked behind a write.
		client.writeMu.Lock()
		client.started = true
		client.sessionID = event.Session.ID
		client.writeMu.Unlock()
		// The read loop armed the watchdog before this event, when the session
		// had not started and arming was refused. A session that starts and
		// then sends nothing at all is exactly the stall worth naming.
		client.armStallWatch(ctx)
		// The Realtime equivalent, carrying what an operator needs to find
		// this session again: its vendor identity and when it expires.
		if err := client.emit(ctx, "session.created", map[string]any{
			"type": "session.created",
			"session": map[string]any{
				"id": event.Session.ID, "expires_at": event.Session.ExpiresAt,
				"model": client.config.Model,
			},
		}); err != nil {
			return err
		}
		// Releasing happens off this goroutine: a caller that streams audio
		// would drain the queue on its next send, but one that sends an
		// opening and then listens would wait for a send that never comes.
		go func() {
			if err := client.release(ctx); err != nil {
				client.fail(err)
			}
		}()
		// Nothing this session does works until frames are arriving, and a
		// caller speaking the Realtime protocol has no reason to send any
		// while the user is quiet.
		client.startFrameClock(ctx)
		return nil

	case "session.input_transcript.delta":
		return client.noteUserTranscript(ctx, event.Delta, event.StartMS, event.EndMS)

	case "session.output_transcript.delta":
		return client.noteAssistantTranscript(ctx, event.Delta)

	case "session.output_audio.delta":
		return client.forwardAudio(ctx, event.Delta, event.StartMS, event.EndMS)

	case "session.input_audio.append":
		// Only a sideband receives this: a copy of what the primary heard,
		// with no timestamps. It is evidence about the audio path this
		// process is not on, named so the binding can keep it if it wants.
		return client.emit(ctx, "openrealtime.upstream.reflected_input", map[string]any{
			"type": "openrealtime.upstream.reflected_input", "audio": event.Audio,
		})

	case "transport.ringing", "transport.answered", "transport.failed",
		"transport.dtmf.received", "transport.dtmf.send":
		// Telephony: the call leg's own lifecycle, which the vendor holds.
		return client.emit(ctx, "openrealtime.upstream.transport", map[string]any{
			"type": "openrealtime.upstream.transport",
			"kind": strings.TrimPrefix(event.Type, "transport."),
		})

	case "session.delegation.created":
		if event.Delegation.Target == "responses" {
			// The session was opened in client mode, so this cannot happen
			// against a correctly configured endpoint. If it does, the
			// reasoner is not the backend and the binding's contribution is
			// going somewhere this process cannot see.
			return client.emit(ctx, "error", map[string]any{
				"type": "error",
				"error": map[string]any{
					"code": "unexpected_responses_delegation",
					"message": "the Live session delegated to a Responses backend; " +
						"this binding configures client delegation so that the " +
						"background reasoner is the backend",
				},
			})
		}
		if err := client.noteDelegation(ctx, event.Delegation.ID, event.Delegation.Target); err != nil {
			return err
		}
		// The boundary above is what a Realtime reader needs; this is what the
		// binding needs - the fact that the voice asked, and for which request.
		// It is the escalation signal the rollout already understands, carried
		// under a name no Realtime endpoint will ever send.
		return client.emit(ctx, "openrealtime.upstream.delegation", map[string]any{
			"type":          "openrealtime.upstream.delegation",
			"delegation_id": event.Delegation.ID,
			"target":        event.Delegation.Target,
			"offset_ms":     event.OffsetMS,
		})

	case "session.usage.updated":
		// A cumulative snapshot, never an increment, and the only regular
		// heartbeat this endpoint sends: it is what the stall watchdog counts.
		return client.emit(ctx, "openrealtime.upstream.usage", map[string]any{
			"type":                 "openrealtime.upstream.usage",
			"seconds":              event.Usage.Seconds,
			"context_window_ratio": event.ContextWindow.UsageRatio,
		})

	case "session.instructions.appended", "session.thinking.appended", "session.commentary.appended",
		"session.input_audio.muted", "session.input_audio.unmuted", "session.updated":
		// An acknowledgement changes nothing in the conversation, and the
		// mirror has no use for it. It is still the only proof that an append
		// was injected - the vendor says so in as many words - so it is named
		// for the debug stream and for a test that has to know.
		return client.emit(ctx, "openrealtime.upstream.ack", map[string]any{
			"type": "openrealtime.upstream.ack", "of": event.Type,
			"client_event_id": event.ClientEventID,
			"start_ms":        event.StartMS, "end_ms": event.EndMS,
		})

	case "info":
		return client.emit(ctx, "openrealtime.upstream.info", map[string]any{
			"type": "openrealtime.upstream.info", "code": event.Code, "message": event.Message,
		})

	case "error":
		if event.Error.Code == "session_storage_not_allowed" && client.storageRefused(ctx) {
			// The project does not permit persistence, so the vendor refused
			// the whole start over the store flag. Storage was a request,
			// not the session; the session is started again without it and
			// the refusal is reported, so that fork and recording are known
			// to be unavailable rather than discovered later.
			return nil
		}
		// A moderation error can cut off speech without ending the session, so
		// what was said up to that point is a real utterance and is committed
		// before the failure is reported.
		client.finishTurns(ctx)
		code := event.Error.Code
		if code == "" {
			code = event.Error.Type
		}
		return client.emit(ctx, "error", map[string]any{
			"type":  "error",
			"error": map[string]any{"code": code, "message": event.Error.Message},
		})

	case "session.closed":
		client.finishTurns(ctx)
		client.stopStallWatch()
		client.finalOnce.Do(func() { close(client.finalised) })
		if event.Reason != "" && event.Reason != "close_requested" {
			// expired, content, remote_hangup, connection_lost: the session is
			// over and this side did not end it. Each is a different thing to
			// tell an operator, so the reason travels as the code.
			return client.emit(ctx, "error", map[string]any{
				"type": "error",
				"error": map[string]any{
					"code":    "session_closed_" + event.Reason,
					"message": "the Live session ended: " + event.Reason,
				},
			})
		}
		return nil

	default:
		// Acknowledgements, usage snapshots, informational notices, and the
		// nested Responses stream all land here. None of them changes the
		// conversation this binding mirrors.
		return nil
	}
}

// storageRefused restarts a session whose store request the project refused,
// reporting whether it did. It only applies before the session has started:
// after that a storage error is something else.
func (client *Client) storageRefused(ctx context.Context) bool {
	client.writeMu.Lock()
	if client.started || !client.config.Store {
		client.writeMu.Unlock()
		return false
	}
	client.config.Store = false
	client.startSent = false
	instruction := client.startInstruction
	client.writeMu.Unlock()
	if err := client.emit(ctx, "openrealtime.upstream.info", map[string]any{
		"type": "openrealtime.upstream.info", "code": "storage_refused",
		"message": "the project does not permit data persistence; the session runs unstored, " +
			"so it cannot be forked after a drop and has no recording",
	}); err != nil {
		return false
	}
	event := clientEvent{}
	event.Session.Instructions = instruction
	if err := client.configure(ctx, event); err != nil {
		client.fail(fmt.Errorf("restart without storage: %w", err))
		return false
	}
	return true
}

// fork continues a dropped session from the vendor's recording, reporting
// whether it did.
//
// It runs on the read goroutine, which is the one that noticed. A drop this
// side asked for, a session the vendor finalised, or one that never started
// has nothing to continue; and a session opened without Store has no
// recording to continue from. Otherwise the stored session is forked - the
// vendor's own recovery guidance - the new connection takes the old one's
// place under the write lock, and the caller's traffic is held by the same
// gate that held it during the first handshake.
func (client *Client) fork(ctx context.Context, cause error) bool {
	select {
	case <-client.closed:
		return false
	case <-client.finalised:
		return false
	default:
	}
	if !client.reconnectable() || ctx.Err() != nil {
		return false
	}
	client.writeMu.Lock()
	source := client.sessionID
	if source == "" || client.reconnects >= client.config.MaxReconnects {
		client.writeMu.Unlock()
		return false
	}
	client.reconnects++
	attempt := client.reconnects
	client.writeMu.Unlock()

	connection, err := client.dial(ctx, forkURL(client.dialURL, source))
	if err != nil {
		_ = client.emit(ctx, "error", map[string]any{
			"type": "error",
			"error": map[string]any{
				"code":    "upstream_fork_failed",
				"message": fmt.Sprintf("the connection dropped (%v) and forking %s failed: %v", cause, source, err),
			},
		})
		return false
	}
	client.writeMu.Lock()
	_ = client.connection.CloseNow()
	client.connection = connection
	client.forking = true
	client.startSent = false
	client.started = false
	client.writeMu.Unlock()
	client.stopStallWatch()
	// The fork's start carries only the audio format; the instruction and
	// everything else are the recording's.
	if err := client.configure(ctx, clientEvent{}); err != nil {
		client.fail(fmt.Errorf("start the fork of %s: %w", source, err))
		return false
	}
	_ = client.emit(ctx, "openrealtime.upstream.reconnected", map[string]any{
		"type": "openrealtime.upstream.reconnected", "from": source, "attempt": attempt,
		"cause": cause.Error(),
	})
	return true
}

// forwardAudio passes on one output frame, if it is part of an utterance.
//
// Live's output is a continuous carrier, not a per-response burst. It emits a
// 100 ms frame every 100 ms for the life of the session whether or not anyone
// is speaking - measured against the real endpoint, 427 of 438 frames in a
// 45-second session were digital silence. That is what full duplex means here:
// the output is a track, and the Realtime protocol's response.output_audio.delta
// is not. Forwarding the carrier would tell a client the assistant had been
// speaking continuously since the session opened.
//
// So silence is dropped unless an utterance is already open, where it is real
// content: the pause between two words belongs to the speech around it, and
// removing it would close the gaps and change the timing of what is played.
func (client *Client) forwardAudio(ctx context.Context, encoded string, startMS, endMS int64) error {
	if encoded == "" {
		return nil
	}
	payload, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("decode audio from GPT-Live: %w", err)
	}
	if client.expand != nil {
		// G.711 is one byte per sample and audibility is a question about
		// samples, so the law is expanded before anything looks at it.
		payload = client.expand(payload)
		encoded = ""
	}
	if !client.noteAssistantAudio(audible(payload)) {
		return nil
	}
	if client.fromLive != nil {
		converted, err := client.fromLive.Push(payload)
		if err != nil {
			return fmt.Errorf("resample audio from GPT-Live: %w", err)
		}
		if len(converted) == 0 {
			return nil
		}
		payload = converted
		encoded = ""
	}
	if encoded == "" {
		encoded = base64.StdEncoding.EncodeToString(payload)
	}
	frame := map[string]any{"type": "response.output_audio.delta", "delta": encoded}
	if endMS > startMS {
		// Reflected output on a sideband carries its range on the session
		// timeline; the primary connection's does not. Passed on when
		// present, because it is the only audio timing the vendor gives.
		frame["start_ms"], frame["end_ms"] = startMS, endMS
	}
	return client.emit(ctx, "response.output_audio.delta", frame)
}

// silenceFloor is the peak sample below which a frame carries no speech.
//
// It is 32 of a possible 32767, about -60 dBFS. The carrier measures exactly
// zero, so this only has to be above the noise a codec might leave behind and
// far below anything a microphone would call quiet.
const silenceFloor = 32

// audible reports whether a mono 16-bit little-endian frame contains sound.
func audible(payload []byte) bool {
	for index := 0; index+1 < len(payload); index += 2 {
		sample := int16(uint16(payload[index]) | uint16(payload[index+1])<<8)
		if sample > silenceFloor || sample < -silenceFloor {
			return true
		}
	}
	return false
}

// emit delivers one synthesised Realtime event.
func (client *Client) emit(ctx context.Context, eventType string, body map[string]any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode translated %s: %w", eventType, err)
	}
	select {
	case client.events <- realtimeclient.Event{Type: eventType, Raw: raw}:
		return nil
	case <-client.closed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// isCleanClose reports an ending that is not a failure.
//
// Cancelling the session's context is how a session ends normally, and the read
// that was in flight fails with it. Reporting that as an upstream error puts a
// failure on every clean shutdown, which teaches an operator to ignore the one
// field that should mean something.
func isCleanClose(err error) bool {
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return true
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed)
}
