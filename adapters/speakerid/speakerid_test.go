package speakerid_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/speakerid"
)

// The embedding service is one POST of raw PCM with the sample rate in a
// header, answered by a unit vector or by a reason it could not tell. Nothing
// exercised that contract from the Go side, so a service that renamed the
// header or the field would have failed only in a live session.
func TestEmbedSendsPCMWithItsRateAndReturnsTheVector(t *testing.T) {
	var (
		gotMethod, gotType, gotRate string
		gotBody                     []byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotType, gotRate = r.Method, r.Header.Get("Content-Type"), r.Header.Get("X-Sample-Rate")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"embedding":[0.6,0.8]}`))
	}))
	defer server.Close()
	adapter, err := speakerid.New(speakerid.Config{Endpoint: server.URL + "/embed"})
	if err != nil {
		t.Fatal(err)
	}
	pcm := []byte{1, 0, 2, 0, 3, 0, 4, 0}
	embedding, err := adapter.Embed(context.Background(), pcm, 16_000)
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotType != "application/octet-stream" || gotRate != "16000" ||
		string(gotBody) != string(pcm) {
		t.Fatalf("request drifted from the service contract: %s %s rate=%s body=%v",
			gotMethod, gotType, gotRate, gotBody)
	}
	if len(embedding) != 2 || embedding[0] != 0.6 || embedding[1] != 0.8 {
		t.Fatalf("embedding = %v, want [0.6 0.8]", embedding)
	}
}

func TestEmbedReportsAnUnidentifiableUtteranceAsNoEmbeddingNotAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"reason":"utterance shorter than the enrolment minimum"}`))
	}))
	defer server.Close()
	adapter, _ := speakerid.New(speakerid.Config{Endpoint: server.URL})
	embedding, err := adapter.Embed(context.Background(), []byte{1, 0}, 16_000)
	if err != nil || embedding != nil {
		t.Fatalf("a service that cannot tell must answer with no embedding and no error, got %v, %v", embedding, err)
	}
}

func TestEmbedRefusesEmptyAudioAndAMissingRateBeforeAnyRequest(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer server.Close()
	adapter, _ := speakerid.New(speakerid.Config{Endpoint: server.URL})
	if _, err := adapter.Embed(context.Background(), nil, 16_000); err == nil {
		t.Fatal("empty audio was sent")
	}
	if _, err := adapter.Embed(context.Background(), []byte{1, 0}, 0); err == nil {
		t.Fatal("a zero sample rate was sent")
	}
	if called {
		t.Fatal("a request left the process for input the adapter should have refused")
	}
}

func TestEmbedNamesTheStatusAndBodyOfAFailedService(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not loaded", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	adapter, _ := speakerid.New(speakerid.Config{Endpoint: server.URL})
	_, err := adapter.Embed(context.Background(), []byte{1, 0}, 16_000)
	if err == nil || !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "model not loaded") {
		t.Fatalf("a failed service must be reported with its status and reason, got %v", err)
	}
}

func TestEmbedRejectsABodyThatIsNotTheContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>not json</html>`))
	}))
	defer server.Close()
	adapter, _ := speakerid.New(speakerid.Config{Endpoint: server.URL})
	if _, err := adapter.Embed(context.Background(), []byte{1, 0}, 16_000); err == nil {
		t.Fatal("a non-JSON body was accepted as an embedding")
	}
}

func TestEmbedIsBoundedByTheConfiguredTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	adapter, _ := speakerid.New(speakerid.Config{Endpoint: server.URL, RequestTimeout: 50 * time.Millisecond})
	started := time.Now()
	_, err := adapter.Embed(context.Background(), []byte{1, 0}, 16_000)
	if err == nil {
		t.Fatal("a stalled service must time out rather than hold the utterance")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("timeout took %v, want about 50ms", elapsed)
	}
}

func TestDefaultsMatchTheServedEndpointAndDeclaredContract(t *testing.T) {
	if speakerid.DefaultEndpoint != "http://127.0.0.1:8124/embed" {
		t.Fatalf("default endpoint drifted from tools/speakerid: %s", speakerid.DefaultEndpoint)
	}
	if speakerid.AdapterVersion != "speakerid-http-pcm16-1" {
		t.Fatalf("adapter version is the pinned contract identity, got %s", speakerid.AdapterVersion)
	}
}
