package httpclient

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// burstServer answers a fixed number of simultaneous requests and counts the
// connections opened to it.
//
// Holding every request until all of them have arrived is what makes a burst
// genuinely concurrent. Served one at a time, a single connection would carry
// all of them and there would be no pool behaviour left to observe.
type burstServer struct {
	*httptest.Server
	opened atomic.Int64

	mu       sync.Mutex
	arrived  int
	expected int
	full     chan struct{}
	release  chan struct{}
}

func newBurstServer(t *testing.T) *burstServer {
	t.Helper()
	server := &burstServer{}
	server.Server = httptest.NewUnstartedServer(http.HandlerFunc(
		func(writer http.ResponseWriter, _ *http.Request) {
			<-server.arrive()
			_, _ = writer.Write([]byte("ok"))
		},
	))
	server.Server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			server.opened.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)
	return server
}

func (server *burstServer) expect(count int) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.arrived = 0
	server.expected = count
	server.full = make(chan struct{})
	server.release = make(chan struct{})
}

func (server *burstServer) arrive() chan struct{} {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.arrived++
	if server.arrived == server.expected {
		close(server.full)
	}
	return server.release
}

// run sends count simultaneous requests and returns when all have completed.
func (server *burstServer) run(t *testing.T, client *http.Client, count int) {
	t.Helper()
	server.expect(count)
	var sent sync.WaitGroup
	for range count {
		sent.Add(1)
		go func() {
			defer sent.Done()
			request, err := http.NewRequestWithContext(
				context.Background(), http.MethodGet, server.URL, nil)
			if err != nil {
				t.Error(err)
				return
			}
			response, err := client.Do(request)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = response.Body.Close() }()
			// Draining and closing is what returns a connection to the pool. A
			// body left unread is a connection the transport must discard,
			// which would make this measure the test rather than the pool.
			if _, err := io.Copy(io.Discard, response.Body); err != nil {
				t.Error(err)
			}
		}()
	}
	select {
	case <-server.full:
	case <-time.After(30 * time.Second):
		t.Fatal("the burst never arrived at the server")
	}
	server.mu.Lock()
	close(server.release)
	server.mu.Unlock()
	sent.Wait()
}

// TestTheAdapterClientKeepsAConcurrentBurstWarm is the reason this package
// exists, expressed as the thing an adapter actually does.
//
// http.DefaultTransport pools two idle connections per host. That is right for
// a program that talks to many hosts occasionally and wrong for a realtime
// runtime, which talks to one recogniser several times a second across every
// live session: past the second concurrent call, a finished connection is
// closed rather than returned to the pool, and the next call redials and
// renegotiates TLS on the path a person is waiting on.
//
// So the test counts connections rather than reading configuration back. A
// burst opens what it needs; a second identical burst must open nothing.
func TestTheAdapterClientKeepsAConcurrentBurstWarm(t *testing.T) {
	const burst = 16
	server := newBurstServer(t)

	server.run(t, Shared(), burst)
	first := server.opened.Load()
	if first < burst {
		t.Fatalf("first burst opened %d connections, want %d concurrent ones", first, burst)
	}

	// The transport returns a connection to the pool after the response body
	// is drained and closed, on its own goroutine. Give it a moment before
	// asking whether it kept them.
	time.Sleep(100 * time.Millisecond)

	server.run(t, Shared(), burst)
	if reopened := server.opened.Load() - first; reopened != 0 {
		t.Fatalf("a second identical burst redialled %d times; the pool kept %d of %d connections",
			reopened, burst-reopened, burst)
	}
}

// TestTheInheritedTransportWouldNotHaveKeptThem is the same measurement
// against the transport the adapters used to get by writing &http.Client{}.
//
// It is here so the number above is a claim about this system rather than a
// property of Go. Without it, a change that quietly restored the inherited
// pool would leave the test above passing on any machine fast enough to
// redial cheaply, which is every development machine and no loaded server.
func TestTheInheritedTransportWouldNotHaveKeptThem(t *testing.T) {
	const burst = 16
	server := newBurstServer(t)
	// A private copy, so this test cannot disturb the process-wide pool.
	inherited := http.DefaultTransport.(*http.Transport).Clone()
	defer inherited.CloseIdleConnections()
	client := &http.Client{Transport: inherited}

	server.run(t, client, burst)
	first := server.opened.Load()
	time.Sleep(100 * time.Millisecond)
	server.run(t, client, burst)

	if reopened := server.opened.Load() - first; reopened == 0 {
		t.Fatalf("the inherited transport kept all %d connections, so the test above is not "+
			"measuring anything this package does", burst)
	}
}

// TestTheAdapterClientImposesNoDeadlineOfItsOwn is a refusal, not an omission.
//
// One timeout for every provider is the mistake that hides a stalled one: a
// recogniser answering in tens of milliseconds and a reasoner answering in
// tens of seconds cannot share a bound that means anything for either. Each
// adapter puts a deadline on its own request, from its own cadence.
func TestTheAdapterClientImposesNoDeadlineOfItsOwn(t *testing.T) {
	if timeout := Shared().Timeout; timeout != 0 {
		t.Fatalf("Shared().Timeout = %s, want no shared deadline", timeout)
	}
}

// TestTheAdapterClientIsShared checks the pool is one pool. A transport is a
// connection pool, so a client built per call would reuse nothing.
func TestTheAdapterClientIsShared(t *testing.T) {
	if Shared() != Shared() {
		t.Fatal("Shared() returned two clients, so adapters would not share a connection pool")
	}
}
