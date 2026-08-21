package simulation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/realtimeclient"
)

// party is one agent: a session, what it is, and the audio waiting for its ear.
type party struct {
	role   Role
	client *realtimeclient.Client
	record *record
	peer   *party

	mu   sync.Mutex
	ear  []int16 // audio the peer has produced and this side has not yet heard
	done bool
}

// connect opens one session and configures it.
func connect(ctx context.Context, config Config, role Role, record *record) (*party, error) {
	client, err := realtimeclient.Dial(ctx, realtimeclient.Config{
		URL: config.Endpoint, Token: config.Token, Model: config.Model,
	})
	if err != nil {
		return nil, err
	}
	side := &party{role: role, client: client, record: record}

	session := map[string]any{
		"type":         "realtime",
		"instructions": role.Instruction,
		"audio": map[string]any{
			"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": sampleRate}},
			"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": sampleRate}},
		},
	}
	if len(role.Tools) > 0 {
		tools := make([]map[string]any, 0, len(role.Tools))
		for _, tool := range role.Tools {
			tools = append(tools, map[string]any{
				"type": "function", "name": tool.Name,
				"description": tool.Description, "parameters": json.RawMessage(tool.Parameters),
			})
		}
		session["tools"] = tools
	}
	if err := side.send(ctx, map[string]any{"type": "session.update", "session": session}); err != nil {
		_ = client.Close()
		return nil, err
	}
	return side, nil
}

func (side *party) send(ctx context.Context, value any) error {
	return side.client.Send(ctx, value)
}

// defaultCue is what an opener is told when a scenario does not say.
const defaultCue = "The call has connected and the other person is listening. Speak first."

// open starts the conversation on this side.
//
// A bare response.create is not enough. The conversation is empty at that
// point, so the model is asked to continue nothing and has nothing to say -
// which looks exactly like a broken link.
//
// The cue goes in as a system message rather than a user one, and the
// difference is not cosmetic. A user message is something the other party
// said, so a model given "say your order has not arrived" as a user turn
// answers it - and a customer who was told to complain instead offers to look
// the complaint up, having quietly become the support agent. A system message
// carries observer authority: direction, not a request. It is delivered only
// here, so it never reaches the other side and never becomes a turn.
func (side *party) open(ctx context.Context, cue string) error {
	if strings.TrimSpace(cue) == "" {
		cue = defaultCue
	}
	if err := side.send(ctx, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": "system",
			"content": []map[string]any{{"type": "input_text", "text": cue}},
		},
	}); err != nil {
		return err
	}
	return side.send(ctx, map[string]any{"type": "response.create"})
}

func (side *party) close() {
	side.mu.Lock()
	side.done = true
	side.mu.Unlock()
	_ = side.client.Close()
}

// hear queues audio for this side to listen to.
//
// The queue exists because synthesis is faster than speech. A model produces a
// ten second answer in two, and forwarding it as it arrives would deliver a
// ten second utterance in two seconds - which the recogniser would hear as
// gabble and the endpoint detector would place in the wrong second.
func (side *party) hear(samples []int16) {
	if len(samples) == 0 {
		return
	}
	side.mu.Lock()
	defer side.mu.Unlock()
	if side.done {
		return
	}
	side.ear = append(side.ear, samples...)
}

// take removes one frame from the ear, reporting whether it held real audio.
func (side *party) take() ([]int16, bool) {
	side.mu.Lock()
	defer side.mu.Unlock()
	if len(side.ear) == 0 {
		return nil, false
	}
	take := min(frameSamples, len(side.ear))
	frame := make([]int16, frameSamples)
	copy(frame, side.ear[:take])
	side.ear = side.ear[take:]
	return frame, true
}

// interrupt stops what this side is still waiting to hear.
//
// When a speaker is cut off, the audio already queued at the listener's ear is
// speech that was never uttered. Delivering it anyway would let the listener
// hear the end of a sentence the speaker abandoned, which is the one thing a
// barge-in must not produce.
func (side *party) interrupt() {
	side.mu.Lock()
	defer side.mu.Unlock()
	side.ear = nil
}

