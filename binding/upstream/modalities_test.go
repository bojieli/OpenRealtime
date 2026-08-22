package upstream_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/upstream"
)

// A text-only session tells the remote so.
//
// The remote is a Realtime API that speaks this protocol and would honour the
// declaration, so withholding it leaves the provider synthesising a full audio
// response for a client that asked for text - billed as audio output, put on
// the network, and decoded and dropped here. On the other bindings a text
// session merely wastes local synthesis; here the waste is the provider's
// meter and the client is the one paying it.
func TestATextSessionTellsTheRemoteItIsTextOnly(t *testing.T) {
	remote := newFakeRemote(t)
	bind, err := upstream.New(upstream.Config{
		URL: remote.url(), Slow: &scriptedSlow{}, Model: "remote-model",
	})
	if err != nil {
		t.Fatalf("new upstream: %v", err)
	}
	runtime, err := bind.Start(context.Background(), binding.Options{
		Sink: &collectingSink{}, SessionID: "test",
		Settings: binding.Settings{Modalities: []string{"text"}},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background(), nil) })
	<-remote.ready

	waitForRemote(t, remote, func(message map[string]any) bool {
		return declaresModality(message, "text")
	}, "the client's modality must reach the remote")
}

// And the remote's text reaches the client.
//
// Forwarding the declaration without handling what it changes would trade
// wasted audio for silence: a text response arrives on its own events, and
// nothing here was listening for them until there were any to hear.
func TestTheRemotesTextReachesTheClient(t *testing.T) {
	remote := newFakeRemote(t)
	bind, err := upstream.New(upstream.Config{
		URL: remote.url(), Slow: &scriptedSlow{}, Model: "remote-model",
	})
	if err != nil {
		t.Fatalf("new upstream: %v", err)
	}
	sink := &collectingSink{}
	runtime, err := bind.Start(context.Background(), binding.Options{
		Sink: sink, SessionID: "test",
		Settings: binding.Settings{Modalities: []string{"text"}},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background(), nil) })
	<-remote.ready

	remote.emit(map[string]any{
		"type": "response.output_text.delta", "delta": "Forty dollars.",
	})
	remote.emit(map[string]any{
		"type": "response.output_text.done", "text": "Forty dollars.",
	})

	waitFor(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		for _, spoken := range sink.spoken {
			if spoken == "Forty dollars." {
				return true
			}
		}
		return false
	}, "a text response must reach the client rather than being dropped")
}

// The declaration rides every session.update, for the same reason the floor
// does: a hand-off re-sends the session to carry an answer, and one sent
// without it puts the remote back on audio as a side effect of speaking.
func TestTheModalitySurvivesAHandoff(t *testing.T) {
	remote := newFakeRemote(t)
	slow := &scriptedSlow{turns: [][]continuation.Event{{
		{Kind: continuation.EventAssistantDelta, Text: "The balance is $40.00."},
	}}}
	bind, err := upstream.New(upstream.Config{
		URL: remote.url(), Slow: slow, Model: "remote-model",
		Handoff: upstream.HandoffSessionInstruction,
	})
	if err != nil {
		t.Fatalf("new upstream: %v", err)
	}
	runtime, err := bind.Start(context.Background(), binding.Options{
		Sink: &collectingSink{}, SessionID: "test",
		Settings: binding.Settings{Modalities: []string{"text"}},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background(), nil) })
	<-remote.ready
	waitForRemote(t, remote, func(message map[string]any) bool {
		return declaresModality(message, "text")
	}, "the first declaration must reach the remote")

	remote.emit(map[string]any{
		"type":    "conversation.item.input_audio_transcription.completed",
		"item_id": "item_1", "transcript": "what is my balance",
	})
	waitForRemote(t, remote, func(message map[string]any) bool {
		session, _ := message["session"].(map[string]any)
		instructions, _ := session["instructions"].(string)
		return message["type"] == "session.update" && strings.Contains(instructions, "$40.00")
	}, "the reasoner's answer must be handed to the remote")

	for _, message := range remote.sent() {
		if message["type"] != "session.update" {
			continue
		}
		if !declaresModality(message, "text") {
			t.Fatalf("a session.update put the remote back on audio: %v", message)
		}
	}
}

func declaresModality(message map[string]any, want string) bool {
	if message["type"] != "session.update" {
		return false
	}
	session, _ := message["session"].(map[string]any)
	declared, _ := session["output_modalities"].([]any)
	for _, entry := range declared {
		if name, _ := entry.(string); name == want {
			return true
		}
	}
	return false
}
