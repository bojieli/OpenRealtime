package geminilive_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/geminilive"
	"github.com/coder/websocket"
)

// newDeafLive accepts the connection and then reads nothing from it.
//
// That is what a provider under load or behind a black-holed route looks like
// from this side: the handshake succeeded, so nothing has failed, and the
// socket stops taking bytes once its receive window closes.
func newDeafLive(t *testing.T) string {
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

// TestAStalledLiveSocketFailsTheSendInsteadOfBlockingForever is the write
// bound on the foreground voice.
//
// The dial was bounded and the sends that follow it were not. The context that
// reaches them comes from the session and has no deadline of its own, so a
// stalled socket blocks a send carrying the caller's audio - and this client
// is the foreground voice, so what stops is the conversation, with no error
// anywhere to say why.
//
// Nothing here closes the client: without the bound, the sending goroutine is
// stuck inside the write lock that Close needs, so closing would hang the test
// rather than fail it.
func TestAStalledLiveSocketFailsTheSendInsteadOfBlockingForever(t *testing.T) {
	t.Parallel()
	client, err := geminilive.Dial(context.Background(), geminilive.Config{
		URL: newDeafLive(t), APIKey: "test-key", Model: "gemini-test",
		WriteTimeout: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// The setup has to go first: until it is sent, audio is buffered rather
	// than written, and this test is about the write.
	if err := client.Send(context.Background(), map[string]any{
		"type": "session.update", "session": map[string]any{"instructions": "test"},
	}); err != nil {
		t.Fatalf("setup: %v", err)
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
		if !strings.Contains(err.Error(), "send to Gemini Live") {
			t.Fatalf("Send() error = %v, want a send failure", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a peer that never read absorbed twenty seconds of audio; the send is unbounded")
	}
}
