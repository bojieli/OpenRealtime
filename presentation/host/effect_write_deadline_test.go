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

// TestAnEffectPeerThatStopsReadingDoesNotWedgeItsSession is the write bound on
// the effect socket, and the lock above it is why it matters more here than
// the same defect does elsewhere.
//
// websocket.Write returns when the peer's receive window has room or when its
// context ends, and the session context ends only when the session does - so a
// browser that stops reading blocks the write with nothing left to end it.
// Every write to a session goes through one mutex, so the first stalled write
// blocks all of them, and the session keeps one of the hub's bounded slots for
// the life of the process. A host that has lost sixty-four peers that way
// refuses everyone, while every one of its own health signals stays green.
func TestAnEffectPeerThatStopsReadingDoesNotWedgeItsSession(t *testing.T) {
	// Not parallel: it shortens the package-wide bound, which is the only way
	// to test a stall without waiting the shipped thirty seconds.
	restore := effectWriteTimeout
	effectWriteTimeout = 250 * time.Millisecond
	t.Cleanup(func() { effectWriteTimeout = restore })

	// A peer that completes the handshake and then reads nothing. Its receive
	// window closes once the buffers fill, which is indistinguishable from a
	// quiet client until something tries to write.
	release := make(chan struct{})
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			connection, err := websocket.Accept(writer, request, nil)
			if err != nil {
				return
			}
			defer connection.CloseNow()
			accepted <- connection
			<-release
		},
	))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	dialContext, cancelDial := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelDial()
	client, _, err := websocket.Dial(dialContext, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.CloseNow() }()
	<-accepted

	sessionContext, cancelSession := context.WithCancelCause(context.Background())
	defer cancelSession(nil)
	session := &effectSession{
		hub:        &effectsHub{limits: effectLimits{MaxMessageBytes: maximumEffectMessageBytes}},
		connection: client,
		ctx:        sessionContext,
	}

	// One large payload per write, so the peer's buffers close in a handful of
	// them rather than thousands.
	message := effectServerMessage{Type: "error", ID: strings.Repeat("x", 1<<20)}

	failed := make(chan error, 1)
	go func() {
		for {
			if err := session.write(message); err != nil {
				failed <- err
				return
			}
		}
	}()

	select {
	case err := <-failed:
		if !strings.Contains(err.Error(), "stopped reading") {
			t.Fatalf("write() error = %v, want the peer-stopped-reading failure", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a peer that never read absorbed twenty seconds of writes; the send is unbounded")
	}
}
