package gateway_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// A response is a turn, and a client that has been told the response is done
// stops reading it. An agent that speaks and then calls a tool must therefore
// produce one response with two output items - not a spoken response the
// client sees finish, followed by calls it has already stopped waiting for.
func TestOneTurnIsOneResponseCarryingEveryOutputItem(t *testing.T) {
	server := startServer(t,
		fast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Checking."}}),
		slow([]continuation.Event{{
			Kind: continuation.EventToolCall,
			ToolCall: &trajectory.ToolCall{
				CallID: "call_1", Name: "get_balance", Arguments: json.RawMessage(`{"account":"A1"}`),
			},
		}}),
		"what is my balance")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.send(map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime",
			"tools": []map[string]any{{
				"type": "function", "name": "get_balance", "description": "read a balance",
				"parameters": map[string]any{"type": "object"},
			}},
			"audio": map[string]any{
				"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
				"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
			},
		},
	})
	client.await("session.updated", 5*time.Second)
	client.speak()

	created := client.await("response.created", 10*time.Second)
	response, _ := created["response"].(map[string]any)
	responseID, _ := response["id"].(string)

	call := client.await("response.function_call_arguments.done", 10*time.Second)
	if got, _ := call["response_id"].(string); got != responseID {
		t.Fatalf("the call belongs to the turn that produced it: %q vs %q", got, responseID)
	}
	if call["name"] != "get_balance" {
		t.Fatalf("unexpected call %v", call["name"])
	}
	// Arguments arrive as a JSON-encoded string, which is what the protocol
	// says and what every client has to unwrap.
	arguments, isString := call["arguments"].(string)
	if !isString || !strings.Contains(arguments, "A1") {
		t.Fatalf("arguments must be a JSON string, got %#v", call["arguments"])
	}

	done := client.await("response.done", 10*time.Second)
	finished, _ := done["response"].(map[string]any)
	if got, _ := finished["id"].(string); got != responseID {
		t.Fatalf("the turn ends once: %q vs %q", got, responseID)
	}
	output, _ := finished["output"].([]any)
	if len(output) < 2 {
		t.Fatalf("the response must list every item it produced, got %d", len(output))
	}
	kinds := make([]string, 0, len(output))
	indices := map[string]bool{}
	for _, entry := range output {
		item, _ := entry.(map[string]any)
		kind, _ := item["type"].(string)
		kinds = append(kinds, kind)
		indices[kind] = true
	}
	if !indices["message"] || !indices["function_call"] {
		t.Fatalf("expected a spoken item and a call, got %v", kinds)
	}

	// Exactly one response was created and exactly one was finished.
	if got := strings.Count(client.seen(), "response.created"); got != 1 {
		t.Fatalf("one turn is one response, got %d created", got)
	}
	if got := strings.Count(client.seen(), "response.done"); got != 1 {
		t.Fatalf("one turn ends once, got %d done", got)
	}
}

// Output items are indexed within the response, and two of them must not
// claim the same index.
func TestOutputItemsAreIndexedWithinTheirResponse(t *testing.T) {
	server := startServer(t,
		fast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Checking."}}),
		slow([]continuation.Event{
			{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
				CallID: "call_1", Name: "get_balance", Arguments: json.RawMessage(`{"account":"A1"}`),
			}},
			{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
				CallID: "call_2", Name: "get_balance", Arguments: json.RawMessage(`{"account":"A2"}`),
			}},
		}),
		"check both accounts")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.send(map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime",
			"tools": []map[string]any{{
				"type": "function", "name": "get_balance", "description": "read a balance",
				"parameters": map[string]any{"type": "object"},
			}},
			"audio": map[string]any{
				"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
				"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
			},
		},
	})
	client.await("session.updated", 5*time.Second)
	client.speak()
	client.await("response.created", 10*time.Second)
	client.await("response.done", 15*time.Second)

	seen := map[float64]bool{}
	for _, message := range client.messages("response.output_item.done") {
		index, _ := message["output_index"].(float64)
		if seen[index] {
			t.Fatalf("two output items claimed index %v", index)
		}
		seen[index] = true
	}
	if len(seen) < 3 {
		t.Fatalf("expected a spoken item and two calls, got %d items", len(seen))
	}
}
