package httpclient

import (
	"context"
	"errors"
	"net"
	"testing"
)

// A resolver that stops answering does not stop a dial to a host that
// answered a moment ago; a host that never answered still fails.
func TestRememberingDialerFallsBackToTheLastAnswer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			conn.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	failing := false
	dialer := newRememberingDialer(&net.Dialer{})
	dialer.resolve = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		if failing {
			return nil, errors.New("i/o timeout")
		}
		if host == "voice.example" {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		}
		return nil, errors.New("no such host")
	}
	conn, err := dialer.DialContext(context.Background(), "tcp", "voice.example:"+port)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	conn.Close()
	failing = true
	conn, err = dialer.DialContext(context.Background(), "tcp", "voice.example:"+port)
	if err != nil {
		t.Fatalf("dial while the resolver fails: %v", err)
	}
	conn.Close()
	if _, err := dialer.DialContext(context.Background(), "tcp", "unknown.example:"+port); err == nil {
		t.Fatal("a host that never resolved must not dial")
	}
	if conn, err := dialer.DialContext(context.Background(), "tcp", "127.0.0.1:"+port); err != nil {
		t.Fatalf("a literal address must dial without a lookup: %v", err)
	} else {
		conn.Close()
	}
}
