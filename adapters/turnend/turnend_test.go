package turnend_test

import (
	"context"
	"encoding/binary"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/turnend"
)

func TestEvaluateSendsTheLastEightSecondsAsFloat32(t *testing.T) {
	t.Parallel()
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received, _ = io.ReadAll(request.Body)
		_, _ = writer.Write([]byte(`{"probability":0.83,"model":"smart-turn-v3.2"}`))
	}))
	defer server.Close()
	client, err := turnend.New(turnend.Config{URL: server.URL + "/v1/endpoint/smart-turn"})
	if err != nil {
		t.Fatal(err)
	}
	// Ten seconds of audio whose last sample is recognisable.
	samples := 10 * 16_000
	pcm := make([]byte, samples*2)
	binary.LittleEndian.PutUint16(pcm[len(pcm)-2:], uint16(16_384))
	evidence, err := client.Evaluate(context.Background(), pcm)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Probability != 0.83 || evidence.Model != "smart-turn-v3.2" || evidence.WindowMS != 8_000 {
		t.Fatalf("evidence = %+v", evidence)
	}
	if len(received) != 8*16_000*4 {
		t.Fatalf("service received %d bytes; want eight seconds of float32", len(received))
	}
	last := math.Float32frombits(binary.LittleEndian.Uint32(received[len(received)-4:]))
	if last != 0.5 {
		t.Fatalf("the window must end at the pause; last sample %v", last)
	}
}

func TestEvaluateRefusesAnswersThatAreNotProbabilities(t *testing.T) {
	t.Parallel()
	for _, body := range []string{`{}`, `{"probability":1.5}`, `not json`} {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(body))
		}))
		client, err := turnend.New(turnend.Config{URL: server.URL})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Evaluate(context.Background(), make([]byte, 3_200)); err == nil {
			t.Errorf("body %q was accepted", body)
		}
		server.Close()
	}
}

func TestASlowClassifierFailsWithinItsBound(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-release:
		case <-request.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	client, err := turnend.New(turnend.Config{URL: server.URL, Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := client.Evaluate(context.Background(), make([]byte, 3_200)); err == nil {
		t.Fatal("a classifier that never answered produced evidence")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("the bound was not enforced: %s", elapsed)
	}
}
