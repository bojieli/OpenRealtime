package upstream_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/upstream"
	"github.com/bojieli/OpenRealtime/continuation"
)

// A client taking the floor is forwarded, not interpreted.
//
// The remote runs the floor here and speaks this same protocol, so a null
// detector means to it exactly what it meant to us. Keeping the declaration to
// ourselves would leave the remote ending turns on silence while the client
// believed it had stopped that - the client waiting to be asked, and the agent
// answering on its own schedule.
func TestTakingTheFloorReachesTheRemote(t *testing.T) {
	remote := newFakeRemote(t)
	bind, err := upstream.New(upstream.Config{
		URL: remote.url(), Slow: &scriptedSlow{}, Model: "remote-model",
	})
	if err != nil {
		t.Fatalf("new upstream: %v", err)
	}
	if !bind.Capabilities().ManualTurns {
		t.Fatal("the remote holds this floor and can be told to give it up")
	}
	runtime, err := bind.Start(context.Background(), binding.Options{
		Sink: &collectingSink{}, SessionID: "test",
		Settings: binding.Settings{ManualTurns: true},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background(), nil) })
	<-remote.ready

	waitForRemote(t, remote, manualTurnsDeclared, "the client's declaration must reach the remote")
}

// And it keeps reaching it. A hand-off borrows the session instruction to
// carry an answer, which means re-sending the whole session - and a session
// re-sent without the declaration hands the floor back to the remote as a side
// effect of saying something.
func TestTheDeclarationSurvivesAHandoff(t *testing.T) {
	remote := newFakeRemote(t)
	slow := &scriptedSlow{turns: [][]continuation.Event{{
		{Kind: continuation.EventAssistantDelta, Text: "The balance is $40.00."},
	}}}
	// The session-instruction hand-off is the one that re-sends the session.
	// The conversation-item hand-off appends a message and never touches it,
	// so it could not lose the declaration and testing it would prove nothing.
	bind, err := upstream.New(upstream.Config{
		URL: remote.url(), Slow: slow, Model: "remote-model",
		Handoff: upstream.HandoffSessionInstruction,
	})
	if err != nil {
		t.Fatalf("new upstream: %v", err)
	}
	runtime, err := bind.Start(context.Background(), binding.Options{
		Sink: &collectingSink{}, SessionID: "test",
		Settings: binding.Settings{ManualTurns: true},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background(), nil) })
	<-remote.ready
	waitForRemote(t, remote, manualTurnsDeclared, "the first declaration must reach the remote")

	// The user says something, so the reasoner produces an answer that has to
	// be handed to the remote to say.
	remote.emit(map[string]any{
		"type":    "conversation.item.input_audio_transcription.completed",
		"item_id": "item_1", "transcript": "what is my balance",
	})
	waitForRemote(t, remote, func(message map[string]any) bool {
		session, _ := message["session"].(map[string]any)
		instructions, _ := session["instructions"].(string)
		return message["type"] == "session.update" && strings.Contains(instructions, "$40.00")
	}, "the reasoner's answer must be handed to the remote")

	// Every session.update the binding sent must still declare it, including
	// the one carrying the answer.
	for _, message := range remote.sent() {
		if message["type"] != "session.update" {
			continue
		}
		if !manualTurnsDeclared(message) {
			t.Fatalf("a session.update gave the floor back without being asked: %v", message)
		}
	}
}

// manualTurnsDeclared reports whether this session.update tells the remote the
// client owns the floor. An explicit null is the declaration; an absent key is
// the remote's own default, which is the opposite.
func manualTurnsDeclared(message map[string]any) bool {
	if message["type"] != "session.update" {
		return false
	}
	session, _ := message["session"].(map[string]any)
	audio, _ := session["audio"].(map[string]any)
	input, _ := audio["input"].(map[string]any)
	if input == nil {
		return false
	}
	detection, present := input["turn_detection"]
	return present && detection == nil
}

func waitForRemote(t *testing.T, remote *fakeRemote, match func(map[string]any) bool, message string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		for _, sent := range remote.sent() {
			if match(sent) {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("%s (saw %d messages)", message, len(remote.sent()))
		case <-time.After(10 * time.Millisecond):
		}
	}
}
