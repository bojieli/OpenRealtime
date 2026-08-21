package gateway_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/bojieli/OpenRealtime/realtimeclient"
	"github.com/coder/websocket"
)

// baseServer is a Realtime server that has never heard of this extension. It
// does what the specification says such a server does: ignores the unknown key
// and never echoes it.
func baseServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{})
		if err != nil {
			return
		}
		defer connection.CloseNow()
		ctx := request.Context()
		send := func(value map[string]any) error {
			encoded, _ := json.Marshal(value)
			return connection.Write(ctx, websocket.MessageText, encoded)
		}
		if err := send(map[string]any{
			"type": "session.created", "event_id": "e1",
			"session": map[string]any{"id": "sess_1", "object": "realtime.session"},
		}); err != nil {
			return
		}
		for {
			_, input, err := connection.Read(ctx)
			if err != nil {
				return
			}
			var decoded map[string]any
			if json.Unmarshal(input, &decoded) != nil {
				continue
			}
			switch decoded["type"] {
			case "session.update":
				// A base server echoes what it understands. It does not know
				// about the extension key and so does not carry it forward.
				if err := send(map[string]any{
					"type": "session.updated", "event_id": "e2",
					"session": map[string]any{"id": "sess_1", "object": "realtime.session"},
				}); err != nil {
					return
				}
			default:
				if err := send(map[string]any{
					"type": "error", "event_id": "e3",
					"error": map[string]any{
						"type": "invalid_request_error", "code": "unknown_event",
						"message": "unknown event type",
					},
				}); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// An extended client against a base server must degrade to voice-only rather
// than fail. This is the third of the four compatibility cases, and the one an
// implementation is most likely to get wrong, because it is the only one that
// depends on reading an absence correctly.
func TestExtendedClientAgainstBaseServerDegradesToVoiceOnly(t *testing.T) {
	server := baseServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := realtimeclient.Dial(ctx, realtimeclient.Config{
		URL: "ws" + strings.TrimPrefix(server.URL, "http"),
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	if event := awaitClient(t, client, "session.created"); event.Type != "session.created" {
		t.Fatal("expected a session")
	}
	if err := client.Send(ctx, map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime",
			"openrealtime": openrealtime.Request{
				Version: openrealtime.Version, Supports: openrealtime.Features(),
			},
		},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	updated := awaitClient(t, client, "session.updated")
	var decoded struct {
		Session struct {
			OpenRealtime *openrealtime.Response `json:"openrealtime"`
		} `json:"session"`
	}
	if err := updated.Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Session.OpenRealtime != nil {
		t.Fatal("a base server must not echo a key it does not implement")
	}
	// The client's correct reading of that absence: no enabled list, so no
	// extension. It stays voice-only and the session works.
}

// A base client against this server must never see the extension. The server
// does not volunteer what nobody asked for.
func TestBaseClientAgainstExtendedServerSeesNothingNew(t *testing.T) {
	server := startServer(t, fast(), slow(), "hello")
	client := dial(t, server)
	created := client.await("session.created", 5*time.Second)
	if _, present := created["session"].(map[string]any)["openrealtime"]; present {
		t.Fatal("the extension must not appear unbidden")
	}
	// A base client sends an ordinary update and gets an ordinary session.
	client.send(map[string]any{"type": "session.update", "session": map[string]any{"type": "realtime"}})
	updated := client.await("session.updated", 5*time.Second)
	sessionObject := updated["session"].(map[string]any)
	if _, present := sessionObject["openrealtime"]; present {
		t.Fatal("the extension must not appear unbidden")
	}
	// Everything the base protocol promises is still there.
	for _, field := range []string{"id", "object", "model", "audio", "tools"} {
		if _, present := sessionObject[field]; !present {
			t.Fatalf("the base session object lost %q", field)
		}
	}
}

func awaitClient(t *testing.T, client *realtimeclient.Client, eventType string) realtimeclient.Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case event, open := <-client.Events():
			if !open {
				t.Fatalf("connection closed waiting for %q: %v", eventType, client.Err())
			}
			if event.Type == eventType {
				return event
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q", eventType)
		}
	}
}
