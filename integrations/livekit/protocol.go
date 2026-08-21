package livekit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
)

// event is one decoded server event with its raw bytes retained, so a caller
// can forward it unchanged rather than re-encoding a model of it.
type event struct {
	Type string
	Raw  []byte
}

// client is a minimal Realtime protocol client.
//
// This module deliberately does not import the server's own client package:
// an integration that can ship separately should not depend on the server's
// internals, and the protocol is small enough that speaking it directly is
// less coupling than sharing code would be.
type client struct {
	connection *websocket.Conn
	events     chan event
	closed     atomic.Bool
	writeMu    sync.Mutex
}

func dial(ctx context.Context, endpoint, token, model string) (*client, error) {
	if strings.TrimSpace(endpoint) == "" {
		return nil, errors.New("a protocol endpoint is required")
	}
	url := endpoint
	if strings.TrimSpace(model) != "" && !strings.Contains(url, "model=") {
		separator := "?"
		if strings.Contains(url, "?") {
			separator = "&"
		}
		url += separator + "model=" + model
	}
	header := http.Header{}
	if strings.TrimSpace(token) != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	connection, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", endpoint, err)
	}
	connection.SetReadLimit(8 << 20)
	result := &client{connection: connection, events: make(chan event, 256)}
	go result.read(ctx)
	return result, nil
}

func (client *client) Events() <-chan event { return client.events }

func (client *client) Send(ctx context.Context, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	if client.closed.Load() {
		return errors.New("protocol client is closed")
	}
	return client.connection.Write(ctx, websocket.MessageText, encoded)
}

func (client *client) Close() error {
	if client.closed.Swap(true) {
		return nil
	}
	return client.connection.Close(websocket.StatusNormalClosure, "agent closed")
}

func (client *client) read(ctx context.Context) {
	defer close(client.events)
	for {
		messageType, input, err := client.connection.Read(ctx)
		if err != nil {
			return
		}
		if messageType != websocket.MessageText {
			continue
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(input, &envelope) != nil {
			continue
		}
		select {
		case client.events <- event{Type: envelope.Type, Raw: append([]byte(nil), input...)}:
		case <-ctx.Done():
			return
		}
	}
}

// StripAudioFormat removes an audio format declaration from a session update.
//
// It is exported so the behaviour can be tested directly: an adapter that
// terminates media owns the protocol connection's audio format, and a
// participant's opinion about it must not reach the endpoint. Everything else
// in the same event survives untouched.
func StripAudioFormat(raw []byte) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, errors.New("event is not a JSON object")
	}
	var eventType string
	if err := json.Unmarshal(envelope["type"], &eventType); err != nil {
		return nil, errors.New("event has no type")
	}
	if eventType != "session.update" {
		return raw, nil
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(envelope["session"], &body); err != nil {
		return raw, nil
	}
	audio, present := body["audio"]
	if !present {
		return raw, nil
	}
	var audioBody map[string]json.RawMessage
	if err := json.Unmarshal(audio, &audioBody); err != nil {
		return raw, nil
	}
	for _, direction := range []string{"input", "output"} {
		section, exists := audioBody[direction]
		if !exists {
			continue
		}
		var sectionBody map[string]json.RawMessage
		if err := json.Unmarshal(section, &sectionBody); err != nil {
			continue
		}
		delete(sectionBody, "format")
		encoded, err := json.Marshal(sectionBody)
		if err != nil {
			continue
		}
		audioBody[direction] = encoded
	}
	encodedAudio, err := json.Marshal(audioBody)
	if err != nil {
		return raw, nil
	}
	body["audio"] = encodedAudio
	encodedBody, err := json.Marshal(body)
	if err != nil {
		return raw, nil
	}
	envelope["session"] = encodedBody
	return json.Marshal(envelope)
}

// LinearToMuLaw encodes one PCM16 sample as G.711 mu-law.
//
// It is duplicated rather than imported so this module stays independently
// shippable. Twenty lines of a frozen standard is a cheaper dependency than a
// module boundary that has to be kept in step.
func LinearToMuLaw(raw int) byte {
	sample := int(int16(raw))
	sign := byte(0)
	if sample < 0 {
		sign = 0x80
		sample = -sample
		if sample > 32767 {
			sample = 32767
		}
	}
	if sample > 32635 {
		sample = 32635
	}
	sample += 0x84
	exponent := byte(7)
	for mask := 0x4000; exponent > 0 && sample&mask == 0; mask >>= 1 {
		exponent--
	}
	mantissa := byte(sample >> (exponent + 3) & 0x0f)
	return ^(sign | exponent<<4 | mantissa)
}
