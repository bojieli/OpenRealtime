package realtimeclient_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/realtimeclient"
	"github.com/coder/websocket"
)

// echoServer is a Realtime endpoint for the purposes of one test: it records
// what the client sent and replies with whatever the test asked it to.
type echoServer struct {
	server *httptest.Server

	mu       sync.Mutex
	received []string
	headers  http.Header
	query    string
}

func newEchoServer(t *testing.T, reply func(send func(any))) *echoServer {
	t.Helper()
	endpoint := &echoServer{}
	endpoint.server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			endpoint.mu.Lock()
			endpoint.headers = request.Header.Clone()
			endpoint.query = request.URL.RawQuery
			endpoint.mu.Unlock()

			connection, err := websocket.Accept(writer, request, nil)
			if err != nil {
				return
			}
			defer connection.CloseNow()
			ctx := request.Context()
			send := func(value any) {
				encoded, err := json.Marshal(value)
				if err != nil {
					return
				}
				_ = connection.Write(ctx, websocket.MessageText, encoded)
			}
			if reply != nil {
				reply(send)
			}
			for {
				_, input, err := connection.Read(ctx)
				if err != nil {
					return
				}
				endpoint.mu.Lock()
				endpoint.received = append(endpoint.received, string(input))
				endpoint.mu.Unlock()
			}
		}))
	t.Cleanup(endpoint.server.Close)
	return endpoint
}

func (endpoint *echoServer) url() string {
	return "ws" + strings.TrimPrefix(endpoint.server.URL, "http")
}

func (endpoint *echoServer) sent() []string {
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	return append([]string(nil), endpoint.received...)
}

func TestDialCarriesTheCredentialAndTheModel(t *testing.T) {
	endpoint := newEchoServer(t, nil)
	client, err := realtimeclient.Dial(context.Background(), realtimeclient.Config{
		URL: endpoint.url(), Token: "secret", Model: "gpt-realtime-2",
		Header: http.Header{"OpenAI-Beta": []string{"realtime=v1"}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	if got := endpoint.headers.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("credential not presented, got %q", got)
	}
	if got := endpoint.headers.Get("OpenAI-Beta"); got != "realtime=v1" {
		t.Fatalf("supplied headers must survive, got %q", got)
	}
	if !strings.Contains(endpoint.query, "model=gpt-realtime-2") {
		t.Fatalf("model not requested, query was %q", endpoint.query)
	}
}

// A URL that already names a model is left alone: the caller was explicit.
func TestDialDoesNotOverrideAModelTheURLAlreadyNames(t *testing.T) {
	endpoint := newEchoServer(t, nil)
	client, err := realtimeclient.Dial(context.Background(), realtimeclient.Config{
		URL: endpoint.url() + "?model=chosen", Model: "ignored",
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	if strings.Contains(endpoint.query, "ignored") {
		t.Fatalf("an explicit model must not be overridden, query was %q", endpoint.query)
	}
}

func TestDialCanSeparateSetupDeadlineFromConnectionLifetime(t *testing.T) {
	release := make(chan struct{})
	endpoint := newEchoServer(t, func(send func(any)) {
		<-release
		send(map[string]any{"type": "session.created", "event_id": "after-setup"})
	})
	setupContext, cancelSetup := context.WithCancel(context.Background())
	lifetimeContext, cancelLifetime := context.WithCancel(context.Background())
	defer cancelLifetime()
	client, err := realtimeclient.Dial(setupContext, realtimeclient.Config{
		URL: endpoint.url(), LifetimeContext: lifetimeContext,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	cancelSetup()
	close(release)
	if event := receive(t, client); event.Field("event_id") != "after-setup" {
		t.Fatalf("setup cancellation closed the established connection: %+v", event)
	}
}

func TestEventsAreDeliveredWithTheirRawBytes(t *testing.T) {
	endpoint := newEchoServer(t, func(send func(any)) {
		send(map[string]any{
			"type": "session.created", "event_id": "e1",
			"session": map[string]any{"id": "sess_1"},
		})
		send(map[string]any{
			"type": "openrealtime.observation.added", "observation_id": "obs_1",
			"observer": "video", "text": "A dialog appeared.",
		})
	})
	client, err := realtimeclient.Dial(context.Background(), realtimeclient.Config{URL: endpoint.url()})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	first := receive(t, client)
	if first.Type != "session.created" || first.Field("event_id") != "e1" {
		t.Fatalf("unexpected first event: %+v", first)
	}
	var decoded struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	if err := first.Decode(&decoded); err != nil || decoded.Session.ID != "sess_1" {
		t.Fatalf("raw bytes must survive for a caller to decode: %v %+v", err, decoded)
	}

	// An extension event reaches the caller like any other. A client that
	// only understood the base protocol would have to drop it, which is the
	// thing the namespace exists to make unnecessary.
	second := receive(t, client)
	if second.Type != "openrealtime.observation.added" || second.Field("observer") != "video" {
		t.Fatalf("unexpected second event: %+v", second)
	}
}

func TestSendReachesTheEndpointAndStopsAfterClose(t *testing.T) {
	endpoint := newEchoServer(t, nil)
	client, err := realtimeclient.Dial(context.Background(), realtimeclient.Config{URL: endpoint.url()})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := client.Send(context.Background(), map[string]any{
		"type": "response.create",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitFor(t, func() bool { return len(endpoint.sent()) == 1 }, "the endpoint never received the event")
	if !strings.Contains(endpoint.sent()[0], "response.create") {
		t.Fatalf("unexpected payload %q", endpoint.sent()[0])
	}

	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := client.Send(context.Background(), map[string]any{"type": "response.create"}); err == nil {
		t.Fatal("writing to a closed client must fail rather than block")
	}
	client.Wait()
	if err := client.Err(); err != nil {
		t.Fatalf("a clean close is not an error: %v", err)
	}
}

// Wire validation is off by default deliberately: a client that refuses to
// talk to a slightly non-conformant server is less useful than one that
// reports the problem. When it is on, it reports.
func TestWireValidationIsOptOutAndReportsWhatItRejects(t *testing.T) {
	malformed := func(send func(any)) {
		send(map[string]any{"type": "session.created"})
	}

	tolerant := newEchoServer(t, malformed)
	client, err := realtimeclient.Dial(context.Background(), realtimeclient.Config{URL: tolerant.url()})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if event := receive(t, client); event.Type != "session.created" {
		t.Fatalf("the default must deliver what arrived: %+v", event)
	}
	_ = client.Close()

	strict := newEchoServer(t, malformed)
	checking, err := realtimeclient.Dial(context.Background(), realtimeclient.Config{
		URL: strict.url(), ValidateWire: true,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer checking.Close()
	checking.Wait()
	if checking.Err() == nil {
		t.Fatal("a validating client must report what it rejected")
	}
}

func receive(t *testing.T, client *realtimeclient.Client) realtimeclient.Event {
	t.Helper()
	select {
	case event, open := <-client.Events():
		if !open {
			t.Fatalf("the connection closed: %v", client.Err())
		}
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("no event arrived")
		return realtimeclient.Event{}
	}
}

func waitFor(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(message)
}
