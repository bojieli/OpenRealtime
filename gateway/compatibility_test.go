package gateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/continuation"
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

// An official client sends events this deployment does not implement, and what
// it gets back has to be usable. Two of the three are refused for a structural
// reason rather than an unfinished one, and a client author can only act on
// that if the server says which.
func TestUnimplementedStandardEventsExplainThemselves(t *testing.T) {
	server := startServer(t, fast(), slow(), "hello")
	connection := dial(t, server)

	for _, test := range []struct {
		event    map[string]any
		contains string
	}{
		{
			event: map[string]any{
				"type": "conversation.item.delete", "event_id": "c2", "item_id": "item_1",
			},
			contains: "append-only",
		},
		{
			event: map[string]any{
				"type": "conversation.item.retrieve", "event_id": "c3", "item_id": "item_1",
			},
			contains: "retrieval",
		},
	} {
		connection.send(test.event)
		failure := connection.await("error", 5*time.Second)
		detail, _ := failure["error"].(map[string]any)
		message, _ := detail["message"].(string)
		if !strings.Contains(message, test.contains) {
			t.Fatalf("%v was refused with %q, which does not mention %q",
				test.event["type"], message, test.contains)
		}
		// The session continues. A refusal is an answer, not a disconnection.
		connection.send(map[string]any{"type": "input_audio_buffer.clear", "event_id": "k"})
		connection.await("input_audio_buffer.cleared", 5*time.Second)
	}
}

// Clearing the output buffer is how a client over WebRTC asks to stop hearing
// the agent: audio it has already buffered keeps playing until it does.
//
// The answer is paced over several seconds so the clear genuinely lands while
// audio is in flight. A test that raced a short utterance would be asserting
// something about scheduling rather than about the event.
func TestClearingTheOutputBufferStopsTheResponseAndNamesIt(t *testing.T) {
	server := startServerWithSpeech(t,
		fast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Here is a long answer."}}),
		slow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "The answer."}}),
		staticASR{text: "tell me something"}, pacedSpeech{chunks: 40})
	connection := dial(t, server)
	connection.await("session.created", 5*time.Second)
	connection.configurePCM16(nil)
	connection.await("session.updated", 5*time.Second)
	connection.speak()

	created := connection.await("response.created", 10*time.Second)
	response, _ := created["response"].(map[string]any)
	responseID, _ := response["id"].(string)
	// Wait until audio is actually on the wire, so there is something to stop.
	connection.await("response.output_audio.delta", 10*time.Second)

	connection.send(map[string]any{"type": "output_audio_buffer.clear", "event_id": "o1"})
	cleared := connection.await("output_audio_buffer.cleared", 10*time.Second)
	if got, _ := cleared["response_id"].(string); got != responseID {
		t.Fatalf("the acknowledgement must name the response it cleared: %q vs %q", got, responseID)
	}
}

// pacedSpeech produces enough audio that emission takes real time, which is
// what makes "while the agent is speaking" a state a test can be in.
type pacedSpeech struct{ chunks int }

func (pacedSpeech) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "paced", Version: "1", Capabilities: v1.Capabilities{}}
}

func (pacedSpeech) Synthesize(context.Context, v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	return nil, errors.New("unused")
}

func (speech pacedSpeech) Stream(
	ctx context.Context, plan v1.SpeechPlan, emit func(v1.SpeechChunk) error,
) error {
	for index := 0; index < speech.chunks; index++ {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		default:
		}
		if err := emit(v1.SpeechChunk{
			ChunkID: "c", CandidateID: plan.CandidateID, SampleRateHz: 24_000,
			PCM16LE: make([]byte, 4800), Final: index == speech.chunks-1,
		}); err != nil {
			return err
		}
	}
	return nil
}

// And with nothing playing there is no response to name, so it is refused
// rather than acknowledged with an empty identifier.
func TestClearingAnEmptyOutputBufferIsRefused(t *testing.T) {
	server := startServer(t, fast(), slow(), "hello")
	connection := dial(t, server)
	connection.send(map[string]any{"type": "output_audio_buffer.clear", "event_id": "o1"})
	failure := connection.await("error", 5*time.Second)
	detail, _ := failure["error"].(map[string]any)
	if message, _ := detail["message"].(string); !strings.Contains(message, "no response is in progress") {
		t.Fatalf("unexpected refusal %q", message)
	}
}
