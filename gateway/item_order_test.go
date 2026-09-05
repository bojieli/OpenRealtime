package gateway_test

import (
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
)

// previous_item_id is how a client puts the conversation in order. The wire
// says it is null only for an item with no predecessor, so a server that sends
// null for all of them tells the client that every item is the first one -
// and an ordered view built from that cannot be repaired, because the events
// carry no other ordering.
func TestEveryConversationItemNamesTheOneItFollows(t *testing.T) {
	server := startServer(t,
		fast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Forty dollars."}},
			[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Since Tuesday."}}),
		slow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "The balance is $40."}},
			[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "It has been $40 since Tuesday."}}),
		"what is my balance")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configurePCM16(nil)
	client.await("session.updated", 5*time.Second)

	client.speak()
	client.await("response.done", 20*time.Second)

	// A second item, added by the client rather than spoken. It follows the
	// answer, and saying so is the whole point of the field.
	client.send(map[string]any{
		"type": "conversation.item.create", "event_id": "c1",
		"item": map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": "and last month?"}},
		},
	})
	client.await("conversation.item.created", 5*time.Second)

	type announced struct {
		id       string
		previous any
		kind     string
	}
	var chain []announced
	for _, message := range client.received {
		kind, _ := message["type"].(string)
		switch kind {
		case "conversation.item.created", "response.output_item.added":
		default:
			continue
		}
		item, _ := message["item"].(map[string]any)
		id, _ := item["id"].(string)
		if id == "" {
			t.Fatalf("%s announced an item with no identity: %v", kind, message)
		}
		entry := announced{id: id, kind: kind}
		if kind == "conversation.item.created" {
			entry.previous = message["previous_item_id"]
		} else {
			entry.previous = "-"
		}
		chain = append(chain, entry)
	}
	if len(chain) < 3 {
		t.Fatalf("announced %d items, want a question, an answer, and a second question: %+v", len(chain), chain)
	}
	if chain[len(chain)-1].previous == nil {
		t.Fatalf("the item added after a whole answered turn still claims no predecessor: %+v", chain)
	}

	var last string
	for index, entry := range chain {
		if entry.kind != "conversation.item.created" {
			last = entry.id
			continue
		}
		var want any
		if last != "" {
			want = last
		}
		if entry.previous != want {
			t.Fatalf("item %d (%s) follows %v, want %v; the chain so far is %+v",
				index, entry.id, entry.previous, want, chain[:index+1])
		}
		last = entry.id
	}
	if last == "" {
		t.Fatal("no item was announced")
	}

}

// A commit announces where the item will go before the item exists. It has to
// name the same predecessor the creation does, or a client that inserts on the
// commit and a client that inserts on the creation build different
// conversations from the same session.
func TestACommitAndTheCreationThatFollowsItAgreeOnThePredecessor(t *testing.T) {
	server := startServer(t,
		fast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Forty dollars."}}),
		slow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "The balance is $40."}}),
		"what is my balance")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configureManualTurns()
	client.await("session.updated", 5*time.Second)

	client.send(map[string]any{
		"type": "conversation.item.create", "event_id": "c0",
		"item": map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": "hello"}},
		},
	})
	first := client.await("conversation.item.created", 5*time.Second)
	firstItem, _ := first["item"].(map[string]any)
	firstID, _ := firstItem["id"].(string)
	if first["previous_item_id"] != nil {
		t.Fatalf("the first item of a session follows %v", first["previous_item_id"])
	}

	client.speak()
	client.send(map[string]any{"type": "input_audio_buffer.commit", "event_id": "c1"})
	committed := client.await("input_audio_buffer.committed", 5*time.Second)
	if committed["previous_item_id"] != firstID {
		t.Fatalf("the committed turn follows %v, want the item before it, %q",
			committed["previous_item_id"], firstID)
	}
	created := client.await("conversation.item.created", 5*time.Second)
	if created["previous_item_id"] != committed["previous_item_id"] {
		t.Fatalf("committed after %v but created after %v",
			committed["previous_item_id"], created["previous_item_id"])
	}
}
