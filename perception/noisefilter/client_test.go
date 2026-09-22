package noisefilter

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func reply(w http.ResponseWriter, r *http.Request, body []byte) {
	w.Header().Set("X-Sequence", r.Header.Get("X-Sequence"))
	w.Header().Set("X-Filter-Model", "rnnoise")
	w.Header().Set("X-Audio-Delay-MS", "20")
	w.Write(body)
}

func TestProcessingOrdersChunksAndNeverMutatesInput(t *testing.T) {
	var sequences []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			return
		}
		sequences = append(sequences, r.Header.Get("X-Sequence"))
		data, _ := io.ReadAll(r.Body)
		if len(data) > 4800 {
			t.Error("request exceeded 100ms")
		}
		for i := range data {
			data[i] = 0
		}
		reply(w, r, data)
	}))
	defer server.Close()
	client, err := New(Config{URL: server.URL, TimeoutMS: 50})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	original := bytes.Repeat([]byte{7, 8}, 12001)
	got, err := client.Process(context.Background(), original, 24000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(original) || !bytes.Equal(got, make([]byte, len(original))) {
		t.Fatal("wrong filtered PCM")
	}
	if original[0] != 7 {
		t.Fatal("input mutated")
	}
	for i, s := range sequences {
		if s != strconv.Itoa(i) {
			t.Fatal("requests reordered")
		}
	}
	if len(sequences) != 6 {
		t.Fatalf("got %d chunks", len(sequences))
	}
}

func TestFailureDoesNotLeakRawAudioOrRetryAdvancedState(t *testing.T) {
	for _, kind := range []string{"late", "short", "oversized", "sequence", "status", "model"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					return
				}
				data, _ := io.ReadAll(r.Body)
				switch kind {
				case "late":
					<-r.Context().Done()
					return
				case "short":
					data = data[:len(data)-2]
				case "oversized":
					data = append(data, 0, 0)
				case "sequence":
					r.Header.Set("X-Sequence", "999")
				case "status":
					w.WriteHeader(503)
					return
				case "model":
					w.Header().Set("X-Filter-Model", "other")
					w.Write(data)
					return
				}
				reply(w, r, data)
			}))
			defer server.Close()
			client, _ := New(Config{URL: server.URL, TimeoutMS: 10})
			defer client.Close()
			started := time.Now()
			got, err := client.Process(context.Background(), []byte{42, 1}, 24000)
			if err == nil || got != nil {
				t.Fatal("failed filter released audio")
			}
			if time.Since(started) > time.Second {
				t.Fatal("deadline not bounded")
			}
			if _, err := client.Process(context.Background(), []byte{42, 1}, 24000); err == nil {
				t.Fatal("reused uncertain model state")
			}
		})
	}
}

func TestBudgetCoversTheEntireIngressPacket(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			return
		}
		data, _ := io.ReadAll(r.Body)
		time.Sleep(15 * time.Millisecond)
		reply(w, r, data)
	}))
	defer server.Close()
	client, _ := New(Config{URL: server.URL, TimeoutMS: 25})
	defer client.Close()
	// Three 100ms subrequests would take at least 45ms if each got a new budget.
	result, err := client.Process(context.Background(), make([]byte, 14400), 24000)
	if err == nil || result != nil {
		t.Fatal("batching multiplied the filter deadline")
	}
}

func TestTargetVoiceContractAndNoRegressionToRawAudio(t *testing.T) {
	phases := []string{"waiting-for-speech", "collecting-reference", "preparing-reference", "extracting", "collecting-reference"}
	index := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Sequence", r.Header.Get("X-Sequence"))
		w.Header().Set("X-Filter-Model", "real-tse")
		w.Header().Set("X-Audio-Delay-MS", "65")
		w.Header().Set("X-Target-Voice-State", phases[index])
		index++
		w.Write(body)
	}))
	defer server.Close()
	client, err := New(Config{URL: server.URL, TimeoutMS: 50, Model: "real-tse"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for i := 0; i < 4; i++ {
		if _, err := client.Process(context.Background(), make([]byte, 320), 16000); err != nil {
			t.Fatal(err)
		}
	}
	if pcm, err := client.Process(context.Background(), make([]byte, 320), 16000); err != nil || !bytes.Equal(pcm, make([]byte, 320)) || client.Status().State != "degraded" {
		t.Fatal("regression did not latch muted degraded state")
	}
	if pcm, err := client.Process(context.Background(), make([]byte, 320), 16000); err != nil || !bytes.Equal(pcm, make([]byte, 320)) || client.Status().State != "degraded" {
		t.Fatal("degraded session did not stay muted")
	}
}

func TestRejectUnknownFilterModel(t *testing.T) {
	if _, err := New(Config{URL: "http://localhost:8126", TimeoutMS: 50, Model: "unverified"}); err == nil {
		t.Fatal("unknown filter accepted")
	}
}

// Each model's waveform delay is part of the contract: the service says what
// it is and the client refuses a frame that claims a different one, because a
// filter that changed its latency would shift every capture position.
func TestEachFilterModelPinsItsWaveformDelay(t *testing.T) {
	t.Parallel()
	// real-tse is excluded: its frames go through the target-voice worker,
	// which has its own enrollment path and is covered by target_worker_test.
	for model, delay := range map[string]string{"rnnoise": "20", "deepfilternet": "40"} {
		var seen string
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			writer.Header().Set("X-Sequence", request.Header.Get("X-Sequence"))
			writer.Header().Set("X-Filter-Model", model)
			writer.Header().Set("X-Audio-Delay-MS", seen)
			writer.Header().Set("X-Target-Voice-State", "extracting")
			_, _ = writer.Write(body)
		}))
		client, err := New(Config{URL: server.URL, TimeoutMS: 50, Model: model})
		if err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		seen = delay
		if _, err := client.Process(context.Background(), make([]byte, 640), 16_000); err != nil {
			t.Errorf("%s declared %s ms and was refused: %v", model, delay, err)
		}
		seen = "999"
		if _, err := client.Process(context.Background(), make([]byte, 640), 16_000); err == nil {
			t.Errorf("%s was accepted while reporting a different delay", model)
		}
		server.Close()
	}
	if _, err := New(Config{URL: "http://127.0.0.1:1", TimeoutMS: 50, Model: "whatever"}); err == nil {
		t.Error("an unknown filter model was accepted")
	}
}
