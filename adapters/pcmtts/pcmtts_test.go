package pcmtts

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

// sent is what a vendor's endpoint actually received.
type sent struct {
	path   string
	query  url.Values
	header http.Header
	body   map[string]json.RawMessage
}

func speechServer(t *testing.T, samples int, received *sent) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if received != nil {
			received.path = request.URL.Path
			received.query = request.URL.Query()
			received.header = request.Header.Clone()
			if err := json.NewDecoder(request.Body).Decode(&received.body); err != nil {
				t.Errorf("decode body: %v", err)
			}
		}
		writer.Header().Set("Content-Type", "audio/pcm")
		_, _ = writer.Write(tone(samples))
	}))
}

func tone(samples int) []byte {
	pcm := make([]byte, samples*2)
	for index := range samples {
		binary.LittleEndian.PutUint16(pcm[index*2:], uint16(int16(math.Round(9000*math.Sin(float64(index)/10)))))
	}
	return pcm
}

func collect(t *testing.T, adapter *Adapter) []v1.SpeechChunk {
	t.Helper()
	chunks, err := adapter.Synthesize(context.Background(), v1.SpeechPlan{
		CandidateID: "cand-1", Text: "hello there",
	})
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("no audio")
	}
	finals := 0
	var offset uint64
	for _, chunk := range chunks {
		if chunk.CandidateID != "cand-1" || chunk.SampleRateHz == 0 {
			t.Fatalf("chunk: %+v", chunk)
		}
		if chunk.SampleOffset != offset {
			t.Fatalf("chunk offsets must be contiguous: got %d want %d", chunk.SampleOffset, offset)
		}
		offset += uint64(len(chunk.PCM16LE) / 2)
		if chunk.Final {
			finals++
		}
	}
	if finals != 1 || !chunks[len(chunks)-1].Final {
		t.Fatalf("exactly one terminal chunk is required, and it must be the last: %d", finals)
	}
	return chunks
}

