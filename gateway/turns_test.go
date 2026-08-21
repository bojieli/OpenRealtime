package gateway_test

import (
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
)

// A client that switches server VAD off has taken the floor. Two things have
// to follow or it gets a session that half-listens to it: silence stops ending
// turns, and the server stops creating responses of its own.
func TestAClientThatTakesTheFloorDeclaresItsOwnTurns(t *testing.T) {
	server := startServer(t,
		fast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Forty dollars."}}),
		slow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "The balance is $40."}}),
		"what is my balance")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configureManualTurns()
	updated := client.await("session.updated", 5*time.Second)
	session, _ := updated["session"].(map[string]any)
	audio, _ := session["audio"].(map[string]any)
	input, _ := audio["input"].(map[string]any)
	if detection, present := input["turn_detection"]; !present || detection != nil {
		t.Fatalf("a session whose client owns turns reports null turn detection, got %v", detection)
	}

	client.speak()
	// Silence past the endpoint threshold must not have ended anything.
	time.Sleep(200 * time.Millisecond)
	if seen := client.seen(); contains(seen, "input_audio_buffer.speech_stopped") {
		t.Fatalf("silence ended a turn the client had not declared over: %s", seen)
	}

	client.send(map[string]any{"type": "input_audio_buffer.commit", "event_id": "c1"})
	committed := client.await("input_audio_buffer.committed", 5*time.Second)
	if committed["item_id"] == "" {
		t.Fatal("a commit must name the item it committed")
	}
	client.await("conversation.item.input_audio_transcription.completed", 5*time.Second)

	// The turn is committed and nothing has answered it, because nothing was
	// asked to.
	time.Sleep(200 * time.Millisecond)
	if seen := client.seen(); contains(seen, "response.created") {
		t.Fatalf("a response was created without being requested: %s", seen)
	}

	client.send(map[string]any{"type": "response.create", "event_id": "r1"})
	client.await("response.created", 10*time.Second)
	client.await("response.done", 10*time.Second)
}

// Committing nothing is not declaring a turn.
func TestCommittingAnEmptyBufferIsRefused(t *testing.T) {
	server := startServer(t, fast(), slow(), "hello")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configureManualTurns()
	client.await("session.updated", 5*time.Second)
	client.send(map[string]any{"type": "input_audio_buffer.commit", "event_id": "c1"})
	failure := client.await("error", 5*time.Second)
	detail, _ := failure["error"].(map[string]any)
	if message, _ := detail["message"].(string); !contains(message, "empty") {
		t.Fatalf("unexpected refusal %q", message)
	}
}

// And a session that never gave up server VAD has nothing to commit: the
// buffer commits at the endpoint, and an explicit commit would have nothing
// to do.
func TestCommittingUnderServerVADIsRefusedWithTheReason(t *testing.T) {
	server := startServer(t, fast(), slow(), "hello")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.send(map[string]any{"type": "input_audio_buffer.commit", "event_id": "c1"})
	failure := client.await("error", 5*time.Second)
	detail, _ := failure["error"].(map[string]any)
	if message, _ := detail["message"].(string); !contains(message, "voice-activity detection") {
		t.Fatalf("unexpected refusal %q", message)
	}
}
