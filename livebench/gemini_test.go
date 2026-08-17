package livebench

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGeminiFDBProfileReconnectsWithoutCredentialInURL(t *testing.T) {
	t.Parallel()
	var connections atomic.Int32
	var setupMu sync.Mutex
	var setups [][]byte
	serverErrors := make(chan error, 4)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connectionNumber := connections.Add(1)
		if got := request.Header.Get("x-goog-api-key"); got != "test-secret" {
			serverErrors <- &testError{"x-goog-api-key", got}
		}
		if strings.Contains(request.URL.RawQuery, "key=") {
			serverErrors <- &testError{"credential-free query", request.URL.RawQuery}
		}
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		defer connection.CloseNow()

		_, setup, err := connection.Read(request.Context())
		if err != nil {
			serverErrors <- err
			return
		}
		setupMu.Lock()
		setups = append(setups, append([]byte(nil), setup...))
		setupMu.Unlock()
		if err := connection.Write(request.Context(), websocket.MessageBinary, []byte(`{"setupComplete":{}}`)); err != nil {
			serverErrors <- err
			return
		}

		if connectionNumber == 1 {
			for {
				_, message, readErr := connection.Read(request.Context())
				if readErr != nil {
					return
				}
				if strings.Contains(string(message), `"audio"`) {
					payload := []byte(`{"serverContent":{"interrupted":true}}`)
					if writeErr := connection.Write(request.Context(), websocket.MessageBinary, payload); writeErr != nil {
						serverErrors <- writeErr
						return
					}
					for {
						if _, _, readErr = connection.Read(request.Context()); readErr != nil {
							return
						}
					}
				}
			}
		}

		for {
			_, message, readErr := connection.Read(request.Context())
			if readErr != nil {
				return
			}
			if !strings.Contains(string(message), `"audioStreamEnd"`) {
				continue
			}
			audio := base64.StdEncoding.EncodeToString(make([]byte, 960))
			payload, _ := json.Marshal(map[string]any{"serverContent": map[string]any{
				"turnComplete": true,
				"modelTurn": map[string]any{"parts": []any{map[string]any{"inlineData": map[string]any{
					"data": audio, "mimeType": "audio/pcm;rate=24000",
				}}}},
			}})
			if writeErr := connection.Write(request.Context(), websocket.MessageText, payload); writeErr != nil {
				serverErrors <- writeErr
				return
			}
			for {
				if _, _, readErr = connection.Read(request.Context()); readErr != nil {
					return
				}
			}
		}
	}))
	defer server.Close()

	adapter, err := NewGeminiAdapter(GeminiConfig{
		APIKey: "test-secret", Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"),
		TailDuration: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := Audio{SampleRateHz: 16_000, PCM16: make([]byte, 3*1_024*2)}
	result, err := adapter.Run(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.ConnectionCount != 2 || connections.Load() != 2 {
		t.Fatalf("connections: result=%d server=%d, want 2", result.ConnectionCount, connections.Load())
	}
	if result.Descriptor.Profile != "fdb-v1.5-minimal-multisession-v1" {
		t.Fatalf("profile=%q", result.Descriptor.Profile)
	}
	if len(result.Chunks) != 2 || !result.Chunks[0].Flush || result.Chunks[1].Flush {
		t.Fatalf("unexpected chunks: %+v", result.Chunks)
	}
	setupMu.Lock()
	defer setupMu.Unlock()
	if len(setups) != 2 {
		t.Fatalf("setups=%d, want 2", len(setups))
	}
	for _, setup := range setups {
		text := string(setup)
		if strings.Contains(text, "systemInstruction") || strings.Contains(text, "AudioTranscription") {
			t.Fatalf("non-minimal Gemini setup: %s", text)
		}
		if !strings.Contains(text, `"thinkingLevel":"minimal"`) {
			t.Fatalf("thinking level missing from setup: %s", text)
		}
	}
	select {
	case serverErr := <-serverErrors:
		t.Fatal(serverErr)
	default:
	}
}

type testError struct {
	want string
	got  string
}

func (err *testError) Error() string { return err.want + ": got " + err.got }

func TestAggregateGeminiUsageUsesLastUpdatePerSession(t *testing.T) {
	t.Parallel()
	events := []WireEvent{
		{Direction: "client", Type: "setup"},
		{Payload: json.RawMessage(`{"usageMetadata":{"promptTokenCount":2,"responseTokenCount":3,"promptTokensDetails":[{"modality":"AUDIO","tokenCount":1}],"responseTokensDetails":[{"modality":"AUDIO","tokenCount":3}]}}`)},
		{Payload: json.RawMessage(`{"usageMetadata":{"promptTokenCount":5,"responseTokenCount":7,"promptTokensDetails":[{"modality":"AUDIO","tokenCount":4}],"responseTokensDetails":[{"modality":"AUDIO","tokenCount":7}]}}`)},
		{Direction: "client", Type: "setup"},
		{Payload: json.RawMessage(`{"usageMetadata":{"promptTokenCount":11,"responseTokenCount":13,"promptTokensDetails":[{"modality":"AUDIO","tokenCount":10}],"responseTokensDetails":[{"modality":"AUDIO","tokenCount":13}]}}`)},
	}
	usage := aggregateGeminiUsage(events, 2)
	if usage.InputTokens != 16 || usage.OutputTokens != 20 || usage.InputAudioTokens != 14 || usage.OutputAudioTokens != 20 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
	if !usage.Complete || usage.SessionsObserved != 2 {
		t.Fatalf("usage completeness: %+v", usage)
	}
}
