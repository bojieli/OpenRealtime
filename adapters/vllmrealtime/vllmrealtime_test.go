package vllmrealtime_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/bojieli/OpenRealtime/adapters/vllmrealtime"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

// fakeRealtime answers like vLLM's realtime route: session.update validates
// the model, the first commit starts one generation, each append yields a
// delta, and commit{final} ends the generation with transcription.done.
type fakeRealtime struct {
	mu       sync.Mutex
	model    string
	samples  int
	sequence []string
}

func (fake *fakeRealtime) handler(t *testing.T) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer connection.CloseNow()
		ctx := request.Context()
		send := func(message map[string]any) {
			payload, _ := json.Marshal(message)
			_ = connection.Write(ctx, websocket.MessageText, payload)
		}
		send(map[string]any{"type": "session.created", "id": "sess-1"})
		words := []string{"hello", " there", " world"}
		text := ""
		for {
			_, payload, err := connection.Read(ctx)
			if err != nil {
				return
			}
			var message struct {
				Type  string `json:"type"`
				Model string `json:"model"`
				Audio string `json:"audio"`
				Final bool   `json:"final"`
			}
			if err := json.Unmarshal(payload, &message); err != nil {
				t.Errorf("decode: %v", err)
				return
			}
			fake.mu.Lock()
			fake.sequence = append(fake.sequence, message.Type)
			fake.mu.Unlock()
			switch message.Type {
			case "session.update":
				fake.mu.Lock()
				fake.model = message.Model
				fake.mu.Unlock()
			case "input_audio_buffer.append":
				audio, err := base64.StdEncoding.DecodeString(message.Audio)
				if err != nil || len(audio)%2 != 0 {
					t.Errorf("append carried invalid PCM16: %v", err)
				}
				fake.mu.Lock()
				fake.samples += len(audio) / 2
				fake.mu.Unlock()
				if len(words) > 0 {
					text += words[0]
					send(map[string]any{"type": "transcription.delta", "delta": words[0]})
					words = words[1:]
				}
			case "input_audio_buffer.commit":
				if message.Final {
					send(map[string]any{"type": "transcription.done", "text": text + "."})
					return
				}
			}
		}
	}
}

func frame(index uint64, offset uint64, samples int) v1.AudioFrame {
	return v1.AudioFrame{
		Index: index, SampleOffset: offset, SampleRateHz: 24_000,
		PCM16LE: make([]byte, samples*2),
	}
}

func TestAnUtteranceStreamsCommittedTextAndFinalizes(t *testing.T) {
	t.Parallel()
	fake := &fakeRealtime{}
	server := httptest.NewServer(fake.handler(t))
	defer server.Close()
	adapter, err := vllmrealtime.New(vllmrealtime.Config{
		URL: "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime", Model: "voxtral-realtime",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var revisions []v1.PerceptionRevision
	var offset uint64
	for index := uint64(0); index < 3; index++ {
		got, err := adapter.PushFrame(ctx, frame(index, offset, 2400))
		if err != nil {
			t.Fatal(err)
		}
		revisions = append(revisions, got...)
		offset += 2400
		// Let the fake's delta arrive before the next frame drains it.
		time.Sleep(20 * time.Millisecond)
	}
	final, err := adapter.Finalize(ctx, offset)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Final || final.StableText != "hello there world." || final.UnstableText != "" {
		t.Fatalf("final revision = %+v", final)
	}
	for _, revision := range revisions {
		if revision.UnstableText != "" || revision.Final {
			t.Errorf("an append-only route reported provisional text: %+v", revision)
		}
		if !strings.HasPrefix("hello there world.", revision.StableText) {
			t.Errorf("committed text %q is not a prefix of the final transcript", revision.StableText)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.model != "voxtral-realtime" {
		t.Errorf("session.update named %q", fake.model)
	}
	// Three 100 ms frames at 24 kHz are 4,800 samples at 16 kHz.
	if fake.samples != 4800 {
		t.Errorf("server received %d samples at 16 kHz; want 4800", fake.samples)
	}
	if len(fake.sequence) < 3 || fake.sequence[0] != "session.update" || fake.sequence[1] != "input_audio_buffer.commit" {
		t.Errorf("the generation must be validated and started before audio: %v", fake.sequence)
	}
	if _, err := adapter.PushFrame(ctx, frame(3, offset, 2400)); err == nil {
		t.Error("a finalized utterance accepted another frame")
	}
}

func TestAnUtteranceRefusesADiscontinuousFrame(t *testing.T) {
	t.Parallel()
	fake := &fakeRealtime{}
	server := httptest.NewServer(fake.handler(t))
	defer server.Close()
	adapter, err := vllmrealtime.New(vllmrealtime.Config{URL: "ws" + strings.TrimPrefix(server.URL, "http")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer adapter.Close()
	if _, err := adapter.PushFrame(ctx, frame(0, 0, 2400)); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.PushFrame(ctx, frame(2, 2400, 2400)); err == nil {
		t.Fatal("a skipped frame index was accepted")
	}
}

func TestConfigurationIsValidatedWithoutASocket(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"http://127.0.0.1:9101/v1/realtime", "relative/path"} {
		if _, err := vllmrealtime.New(vllmrealtime.Config{URL: target}); err == nil {
			t.Errorf("URL %q was accepted", target)
		}
	}
	adapter, err := vllmrealtime.New(vllmrealtime.Config{})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := adapter.Descriptor()
	if descriptor.Name != "vllm-realtime/"+vllmrealtime.DefaultModel || !descriptor.Capabilities[v1.CapabilityStreamingInput] {
		t.Errorf("descriptor = %+v", descriptor)
	}
}