// read pumps one side's server events.
func (side *party) read(ctx context.Context, config Config) {
	for event := range side.client.Events() {
		switch event.Type {
		case "response.output_audio.delta":
			var decoded struct {
				Delta string `json:"delta"`
			}
			_ = event.Decode(&decoded)
			// This side's mouth is the other side's ear.
			side.peer.hear(decodeSamples(decoded.Delta))

		case "response.output_audio_transcript.done":
			var decoded struct {
				Transcript string `json:"transcript"`
			}
			_ = event.Decode(&decoded)
			side.record.spokenBy(side.role.Name, decoded.Transcript)

		case "conversation.item.input_audio_transcription.completed":
			// What this side heard is the peer's turn, and it only exists
			// because the audio survived synthesis, the link, and recognition.
			var decoded struct {
				Transcript string `json:"transcript"`
			}
			_ = event.Decode(&decoded)
			side.record.heard(side.peer.role.Name, decoded.Transcript, false)

		case "response.function_call_arguments.done":
			var decoded struct {
				CallID string `json:"call_id"`
				Name   string `json:"name"`
				// Arguments arrive as a JSON string, not an object.
				Arguments string `json:"arguments"`
			}
			_ = event.Decode(&decoded)
			arguments := json.RawMessage(decoded.Arguments)
			if !json.Valid(arguments) {
				arguments = json.RawMessage(`{}`)
			}
			side.record.tool(side.role.Name, decoded.Name, arguments)
			side.answer(ctx, decoded.CallID, decoded.Name, arguments)

		case "error":
			var decoded struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = event.Decode(&decoded)
			side.record.failed(side.role.Name + ": " + decoded.Error.Message)
		}
	}
	if err := side.client.Err(); err != nil && !errors.Is(err, context.Canceled) {
		side.record.failed(side.role.Name + ": " + err.Error())
	}
}

// answer returns a tool result and lets the turn continue.
//
// A call left unanswered stalls the side that made it, and a stalled side in a
// two-party conversation stalls both. Every call is answered, including one
// the scenario did not expect.
func (side *party) answer(ctx context.Context, callID, name string, arguments json.RawMessage) {
	output := json.RawMessage(`{"error":"this side declares no tools"}`)
	if side.role.Tool != nil {
		result, err := side.role.Tool(name, arguments)
		switch {
		case err != nil:
			encoded, _ := json.Marshal(map[string]string{"error": err.Error()})
			output = encoded
		case len(result) > 0:
			output = result
		}
	}
	encoded, err := json.Marshal(string(output))
	if err != nil {
		return
	}
	if err := side.send(ctx, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "function_call_output", "call_id": callID, "output": json.RawMessage(encoded),
		},
	}); err != nil {
		return
	}
	_ = side.send(ctx, map[string]any{"type": "response.create"})
}

// link is the wire between the two sides.
//
// It ticks every 20 ms and always sends something in both directions. That is
// the whole trick: a real microphone does not stop producing frames when
// nobody is talking, so an endpoint detector on the far side only works if
// silence arrives as reliably as speech does.
type link struct {
	left, right *party
	record      *record
	stopOnce    sync.Once
	stopped     chan struct{}
}

func newLink(left, right *party, record *record) *link {
	return &link{left: left, right: right, record: record, stopped: make(chan struct{})}
}

func (link *link) stop() { link.stopOnce.Do(func() { close(link.stopped) }) }

func (link *link) run(ctx context.Context) {
	ticker := time.NewTicker(framePeriod)
	defer ticker.Stop()
	silence := make([]int16, frameSamples)
	for {
		select {
		case <-ctx.Done():
			return
		case <-link.stopped:
			return
		case <-ticker.C:
			leftFrame, leftSpeech := link.left.take()
			rightFrame, rightSpeech := link.right.take()
			overlapping := leftSpeech && rightSpeech
			if leftSpeech {
				// The left party is hearing the right party's voice.
				link.record.carried(link.right.role.Name, overlapping)
			} else {
				leftFrame = silence
			}
			if rightSpeech {
				link.record.carried(link.left.role.Name, overlapping)
			} else {
				rightFrame = silence
			}
			link.deliver(ctx, link.left, leftFrame)
			link.deliver(ctx, link.right, rightFrame)
		}
	}
}

func (link *link) deliver(ctx context.Context, side *party, frame []int16) {
	_ = side.send(ctx, map[string]any{
		"type": "input_audio_buffer.append", "audio": encodeFrame(frame),
	})
}

// contains is a case-insensitive substring test, which is what almost every
// content check in a scenario needs.
func contains(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}
