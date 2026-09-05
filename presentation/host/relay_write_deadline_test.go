package host

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestARelayPeerThatStopsReadingDoesNotWedgeTheSession is the write bound on
// the relay's data path, and the reason one stalled peer used to take both
// halves down.
//
// copyWebSocket reads one side and writes the other on a context that ends
// only when the relayed session does. A browser that stops reading blocks the
// write, so this goroutine stops reading its own side too, so the other half's
// writes back up behind a socket nobody is draining. The relay is then wedged
// in both directions with nothing to end it and no error anywhere.
func TestARelayPeerThatStopsReadingDoesNotWedgeTheSession(t *testing.T) {
	// Not parallel: it shortens the package-wide bound, which is the only way
	// to test a stall without waiting the shipped thirty seconds.
	restore := relayWriteTimeout
	relayWriteTimeout = 250 * time.Millisecond
	t.Cleanup(func() { relayWriteTimeout = restore })

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
	deaf, _, err := websocket.Dial(dialContext, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deaf.CloseNow() }()

	payload := make([]byte, 1<<20)
	failed := make(chan error, 1)
	go func() {
		for {
			if err := writeRelay(
				context.Background(), deaf, websocket.MessageBinary, payload,
			); err != nil {
				failed <- err
				return
			}
		}
	}()

	select {
	case <-failed:
	case <-time.After(20 * time.Second):
		t.Fatal("a peer that never read absorbed twenty seconds of relayed frames; the send is unbounded")
	}
}
