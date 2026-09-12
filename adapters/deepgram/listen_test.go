package deepgram

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/coder/websocket"
)

// fakeDeepgram is a WebSocket that replays a scripted result stream. The
// script is what the real service sends: interim hypotheses that get replaced,
// then a final segment that settles.
type fakeDeepgram struct {
	server *httptest.Server
	query  chan string
	auth   chan string
	audio  chan int

	accepts    atomic.Int32
	finalizes  atomic.Int32
	keepAlives atomic.Int32
	mu         sync.Mutex
	live       []*websocket.Conn
	// next is the script position, shared by every connection: the script is
	// what the service says next, whichever socket carries it, so a stream
	// repaired mid-conversation continues rather than replays.
	next int
}

// dropConnections closes every live connection from the server side, the way
// the service does after ten silent seconds.
func (fake *fakeDeepgram) dropConnections() {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, connection := range fake.live {
		_ = connection.Close(websocket.StatusGoingAway, "dropped")
	}
	fake.live = nil
}

func newFakeDeepgram(t *testing.T, script []string) *fakeDeepgram {
	t.Helper()
	fake := &fakeDeepgram{
		query: make(chan string, 1), auth: make(chan string, 1), audio: make(chan int, 64),
	}
	fake.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// Tests that inspect the request read these once; a later connection
		// on the same fake must not block behind an unread value, because
		// httptest waits for every handler before it will close.
		select {
		case fake.query <- request.URL.RawQuery:
		default:
		}
		select {
		case fake.auth <- request.Header.Get("Authorization"):
		default:
		}
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer connection.CloseNow()
		fake.accepts.Add(1)
		fake.mu.Lock()
		fake.live = append(fake.live, connection)
		fake.mu.Unlock()
		take := func() (string, bool) {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if fake.next >= len(script) {
				return "", false
			}
			fake.next++
			return script[fake.next-1], true
		}
		for {
			kind, payload, err := connection.Read(request.Context())
			if err != nil {
				return
			}
			if kind == websocket.MessageBinary {
				select {
				case fake.audio <- len(payload):
				default:
				}
				if entry, ok := take(); ok {
					_ = connection.Write(request.Context(), websocket.MessageText, []byte(entry))
				}
				continue
			}
			var control struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(payload, &control)
			switch control.Type {
			case "KeepAlive":
				fake.keepAlives.Add(1)
			case "Finalize":
				// Finalize flushes what the service holds for the utterance
				// and answers with results marked from_finalize; the stream
				// stays open. The script is per connection, so a persistent
				// stream reads its later entries at later frames: only the
				// results already owed to this utterance's audio are flushed.
				fake.finalizes.Add(1)
				_ = connection.Write(request.Context(), websocket.MessageText, []byte(finalizeEvent("")))
			default:
				// A CloseStream flushes whatever is left and ends the stream,
				// which is exactly the behaviour a closing Finalize depends on.
				for {
					entry, ok := take()
					if !ok {
						break
					}
					_ = connection.Write(request.Context(), websocket.MessageText, []byte(entry))
				}
				_ = connection.Close(websocket.StatusNormalClosure, "stream closed")
				return
			}
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *fakeDeepgram) url() string {
	return "ws" + strings.TrimPrefix(fake.server.URL, "http")
}

func results(transcript string, final bool) string {
	return resultEvent(transcript, final, false)
}

// finalizeEvent is the result Deepgram sends in answer to Finalize.
func finalizeEvent(transcript string) string {
	payload, _ := json.Marshal(map[string]any{
		"type": "Results", "is_final": true, "from_finalize": true,
		"channel": map[string]any{"alternatives": []map[string]any{{"transcript": transcript}}},
	})
	return string(payload)
}

func resultEvent(transcript string, final, speechFinal bool) string {
	payload, _ := json.Marshal(map[string]any{
		"type": "Results", "is_final": final, "speech_final": speechFinal,
		"channel": map[string]any{"alternatives": []map[string]any{{"transcript": transcript}}},
	})
	return string(payload)
}

func tone(samples int) []byte {
	pcm := make([]byte, samples*2)
	for index := range samples {
		binary.LittleEndian.PutUint16(pcm[index*2:], uint16(int16(math.Round(6000*math.Sin(float64(index)/6)))))
	}
	return pcm
}

func push(t *testing.T, listener *Listener, index uint64, offset uint64, samples int) []v1.PerceptionRevision {
	t.Helper()
	revisions, err := listener.PushFrame(context.Background(), v1.AudioFrame{
		Index: index, SampleOffset: offset, SampleRateHz: 16_000, PCM16LE: tone(samples),
	})
	if err != nil {
		t.Fatalf("push %d: %v", index, err)
	}
	return revisions
}

// Interim results are hypotheses; a final segment settles. The transcript the
// runtime sees has to reflect that, or a corrected word would stay corrected
// only until the next frame.
func TestInterimResultsAreReplacedAndFinalSegmentsAccumulate(t *testing.T) {
	t.Parallel()
	fake := newFakeDeepgram(t, []string{
		results("what is", false),
		results("what is my", false),
		results("what is my balance", true),
		results("please", false),
	})
	listener, err := NewListener(ListenConfig{URL: fake.url(), APIKey: "secret", Model: "nova-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	push(t, listener, 0, 0, 800)
	// The reader is a separate goroutine, so the first drain may be empty.
	// What matters is where the transcript ends up, not which push carried it.
	var text string
	var stable string
	deadline := time.Now().Add(2 * time.Second)
	offset := uint64(800)
	for index := uint64(1); time.Now().Before(deadline); index++ {
		for _, revision := range push(t, listener, index, offset, 800) {
			if revision.Final {
				t.Fatal("a mid-stream revision must not be marked final")
			}
			text = revision.StableText + revision.UnstableText
			stable = revision.StableText
		}
		offset += 800
		if strings.Contains(text, "please") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if text != "what is my balance please" {
		t.Fatalf("settled text plus the current hypothesis = %q", text)
	}
	if stable != "what is my balance" {
		t.Fatalf("Deepgram's settled segment must be exposed as stable text, got %q", stable)
	}

	final, err := listener.Finalize(context.Background(), offset)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Final || final.StableText != "what is my balance please" {
		t.Fatalf("final revision: %+v", final)
	}
}

// The audio format is declared from the first frame, so nothing is resampled
// on the way in.
func TestTheStreamDeclaresTheCallersOwnSampleRate(t *testing.T) {
	t.Parallel()
	fake := newFakeDeepgram(t, []string{results("hi", true)})
	listener, err := NewListener(ListenConfig{
		URL: fake.url(), APIKey: "secret", Model: "nova-test", Language: "en-US",
		Keyterms: []string{"sea bass", "fennel"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	push(t, listener, 0, 0, 400)

	query := <-fake.query
	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	if got := values["keyterm"]; !reflect.DeepEqual(got, []string{"sea bass", "fennel"}) {
		t.Fatalf("Deepgram keyterm query values = %q", got)
	}
	for _, want := range []string{
		"encoding=linear16", "sample_rate=16000", "channels=1",
		"interim_results=true", "model=nova-test", "language=en-US",
	} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q is missing %q", query, want)
		}
	}
	if auth := <-fake.auth; auth != "Token secret" {
		t.Errorf("Deepgram authenticates with a Token header, got %q", auth)
	}
	if sent := <-fake.audio; sent != 800 {
		t.Errorf("audio must reach the service unresampled, got %d bytes", sent)
	}
}

func TestSpeechFinalExposesDeepgramsEndpointAndEndpointingQuery(t *testing.T) {
	t.Parallel()
	fake := newFakeDeepgram(t, []string{resultEvent("that is all", true, true)})
	listener, err := NewListener(ListenConfig{
		URL: fake.url(), APIKey: "secret", Endpointing: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	push(t, listener, 0, 0, 400)
	query := <-fake.query
	for _, want := range []string{"vad_events=true", "endpointing=300"} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q is missing %q", query, want)
		}
	}

	// The socket reader is asynchronous. Advance the stream until the result is
	// folded into listener state; the endpoint must not depend on a changed text
	// revision being emitted to its caller.
	offset := uint64(400)
	for index, deadline := uint64(1), time.Now().Add(2*time.Second); time.Now().Before(deadline); index++ {
		push(t, listener, index, offset, 400)
		offset += 400
		if listener.SpeechEndpointed() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("speech_final was not exposed as a recogniser endpoint")
}

func TestAReportedErrorFailsTheUtterance(t *testing.T) {
	t.Parallel()
	fake := newFakeDeepgram(t, []string{
		`{"type":"Error","description":"unsupported encoding"}`,
	})
	listener, err := NewListener(ListenConfig{URL: fake.url(), APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	push(t, listener, 0, 0, 400)
	time.Sleep(50 * time.Millisecond)
	_, err = listener.Finalize(context.Background(), 400)
	if err == nil || !strings.Contains(err.Error(), "unsupported encoding") {
		t.Fatalf("a reported error must fail the utterance: %v", err)
	}
}

func TestFramesMustBeContiguousAndAnEmptyUtteranceCannotFinalize(t *testing.T) {
	t.Parallel()
	fake := newFakeDeepgram(t, nil)
	listener, err := NewListener(ListenConfig{URL: fake.url(), APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if _, err := listener.Finalize(context.Background(), 0); err == nil {
		t.Fatal("an utterance with no audio must not be finalized")
	}
	push(t, listener, 0, 0, 400)
	if _, err := listener.PushFrame(context.Background(), v1.AudioFrame{
		Index: 7, SampleOffset: 400, SampleRateHz: 16_000, PCM16LE: tone(400),
	}); err == nil {
		t.Fatal("a frame out of sequence must be refused")
	}
	if _, err := listener.PushFrame(context.Background(), v1.AudioFrame{
		Index: 1, SampleOffset: 400, SampleRateHz: 24_000, PCM16LE: tone(400),
	}); err == nil {
		t.Fatal("a sample-rate change mid-utterance must be refused")
	}
}

func TestAMissingCredentialIsRefusedBeforeDialling(t *testing.T) {
	t.Parallel()
	if _, err := NewListener(ListenConfig{URL: "wss://example.invalid/v1/listen"}); err == nil {
		t.Fatal("Deepgram requires a key")
	}
	if _, err := NewListener(ListenConfig{URL: "https://example.invalid", APIKey: "k"}); err == nil {
		t.Fatal("a non-WebSocket URL must be refused")
	}
}
