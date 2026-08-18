package qwenasr

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

func TestAdapterStreamsRevisionsAndFinalizes(t *testing.T) {
	var mu sync.Mutex
	var chunks [][]byte
	chunkCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/start":
			if request.Header.Get("Authorization") != "Bearer secret" {
				t.Errorf("authorization = %q", request.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(writer).Encode(map[string]string{"session_id": "session/one"})
		case "/api/chunk":
			if request.URL.Query().Get("session_id") != "session/one" {
				t.Errorf("session ID = %q", request.URL.Query().Get("session_id"))
			}
			body, _ := io.ReadAll(request.Body)
			mu.Lock()
			chunks = append(chunks, body)
			chunkCalls++
			call := chunkCalls
			mu.Unlock()
			text := "hello"
			if call > 1 {
				text = "hello world"
			}
			_ = json.NewEncoder(writer).Encode(map[string]string{"language": "English", "text": text})
		case "/api/finish":
			_ = json.NewEncoder(writer).Encode(map[string]string{"language": "English", "text": "hello world"})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	adapter, err := New(Config{BaseURL: server.URL, BearerToken: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	first := pcm16(4_800)
	revisions, err := adapter.PushFrame(context.Background(), v1.AudioFrame{
		Index: 0, SampleOffset: 0, SampleRateHz: 24_000, PCM16LE: first,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 1 || revisions[0].UnstableText != "hello" || revisions[0].StableText != "" || revisions[0].Delta != "hello" {
		t.Fatalf("unexpected first revisions: %+v", revisions)
	}
	second := pcm16(4_800)
	revisions, err = adapter.PushFrame(context.Background(), v1.AudioFrame{
		Index: 1, SampleOffset: 4_800, SampleRateHz: 24_000, PCM16LE: second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 1 || revisions[0].UnstableText != "hello world" || revisions[0].Delta != " world" {
		t.Fatalf("unexpected second revisions: %+v", revisions)
	}
	final, err := adapter.Finalize(context.Background(), 9_600)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Final || final.StableText != "hello world" || final.UnstableText != "" || final.Delta != "" {
		t.Fatalf("unexpected final revision: %+v", final)
	}
	if adapter.Language() != "English" {
		t.Fatalf("language = %q", adapter.Language())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(chunks) != 2 {
		t.Fatalf("received %d chunks, want 2", len(chunks))
	}
	for _, chunk := range chunks {
		if len(chunk) != 3_200*4 {
			t.Fatalf("chunk length = %d, want %d", len(chunk), 3_200*4)
		}
		if got := math.Float32frombits(binary.LittleEndian.Uint32(chunk[:4])); got != 0 {
			t.Fatalf("first float = %v, want 0", got)
		}
	}
}

func TestAdapterSuppressesUnchangedPartialAndProtectsDescriptor(t *testing.T) {
	server := transcriptServer(t, "same")
	defer server.Close()
	adapter, err := New(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := adapter.Descriptor()
	descriptor.Capabilities[v1.CapabilityRevisions] = false
	if !adapter.Descriptor().Capabilities.Has(v1.CapabilityRevisions) {
		t.Fatal("descriptor capabilities were mutated by caller")
	}
	frame := pcm16(3_200)
	first, err := adapter.PushFrame(context.Background(), v1.AudioFrame{Index: 0, SampleRateHz: 16_000, PCM16LE: frame})
	if err != nil || len(first) != 1 {
		t.Fatalf("first = %+v, err = %v", first, err)
	}
	second, err := adapter.PushFrame(context.Background(), v1.AudioFrame{Index: 1, SampleOffset: 3_200, SampleRateHz: 16_000, PCM16LE: frame})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("unchanged revision was emitted: %+v", second)
	}
}

func TestAdapterRejectsDiscontinuityAndPoisonsIndeterminateSession(t *testing.T) {
	chunkCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/start":
			_, _ = io.WriteString(writer, `{"session_id":"s"}`)
		case "/api/chunk":
			chunkCalls++
			http.Error(writer, "failed", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	adapter, err := New(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	frame := v1.AudioFrame{Index: 0, SampleRateHz: 16_000, PCM16LE: pcm16(3_200)}
	if _, err := adapter.PushFrame(context.Background(), frame); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("first error = %v", err)
	}
	if _, err := adapter.PushFrame(context.Background(), frame); err == nil || !strings.Contains(err.Error(), "unusable") {
		t.Fatalf("second error = %v", err)
	}
	if chunkCalls != 1 {
		t.Fatalf("chunk calls = %d, want 1", chunkCalls)
	}
}

func TestPCM16ToFloat32(t *testing.T) {
	input := []byte{0x00, 0x80, 0x00, 0x00, 0xff, 0x7f}
	encoded := pcm16ToFloat32(input)
	got := []float32{
		math.Float32frombits(binary.LittleEndian.Uint32(encoded[0:4])),
		math.Float32frombits(binary.LittleEndian.Uint32(encoded[4:8])),
		math.Float32frombits(binary.LittleEndian.Uint32(encoded[8:12])),
	}
	want := []float32{-1, 0, float32(32767) / 32768}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func transcriptServer(t *testing.T, text string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/start":
			_ = json.NewEncoder(writer).Encode(map[string]string{"session_id": "s"})
		case "/api/chunk", "/api/finish":
			_ = json.NewEncoder(writer).Encode(map[string]string{"language": "English", "text": text})
		default:
			http.NotFound(writer, request)
		}
	}))
}

func pcm16(samples int) []byte { return make([]byte, samples*2) }
