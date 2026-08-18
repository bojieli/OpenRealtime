package openaitts

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/pcm"
)

func TestAdapterStreamsRawPCMWithOpenAIRequestAndSGLangControls(t *testing.T) {
	source := testPCM(4_410)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/audio/speech" {
			http.NotFound(writer, request)
			return
		}
		if request.Header.Get("Authorization") != "Bearer secret" || request.Header.Get("X-Test") != "present" {
			t.Errorf("unexpected request headers: %+v", request.Header)
		}
		if request.Header.Get("Accept") != "audio/pcm" {
			t.Errorf("Accept = %q", request.Header.Get("Accept"))
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["model"] != "fishaudio/s2-pro" || payload["voice"] != "voice-a" || payload["input"] != "hello" {
			t.Errorf("unexpected identity fields: %+v", payload)
		}
		if payload["response_format"] != "pcm" || payload["stream"] != true || payload["initial_codec_chunk_frames"] != float64(0) {
			t.Errorf("unexpected streaming fields: %+v", payload)
		}
		if payload["temperature"] != 0.7 || payload["language"] != "en" {
			t.Errorf("unexpected controls: %+v", payload)
		}
		references, ok := payload["references"].([]any)
		if !ok || len(references) != 1 {
			t.Fatalf("references = %#v", payload["references"])
		}
		writer.Header().Set("Content-Type", "audio/pcm")
		writer.Header().Set("X-Sample-Rate", "44100")
		for _, fragment := range [][]byte{source[:1], source[1:37], source[37:]} {
			_, _ = writer.Write(fragment)
			if flusher, ok := writer.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
	defer server.Close()

	temperature := 0.7
	initialFrames := 0
	adapter, err := New(Config{
		Endpoint: server.URL + "/v1/audio/speech", Voice: "voice-a", BearerToken: "secret",
		Headers: http.Header{"X-Test": {"present"}}, OutputSampleRateHz: 24_000,
		References:  []Reference{{AudioPath: "file:///voice.wav", Text: "reference"}},
		Temperature: &temperature, InitialChunkFrames: &initialFrames,
		ExtraBody: map[string]json.RawMessage{"language": json.RawMessage(`"en"`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	// New must own copies of mutable caller configuration.
	temperature = 0.1
	initialFrames = 9

	var chunks []v1.SpeechChunk
	err = adapter.Stream(context.Background(), v1.SpeechPlan{CandidateID: "candidate", Text: "hello"}, func(chunk v1.SpeechChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertOpenAIChunks(t, chunks, source, 44_100, 24_000)
	descriptor := adapter.Descriptor()
	descriptor.Capabilities[v1.CapabilityStreamingOutput] = false
	if !adapter.Descriptor().Capabilities.Has(v1.CapabilityStreamingOutput) {
		t.Fatal("Descriptor returned aliased capabilities")
	}
}

func TestAdapterUsesFallbackRateAndCollectsSynthesis(t *testing.T) {
	source := testPCM(800)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/octet-stream")
		_, _ = writer.Write(source)
	}))
	defer server.Close()
	adapter, err := New(Config{Endpoint: server.URL, FallbackSampleRate: 16_000, OutputSampleRateHz: 16_000})
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := adapter.Synthesize(context.Background(), v1.SpeechPlan{CandidateID: "c", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	assertOpenAIChunks(t, chunks, source, 16_000, 16_000)
}

func TestAdapterRejectsBadResponsesAndBoundsAudio(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		mediaType  string
		rate       string
		body       []byte
		maxBytes   int64
		wantPhrase string
	}{
		{name: "status", status: http.StatusServiceUnavailable, body: []byte("unavailable"), wantPhrase: "HTTP 503"},
		{name: "media", status: http.StatusOK, mediaType: "application/json", body: []byte("{}"), wantPhrase: "content type"},
		{name: "rate", status: http.StatusOK, mediaType: "audio/pcm", rate: "fast", body: testPCM(2), wantPhrase: "X-Sample-Rate"},
		{name: "empty", status: http.StatusOK, mediaType: "audio/pcm", rate: "24000", wantPhrase: "no audio"},
		{name: "partial", status: http.StatusOK, mediaType: "audio/pcm", rate: "24000", body: []byte{1}, wantPhrase: "partial PCM16"},
		{name: "bounded", status: http.StatusOK, mediaType: "audio/pcm", rate: "24000", body: testPCM(4), maxBytes: 4, wantPhrase: "exceeds 4 bytes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				if test.mediaType != "" {
					writer.Header().Set("Content-Type", test.mediaType)
				}
				if test.rate != "" {
					writer.Header().Set("X-Sample-Rate", test.rate)
				}
				writer.WriteHeader(test.status)
				_, _ = writer.Write(test.body)
			}))
			defer server.Close()
			adapter, err := New(Config{Endpoint: server.URL, MaxAudioBytes: test.maxBytes})
			if err != nil {
				t.Fatal(err)
			}
			err = adapter.Stream(context.Background(), v1.SpeechPlan{CandidateID: "c", Text: "hello"}, func(v1.SpeechChunk) error { return nil })
			if err == nil || !contains(err.Error(), test.wantPhrase) {
				t.Fatalf("error = %v; want phrase %q", err, test.wantPhrase)
			}
		})
	}
}

