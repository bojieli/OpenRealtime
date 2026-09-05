package gateway_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/gateway"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	"github.com/coder/websocket"
	"net/http/httptest"
)

// startConfiguredServer is startServer with the deployment-owned bounds left
// open for a test to set. The liveness bounds are the one part of the gateway
// configuration whose defaults are minutes long, so a test that means to
// exercise them has to name its own.
func startConfiguredServer(t *testing.T, adjust func(*gateway.Config)) *httptest.Server {
	t.Helper()
	legacyBind, err := cascade.New(cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) { return staticASR{text: "hello"}, nil },
		Fast:       fast(), Slow: slow(), Speech: toneSpeech{},
		Voice: "test-voice", FastMaxTokens: 512,
	})
	if err != nil {
		t.Fatalf("new cascade: %v", err)
	}
	bind, err := graphbinding.New(legacyBind)
	if err != nil {
		t.Fatalf("mount cascade through graph: %v", err)
	}
	config := gateway.Config{Binding: bind, Model: "openrealtime-test", ValidateWire: true}
	adjust(&config)
	server, err := gateway.New(config)
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	http := httptest.NewServer(testGatewayHandler(server))
	t.Cleanup(http.Close)
	return http
}

func readMetrics(t *testing.T, server *httptest.Server) map[string]any {
	t.Helper()
	response, err := http.Get(server.URL + "/metrics")
	if err != nil {
		t.Fatalf("get metrics: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	var snapshot map[string]any
	if err := json.NewDecoder(response.Body).Decode(&snapshot); err != nil {
		t.Fatalf("decode metrics: %v", err)
	}
	return snapshot
}

// awaitMetric waits for one counter to reach want. Polling is the honest shape
// here: the session ends on its own goroutine after the bound expires, and the
// test is asserting that it ends at all rather than exactly when.
func awaitMetric(t *testing.T, server *httptest.Server, name string, want float64) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last float64
	for time.Now().Before(deadline) {
		value, _ := readMetrics(t, server)[name].(float64)
		last = value
		if value >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("metric %s reached %v, want at least %v before the deadline", name, last, want)
}

// awaitMetricDown is awaitMetric for a gauge that has to come back down.
//
// A counter and a gauge are not updated in the same instant: a session that
// failed increments sessions_failed and then releases its slot, so a test that
// waits for the counter and reads the gauge in the next statement is asserting
// on a window rather than on a state. It passes on an idle machine and fails
// on a loaded one, which is the worst way for a gate to be wrong.
func awaitMetricDown(t *testing.T, server *httptest.Server, name string, want float64) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last float64
	for time.Now().Before(deadline) {
		value, _ := readMetrics(t, server)[name].(float64)
		last = value
		if value <= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("metric %s stayed at %v, want at most %v before the deadline", name, last, want)
}

// dialSilent opens a session and never reads from it again.
//
// It is the client this file is about: one that completed the handshake and
// then stopped collecting anything the server sends. Every failure below
// starts here, because a connection that is open and not being read is
// indistinguishable, from the socket's point of view, from one that is simply
// quiet.
func dialSilent(t *testing.T, server *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime?model=openrealtime-test"
	connection, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close(websocket.StatusNormalClosure, "done") })
	return connection
}

// TestASessionEndsWhenTheClientStopsReadingInsteadOfWedgingForever is the
// write-deadline claim.
//
// A client that stops reading closes its receive window. The server's write
// then blocks, and it used to block on the session context, which ends only
// when the session does - so nothing ended it. The writer goroutine stopped
// draining, the send buffer filled, the handler blocked in send, and the read
// loop blocked handing it the next event. The session was wedged for the life
// of the process, holding its binding runtime and provider connections, and no
// health check could see it because the process was fine.
//
// The test drives real volume rather than reaching into the socket, because
// the property is about what the operating system does when a peer stops
// reading, and a fake write would not do it.
func TestASessionEndsWhenTheClientStopsReadingInsteadOfWedgingForever(t *testing.T) {
	server := startConfiguredServer(t, func(config *gateway.Config) {
		config.WriteTimeout = 500 * time.Millisecond
		// Disabled so this test can only pass for the reason it names. With
		// the keepalive on, a client that stops reading also stops answering
		// pings, and the session would end either way.
		config.KeepaliveInterval = -1
	})
	connection := dialSilent(t, server)

	// Each item is echoed back as conversation.item.created, so every message
	// sent is a message of the same size the server must write to a peer that
	// is not reading. A megabyte at a time reaches past any socket buffer in a
	// few rounds without approaching the connection's read limit.
	payload := strings.Repeat("x", 900_000)
	go func() {
		for range 32 {
			message, err := json.Marshal(map[string]any{
				"type": "conversation.item.create",
				"item": map[string]any{
					"type": "message", "role": "user",
					"content": []map[string]any{{"type": "input_text", "text": payload}},
				},
			})
			if err != nil {
				return
			}
			if err := connection.Write(context.Background(), websocket.MessageText, message); err != nil {
				return
			}
		}
	}()

	awaitMetric(t, server, "sessions_failed", 1)
	awaitMetricDown(t, server, "sessions_in_flight", 0)
}

// TestASessionEndsWhenThePeerStopsAnsweringKeepalives is the other half.
//
// The WebSocket library answers the client's pings from inside Read, so a
// client that sends them learns the server is alive. Nothing told the server
// the reverse. A peer that vanished without a FIN left a connection that was
// open on this side only, and in a session where neither side was speaking
// there was no write to discover it with - the process held the session, its
// runtime, and its provider connections indefinitely.
//
// A client that has stopped reading has also stopped answering pings, which is
// exactly the peer this is about, and it needs no traffic at all to detect.
func TestASessionEndsWhenThePeerStopsAnsweringKeepalives(t *testing.T) {
	server := startConfiguredServer(t, func(config *gateway.Config) {
		config.KeepaliveInterval = 250 * time.Millisecond
		// Disabled so the session can only end because a ping went unanswered.
		// This connection is idle; nothing is being written to time out.
		config.WriteTimeout = -1
	})
	dialSilent(t, server)
	awaitMetric(t, server, "sessions_failed", 1)
}

// TestTheGatewayRefusesSessionsBeyondItsDeclaredCapacity is admission.
//
// Past admission a session holds a binding runtime and its provider
// connections, so an unbounded gateway does not degrade under load, it
// exhausts the process - and takes every established session with it. A
// refusal is a 503 the caller can retry.
func TestTheGatewayRefusesSessionsBeyondItsDeclaredCapacity(t *testing.T) {
	server := startConfiguredServer(t, func(config *gateway.Config) {
		config.MaxSessions = 2
	})
	for range 2 {
		dialSilent(t, server)
	}
	// The admitted sessions have to be counted before the refusal is
	// meaningful: a 503 from a gateway that admitted nothing would pass this
	// test for the wrong reason.
	awaitMetric(t, server, "sessions_in_flight", 2)

	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime?model=openrealtime-test"
	_, response, err := websocket.Dial(context.Background(), url, nil)
	if err == nil {
		t.Fatal("Dial() error = nil, want refusal at capacity")
	}
	if response == nil {
		t.Fatalf("Dial() error = %v with no response, want 503", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
	if retry := response.Header.Get("Retry-After"); retry == "" {
		t.Fatal("a capacity refusal carries no Retry-After, so a caller cannot tell it from a broken process")
	}
	if rejected, _ := readMetrics(t, server)["sessions_rejected"].(float64); rejected != 1 {
		t.Fatalf("sessions_rejected = %v, want 1", rejected)
	}
}
