package wordtimings

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/spoken"
)

func clip(ms int) spoken.Audio {
	return spoken.Audio{PCM16LE: make([]byte, 24*2*ms), SampleRateHz: 24_000}
}

// The request has to ask for the one thing this adapter exists to get. An
// endpoint that is never asked for word times answers without them, and the
// runtime then estimates for ever while a perfectly good recogniser sits
// beside it doing nothing.
func TestTheRequestAsksForWordTimestamps(t *testing.T) {
	var fields map[string][]string
	var uploaded int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			t.Errorf("content type %q", request.Header.Get("Content-Type"))
		}
		reader := multipart.NewReader(request.Body, parameters["boundary"])
		fields = map[string][]string{}
		for {
			part, err := reader.NextPart()
			if err != nil {
				break
			}
			body, _ := io.ReadAll(part)
			if part.FormName() == "file" {
				uploaded = len(body)
				if !strings.HasPrefix(string(body), "RIFF") {
					t.Errorf("the upload is not a WAV: %q", string(body[:4]))
				}
				continue
			}
			fields[part.FormName()] = append(fields[part.FormName()], string(body))
		}
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization %q", request.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(writer,
			`{"text":"one two","words":[{"word":"one","start":0.0,"end":0.5},{"word":"two","start":0.5,"end":1.0}]}`)
	}))
	defer server.Close()

	adapter, err := New(Config{Endpoint: server.URL, Model: "whisper-1", APIKey: "secret", Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	words, err := adapter.Words(context.Background(), clip(1000))
	if err != nil {
		t.Fatal(err)
	}
	if got := fields["timestamp_granularities[]"]; len(got) != 1 || got[0] != "word" {
		t.Fatalf("the request asked for granularities %v", got)
	}
	if got := fields["response_format"]; len(got) != 1 || got[0] != "verbose_json" {
		t.Fatalf("the request asked for format %v", got)
	}
	if got := fields["model"]; len(got) != 1 || got[0] != "whisper-1" {
		t.Fatalf("the request named model %v", got)
	}
	if got := fields["language"]; len(got) != 1 || got[0] != "en" {
		t.Fatalf("the request named language %v", got)
	}
	if uploaded == 0 {
		t.Fatal("no audio was uploaded")
	}
	want := []spoken.Word{
		{Text: "one", StartMS: 0, EndMS: 500}, {Text: "two", StartMS: 500, EndMS: 1000},
	}
	if len(words) != len(want) {
		t.Fatalf("got %+v", words)
	}
	for index := range want {
		if words[index] != want[index] {
			t.Fatalf("word %d is %+v, want %+v", index, words[index], want[index])
		}
	}
}

// Several servers put the words inside the segments instead of beside them.
// Both are the same answer, and refusing one halves the endpoints this works
// against.
func TestWordsNestedInSegmentsAreTheSameAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer,
			`{"text":"one two","segments":[{"words":[{"word":"one","start":0,"end":0.4}]},`+
				`{"words":[{"word":"two","start":0.4,"end":0.9}]}]}`)
	}))
	defer server.Close()
	adapter, _ := New(Config{Endpoint: server.URL})
	words, err := adapter.Words(context.Background(), clip(900))
	if err != nil {
		t.Fatal(err)
	}
	if len(words) != 2 || words[1].StartMS != 400 || words[1].EndMS != 900 {
		t.Fatalf("got %+v", words)
	}
}

// An endpoint that transcribes but never times is a misconfiguration that
// would otherwise degrade silently for ever: every boundary estimated, with a
// working recogniser sitting beside it. It says so, once, by name.
func TestAnEndpointThatIgnoresTheRequestSaysSo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{"text":"one two three"}`)
	}))
	defer server.Close()
	adapter, _ := New(Config{Endpoint: server.URL})
	_, err := adapter.Words(context.Background(), clip(900))
	if !errors.Is(err, ErrNoWordTimes) {
		t.Fatalf("error was %v", err)
	}
	if !strings.Contains(err.Error(), "timestamp_granularities") {
		t.Fatalf("the error does not say how to fix it: %v", err)
	}
}

// Silence transcribes to nothing, and that is a fact about the audio rather
// than a broken endpoint. Reported as an error it would fill a session's logs
// with failures for every pause.
func TestSilenceIsAnAnswerRatherThanAFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{"text":"  ","words":[]}`)
	}))
	defer server.Close()
	adapter, _ := New(Config{Endpoint: server.URL})
	words, err := adapter.Words(context.Background(), clip(400))
	if err != nil || len(words) != 0 {
		t.Fatalf("got %+v, %v", words, err)
	}
}

// An anchor at an impossible position drags every interpolated word around it
// out of place, so it is dropped rather than clamped: one fewer anchor costs a
// word of resolution, and a wrong one costs the sentence.
func TestImpossibleTimesAreDroppedRatherThanClamped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{"text":"a b c","words":[`+
			`{"word":"a","start":-1,"end":0.2},`+
			`{"word":" ","start":0.2,"end":0.3},`+
			`{"word":"b","start":0.3,"end":0.2},`+
			`{"word":"c","start":0.6,"end":0.9}]}`)
	}))
	defer server.Close()
	adapter, _ := New(Config{Endpoint: server.URL})
	words, err := adapter.Words(context.Background(), clip(900))
	if err != nil {
		t.Fatal(err)
	}
	if len(words) != 2 {
		t.Fatalf("kept %+v", words)
	}
	if words[0].Text != "b" || words[0].StartMS != 300 || words[0].EndMS != 300 {
		t.Fatalf("an end before its start is pulled up to it, got %+v", words[0])
	}
}

// A refusal reaches the caller with the endpoint's own explanation, because a
// tracker that only learns "it failed" cannot tell a wrong port from a model
// that never loaded.
func TestARefusalCarriesTheEndpointsExplanation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(writer, "model is still loading")
	}))
	defer server.Close()
	adapter, _ := New(Config{Endpoint: server.URL})
	_, err := adapter.Words(context.Background(), clip(400))
	if err == nil || !strings.Contains(err.Error(), "model is still loading") {
		t.Fatalf("error was %v", err)
	}
}

func TestAudioIsRequired(t *testing.T) {
	adapter, _ := New(Config{})
	if _, err := adapter.Words(context.Background(), spoken.Audio{}); err == nil {
		t.Fatal("no audio is not a request")
	}
	if _, err := adapter.Words(context.Background(), spoken.Audio{PCM16LE: []byte{0, 0}}); err == nil {
		t.Fatal("audio with no rate has no timeline to report against")
	}
}