func TestAdapterPropagatesConsumerErrorAndCancellation(t *testing.T) {
	consumerServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "audio/pcm")
		_, _ = writer.Write(testPCM(20))
	}))
	defer consumerServer.Close()
	adapter, _ := New(Config{Endpoint: consumerServer.URL})
	want := errors.New("stop")
	err := adapter.Stream(context.Background(), v1.SpeechPlan{CandidateID: "c", Text: "hello"}, func(v1.SpeechChunk) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("consumer error = %v", err)
	}

	cancelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "audio/pcm")
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		<-request.Context().Done()
	}))
	defer cancelServer.Close()
	cancelAdapter, _ := New(Config{Endpoint: cancelServer.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err = cancelAdapter.Stream(ctx, v1.SpeechPlan{CandidateID: "c", Text: "hello"}, func(v1.SpeechChunk) error { return nil })
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestAdapterRejectsInvalidConfigurationAndPlans(t *testing.T) {
	invalid := []Config{
		{Endpoint: "://bad"},
		{Endpoint: "ftp://example.test/speech"},
		{FallbackSampleRate: maximumSampleRate + 1},
		{ReadBufferBytes: 1},
		{References: []Reference{{AudioPath: "voice.wav"}}},
		{ExtraBody: map[string]json.RawMessage{"model": json.RawMessage(`"other"`)}},
		{ExtraBody: map[string]json.RawMessage{"future": json.RawMessage(`{`)}},
	}
	for index, config := range invalid {
		if _, err := New(config); err == nil {
			t.Fatalf("invalid config %d accepted", index)
		}
	}
	adapter, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Stream(context.Background(), v1.SpeechPlan{CandidateID: "c", Text: "hello"}, nil); !errors.Is(err, v1.ErrNilConsumer) {
		t.Fatalf("nil consumer error = %v", err)
	}
	if err := adapter.Stream(context.Background(), v1.SpeechPlan{}, func(v1.SpeechChunk) error { return nil }); err == nil {
		t.Fatal("empty plan accepted")
	}
}

func assertOpenAIChunks(t *testing.T, chunks []v1.SpeechChunk, source []byte, sourceRate, outputRate uint32) {
	t.Helper()
	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}
	var got []byte
	var offset uint64
	for index, chunk := range chunks {
		if chunk.SampleOffset != offset || chunk.SampleRateHz != outputRate {
			t.Fatalf("chunk %d metadata: %+v", index, chunk)
		}
		if chunk.Final != (index == len(chunks)-1) {
			t.Fatalf("chunk %d final = %v", index, chunk.Final)
		}
		got = append(got, chunk.PCM16LE...)
		offset += uint64(len(chunk.PCM16LE) / 2)
	}
	resampler, err := pcm.NewResampler(sourceRate, outputRate)
	if err != nil {
		t.Fatal(err)
	}
	want, err := resampler.Push(source)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := resampler.Finalize()
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, terminal...)
	if !slices.Equal(got, want) {
		t.Fatalf("output length = %d, want %d", len(got), len(want))
	}
}

func testPCM(samples int) []byte {
	output := make([]byte, samples*2)
	for index := 0; index < samples; index++ {
		binary.LittleEndian.PutUint16(output[index*2:], uint16(int16(index%20_000-10_000)))
	}
	return output
}

func contains(value, phrase string) bool {
	for index := 0; index+len(phrase) <= len(value); index++ {
		if value[index:index+len(phrase)] == phrase {
			return true
		}
	}
	return phrase == ""
}
