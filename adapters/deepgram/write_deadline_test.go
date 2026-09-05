package deepgram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/coder/websocket"
)

// newDeafDeepgram accepts the connection and then reads nothing from it.
//
// That is what a service under load or behind a black-holed route looks like
// from here: the handshake succeeded, so nothing has failed, and the socket
// simply stops taking bytes once its receive window closes.
//
// The handler waits on a channel the test controls rather than on the request
// context, because the request context ends when the connection does, and the
// connection is the thing this test is deliberately leaving stuck.
func newDeafDeepgram(t *testing.T) string {
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

// TestAStalledDeepgramSocketFailsTheFrameInsteadOfBlockingForever is the write
// bound, and the reason it is a bound rather than an inherited deadline.
//
// PushFrame holds this listener's lock and takes the context the session gave
// it, which has no deadline of its own - a session ends when the conversation
// does. The dial was bounded and the drain was bounded, so the one call on the
// hot path, made every cadence for as long as someone is speaking, was the
// only unbounded one. A stalled socket there does not slow recognition down,
// it stops it, and the block propagates back through the observer into the
// binding and then into the session's own event loop.
//
// Nothing here calls Close on the listener. Without the bound, the pushing
// goroutine is stuck inside the lock that Close needs, so closing would hang
// the test rather than fail it - which is the defect itself, and a poor way to
// report it.
func TestAStalledDeepgramSocketFailsTheFrameInsteadOfBlockingForever(t *testing.T) {
	t.Parallel()
	listener, err := NewListener(ListenConfig{
		URL: newDeafDeepgram(t), APIKey: "secret", Model: "nova-test",
		WriteTimeout: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The pushes run on their own goroutine so that an unbounded send fails
	// this test in twenty seconds with a sentence, rather than hanging until
	// the package timeout kills the whole run. The failure being demonstrated
	// is precisely "this call does not return", so the test must not be
	// written as a loop that depends on it returning.
	failed := make(chan error, 1)
	go func() {
		// Four seconds of 16 kHz audio per frame. The socket buffers absorb
		// the first few and then stop, which is the state this is about.
		var offset uint64
		for index := uint64(0); ; index++ {
			const samples = 64_000
			_, err := listener.PushFrame(context.Background(), v1.AudioFrame{
				Index: index, SampleOffset: offset, SampleRateHz: 16_000, PCM16LE: tone(samples),
			})
			offset += samples
			if err != nil {
				failed <- err
				return
			}
		}
	}()

	select {
	case err := <-failed:
		if !strings.Contains(err.Error(), "send Deepgram audio") {
			t.Fatalf("PushFrame() error = %v, want a send failure", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a peer that never read absorbed twenty seconds of frames; the send is unbounded")
	}
}
