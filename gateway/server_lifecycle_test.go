package gateway_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/gateway"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	"github.com/coder/websocket"
)

func TestServerCloseDrainsActiveSessionsAndStopsAdmission(t *testing.T) {
	legacy, err := cascade.New(cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) {
			return staticASR{text: "lifecycle"}, nil
		},
		Fast: fast(), Slow: slow(), Speech: toneSpeech{},
		Voice: "test-voice", FastMaxTokens: 512,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := graphbinding.New(legacy)
	if err != nil {
		t.Fatal(err)
	}
	server, err := gateway.New(gateway.Config{
		Binding: binding, Model: "openrealtime-test", ValidateWire: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(testGatewayHandler(server))
	t.Cleanup(httpServer.Close)
	endpoint := "ws" + strings.TrimPrefix(httpServer.URL, "http") +
		"/v1/realtime?model=openrealtime-test"
	connection, _, err := websocket.Dial(context.Background(), endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.CloseNow() })
	readContext, cancelRead := context.WithTimeout(context.Background(), 2*time.Second)
	if _, _, err := connection.Read(readContext); err != nil {
		cancelRead()
		t.Fatalf("read initial session event: %v", err)
	}
	cancelRead()

	closeContext, cancelClose := context.WithTimeout(context.Background(), 2*time.Second)
	if err := server.Close(closeContext); err != nil {
		cancelClose()
		t.Fatal(err)
	}
	cancelClose()
	readContext, cancelRead = context.WithTimeout(context.Background(), 2*time.Second)
	for {
		if _, _, err := connection.Read(readContext); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				cancelRead()
				t.Fatal("gateway close left its admitted session active")
			}
			break
		}
	}
	cancelRead()

	replacement, response, err := websocket.Dial(context.Background(), endpoint, nil)
	if replacement != nil {
		_ = replacement.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("post-close admission = connection %v response %v error %v",
			replacement, response, err)
	}
	if err := server.Close(context.Background()); err != nil {
		t.Fatalf("idempotent gateway close: %v", err)
	}
}
