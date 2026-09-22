package speechsocket_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/bojieli/OpenRealtime/adapters/speechsocket"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

// fakeService speaks openrealtime-incremental-speech/1. Every appended word
// becomes 100 ms of audio at 16 kHz as soon as it arrives - an incremental
// synthesiser - unless stall is set, in which case it answers nothing after
// text.end. endless keeps producing audio until the context is cancelled.
type fakeService struct {
	stall   bool
	endless bool
	appends chan string
}

func (fake *fakeService) handler(t *testing.T) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		ctx := request.Context()
		send := func(message map[string]any) error {
			payload, _ := json.Marshal(message)
			return connection.Write(ctx, websocket.MessageText, payload)
		}
		seq := 0
		chars := 0
		audio := func(id string) error {
			pcm := make([]byte, 1_600*2)
			for index := range pcm {
				pcm[index] = byte(index)
			}
			seq++
			return send(map[string]any{"type": "audio", "context_id": id, "seq": seq,
				"pcm16": base64.StdEncoding.EncodeToString(pcm)})
		}
		cancelled := make(chan struct{})
		for {
			_, payload, err := connection.Read(ctx)
			if err != nil {
				return
			}
			var message struct {
				Type      string `json:"type"`
				ContextID string `json:"context_id"`
				Text      string `json:"text"`
			}
			_ = json.Unmarshal(payload, &message)
			id := message.ContextID
			switch message.Type {
			case "context.open":
				_ = send(map[string]any{"type": "context.ready", "context_id": id, "sample_rate": 16_000,
					"model": "fake", "capabilities": map[string]any{
						"incremental_text": true, "nonterminal_flush": true, "input_granularity": "token"}})
			case "text.append":
				chars += len([]rune(message.Text))
				_ = send(map[string]any{"type": "text.accepted", "context_id": id, "chars": chars})
				if fake.appends != nil {
					fake.appends <- message.Text
				}
				for range strings.Fields(message.Text) {
					_ = audio(id)
				}
				if fake.endless {
					go func() {
						for {
							select {
							case <-cancelled:
								return
							case <-time.After(5 * time.Millisecond):
								if audio(id) != nil {
									return
								}
							}
						}
					}()
				}
			case "text.end":
				if fake.stall || fake.endless {
					continue
				}
				_ = send(map[string]any{"type": "audio.done", "context_id": id})
			case "context.cancel":
				close(cancelled)
				_ = send(map[string]any{"type": "context.cancelled", "context_id": id})
			}
		}
	}
}

func serve(t *testing.T, fake *fakeService) string {
	t.Helper()
	server := httptest.NewServer(fake.handler(t))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/tts/stream"
}

func TestAPlanStreamsResampledAudioWithOneFinalChunk(t *testing.T) {
	t.Parallel()
	adapter, err := speechsocket.New(speechsocket.Config{URL: serve(t, &fakeService{}), OutputRateHz: 24_000})
	if err != nil {
		t.Fatal(err)
	}
	var chunks []v1.SpeechChunk
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = adapter.Stream(ctx, v1.SpeechPlan{CandidateID: "utt-1", Text: "one two three"}, func(chunk v1.SpeechChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	samples, finals := 0, 0
	for _, chunk := range chunks {
		if chunk.CandidateID != "utt-1" || chunk.SampleRateHz != 24_000 {
			t.Fatalf("chunk = %+v", chunk)
		}
		samples += len(chunk.PCM16LE) / 2
		if chunk.Final {
			finals++
		}
	}
	// Three words of 100 ms at 16 kHz is 300 ms, 7,200 samples at 24 kHz.
	if samples < 7_100 || samples > 7_300 || finals != 1 || !chunks[len(chunks)-1].Final {
		t.Fatalf("got %d samples and %d final chunks", samples, finals)
	}
}

// The plan's acceptance test for incremental synthesis: audio for an
// unfinished prefix arrives before the rest of the text is sent, and the rest
// continues in the same context.
func TestAudioArrivesBeforeTheTextEnds(t *testing.T) {
	t.Parallel()
	fake := &fakeService{appends: make(chan string, 8)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	speech, err := speechsocket.Open(ctx, speechsocket.Config{URL: serve(t, fake)})
	if err != nil {
		t.Fatal(err)
	}
	defer speech.Close()
	if !speech.Capabilities().IncrementalText || speech.SampleRate() != 16_000 {
		t.Fatalf("ready = %+v at %d Hz", speech.Capabilities(), speech.SampleRate())
	}
	if err := speech.Append(ctx, "The weather today "); err != nil {
		t.Fatal(err)
	}
	select {
	case <-speech.Audio():
	case <-ctx.Done():
		t.Fatal("no audio for the unfinished prefix")
	}
	if err := speech.Append(ctx, "is sunny"); err != nil {
		t.Fatal(err)
	}
	if err := speech.End(ctx); err != nil {
		t.Fatal(err)
	}
	remaining := 0
	for range speech.Audio() {
		remaining++
	}
	if err := speech.Err(); err != nil {
		t.Fatal(err)
	}
	if remaining != 4 || speech.Sent() != int64(len("The weather today is sunny")) {
		t.Fatalf("remaining chunks %d, sent %d", remaining, speech.Sent())
	}
}

func TestCancelDeliversNoFurtherAudio(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	speech, err := speechsocket.Open(ctx, speechsocket.Config{URL: serve(t, &fakeService{endless: true})})
	if err != nil {
		t.Fatal(err)
	}
	if err := speech.Append(ctx, "keep talking"); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		<-speech.Audio()
	}
	if err := speech.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	// Whatever was already queued before Cancel may drain; nothing generated
	// afterwards may appear, and the channel must close.
	drained := 0
	for range speech.Audio() {
		drained++
		if drained > 256 {
			t.Fatal("audio kept arriving after cancel")
		}
	}
	if !errors.Is(speech.Err(), context.Canceled) {
		t.Fatalf("err = %v", speech.Err())
	}
}

func TestAServiceThatGoesSilentAfterEndFailsTheContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	speech, err := speechsocket.Open(ctx, speechsocket.Config{
		URL: serve(t, &fakeService{stall: true}), IdleTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := speech.Append(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := speech.End(ctx); err != nil {
		t.Fatal(err)
	}
	for range speech.Audio() {
	}
	if err := speech.Err(); err == nil || !strings.Contains(err.Error(), "sent nothing") {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamStopsWhenItsContextIsCancelled(t *testing.T) {
	t.Parallel()
	adapter, err := speechsocket.New(speechsocket.Config{URL: serve(t, &fakeService{endless: true})})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	received := 0
	err = adapter.Stream(ctx, v1.SpeechPlan{CandidateID: "u", Text: "go on"}, func(chunk v1.SpeechChunk) error {
		received++
		if received == 3 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}
