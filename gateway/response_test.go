package gateway_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// A turn spans as many responses as the agent did things, and every output
// item belongs to the response that produced it.
//
// The voice answers in one response and hands the work on; the reasoner's call
// arrives in another. A client is not harmed by the split: audio reaches it on
// the audio channel rather than inside a response envelope, and a client
// executing a tool reads function_call items as they arrive. What it must be
// able to rely on is that each item names the response it came from, and that
// each response ends once.
func TestEveryOutputItemNamesTheResponseThatProducedIt(t *testing.T) {
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

	call := client.await("response.function_call_arguments.done", 10*time.Second)
	callResponse, _ := call["response_id"].(string)
	if callResponse == "" {
		t.Fatal("a call must name the response that produced it")
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

	// The response carrying the call lists it, and lists it once.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("the response carrying the call never finished: %s", client.seen())
		}
		done := client.await("response.done", 10*time.Second)
		finished, _ := done["response"].(map[string]any)
		id, _ := finished["id"].(string)
		if id != callResponse {
			continue
		}
		output, _ := finished["output"].([]any)
		calls := 0
		for _, entry := range output {
			item, _ := entry.(map[string]any)
			if kind, _ := item["type"].(string); kind == "function_call" {
				calls++
			}
		}
		if calls != 1 {
			t.Fatalf("the response that produced the call must list it exactly once, got %d", calls)
		}
		break
	}

	// Every response that opened also closed. A client waiting on one that
	// never ends is the failure this guards.
	if created, ended := strings.Count(client.seen(), "response.created"),
		strings.Count(client.seen(), "response.done"); created != ended {
		t.Fatalf("%d responses opened and %d closed: %s", created, ended, client.seen())
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
