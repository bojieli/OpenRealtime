package livekit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestAStalledEndpointFailsTheSendInsteadOfBlockingForever is the write bound
// on the agent's link to the OpenRealtime endpoint.
//
// Send carries the room's inbound audio, and the context it gets is the
// agent's run context, which ends when the agent does. An endpoint that stops
// reading blocks the send with nothing left to end it, and because Send holds
// the write lock, every later send blocks behind the first: the room keeps
// talking and nothing reaches the model, with no error to say so.
func TestAStalledEndpointFailsTheSendInsteadOfBlockingForever(t *testing.T) {
	// Not parallel: it shortens the package-wide bound, which is the only way
	// to test a stall without waiting the shipped thirty seconds.
	restore := writeTimeout
	writeTimeout = 250 * time.Millisecond
	t.Cleanup(func() { writeTimeout = restore })

	// A peer that completes the handshake and then reads nothing.
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			connection, err := websocket.Accept(writer, request, nil)
			if err != nil {
				return
			}
			defer connection.CloseNow()
			<-release
		},
	))
	// Registered in this order so they run in the other one: the handler has
	// to return before Close can, and Close waits for outstanding requests.
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	dialContext, cancelDial := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelDial()
	connection, _, err := websocket.Dial(
		dialContext, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.CloseNow() }()
	client := &client{connection: connection}

	audio := strings.Repeat("A", 1<<20)
	failed := make(chan error, 1)
	go func() {
		for {
			err := client.Send(context.Background(), map[string]any{
				"type": "input_audio_buffer.append", "audio": audio,
			})
			if err != nil {
				failed <- err
				return
			}
		}
	}()

	select {
	case err := <-failed:
		if !strings.Contains(err.Error(), "send to the OpenRealtime endpoint") {
			t.Fatalf("Send() error = %v, want a send failure", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("an endpoint that never read absorbed twenty seconds of audio; the send is unbounded")
	}
}
