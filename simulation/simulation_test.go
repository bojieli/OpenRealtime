package simulation_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/simulation"
	"github.com/coder/websocket"
)

// endpoint is a Realtime server just real enough to hold a conversation.
//
// It speaks the wire, not the models: each session answers with a scripted
// line, delivered as audio, once it has heard enough inbound audio to count as
// a turn. That is enough to test the link, the pacing, the turn record, and
// the checks without a GPU anywhere near the test.
type endpoint struct {
	server *httptest.Server

	mu       sync.Mutex
	sessions int
	// lines are what successive sessions say, in connection order.
	lines [][]string
	// toolCall, when set, is emitted by the second session on its first turn.
	toolCall *toolCall
}

type toolCall struct {
	name      string
	arguments string
}

func newEndpoint(t *testing.T, lines [][]string) *endpoint {
	t.Helper()
	end := &endpoint{lines: lines}
	end.server = httptest.NewServer(http.HandlerFunc(end.serve))
	t.Cleanup(end.server.Close)
	return end
}

func (end *endpoint) url() string {
	return "ws" + strings.TrimPrefix(end.server.URL, "http") + "/v1/realtime"
}

func (end *endpoint) serve(writer http.ResponseWriter, request *http.Request) {
	connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer connection.CloseNow()
	ctx := request.Context()

	end.mu.Lock()
	index := end.sessions
	end.sessions++
	var lines []string
	if index < len(end.lines) {
		lines = end.lines[index]
	}
	emitTool := end.toolCall != nil && index == 1
	call := end.toolCall
	end.mu.Unlock()

	write := func(value map[string]any) error {
		encoded, _ := json.Marshal(value)
		return connection.Write(ctx, websocket.MessageText, encoded)
	}
	_ = write(map[string]any{"type": "session.created", "event_id": "e0",
		"session": map[string]any{"id": "sess_1", "type": "realtime"}})

	// A turn is produced for every response.create and for every second of
	// inbound audio that follows one, which is enough to make the two sides
	// take turns without modelling endpointing.
	spoken, inboundFrames := 0, 0
	speak := func() {
		if spoken >= len(lines) {
			return
		}
		line := lines[spoken]
		spoken++
		if emitTool && spoken == 1 {
			_ = write(map[string]any{
				"type": "response.function_call_arguments.done", "event_id": "t1",
				"call_id": "call_1", "name": call.name, "arguments": call.arguments,
			})
		}
		// Half a second of non-silent audio stands in for the utterance.
		payload := make([]byte, 24000)
		for index := range payload {
			payload[index] = byte(index%7) + 1
		}
		_ = write(map[string]any{
			"type": "response.output_audio.delta", "event_id": "a1",
			"delta": base64.StdEncoding.EncodeToString(payload),
		})
		_ = write(map[string]any{
			"type": "response.output_audio_transcript.done", "event_id": "d1", "transcript": line,
		})
		_ = write(map[string]any{"type": "response.done", "event_id": "r1",
			"response": map[string]any{"id": "resp_1"}})
	}

	for {
		_, raw, err := connection.Read(ctx)
		if err != nil {
			return
		}
		var decoded struct {
			Type  string `json:"type"`
			Audio string `json:"audio"`
		}
		if json.Unmarshal(raw, &decoded) != nil {
			continue
		}
		switch decoded.Type {
		case "response.create":
			speak()
		case "input_audio_buffer.append":
			payload, _ := base64.StdEncoding.DecodeString(decoded.Audio)
			silent := true
			for _, value := range payload {
				if value != 0 {
					silent = false
					break
				}
			}
			if silent {
				continue
			}
			inboundFrames++
			// Every 25 frames of real audio - half a second - is one heard
			// turn, answered in kind.
			if inboundFrames%25 == 0 {
				_ = write(map[string]any{
					"type":       "conversation.item.input_audio_transcription.completed",
					"event_id":   "h1",
					"item_id":    "item_1",
					"transcript": "heard something",
				})
				speak()
			}
		}
	}
}

func TestTwoAgentsTakeTurnsOverTheAudioLink(t *testing.T) {
	end := newEndpoint(t, [][]string{
		{"hello from the left", "left again"},
		{"hello from the right", "right again"},
	})

	scenario := simulation.Scenario{
		Name:     "test",
		Question: "does the link carry turns in both directions?",
		Left:     simulation.Role{Name: "left", Instruction: "be left", Opens: true},
		Right:    simulation.Role{Name: "right", Instruction: "be right"},
		MaxTurns: 4,
		Budget:   20 * time.Second,
		Settle:   2 * time.Second,
		Checks:   []simulation.Check{},
	}
	conversation, err := simulation.Run(context.Background(),
		simulation.Config{Endpoint: end.url()}, scenario)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(conversation.Turns) < 2 {
		t.Fatalf("expected turns in both directions, got %d: %+v",
			len(conversation.Turns), conversation.Turns)
	}
	speakers := map[string]int{}
	for _, turn := range conversation.Turns {
		speakers[turn.Speaker]++
	}
	if speakers["left"] == 0 || speakers["right"] == 0 {
		t.Fatalf("both sides must be heard, got %v", speakers)
	}
	// Speech is only counted when the link actually carried it, so a non-zero
	// figure is proof the pacing ran rather than that a delta arrived.
	if conversation.SpeechMS["left"] == 0 || conversation.SpeechMS["right"] == 0 {
		t.Fatalf("both sides must have been carried, got %v", conversation.SpeechMS)
	}
}

