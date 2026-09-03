package bench

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bojieli/OpenRealtime/realtimeclient"
)

type toolAnswerSession struct {
	events chan realtimeclient.Event
	sent   []any
}

func (session *toolAnswerSession) Events() <-chan realtimeclient.Event { return session.events }
func (session *toolAnswerSession) Err() error                          { return nil }
func (session *toolAnswerSession) Close() error                        { return nil }
func (session *toolAnswerSession) Send(_ context.Context, value any) error {
	session.sent = append(session.sent, value)
	return nil
}

func TestToolHandlerErrorUsesExplicitRealtimeFailureEncoding(t *testing.T) {
	client := &toolAnswerSession{events: make(chan realtimeclient.Event)}
	recorder := &recorder{}
	err := recorder.answer(context.Background(), client, SessionConfig{
		HandleTool: func(context.Context, ToolRequest) (json.RawMessage, error) {
			return nil, errors.New("action budget exhausted")
		},
	}, "call-1", "computer.click", json.RawMessage(`{"x":1,"y":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(client.sent) != 2 {
		t.Fatalf("sent messages = %d, want result and response.create", len(client.sent))
	}
	created, ok := client.sent[0].(map[string]any)
	if !ok {
		t.Fatalf("tool result message has type %T", client.sent[0])
	}
	item, ok := created["item"].(map[string]any)
	if !ok || item["output"] != "Error: action budget exhausted" {
		t.Fatalf("tool failure output = %+v", created)
	}
	if len(recorder.moments) != 1 || recorder.moments[0].Kind != MomentToolResult ||
		recorder.moments[0].Text != "Error: action budget exhausted" {
		t.Fatalf("recorded tool result = %+v", recorder.moments)
	}
}
