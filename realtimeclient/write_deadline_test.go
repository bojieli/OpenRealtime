package realtimeclient_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/realtimeclient"
	"github.com/coder/websocket"
)

// newDeafEndpoint completes the handshake and then reads nothing.
//
// From this side that is indistinguishable from a quiet server until something
// tries to write: nothing failed, and the socket simply stops taking bytes once
// its receive window closes.
func newDeafEndpoint(t *testing.T) string {
	t.Helper()
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
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

// TestAStalledEndpointFailsTheSendInsteadOfBlockingForever is the write bound,
// and the reason a context parameter was not enough on its own.
//
// Send takes a context, so a caller can always bound its own call. The callers
// that matter hold a session-scoped socket and pass the session's context,
// which ends when the conversation does - the upstream binding forwards a
// caller's audio through this client, and the WebRTC adapter bridges a peer
// through it. A remote that stops reading blocks the send with nothing left to
// end it, and because Send holds the write lock, every later send blocks
// behind the first. What stops is the conversation, with no error anywhere.
//
// Nothing here closes the client: without the bound, the sending goroutine is
// stuck inside the write lock Close needs, so closing would hang the test
// rather than fail it.
func TestAStalledEndpointFailsTheSendInsteadOfBlockingForever(t *testing.T) {
	t.Parallel()
	client, err := realtimeclient.Dial(context.Background(), realtimeclient.Config{
		URL: newDeafEndpoint(t), WriteTimeout: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	audio := base64.StdEncoding.EncodeToString(make([]byte, 1<<20))
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
		if !strings.Contains(err.Error(), "send to the Realtime endpoint") {
			t.Fatalf("Send() error = %v, want a send failure", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a peer that never read absorbed twenty seconds of audio; the send is unbounded")
	}
}
