package fishaudio

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/pcm"
)

func TestAdapterStreamsRawFishPCMAndRequestControls(t *testing.T) {
	source := samplePCM(4_410)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/tts" {
			http.NotFound(writer, request)
			return
		}
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["text"] != "hello" || payload["streaming"] != true || payload["format"] != "wav" ||
			payload["reference_id"] != "default" {
			t.Errorf("unexpected request: %+v", payload)
		}
		if _, exists := payload["model"]; exists {
			t.Error("native Fish request must not contain a model field")
		}
		writer.Header().Set("Content-Type", "audio/wav")
		for _, fragment := range [][]byte{source[:1], source[1:37], source[37:]} {
			_, _ = writer.Write(fragment)
			if flusher, ok := writer.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
	defer server.Close()

	adapter, err := New(Config{
		Endpoint: server.URL + "/v1/tts", BearerToken: "secret", ReferenceID: "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	var chunks []v1.SpeechChunk
	err = adapter.Stream(context.Background(), v1.SpeechPlan{CandidateID: "candidate", Text: "hello"}, func(chunk v1.SpeechChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertChunks(t, chunks, source, 44_100, 24_000)
}

func TestAdapterAcceptsStreamingWAVHeader(t *testing.T) {
	source := samplePCM(1_600)
	header := wavHeader(16_000)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		for _, fragment := range [][]byte{header[:7], header[7:41], append(header[41:], source...)} {
			_, _ = writer.Write(fragment)
			if flusher, ok := writer.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
	defer server.Close()
	adapter, err := New(Config{Endpoint: server.URL, ServerSampleRateHz: 44_100, OutputSampleRateHz: 16_000})
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := adapter.Synthesize(context.Background(), v1.SpeechPlan{CandidateID: "c", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	assertChunks(t, chunks, source, 16_000, 16_000)
}

func TestAdapterPropagatesConsumerAndHTTPFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("fail") == "yes" {
			http.Error(writer, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = writer.Write(samplePCM(100))
	}))
	defer server.Close()
	adapter, _ := New(Config{Endpoint: server.URL})
	want := errors.New("stop")
	err := adapter.Stream(context.Background(), v1.SpeechPlan{CandidateID: "c", Text: "hello"}, func(v1.SpeechChunk) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("consumer error = %v", err)
	}
	failed, _ := New(Config{Endpoint: server.URL + "?fail=yes"})
	if err := failed.Stream(context.Background(), v1.SpeechPlan{CandidateID: "c", Text: "hello"}, func(v1.SpeechChunk) error { return nil }); err == nil {
		t.Fatal("expected HTTP error")
	}
}

func TestAdapterRejectsInvalidConfigAndNilConsumer(t *testing.T) {
	if _, err := New(Config{Endpoint: "://bad"}); err == nil {
		t.Fatal("expected endpoint error")
	}
	if _, err := New(Config{ChunkLength: 99}); err == nil {
		t.Fatal("expected chunk-length error")
	}
	adapter, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Stream(context.Background(), v1.SpeechPlan{CandidateID: "c", Text: "hello"}, nil); !errors.Is(err, v1.ErrNilConsumer) {
		t.Fatalf("nil consumer error = %v", err)
	}
}

func assertChunks(t *testing.T, chunks []v1.SpeechChunk, source []byte, sourceRate, outputRate uint32) {
	t.Helper()
	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}
	var got []byte
	var expectedOffset uint64
	for index, chunk := range chunks {
		if chunk.SampleOffset != expectedOffset || chunk.SampleRateHz != outputRate {
			t.Fatalf("chunk %d metadata: %+v", index, chunk)
		}
		if chunk.Final != (index == len(chunks)-1) {
			t.Fatalf("chunk %d final = %v", index, chunk.Final)
		}
		got = append(got, chunk.PCM16LE...)
		expectedOffset += uint64(len(chunk.PCM16LE) / 2)
	}
	resampler, _ := pcm.NewResampler(sourceRate, outputRate)
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

func samplePCM(samples int) []byte {
	output := make([]byte, samples*2)
	for index := 0; index < samples; index++ {
		binary.LittleEndian.PutUint16(output[index*2:], uint16(int16(index%20_000-10_000)))
	}
	return output
}

func wavHeader(rate uint32) []byte {
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], 36)
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], 1)
	binary.LittleEndian.PutUint32(header[24:28], rate)
	binary.LittleEndian.PutUint32(header[28:32], rate*2)
	binary.LittleEndian.PutUint16(header[32:34], 2)
	binary.LittleEndian.PutUint16(header[34:36], 16)
	copy(header[36:40], "data")
	return header
}
