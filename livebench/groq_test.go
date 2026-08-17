package livebench

import (
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestGroqCascade(t *testing.T) {
	t.Parallel()
	outputWAV, err := EncodeWAV(Audio{SampleRateHz: 24_000, PCM16: make([]byte, 2_400*2)})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		requests = append(requests, request.URL.Path)
		mu.Unlock()
		switch request.URL.Path {
		case "/audio/transcriptions":
			writer.Header().Set("Content-Type", "application/json")
			fmt.Fprint(writer, `{"text":"What time is it?"}`)
		case "/chat/completions":
			writer.Header().Set("Content-Type", "application/json")
			fmt.Fprint(writer, `{"choices":[{"message":{"role":"assistant","content":"It is noon."}}],"usage":{"prompt_tokens":10,"completion_tokens":4}}`)
		case "/audio/speech":
			writer.Header().Set("Content-Type", "audio/wav")
			_, _ = writer.Write(outputWAV)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	adapter, err := NewGroqAdapter(GroqConfig{
		APIKey: "test", Endpoint: server.URL, TailDuration: 10 * time.Millisecond,
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	input := Audio{SampleRateHz: 16_000, PCM16: make([]byte, 8_000*2)}
	for sample := 0; sample < 3_200; sample++ {
		binary.LittleEndian.PutUint16(input.PCM16[sample*2:], uint16(int16(10_000)))
	}
	result, err := adapter.Run(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.OutputTranscript != "It is noon." || len(result.Chunks) != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.Usage.InputTokens != 10 || result.Usage.OutputTokens != 4 {
		t.Fatalf("unexpected usage: %+v", result.Usage)
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := fmt.Sprint(requests), "[/audio/transcriptions /chat/completions /audio/speech]"; got != want {
		t.Fatalf("requests=%s, want %s", got, want)
	}
}
