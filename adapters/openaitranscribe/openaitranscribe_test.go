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
	// intervals: two partials, not ten. Let the first asynchronous request
	// complete before crossing the second threshold so this tests the schedule
	// rather than the deliberate one-request backpressure.
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
		if index == 4 {
			await(t, func() bool {
				adapter.mu.Lock()
				defer adapter.mu.Unlock()
				return adapter.partialDone != nil && len(adapter.partialDone) == 1
			})
		}
	}
	await(t, func() bool { return calls.Load() == 2 })
	await(t, func() bool {
		adapter.mu.Lock()
		defer adapter.mu.Unlock()
		return adapter.partialDone != nil && len(adapter.partialDone) == 1
	})
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected two scheduled partials over one second, got %d", got)
	}
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}
}

// A configured partial on a batch endpoint is speculative. It must never turn
// the provider's HTTP latency into acoustic latency: frames still have to
// reach VAD while that request is outstanding, and the endpoint must replace
// the stale snapshot with one terminal transcription.
func TestAPartialDoesNotBlockAudioIngestionOrEndpointFinalization(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-request.Context().Done():
			case <-release:
			}
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"text":"complete utterance","language":"en"}`))
	}))
	defer server.Close()
	defer close(release)

	adapter, err := New(Config{
		BaseURL: server.URL + "/v1", APIKey: "secret",
		PartialInterval: 500 * time.Millisecond, SampleRateHz: 16_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	type pushResult struct {
		revisions []v1.PerceptionRevision
		err       error
	}
	firstDone := make(chan pushResult, 1)
	go func() {
		revisions, err := adapter.PushFrame(context.Background(), v1.AudioFrame{
			Index: 0, SampleOffset: 0, SampleRateHz: 16_000, PCM16LE: tone(8_000),
		})
		firstDone <- pushResult{revisions: revisions, err: err}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("the scheduled partial did not reach the endpoint")
	}
	select {
	case result := <-firstDone:
		if result.err != nil || len(result.revisions) != 0 {
			t.Fatalf("start partial: revisions=%+v error=%v", result.revisions, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("starting a remote partial blocked the audio frame that scheduled it")
	}

	secondDone := make(chan pushResult, 1)
	go func() {
		revisions, err := adapter.PushFrame(context.Background(), v1.AudioFrame{
			Index: 1, SampleOffset: 8_000, SampleRateHz: 16_000, PCM16LE: tone(8_000),
		})
		secondDone <- pushResult{revisions: revisions, err: err}
	}()
	select {
	case result := <-secondDone:
		if result.err != nil || len(result.revisions) != 0 {
			t.Fatalf("ingest while partial is pending: revisions=%+v error=%v", result.revisions, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("a pending batch partial blocked the next audio frame")
	}

	final, err := adapter.Finalize(context.Background(), 16_000)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Final || final.StableText != "complete utterance" {
		t.Fatalf("final revision: %+v", final)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("one obsolete partial and one terminal request should cross the endpoint, got %d", got)
	}
}

func TestAPartialOutlivesThePushFrameCallContext(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		close(started)
		select {
		case <-release:
		case <-request.Context().Done():
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"text":"context survived","language":"en"}`))
	}))
	defer server.Close()

	adapter, err := New(Config{
		BaseURL: server.URL + "/v1", APIKey: "secret",
		PartialInterval: 500 * time.Millisecond, SampleRateHz: 16_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	callContext, cancelCall := context.WithCancel(context.Background())
	if _, err := adapter.PushFrame(callContext, v1.AudioFrame{
		Index: 0, SampleOffset: 0, SampleRateHz: 16_000, PCM16LE: tone(8_000),
	}); err != nil {
		t.Fatal(err)
	}
	// A graph event callback cancels this context immediately after PushFrame
	// returns. That must not cancel the adapter-owned asynchronous request.
	cancelCall()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("partial was canceled with its PushFrame call context")
	}
	close(release)
	await(t, func() bool {
		adapter.mu.Lock()
		defer adapter.mu.Unlock()
		return adapter.partialDone != nil && len(adapter.partialDone) == 1
	})
	revisions, err := adapter.PushFrame(context.Background(), v1.AudioFrame{
		Index: 1, SampleOffset: 8_000, SampleRateHz: 16_000, PCM16LE: tone(160),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 1 || revisions[0].UnstableText != "context survived" {
		t.Fatalf("partial revision after call context ended: %+v", revisions)
	}
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestASlowPartialCoalescesMissedIntervals(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if calls.Add(1) == 1 {
			close(firstStarted)
			select {
			case <-releaseFirst:
			case <-request.Context().Done():
				return
			}
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"text":"current partial","language":"en"}`))
	}))
	defer server.Close()

	adapter, err := New(Config{
		BaseURL: server.URL + "/v1", APIKey: "secret",
		PartialInterval: 500 * time.Millisecond, SampleRateHz: 16_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.PushFrame(context.Background(), v1.AudioFrame{
		Index: 0, SampleOffset: 0, SampleRateHz: 16_000, PCM16LE: tone(8_000),
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("the first partial did not start")
	}
	// Four more intervals arrive while the first partial is still in flight.
	// They are one current snapshot, not four future requests.
	if _, err := adapter.PushFrame(context.Background(), v1.AudioFrame{
		Index: 1, SampleOffset: 8_000, SampleRateHz: 16_000, PCM16LE: tone(32_000),
	}); err != nil {
		t.Fatal(err)
	}
	close(releaseFirst)
	await(t, func() bool {
		adapter.mu.Lock()
		defer adapter.mu.Unlock()
		return adapter.partialDone != nil && len(adapter.partialDone) == 1
	})
	if _, err := adapter.PushFrame(context.Background(), v1.AudioFrame{
		Index: 2, SampleOffset: 40_000, SampleRateHz: 16_000, PCM16LE: tone(1_600),
	}); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool {
		adapter.mu.Lock()
		defer adapter.mu.Unlock()
		return calls.Load() == 2 && adapter.partialDone != nil && len(adapter.partialDone) == 1
	})
	if _, err := adapter.PushFrame(context.Background(), v1.AudioFrame{
		Index: 3, SampleOffset: 41_600, SampleRateHz: 16_000, PCM16LE: tone(1_600),
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if got := calls.Load(); got != 2 {
		t.Fatalf("stale intervals became a request backlog: got %d calls, want 2", got)
	}
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}
}

func await(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for asynchronous transcription work")
		}
		time.Sleep(time.Millisecond)
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

// TestAPartialReportsWhatHasSettled is the regression for a guard that never
// fired. A partial is a whole re-transcription of the buffer, so the recogniser
// revises its own tail constantly while the front of the sentence stops moving
// almost at once - and every partial was reported as entirely unstable, so
// anything downstream asking what had been committed got nothing and fell back
// to the whole revision. Measured, that was twenty chances to answer one
// occurrence and counts of "1 3 3 1".
func TestAPartialReportsWhatHasSettled(t *testing.T) {
	stable, unstable := settled("a capybara wandered", "a capybara wandered over and sat")
	if stable != "a capybara wandered" {
		t.Fatalf("the settled front was %q", stable)
	}
	if strings.TrimSpace(unstable) != "over and sat" {
		t.Fatalf("the moving tail was %q", unstable)
	}
	// Re-punctuating and re-capitalising is not a change of words.
	stable, _ = settled("a warm afternoon and I", "A warm afternoon. And I was walking")
	if stable == "" {
		t.Fatal("re-punctuating threw the settled front away")
	}
	// A different sentence settles nothing.
	if stable, _ := settled("a capybara wandered", "then a heron landed"); stable != "" {
		t.Fatalf("a different sentence reported %q as settled", stable)
	}
}
