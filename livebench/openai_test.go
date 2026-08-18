package livebench

import (
	"context"
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

func TestOpenAIAdapterRequiresTerminalResponseWhenRequested(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		if _, _, err := connection.Read(request.Context()); err != nil {
			return
		}
		_ = connection.Write(request.Context(), websocket.MessageText, []byte(`{"type":"session.updated"}`))
		for {
			if _, _, err := connection.Read(request.Context()); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	adapter, err := NewOpenAIAdapter(OpenAIConfig{
		APIKey: "test-secret", Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"),
		ChunkDuration: 20 * time.Millisecond, TailDuration: 40 * time.Millisecond,
		AwaitTerminalResponse: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Run(t.Context(), Audio{SampleRateHz: 24_000, PCM16: make([]byte, 960)})
	if err == nil || !strings.Contains(err.Error(), "waiting for a terminal Realtime response") {
		t.Fatalf("expected terminal-response timeout, got %v", err)
	}
}

func TestOpenAIAdapterSupportsTruthfulCompatibleEndpointDescriptor(t *testing.T) {
	t.Parallel()
	adapter, err := NewOpenAIAdapter(OpenAIConfig{
		APIKey: "local-only", Endpoint: "ws://127.0.0.1:8765/v1/realtime",
		Model: "openrealtime-local", Provider: "openrealtime",
		Architecture: "canonical-local-asr-fast-slow-tts",
		Profile:      "fdb-v1.5-openai-realtime-adapter-i1-qg-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := adapter.Descriptor()
	if descriptor.Provider != "openrealtime" || descriptor.Model != "openrealtime-local" ||
		descriptor.Transport != "websocket-openai-realtime" ||
		descriptor.Architecture != "canonical-local-asr-fast-slow-tts" ||
		descriptor.Profile != "fdb-v1.5-openai-realtime-adapter-i1-qg-v1" {
		t.Fatalf("unexpected compatible-endpoint descriptor: %+v", descriptor)
	}
}

func TestOpenAIAdapterExecutesToolsAndResumesTrajectory(t *testing.T) {
	t.Parallel()
	serverErrors := make(chan error, 4)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
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
		if !strings.Contains(string(update), `"name":"track_order"`) || !strings.Contains(string(update), `"tool_choice":"auto"`) {
			serverErrors <- &testError{"tool-aware session.update", string(update)}
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
			toolResponse := []byte(`{"type":"response.done","event_id":"evt_tool","response":{"id":"resp_tool","output":[{"type":"function_call","call_id":"call_1","name":"track_order","arguments":"{\"order_id\":\"ABC123\"}"}]}}`)
			if err := connection.Write(request.Context(), websocket.MessageText, toolResponse); err != nil {
				serverErrors <- err
				return
			}
			_, result, err := connection.Read(request.Context())
			if err != nil {
				serverErrors <- err
				return
			}
			if !strings.Contains(string(result), `"type":"function_call_output"`) || !strings.Contains(string(result), `shipping_status`) || !strings.Contains(string(result), `Out for delivery`) {
				serverErrors <- &testError{"function_call_output", string(result)}
			}
			_, resume, err := connection.Read(request.Context())
			if err != nil {
				serverErrors <- err
				return
			}
			if !strings.Contains(string(resume), `"type":"response.create"`) {
				serverErrors <- &testError{"response.create", string(resume)}
			}
			finalEvents := [][]byte{
				[]byte(`{"type":"input_audio_buffer.speech_stopped","event_id":"evt_stop","audio_end_ms":321}`),
				[]byte(`{"type":"conversation.item.input_audio_transcription.completed","event_id":"evt_input","transcript":"track ABC123"}`),
				[]byte(`{"type":"response.output_audio_transcript.delta","event_id":"evt_text","delta":"It is out for delivery."}`),
				[]byte(`{"type":"response.done","event_id":"evt_final","response":{"id":"resp_final","output":[]}}`),
			}
			for _, event := range finalEvents {
				if err := connection.Write(request.Context(), websocket.MessageText, event); err != nil {
					serverErrors <- err
					return
				}
			}
			for {
				if _, _, err := connection.Read(request.Context()); err != nil {
					return
				}
			}
		}
	}))
	defer server.Close()

	adapter, err := NewOpenAIAdapter(OpenAIConfig{
		APIKey: "test-secret", Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"),
		ChunkDuration: 20 * time.Millisecond, TailDuration: 100 * time.Millisecond,
		AwaitTerminalResponse: true,
		Tools: []RealtimeTool{{
			Name: "track_order", Description: "Track an order.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"order_id":{"type":"string"}},"required":["order_id"]}`),
		}},
		ExecuteTool: func(_ context.Context, name string, arguments json.RawMessage) (json.RawMessage, error) {
			if name != "track_order" || string(arguments) != `{"order_id":"ABC123"}` {
				return nil, &testError{"track_order arguments", name + " " + string(arguments)}
			}
			return json.RawMessage(`{"status":"success","shipping_status":"Out for delivery"}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Run(t.Context(), Audio{SampleRateHz: 24_000, PCM16: make([]byte, 960)})
	if err != nil {
		t.Fatal(err)
	}
	if result.OutputTranscript != "It is out for delivery." || len(result.ToolCalls) != 1 || result.ToolCalls[0].CallID != "call_1" {
		t.Fatalf("unexpected resumed result: %+v", result)
	}
	if len(result.InputTranscripts) != 1 || result.InputTranscripts[0] != "track ABC123" || result.UserSpeechEndMS == nil || *result.UserSpeechEndMS != 321 {
		t.Fatalf("unexpected input evidence: %+v", result)
	}
	select {
	case serverErr := <-serverErrors:
		t.Fatal(serverErr)
	default:
	}
}
