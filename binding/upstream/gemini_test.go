package upstream_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/upstream"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/providers"
	"github.com/coder/websocket"
)

// fakeGemini is a Live endpoint. It speaks BidiGenerateContent, not the
// Realtime protocol, which is the point: this test drives the whole binding
// against a wire format it does not understand and would fail on without the
// translator.
type fakeGemini struct {
	server *httptest.Server

	mu       sync.Mutex
	received []map[string]json.RawMessage

	send  chan map[string]any
	ready chan struct{}
	once  sync.Once
}

func newFakeGemini(t *testing.T) *fakeGemini {
	fake := &fakeGemini{send: make(chan map[string]any, 32), ready: make(chan struct{})}
	fake.server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			connection, err := websocket.Accept(writer, request, nil)
			if err != nil {
				return
			}
			defer connection.CloseNow()
			ctx := request.Context()
			fake.once.Do(func() { close(fake.ready) })
			go func() {
				for {
					_, payload, err := connection.Read(ctx)
					if err != nil {
						return
					}
					var decoded map[string]json.RawMessage
					if json.Unmarshal(payload, &decoded) == nil {
						fake.mu.Lock()
						fake.received = append(fake.received, decoded)
						fake.mu.Unlock()
					}
				}
			}()
			for {
				select {
				case <-ctx.Done():
					return
				case message := <-fake.send:
					encoded, _ := json.Marshal(message)
					if connection.Write(ctx, websocket.MessageText, encoded) != nil {
						return
					}
				}
			}
		}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *fakeGemini) url() string {
	return "ws" + strings.TrimPrefix(fake.server.URL, "http")
}

func (fake *fakeGemini) emit(message map[string]any) { fake.send <- message }

func (fake *fakeGemini) sent() []map[string]json.RawMessage {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]map[string]json.RawMessage(nil), fake.received...)
}

func (fake *fakeGemini) has(key string) bool {
	for _, message := range fake.sent() {
		if _, present := message[key]; present {
			return true
		}
	}
	return false
}

// The whole binding over an endpoint that does not speak the Realtime protocol:
// Gemini hears the user, the engine's reasoner works over the same
// conversation, and the answer goes back as a Live turn for Gemini to say.
func TestTheBindingRunsOverGeminiLiveThroughTheCatalogue(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "test-key")
	fake := newFakeGemini(t)
	settings, err := providers.ResolveUpstream(providers.UpstreamRequest{
		Provider: "gemini", URL: fake.url(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Dial == nil {
		t.Fatal("Gemini does not speak the Realtime protocol and needs a translator")
	}
	slow := &scriptedSlow{turns: [][]continuation.Event{{
		{Kind: continuation.EventAssistantDelta, Text: "The balance is $40.00."},
	}}}
	bind, err := upstream.New(upstream.Config{
		URL: settings.URL, Token: settings.Token, Model: settings.Model,
		Handoff: settings.Handoff, Dial: settings.Dial, Slow: slow,
	})
	if err != nil {
		t.Fatal(err)
	}
	sink := &collectingSink{}
	runtime, err := bind.Start(context.Background(), binding.Options{
		Sink: sink, SessionID: "gemini",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background(), nil) })
	<-fake.ready

	// The binding's session declaration must have become the handshake.
	waitFor(t, func() bool { return fake.has("setup") }, "the handshake never happened")

	// Gemini reports what it heard, incrementally, and ends the turn.
	fake.emit(map[string]any{"serverContent": map[string]any{
		"inputTranscription": map[string]any{"text": "what is my balance"},
	}})
	fake.emit(map[string]any{"serverContent": map[string]any{"turnComplete": true}})

	// The reasoner's answer has to come back as a completed Live turn.
	waitFor(t, func() bool {
		for _, message := range fake.sent() {
			raw, present := message["clientContent"]
			if !present {
				continue
			}
			if strings.Contains(string(raw), "The balance is $40.00.") &&
				strings.Contains(string(raw), `"turnComplete":true`) {
				return true
			}
		}
		return false
	}, "the reasoner's answer never reached Gemini as a client turn")

	sink.mu.Lock()
	transcripts := len(sink.transcripts)
	sink.mu.Unlock()
	if transcripts == 0 {
		t.Fatal("the client must see what Gemini heard")
	}
}

// The probe is what raises a verification level, so it has to work. Driving it
// against the Gemini translator covers both halves at once: a foreign protocol
// and the probe's own turn logic.
func TestTheProbeCompletesATurnAgainstATranslatedEndpoint(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "test-key")
	fake := newFakeGemini(t)
	go func() {
		<-fake.ready
		// Answer the probe the way Gemini does: a transcript, then the turn
		// ends.
		fake.emit(map[string]any{"serverContent": map[string]any{
			"outputTranscription": map[string]any{"text": "probe ok."},
		}})
		fake.emit(map[string]any{"serverContent": map[string]any{"turnComplete": true}})
	}()

	result := providers.ProbeUpstream(context.Background(), providers.UpstreamRequest{
		Provider: "gemini", URL: fake.url(),
	}, 10*time.Second)

	if !result.Connected {
		t.Fatalf("the probe did not connect: %s", result.Failure)
	}
	if result.Failure != "" {
		t.Fatalf("probe failed: %s", result.Failure)
	}
	if result.Spoken != "probe ok." {
		t.Errorf("the probe must report what the endpoint said, got %q", result.Spoken)
	}
	if result.Events["response.done"] != 1 {
		t.Errorf("the probe must record the events it saw: %+v", result.Events)
	}
}

// A refused endpoint has to be reported as a failure rather than a silent
// success, or the probe would raise a verification level it never earned.
func TestTheProbeReportsARefusal(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "test-key")
	fake := newFakeGemini(t)
	go func() {
		<-fake.ready
		fake.emit(map[string]any{"error": map[string]any{
			"status": "PERMISSION_DENIED", "message": "no access to this model",
		}})
	}()

	result := providers.ProbeUpstream(context.Background(), providers.UpstreamRequest{
		Provider: "gemini", URL: fake.url(),
	}, 10*time.Second)

	if !result.Connected {
		t.Fatal("the socket did open, and that is worth distinguishing from a refusal")
	}
	if !strings.Contains(result.Failure, "no access to this model") {
		t.Fatalf("a refusal must be reported: %q", result.Failure)
	}
}
