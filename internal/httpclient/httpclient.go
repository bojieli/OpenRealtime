// Package httpclient owns the HTTP client that model adapters use when their
// caller supplies none.
//
// It exists because http.DefaultTransport is the wrong default for this
// system. Its MaxIdleConnsPerHost is 2, which is a sensible number for a
// program that talks to many hosts occasionally and the wrong one for a
// realtime runtime, which talks to a handful of hosts constantly: a recogniser
// is called several times a second per session, and every concurrent call past
// the second returns a connection that is closed rather than pooled. The next
// call redials and renegotiates TLS, on the path a person is waiting on.
//
// The repository already knew this. The offline review client tunes its own
// transport; the adapters that serve live conversations did not, which is the
// wrong way round.
package httpclient

import (
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	// idlePerHost is the pool an adapter needs to keep a burst warm. A turn
	// can have a recogniser advance, a synthesiser call, and a model
	// continuation in flight at once, several sessions deep, and all of them
	// may be the same host in a colocated deployment.
	idlePerHost = 64
	// idleTotal bounds the pool across every host an adapter set reaches.
	idleTotal = 256
	// idleTimeout is Go's own default, kept explicitly because setting a
	// transport at all replaces every default it had.
	idleTimeout = 90 * time.Second

	dialTimeout           = 10 * time.Second
	dialKeepAlive         = 30 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	expectContinueTimeout = 1 * time.Second
)

// shared is built once. A transport is a connection pool, so a per-adapter
// transport would be a per-adapter pool and the reuse this package exists for
// would not happen across them.
var shared = sync.OnceValue(func() *http.Client {
	return &http.Client{Transport: Transport()}
})

// Shared returns the process-wide adapter HTTP client.
//
// It carries no Timeout. A single deadline for every provider is the mistake
// that hides a stalled one: a recogniser answering in tens of milliseconds and
// a reasoner answering in tens of seconds cannot share a bound that is
// meaningful for either. Adapters put a deadline on each request from their
// own cadence, which is where the knowledge is.
func Shared() *http.Client {
	return shared()
}

// WithTimeout returns a client that shares the process-wide connection pool
// and carries one whole-request deadline.
//
// It is for the callers that already had a client-level timeout of their own,
// derived from what that particular service is expected to take. Sharing the
// pool is the point: a per-caller client would be a per-caller pool, and the
// reuse this package exists for would stop at the first one.
func WithTimeout(timeout time.Duration) *http.Client {
	return &http.Client{Transport: Shared().Transport, Timeout: timeout}
}

// Transport returns a new transport with the same settings, for a caller that
// needs its own pool - a credential boundary, a proxy, a test that counts
// connections.
func Transport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: newRememberingDialer(&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: dialKeepAlive,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          idleTotal,
		MaxIdleConnsPerHost:   idlePerHost,
		IdleConnTimeout:       idleTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
	}
}
