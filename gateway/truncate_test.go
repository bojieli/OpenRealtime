package gateway_test

import (
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
)

// The client is the only party that knows where playback actually stopped, and
// conversation.item.truncate is the only way it can say so. The utterance it
// names has usually just ended - the server finished sending while the
// listener was already talking over it - so a session that forgets an
// utterance the moment it ends turns that message into a confirmation nothing
// acted on, and goes on recording the whole answer as heard.
func TestATruncationThatArrivesJustAfterTheAnswerEndedStillReachesTheRuntime(t *testing.T) {
	server := startServer(t,
		fast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Forty dollars."}}),
		slow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "The balance is $40."}}),
		"what is my balance")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configurePCM16(nil)
	client.await("session.updated", 5*time.Second)
	client.speak()
	client.await("response.done", 20*time.Second)

	spoken := client.messages("response.output_item.done")
	if len(spoken) == 0 {
		t.Fatalf("no assistant item was completed: %s", client.seen())
	}
	item, _ := spoken[len(spoken)-1]["item"].(map[string]any)
	itemID, _ := item["id"].(string)
	if itemID == "" {
		t.Fatalf("completed assistant item has no identity: %+v", spoken[len(spoken)-1])
	}

	client.send(map[string]any{
		"type": "conversation.item.truncate", "event_id": "t1",
		"item_id": itemID, "content_index": 0, "audio_end_ms": 120,
	})
	truncated := client.await("conversation.item.truncated", 5*time.Second)
	if truncated["item_id"] != itemID {
		t.Fatalf("truncation confirmed a different item: %+v", truncated)
	}
	if truncated["audio_end_ms"] != float64(120) {
		t.Fatalf("truncation confirmed a different boundary: %+v", truncated)
	}
}

// Confirming a truncation that reached nothing is worse than refusing it: the
// client goes on believing the server knows the listener stopped it early.
func TestATruncationForAnItemThisSessionNeverPlayedIsRefused(t *testing.T) {
	server := startServer(t, fast(), slow(), "hello")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configurePCM16(nil)
	client.await("session.updated", 5*time.Second)

	client.send(map[string]any{
		"type": "conversation.item.truncate", "event_id": "t1",
		"item_id": "item_that_was_never_played", "content_index": 0, "audio_end_ms": 40,
	})
	failure := client.await("error", 5*time.Second)
	detail, _ := failure["error"].(map[string]any)
	message, _ := detail["message"].(string)
	if message == "" {
		t.Fatalf("refusal carried no reason: %+v", failure)
	}
	if _, confirmed := client.awaitOptional("conversation.item.truncated", 300*time.Millisecond); confirmed {
		t.Fatal("the session both refused the truncation and confirmed it")
	}
}
