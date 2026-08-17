package livebench

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestOpenAIGARealtimeProfile(t *testing.T) {
	t.Parallel()
	serverErrors := make(chan error, 4)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer test-secret" {
			serverErrors <- &testError{"bearer credential", got}
		}
		if got := request.URL.Query().Get("model"); got != DefaultGPT4oRealtimeModel {
			serverErrors <- &testError{DefaultGPT4oRealtimeModel, got}
		}
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		defer connection.CloseNow()

		_, update, err := connection.Read(request.Context())
		if err != nil {
			serverErrors <- err
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(update, &payload); err != nil {
			serverErrors <- err
			return
		}
		if payload["type"] != "session.update" || !strings.Contains(string(update), `"type":"realtime"`) || !strings.Contains(string(update), `"output_modalities":["audio"]`) {
			serverErrors <- &testError{"GA session.update", string(update)}
		}
		if err := connection.Write(request.Context(), websocket.MessageText, []byte(`{"type":"session.updated","event_id":"evt_setup"}`)); err != nil {
			serverErrors <- err
			return
		}
		for {
			_, message, readErr := connection.Read(request.Context())
			if readErr != nil {
				return
			}
			if !strings.Contains(string(message), "input_audio_buffer.append") {
				continue
			}
			audio := base64.StdEncoding.EncodeToString(make([]byte, 960))
			events := []any{
				map[string]any{"type": "response.output_audio.delta", "event_id": "evt_audio", "delta": audio},
				map[string]any{"type": "input_audio_buffer.speech_started", "event_id": "evt_speech"},
				map[string]any{"type": "response.output_audio_transcript.delta", "event_id": "evt_text", "delta": "hello"},
				map[string]any{"type": "response.done", "event_id": "evt_done", "response": map[string]any{"usage": map[string]any{
					"input_tokens": 4, "output_tokens": 2,
					"input_token_details":  map[string]any{"audio_tokens": 3},
					"output_token_details": map[string]any{"audio_tokens": 1},
				}}},
			}
			for _, event := range events {
				encoded, _ := json.Marshal(event)
				if writeErr := connection.Write(request.Context(), websocket.MessageText, encoded); writeErr != nil {
					serverErrors <- writeErr
					return
				}
			}
			for {
				if _, _, readErr = connection.Read(request.Context()); readErr != nil {
					return
				}
			}
		}
	}))
	defer server.Close()

	adapter, err := NewOpenAIAdapter(OpenAIConfig{
		APIKey: "test-secret", Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"),
		ChunkDuration: 20 * time.Millisecond, TailDuration: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Run(t.Context(), Audio{SampleRateHz: 24_000, PCM16: make([]byte, 960)})
	if err != nil {
		t.Fatal(err)
	}
	if result.OutputTranscript != "hello" || len(result.Chunks) != 2 || !result.Chunks[1].Flush {
		t.Fatalf("unexpected output: %+v", result)
	}
	if result.Usage.InputTokens != 4 || result.Usage.OutputAudioTokens != 1 {
		t.Fatalf("unexpected usage: %+v", result.Usage)
	}
	select {
	case serverErr := <-serverErrors:
		t.Fatal(serverErr)
	default:
	}
}
