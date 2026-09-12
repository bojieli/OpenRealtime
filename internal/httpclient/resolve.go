package httpclient

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"
)

// lookupTimeout bounds one name resolution. The system resolver's own
// timeout is ten seconds and it is on the path a person is waiting on:
// measured, a lookup of the voice model's host timed out for ten seconds
// while a turn waited, and the turn was lost, with the host's address
// unchanged from the call a second earlier.
const lookupTimeout = 2 * time.Second

// rememberingDialer resolves a host and remembers the answer, so that a
// resolver that fails to answer does not fail a dial to a host that answered
// a moment ago. Addresses are refreshed on every successful lookup; the
// remembered ones are used only when a lookup fails, never in preference to
// a fresh answer.
type rememberingDialer struct {
	dialer  *net.Dialer
	resolve func(ctx context.Context, host string) ([]net.IPAddr, error)

	mu         sync.Mutex
	remembered map[string][]net.IPAddr
}

func newRememberingDialer(dialer *net.Dialer) *rememberingDialer {
	resolver := net.DefaultResolver
	return &rememberingDialer{
		dialer:     dialer,
		resolve:    resolver.LookupIPAddr,
		remembered: map[string][]net.IPAddr{},
	}
}

// DialContext dials host:port, resolving the host with a bounded lookup and
// falling back to the addresses the host last resolved to.
func (remembering *rememberingDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || net.ParseIP(host) != nil {
		return remembering.dialer.DialContext(ctx, network, address)
	}
	addresses, err := remembering.lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	var last error
	for _, candidate := range addresses {
		conn, dialErr := remembering.dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		last = dialErr
		if ctx.Err() != nil {
			break
		}
	}
	if last == nil {
		last = errors.New("no address to dial")
	}
	return nil, last
}

func (remembering *rememberingDialer) lookup(ctx context.Context, host string) ([]net.IPAddr, error) {
	bounded, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	addresses, err := remembering.resolve(bounded, host)
	key := strings.ToLower(host)
	if err == nil && len(addresses) > 0 {
		remembering.mu.Lock()
		remembering.remembered[key] = append([]net.IPAddr(nil), addresses...)
		remembering.mu.Unlock()
		return addresses, nil
	}
	remembering.mu.Lock()
	known := remembering.remembered[key]
	remembering.mu.Unlock()
	if len(known) > 0 {
		return known, nil
	}
	if err == nil {
		err = errors.New("no addresses")
	}
	return nil, &net.DNSError{Err: err.Error(), Name: host, IsTimeout: errors.Is(err, context.DeadlineExceeded)}
}
