package openaitranscribe

import (
	"context"
	"encoding/binary"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

// upload is what the endpoint actually received.
type upload struct {
	fields map[string]string
	audio  []byte
	auth   string
	path   string
}

func transcriptionServer(t *testing.T, text string, calls *atomic.Int64, last *upload) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			t.Errorf("transcription must be a multipart upload, got %q", request.Header.Get("Content-Type"))
			return
		}
		reader := multipart.NewReader(request.Body, parameters["boundary"])
		received := upload{
			fields: map[string]string{},
			auth:   request.Header.Get("Authorization") + request.Header.Get("xi-api-key"),
			path:   request.URL.Path,
		}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("read part: %v", err)
				return
			}
			payload, _ := io.ReadAll(part)
			if part.FormName() == "file" {
				received.audio = payload
				continue
			}
			received.fields[part.FormName()] = string(payload)
		}
		if last != nil {
			*last = received
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"text":"` + text + `","language":"en"}`))
	}))
}

func tone(samples int) []byte {
	pcm := make([]byte, samples*2)
	for index := range samples {
		value := int16(math.Round(8000 * math.Sin(float64(index)/8)))
		binary.LittleEndian.PutUint16(pcm[index*2:], uint16(value))
	}
	return pcm
}

func TestFinalizeUploadsAWAVAndReturnsOneFinalRevision(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	var received upload
	server := transcriptionServer(t, "hello there", &calls, &received)
	defer server.Close()

	adapter, err := New(Config{BaseURL: server.URL + "/v1", Model: "whisper-test", APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	revisions, err := adapter.PushFrame(context.Background(), v1.AudioFrame{
		Index: 0, SampleOffset: 0, SampleRateHz: 16_000, PCM16LE: tone(1_600),
	})
	if err != nil {
		t.Fatal(err)
	}
	// A batch endpoint has no partial to give, so nothing must be invented.
	if len(revisions) != 0 || calls.Load() != 0 {
		t.Fatalf("a batch recogniser must not transcribe before the endpoint: %d revisions, %d calls",
			len(revisions), calls.Load())
	}
	final, err := adapter.Finalize(context.Background(), 1_600)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Final || final.StableText != "hello there" || final.RevisionID != 1 {
		t.Fatalf("final revision: %+v", final)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected exactly one transcription, got %d", calls.Load())
	}
	if received.fields["model"] != "whisper-test" || received.auth != "Bearer secret" {
		t.Fatalf("upload fields: %+v auth %q", received.fields, received.auth)
	}
	if string(received.audio[0:4]) != "RIFF" || string(received.audio[8:12]) != "WAVE" {
		t.Fatalf("the upload must be a WAV container, got %q", received.audio[:12])
	}
	if rate := binary.LittleEndian.Uint32(received.audio[24:28]); rate != 16_000 {
		t.Fatalf("declared sample rate = %d", rate)
	}
}

// A partial costs a whole re-transcription, so the schedule has to be a
// schedule: once the threshold is crossed it must not fire on every frame.
func TestPartialsFireOnAScheduleRatherThanEveryFrame(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	server := transcriptionServer(t, "partial text", &calls, nil)
	defer server.Close()

	adapter, err := New(Config{
		BaseURL: server.URL + "/v1", APIKey: "secret",
		PartialInterval: 500 * time.Millisecond, SampleRateHz: 16_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Ten frames of 100 ms each is one second of audio, which is two
	// intervals: two partials, not ten.
	offset := uint64(0)
	for index := range uint64(10) {
		revisions, err := adapter.PushFrame(context.Background(), v1.AudioFrame{
			Index: index, SampleOffset: offset, SampleRateHz: 16_000, PCM16LE: tone(1_600),
		})
		if err != nil {
			t.Fatal(err)
		}
		offset += 1_600
		for _, revision := range revisions {
			if revision.Final {
				t.Fatal("a partial must not be marked final")
			}
			if revision.UnstableText != "partial text" {
				t.Fatalf("partial text: %+v", revision)
			}
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected two scheduled partials over one second, got %d", got)
	}
}

// ElevenLabs is the same shape with two names changed, which is the reason it
// shares this adapter rather than getting its own.
func TestTheElevenLabsProfileRenamesTheModelFieldAndHeader(t *testing.T) {
	t.Parallel()
	var received upload
	server := transcriptionServer(t, "scribed", nil, &received)
	defer server.Close()

	adapter, err := New(Config{
		BaseURL: server.URL + "/v1", Path: "/speech-to-text", Model: "scribe_v2",
		APIKey: "secret", AuthHeader: "xi-api-key", ModelField: "model_id",
		Provider: "elevenlabs",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.PushFrame(context.Background(), v1.AudioFrame{
		Index: 0, SampleOffset: 0, SampleRateHz: 16_000, PCM16LE: tone(800),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Finalize(context.Background(), 800); err != nil {
		t.Fatal(err)
	}
	if received.fields["model_id"] != "scribe_v2" {
		t.Fatalf("ElevenLabs names the model field model_id: %+v", received.fields)
	}
	if received.auth != "secret" || received.path != "/v1/speech-to-text" {
		t.Fatalf("auth %q path %q", received.auth, received.path)
	}
}

func TestOutOfOrderFramesAreRefused(t *testing.T) {
	t.Parallel()
	server := transcriptionServer(t, "text", nil, nil)
	defer server.Close()
	adapter, err := New(Config{BaseURL: server.URL + "/v1", APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.PushFrame(context.Background(), v1.AudioFrame{
		Index: 0, SampleOffset: 0, SampleRateHz: 16_000, PCM16LE: tone(160),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.PushFrame(context.Background(), v1.AudioFrame{
		Index: 5, SampleOffset: 160, SampleRateHz: 16_000, PCM16LE: tone(160),
	}); err == nil {
		t.Fatal("a frame out of sequence must be refused")
	}
	if _, err := adapter.Finalize(context.Background(), 999); err == nil {
		t.Fatal("finalizing at the wrong sample must be refused")
	}
}

func TestAnEmptyUtteranceCannotBeFinalized(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{BaseURL: "https://example.invalid/v1", APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Finalize(context.Background(), 0); err == nil {
		t.Fatal("an utterance with no audio must not be finalized")
	}
}

func TestAnEndpointErrorIsReported(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusPaymentRequired)
		_, _ = writer.Write([]byte("out of credit"))
	}))
	defer server.Close()
	adapter, err := New(Config{BaseURL: server.URL + "/v1", APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.PushFrame(context.Background(), v1.AudioFrame{
		Index: 0, SampleOffset: 0, SampleRateHz: 16_000, PCM16LE: tone(160),
	}); err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Finalize(context.Background(), 160)
	if err == nil || !strings.Contains(err.Error(), "402") {
		t.Fatalf("an endpoint failure must be reported: %v", err)
	}
}
