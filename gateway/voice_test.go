package gateway_test

import (
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
)

// The voice a session reports is the voice it will use.
//
// A protocol default here is a client told what it is hearing and told wrong:
// a cascade deployment synthesises with whatever its speech provider was built
// with, which is not a name from a hosted catalogue. And because a speech plan
// carries text and nothing else, that voice is fixed when the provider is
// created - so a session cannot choose one, and being told it did is the
// failure this guards.
func TestTheReportedVoiceIsTheVoiceInForce(t *testing.T) {
	server := startServer(t, fast([]continuation.Event{}), slow([]continuation.Event{}), "hello")
	client := dial(t, server)
	created := client.await("session.created", 5*time.Second)
	if got := voiceOf(created); got != "test-voice" {
		t.Fatalf("the session must report the voice its synthesiser was built with, got %q", got)
	}

	client.send(map[string]any{"type": "session.update", "session": map[string]any{
		"type": "realtime",
		"audio": map[string]any{
			"input": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
			"output": map[string]any{
				"format": map[string]any{"type": "audio/pcm", "rate": 24000},
				"voice":  "a-voice-that-does-not-exist",
			},
		},
	}})
	updated := client.await("session.updated", 5*time.Second)
	if got := voiceOf(updated); got != "test-voice" {
		t.Fatalf("a voice this binding cannot use must not be echoed back as fact, got %q", got)
	}

	event, ok := client.awaitOptional("error", 5*time.Second)
	if !ok {
		t.Fatal("a voice that was not applied must be refused by name, not dropped in silence")
	}
	failure, _ := event["error"].(map[string]any)
	if param, _ := failure["param"].(string); param != "session.audio.output.voice" {
		t.Fatalf("the refusal must name the field, got %v", failure)
	}
	if message, _ := failure["message"].(string); !strings.Contains(message, "test-voice") {
		t.Fatalf("the refusal must say what is in force instead, got %q", message)
	}
}

func TestRepeatingTheFixedVoiceIsAnIdempotentUpdate(t *testing.T) {
	server := startServer(t, fast([]continuation.Event{}), slow([]continuation.Event{}), "hello")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)

	client.send(map[string]any{"type": "session.update", "session": map[string]any{
		"type": "realtime",
		"audio": map[string]any{
			"input": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
			"output": map[string]any{
				"format": map[string]any{"type": "audio/pcm", "rate": 24000},
				"voice":  "test-voice",
			},
		},
	}})
	updated := client.await("session.updated", 5*time.Second)
	if got := voiceOf(updated); got != "test-voice" {
		t.Fatalf("the idempotent update changed the fixed voice to %q", got)
	}
	if event, ok := client.awaitOptional("error", 250*time.Millisecond); ok {
		t.Fatalf("asking for the voice already in force must not be refused: %v", event)
	}
}

// A session that says nothing about the limit is told what it is, because a
// turn cut short reports max_output_tokens as the reason it stopped - and a
// client told there was no maximum cannot make sense of that.
func TestTheSessionStatesTheOutputLimitItWillEnforce(t *testing.T) {
	server := startServer(t, fast([]continuation.Event{}), slow([]continuation.Event{}), "hello")
	client := dial(t, server)
	created := client.await("session.created", 5*time.Second)
	session, _ := created["session"].(map[string]any)
	limit, isNumber := session["max_output_tokens"].(float64)
	if !isNumber {
		t.Fatalf("a binding that enforces a limit must state it, got %#v", session["max_output_tokens"])
	}
	if int(limit) != 512 {
		t.Fatalf("the stated limit must be the one in force, got %v", limit)
	}
}

func voiceOf(event map[string]any) string {
	session, _ := event["session"].(map[string]any)
	audio, _ := session["audio"].(map[string]any)
	output, _ := audio["output"].(map[string]any)
	voice, _ := output["voice"].(string)
	return voice
}