// A run where one side never says anything is a monologue, and the checks have
// to say so rather than reporting a short but successful conversation.
func TestAMonologueFailsRatherThanPassingQuietly(t *testing.T) {
	end := newEndpoint(t, [][]string{{"only the left ever speaks"}, nil})

	scenario := simulation.Scenario{
		Name:     "monologue",
		Left:     simulation.Role{Name: "left", Instruction: "be left", Opens: true},
		Right:    simulation.Role{Name: "right", Instruction: "be right"},
		MaxTurns: 4,
		Budget:   12 * time.Second,
		Settle:   2 * time.Second,
	}
	scenario.Checks = simulation.Interview().Checks[:1] // "both sides spoke"

	conversation, err := simulation.Run(context.Background(),
		simulation.Config{Endpoint: end.url()}, scenario)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if conversation.Passed() {
		t.Fatal("a conversation where one side never spoke must not pass")
	}
	if conversation.EndedBy != "budget" && conversation.EndedBy != "turns" {
		t.Fatalf("a conversation that never settled must not report settling, got %q",
			conversation.EndedBy)
	}
}

// A tool call is answered even when the scenario did not expect one. An
// unanswered call stalls the side that made it, and in a two-party
// conversation that stalls both.
func TestToolCallsAreAnsweredAndRecorded(t *testing.T) {
	end := newEndpoint(t, [][]string{{"my order is R7742"}, {"it is delayed until Thursday"}})
	end.toolCall = &toolCall{name: "lookup_order", arguments: `{"order_id":"R7742"}`}

	called := make(chan json.RawMessage, 1)
	scenario := simulation.Scenario{
		Name: "tools",
		Left: simulation.Role{Name: "customer", Instruction: "be a customer", Opens: true},
		Right: simulation.Role{Name: "agent", Instruction: "be an agent",
			Tools: simulation.SupportCall().Right.Tools,
			Tool: func(_ string, arguments json.RawMessage) (json.RawMessage, error) {
				select {
				case called <- arguments:
				default:
				}
				return json.RawMessage(`{"status":"delayed"}`), nil
			}},
		MaxTurns: 4,
		Budget:   20 * time.Second,
		Settle:   2 * time.Second,
	}
	conversation, err := simulation.Run(context.Background(),
		simulation.Config{Endpoint: end.url()}, scenario)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	select {
	case arguments := <-called:
		if !strings.Contains(string(arguments), "R7742") {
			t.Fatalf("the tool must receive its arguments, got %s", arguments)
		}
	default:
		t.Fatal("the tool was never called")
	}
	if len(conversation.ToolCalls) == 0 {
		t.Fatal("the call must be recorded for the checks to see it")
	}
}

// Exactly one side opens. Two openers talk over each other from the first
// frame and neither ever hears a complete turn; no opener is silence.
func TestExactlyOneSideMustOpen(t *testing.T) {
	end := newEndpoint(t, nil)
	for _, testCase := range []struct{ left, right bool }{{true, true}, {false, false}} {
		scenario := simulation.Scenario{
			Name:  "openers",
			Left:  simulation.Role{Name: "left", Opens: testCase.left},
			Right: simulation.Role{Name: "right", Opens: testCase.right},
		}
		if _, err := simulation.Run(context.Background(),
			simulation.Config{Endpoint: end.url()}, scenario); err == nil {
			t.Fatalf("opens=%v/%v must be refused", testCase.left, testCase.right)
		}
	}
}

// A role that declares tools and cannot answer a call would hang its own turn
// and time the scenario out, which reads as the system failing.
func TestARoleWithToolsMustBeAbleToAnswer(t *testing.T) {
	end := newEndpoint(t, nil)
	scenario := simulation.Scenario{
		Name:  "unanswerable",
		Left:  simulation.Role{Name: "left", Opens: true, Tools: simulation.SupportCall().Right.Tools},
		Right: simulation.Role{Name: "right"},
	}
	_, err := simulation.Run(context.Background(), simulation.Config{Endpoint: end.url()}, scenario)
	if err == nil || !strings.Contains(err.Error(), "hangs the turn") {
		t.Fatalf("expected a refusal explaining the consequence, got %v", err)
	}
}

// The needle has to be hard to find: near neither end of the document, and
// present exactly once.
func TestTheLargeContextBuriesItsNeedle(t *testing.T) {
	document := simulation.LargeContext("eleven milliseconds")
	if count := strings.Count(document, "eleven milliseconds"); count != 1 {
		t.Fatalf("the needle must appear exactly once, found %d", count)
	}
	if len(document) < 20000 {
		t.Fatalf("the document must be large enough that the needle is not on the first page, got %d bytes",
			len(document))
	}
	position := float64(strings.Index(document, "eleven milliseconds")) / float64(len(document))
	if position < 0.25 || position > 0.75 {
		t.Fatalf("the needle must sit away from both ends, found at %.0f%%", position*100)
	}
}
