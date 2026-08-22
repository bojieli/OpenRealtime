package geminilive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/bojieli/OpenRealtime/realtimeclient"
	"github.com/coder/websocket"
)

// serverMessage is the Live server union. It has no type field: which key is
// present is what distinguishes one message from another.
type serverMessage struct {
	SetupComplete *struct{} `json:"setupComplete,omitempty"`
	ServerContent *struct {
		ModelTurn *struct {
			Parts []struct {
				Text       string `json:"text"`
				InlineData *struct {
					MIMEType string `json:"mimeType"`
					Data     string `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"modelTurn"`
		InputTranscription  *struct{ Text string } `json:"inputTranscription"`
		OutputTranscription *struct{ Text string } `json:"outputTranscription"`
		TurnComplete        bool                   `json:"turnComplete"`
		GenerationComplete  bool                   `json:"generationComplete"`
		Interrupted         bool                   `json:"interrupted"`
	} `json:"serverContent,omitempty"`
	ToolCall *struct {
		FunctionCalls []struct {
			ID   string          `json:"id"`
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		} `json:"functionCalls"`
	} `json:"toolCall,omitempty"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error,omitempty"`
}

// read is the only goroutine touching the connection's reader.
func (client *Client) read(ctx context.Context) {
	defer close(client.events)
	defer close(client.done)
	for {
		kind, payload, err := client.connection.Read(ctx)
		if err != nil {
			if !isCleanClose(err) {
				client.fail(fmt.Errorf("read Gemini Live stream: %w", err))
			}
			return
		}
		if kind != websocket.MessageText && kind != websocket.MessageBinary {
			continue
		}
		var message serverMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			client.fail(fmt.Errorf("decode Gemini Live message: %w", err))
			return
		}
		if err := client.translate(ctx, message); err != nil {
			client.fail(err)
			return
		}
	}
}

// translate turns one Live message into the Realtime events the mirror reads.
func (client *Client) translate(ctx context.Context, message serverMessage) error {
	if message.Error != nil {
		return client.emit(ctx, "error", map[string]any{
			"type": "error",
			"error": map[string]any{
				"code": message.Error.Status, "message": message.Error.Message,
			},
		})
	}
	if message.ToolCall != nil {
		// The remote has no tool authority in this arrangement - the engine's
		// slow provider is the only thing that calls tools. Surfacing the
		// attempt is better than silence, because a session configured so the
		// remote thinks it can act is a misconfiguration worth seeing.
		var names []string
		for _, call := range message.ToolCall.FunctionCalls {
			names = append(names, call.Name)
		}
		return client.emit(ctx, "error", map[string]any{
			"type": "error",
			"error": map[string]any{
				"code": "remote_tool_call",
				"message": "the Live endpoint attempted to call " + strings.Join(names, ", ") +
					"; tool authority in this binding belongs to the background reasoner",
			},
		})
	}
	content := message.ServerContent
	if content == nil {
		return nil
	}

	if content.InputTranscription != nil && content.InputTranscription.Text != "" {
		client.transcriptMu.Lock()
		client.inputText.WriteString(content.InputTranscription.Text)
		client.transcriptMu.Unlock()
	}
	if content.OutputTranscription != nil && content.OutputTranscription.Text != "" {
		client.transcriptMu.Lock()
		client.outputText.WriteString(content.OutputTranscription.Text)
		client.transcriptMu.Unlock()
		if err := client.emit(ctx, "response.output_audio_transcript.delta", map[string]any{
			"type":  "response.output_audio_transcript.delta",
			"delta": content.OutputTranscription.Text,
		}); err != nil {
			return err
		}
	}
	if content.ModelTurn != nil {
		for _, part := range content.ModelTurn.Parts {
			if part.InlineData == nil || part.InlineData.Data == "" {
				continue
			}
			if !strings.HasPrefix(part.InlineData.MIMEType, "audio/") {
				continue
			}
			if err := client.emit(ctx, "response.output_audio.delta", map[string]any{
				"type": "response.output_audio.delta", "delta": part.InlineData.Data,
			}); err != nil {
				return err
			}
		}
	}

	// An interruption and a completed turn are different events, and
	// conflating them corrupts the trajectory.
	//
	// Interrupted means the model stopped speaking because the user carried
	// on. What it managed to say is real and the utterance is over, so both
	// are reported - but the user has not finished, so what has been heard so
	// far is not a completed transcript and must keep accumulating. Reporting
	// it here would commit a fragment of a sentence as the user's turn and
	// hand the reasoner half a question.
	if content.Interrupted {
		return client.endUtterance(ctx)
	}
	if content.TurnComplete {
		return client.completeTurn(ctx)
	}
	return nil
}

// endUtterance reports what the model said and closes the response, leaving
// what the user has said so far still accumulating.
func (client *Client) endUtterance(ctx context.Context) error {
	client.transcriptMu.Lock()
	said := strings.TrimSpace(client.outputText.String())
	client.outputText.Reset()
	client.transcriptMu.Unlock()
	if said != "" {
		if err := client.emit(ctx, "response.output_audio_transcript.done", map[string]any{
			"type": "response.output_audio_transcript.done", "transcript": said,
		}); err != nil {
			return err
		}
	}
	return client.emit(ctx, "response.done", map[string]any{"type": "response.done"})
}

// completeTurn reports the accumulated transcripts and closes the response.
//
// The Live API streams transcription and never says it has finished, so the
// end of the turn is the only completion signal there is. That is also the
// right moment: it is when the Realtime protocol reports the same thing.
func (client *Client) completeTurn(ctx context.Context) error {
	client.transcriptMu.Lock()
	heard := strings.TrimSpace(client.inputText.String())
	said := strings.TrimSpace(client.outputText.String())
	client.inputText.Reset()
	client.outputText.Reset()
	client.transcriptMu.Unlock()

	if heard != "" {
		if err := client.emit(ctx, "conversation.item.input_audio_transcription.completed",
			map[string]any{
				"type":       "conversation.item.input_audio_transcription.completed",
				"transcript": heard,
			}); err != nil {
			return err
		}
	}
	if said != "" {
		if err := client.emit(ctx, "response.output_audio_transcript.done", map[string]any{
			"type": "response.output_audio_transcript.done", "transcript": said,
		}); err != nil {
			return err
		}
	}
	return client.emit(ctx, "response.done", map[string]any{"type": "response.done"})
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
// Cancelling the session's context is how a session ends normally, and the
// read that was in flight fails with it. Reporting that as an upstream error
// puts a failure on every clean shutdown, which teaches an operator to ignore
// the one field that should mean something.
func isCleanClose(err error) bool {
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return true
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed)
}
