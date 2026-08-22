package geminilive

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/coder/websocket"
)

// clientEvent is the part of a Realtime client event this translator reads.
type clientEvent struct {
	Type    string `json:"type"`
	Audio   string `json:"audio"`
	Session struct {
		Instructions string           `json:"instructions"`
		Tools        []map[string]any `json:"tools"`
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
}

// Send translates one Realtime client event into Live protocol messages.
//
// An event with no Live equivalent is accepted and dropped rather than
// refused. The caller is speaking a protocol this endpoint does not implement,
// and failing its ordinary traffic would take down a session over a message
// that changes nothing - response.cancel being the clear case, since Gemini
// interrupts on new input rather than on request.
func (client *Client) Send(ctx context.Context, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode Realtime event for Gemini Live: %w", err)
	}
	var event clientEvent
	if err := json.Unmarshal(encoded, &event); err != nil {
		return fmt.Errorf("decode Realtime event for Gemini Live: %w", err)
	}

	switch event.Type {
	case "session.update":
		return client.setup(ctx, event)
	case "input_audio_buffer.append":
		return client.appendAudio(ctx, event.Audio)
	case "input_audio_buffer.commit":
		// Gemini's own activity detection owns the turn, which is exactly the
		// arrangement this binding defaults to. A commit is the client saying
		// what the endpoint has already decided, so it changes nothing.
		return nil
	case "conversation.item.create":
		return client.appendTurn(event)
	case "response.create":
		return client.requestResponse(ctx)
	case "conversation.item.truncate", "response.cancel":
		return nil
	default:
		return nil
	}
}

// setup performs the handshake, carrying the instruction the caller declared.
//
// The Live API takes the system instruction only here, so a later
// session.update cannot change it. Rather than pretend otherwise, the second
// and subsequent ones are ignored, and the catalogue routes Gemini's hand-off
// through a conversation item for the same reason.
func (client *Client) setup(ctx context.Context, event clientEvent) error {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	if client.setupSent {
		return nil
	}
	setup := map[string]any{
		"model": client.model,
		"generationConfig": map[string]any{
			"responseModalities": []string{"AUDIO"},
		},
		// Both transcriptions are asked for because the mirror is what feeds
		// the background reasoner: without them the reasoner would share a
		// conversation it cannot read.
		"inputAudioTranscription":  map[string]any{},
		"outputAudioTranscription": map[string]any{},
	}
	if instruction := strings.TrimSpace(event.Session.Instructions); instruction != "" {
		setup["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": instruction}},
		}
	}
	if err := client.write(ctx, map[string]any{"setup": setup}); err != nil {
		return err
	}
	client.setupSent = true
	for _, held := range client.pending {
		if err := client.writeRaw(ctx, held); err != nil {
			return err
		}
	}
	client.pending = nil
	return nil
}

// appendAudio resamples one frame and streams it.
func (client *Client) appendAudio(ctx context.Context, encoded string) error {
	if encoded == "" {
		return nil
	}
	payload, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("decode audio for Gemini Live: %w", err)
	}
	if len(payload) == 0 {
		return nil
	}
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	converted, err := client.resampler.Push(payload)
	if err != nil {
		return fmt.Errorf("resample audio for Gemini Live: %w", err)
	}
	if len(converted) == 0 {
		return nil
	}
	return client.write(ctx, map[string]any{
		"realtimeInput": map[string]any{
			"audio": map[string]any{
				"mimeType": fmt.Sprintf("audio/pcm;rate=%d", InputSampleRateHz),
				"data":     base64.StdEncoding.EncodeToString(converted),
			},
		},
	})
}

// appendTurn buffers a conversation item as a client turn.
//
// Nothing is sent yet: a Live turn is delivered complete, and the Realtime
// protocol splits the same thing into an item followed by a request to
// respond. Buffering here is what makes those two events one turn.
func (client *Client) appendTurn(event clientEvent) error {
	if event.Item.Type == "function_call_output" {
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
	// Live turns are user or model. A Realtime system message is context the
	// endpoint should act on now, which is what a user turn is here.
	role := "user"
	if event.Item.Role == "assistant" {
		role = "model"
	}
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	client.turn = append(client.turn, map[string]any{
		"role":  role,
		"parts": []map[string]any{{"text": text.String()}},
	})
	return nil
}

// requestResponse completes the buffered turn, which is what makes Gemini
// answer. With nothing buffered there is nothing to complete: the endpoint is
// already generating from the audio it heard.
func (client *Client) requestResponse(ctx context.Context) error {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	if len(client.turn) == 0 {
		return nil
	}
	turns := client.turn
	client.turn = nil
	return client.write(ctx, map[string]any{
		"clientContent": map[string]any{"turns": turns, "turnComplete": true},
	})
}

// write encodes and sends, holding traffic that precedes the handshake.
func (client *Client) write(ctx context.Context, message map[string]any) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode Gemini Live message: %w", err)
	}
	if _, isSetup := message["setup"]; !isSetup && !client.setupSent {
		if len(client.pending) >= pendingLimit {
			return errors.New("Gemini Live handshake did not complete before the audio buffer filled")
		}
		client.pending = append(client.pending, encoded)
		return nil
	}
	return client.writeRaw(ctx, encoded)
}

func (client *Client) writeRaw(ctx context.Context, encoded []byte) error {
	select {
	case <-client.closed:
		return errors.New("Gemini Live connection is closed")
	default:
	}
	if err := client.connection.Write(ctx, websocket.MessageText, encoded); err != nil {
		return fmt.Errorf("send to Gemini Live: %w", err)
	}
	return nil
}