func TestDeepgramSendsTheTextAndAsksForLinearPCM(t *testing.T) {
	t.Parallel()
	var received sent
	server := speechServer(t, 2_000, &received)
	defer server.Close()

	adapter, err := NewDeepgram(Config{
		APIKey: "secret", Model: "aura-test", Endpoint: server.URL + "/v1/speak",
		RequestSampleRateHz: 24_000, OutputSampleRateHz: 24_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	collect(t, adapter)
	if string(received.body["text"]) != `"hello there"` {
		t.Fatalf("text field: %s", received.body["text"])
	}
	if received.query.Get("encoding") != "linear16" || received.query.Get("sample_rate") != "24000" ||
		received.query.Get("model") != "aura-test" {
		t.Fatalf("query: %v", received.query)
	}
	if received.header.Get("Authorization") != "Token secret" {
		t.Fatalf("Deepgram authenticates with a Token header, got %q", received.header.Get("Authorization"))
	}
}

// The voice is in the path for ElevenLabs, which is why it is required rather
// than optional.
func TestElevenLabsPutsTheVoiceInThePathAndNamesThePCMFormat(t *testing.T) {
	t.Parallel()
	var received sent
	server := speechServer(t, 2_000, &received)
	defer server.Close()

	adapter, err := NewElevenLabs(Config{
		APIKey: "secret", Model: "eleven-test", Voice: "voice-42", Endpoint: server.URL + "/v1/text-to-speech",
		RequestSampleRateHz: 24_000, OutputSampleRateHz: 24_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	collect(t, adapter)
	if received.path != "/v1/text-to-speech/voice-42/stream" {
		t.Fatalf("path: %q", received.path)
	}
	if received.query.Get("output_format") != "pcm_24000" {
		t.Fatalf("output format: %q", received.query.Get("output_format"))
	}
	if received.header.Get("xi-api-key") != "secret" {
		t.Fatalf("ElevenLabs authenticates with xi-api-key, got %v", received.header)
	}
	if string(received.body["model_id"]) != `"eleven-test"` {
		t.Fatalf("model field: %s", received.body["model_id"])
	}

	if _, err := NewElevenLabs(Config{APIKey: "secret", RequestSampleRateHz: 24_000}); err == nil {
		t.Fatal("ElevenLabs cannot synthesise without a voice")
	}
	// The rate set is closed, so an unserved rate is refused rather than
	// silently answered at another one.
	if _, err := NewElevenLabs(Config{
		APIKey: "secret", Voice: "v", RequestSampleRateHz: 48_000,
	}); err == nil {
		t.Fatal("an unsupported PCM rate must be refused")
	}
}

func TestCartesiaNestsTheOutputFormatAndSendsItsVersionHeader(t *testing.T) {
	t.Parallel()
	var received sent
	server := speechServer(t, 2_000, &received)
	defer server.Close()

	adapter, err := NewCartesia(Config{
		APIKey: "secret", Model: "sonic-test", Voice: "voice-7", Endpoint: server.URL + "/tts/bytes",
		Language: "en", RequestSampleRateHz: 24_000, OutputSampleRateHz: 24_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	collect(t, adapter)
	if string(received.body["transcript"]) != `"hello there"` {
		t.Fatalf("Cartesia names the text field transcript: %v", received.body)
	}
	var format struct {
		Container  string `json:"container"`
		Encoding   string `json:"encoding"`
		SampleRate int    `json:"sample_rate"`
	}
	if err := json.Unmarshal(received.body["output_format"], &format); err != nil {
		t.Fatal(err)
	}
	if format.Container != "raw" || format.Encoding != "pcm_s16le" || format.SampleRate != 24_000 {
		t.Fatalf("output format: %+v", format)
	}
	if received.header.Get("Cartesia-Version") != CartesiaDefaultVersion ||
		received.header.Get("X-API-Key") != "secret" {
		t.Fatalf("headers: %v", received.header)
	}
}

// Resampling has to happen when the vendor's rate is not the wire's, and the
// chunk stream must stay contiguous across it.
func TestAServedRateIsResampledToTheOutputRate(t *testing.T) {
	t.Parallel()
	server := speechServer(t, 4_000, nil)
	defer server.Close()

	adapter, err := NewDeepgram(Config{
		APIKey: "secret", Endpoint: server.URL + "/v1/speak",
		RequestSampleRateHz: 16_000, OutputSampleRateHz: 24_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	chunks := collect(t, adapter)
	var samples uint64
	for _, chunk := range chunks {
		if chunk.SampleRateHz != 24_000 {
			t.Fatalf("chunks must carry the output rate, got %d", chunk.SampleRateHz)
		}
		samples += uint64(len(chunk.PCM16LE) / 2)
	}
	// 4000 samples at 16 kHz is a quarter second, which is 6000 at 24 kHz.
	// A resampler may hold a sample at each boundary, so allow a small margin
	// rather than asserting an exact count.
	if samples < 5_900 || samples > 6_100 {
		t.Fatalf("resampled length = %d samples, want about 6000", samples)
	}
}

// A vendor that answers 200 with a JSON error would otherwise be synthesised
// as noise and played to the caller.
func TestAJSONErrorBodyIsNotPlayedAsAudio(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"error":"voice not found"}`))
	}))
	defer server.Close()

	adapter, err := NewDeepgram(Config{APIKey: "secret", Endpoint: server.URL + "/v1/speak"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Synthesize(context.Background(), v1.SpeechPlan{CandidateID: "c", Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "content type") {
		t.Fatalf("a JSON body must not be treated as PCM: %v", err)
	}
}

func TestAnErrorStatusIsReported(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte("bad key"))
	}))
	defer server.Close()

	adapter, err := NewCartesia(Config{APIKey: "secret", Voice: "v", Endpoint: server.URL + "/tts/bytes"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Synthesize(context.Background(), v1.SpeechPlan{CandidateID: "c", Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("an error status must be reported: %v", err)
	}
}

// Deepgram names the voice inside the model, so a separate one is a
// configuration mistake worth catching at construction.
func TestDeepgramRefusesASeparateVoice(t *testing.T) {
	t.Parallel()
	if _, err := NewDeepgram(Config{APIKey: "secret", Voice: "thalia"}); err == nil {
		t.Fatal("Deepgram has no voice field")
	}
	// The server's own placeholder is not a voice and must not be treated as
	// one.
	if _, err := NewDeepgram(Config{APIKey: "secret", Voice: "default"}); err != nil {
		t.Fatalf("the placeholder voice must be ignored: %v", err)
	}
}
