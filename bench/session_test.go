package bench_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/coder/websocket"
)

type realtimeStub struct {
	mu       sync.Mutex
	messages []map[string]any
	done     chan struct{}
	once     sync.Once
}

func (stub *realtimeStub) append(message map[string]any) {
	stub.mu.Lock()
	stub.messages = append(stub.messages, message)
	stub.mu.Unlock()
}

func (stub *realtimeStub) snapshot() []map[string]any {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return append([]map[string]any(nil), stub.messages...)
}

func (stub *realtimeStub) serve(writer http.ResponseWriter, request *http.Request) {
	connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{})
	if err != nil {
		stub.once.Do(func() { close(stub.done) })
		return
	}
	defer connection.Close(websocket.StatusNormalClosure, "test complete")
	defer stub.once.Do(func() { close(stub.done) })
	sentEvents := false
	for {
		_, raw, err := connection.Read(request.Context())
		if err != nil {
			return
		}
		var message map[string]any
		if json.Unmarshal(raw, &message) != nil {
			continue
		}
		stub.append(message)
		if message["type"] != openrealtime.EventVideoFrameAppend || sentEvents {
			continue
		}
		sentEvents = true
		observation, _ := json.Marshal(map[string]any{
			"type": openrealtime.EventObservationAdded, "observer": "video",
			"source": "screen", "text": "violet control visible",
		})
		_ = connection.Write(request.Context(), websocket.MessageText, observation)
		tool, _ := json.Marshal(map[string]any{
			"type": "response.function_call_arguments.done", "call_id": "fast_click_1",
			"name": "computer.click_element", "arguments": `{"source":"screen","element_id":"1"}`,
		})
		_ = connection.Write(request.Context(), websocket.MessageText, tool)
	}
}

func TestSessionNegotiatesAndStreamsLiveVideo(t *testing.T) {
	stub := &realtimeStub{done: make(chan struct{})}
	server := httptest.NewServer(http.HandlerFunc(stub.serve))
	t.Cleanup(server.Close)

	var ready atomic.Bool
	var captured atomic.Int32
	var capturedBeforeReady atomic.Bool
	var handled atomic.Bool
	transcript, err := bench.PlaySamples(context.Background(), bench.SessionConfig{
		Endpoint:        "ws" + strings.TrimPrefix(server.URL, "http"),
		Timeout:         10 * time.Second,
		TrailingSilence: time.Millisecond,
		Ready: func(context.Context) error {
			ready.Store(true)
			return nil
		},
		Video: []bench.VideoStream{{
			Source: "screen", Width: 640, Height: 360, Interval: 50 * time.Millisecond,
			Capture: func(context.Context) ([]byte, error) {
				if !ready.Load() {
					capturedBeforeReady.Store(true)
				}
				captured.Add(1)
				return []byte{0xff, 0xd8, 0xff, 0xd9}, nil
			},
		}},
		HandleTool: func(_ context.Context, request bench.ToolRequest) (json.RawMessage, error) {
			if request.CallID != "fast_click_1" || request.Name != "computer.click_element" ||
				!strings.Contains(string(request.Arguments), `"element_id":"1"`) || request.Received.IsZero() {
				t.Errorf("unexpected tool request: %+v", request)
			}
			handled.Store(true)
			return json.RawMessage(`{"ok":true}`), nil
		},
	}, nil)
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	select {
	case <-stub.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the benchmark client did not close its session")
	}
	if capturedBeforeReady.Load() {
		t.Fatal("video capture began before the environment-ready hook")
	}
	if captured.Load() < 2 {
		t.Fatalf("a live stream was sampled only %d time(s)", captured.Load())
	}
	if !handled.Load() {
		t.Fatal("the context-aware tool handler was not called")
	}

	messages := stub.snapshot()
	if len(messages) < 3 {
		t.Fatalf("too few protocol messages: %+v", messages)
	}
	updateIndex, sourceIndex, firstFrameIndex := -1, -1, -1
	frames := 0
	var sawToolOutput bool
	for index, message := range messages {
		switch message["type"] {
		case "session.update":
			updateIndex = index
			session, _ := message["session"].(map[string]any)
			extension, _ := session["openrealtime"].(map[string]any)
			if extension["version"] != float64(openrealtime.Version) {
				t.Fatalf("video session did not negotiate OpenRealtime %d: %+v", openrealtime.Version, message)
			}
			supports, _ := extension["supports"].([]any)
			if len(supports) != 3 {
				t.Fatalf("video feature negotiation is incomplete: %+v", extension)
			}
		case openrealtime.EventVideoSourceUpdate:
			sourceIndex = index
			if message["source"] != "screen" || message["width"] != float64(640) || message["height"] != float64(360) {
				t.Fatalf("wrong video source declaration: %+v", message)
			}
		case openrealtime.EventVideoFrameAppend:
			if firstFrameIndex < 0 {
				firstFrameIndex = index
			}
			frames++
			encoded, _ := message["frame"].(string)
			decoded, decodeErr := base64.StdEncoding.DecodeString(encoded)
			if decodeErr != nil || string(decoded) != string([]byte{0xff, 0xd8, 0xff, 0xd9}) {
				t.Fatalf("frame payload was not preserved: %+v", message)
			}
		case "conversation.item.create":
			item, _ := message["item"].(map[string]any)
			if item["call_id"] == "fast_click_1" && item["output"] == `{"ok":true}` {
				sawToolOutput = true
			}
		}
	}
	if updateIndex < 0 || sourceIndex <= updateIndex || firstFrameIndex <= sourceIndex {
		t.Fatalf("session, source, and frame were not ordered correctly: update=%d source=%d frame=%d",
			updateIndex, sourceIndex, firstFrameIndex)
	}
	if frames < 2 {
		t.Fatalf("the wire carried only %d live frame(s)", frames)
	}
	if !sawToolOutput {
		t.Fatal("the handled action result did not return over the protocol")
	}

	kinds := make(map[string]int)
	var observation bench.Moment
	for _, moment := range transcript.Moments {
		kinds[moment.Kind]++
		if moment.Kind == bench.MomentObservation {
			observation = moment
		}
	}
	for _, kind := range []string{
		bench.MomentReady, bench.MomentVideoFrame, bench.MomentObservation,
		bench.MomentToolCall, bench.MomentToolResult,
	} {
		if kinds[kind] == 0 {
			t.Errorf("transcript is missing %q: %+v", kind, transcript.Moments)
		}
	}
	if observation.Observer != "video" || observation.Source != "screen" || observation.Text != "violet control visible" {
		t.Fatalf("observation provenance was lost: %+v", observation)
	}
}

func TestSessionRejectsInvalidVideoBeforeDialling(t *testing.T) {
	for name, stream := range map[string]bench.VideoStream{
		"missing source":  {Width: 640, Height: 360, Capture: func(context.Context) ([]byte, error) { return nil, nil }},
		"missing width":   {Source: "screen", Height: 360, Capture: func(context.Context) ([]byte, error) { return nil, nil }},
		"missing capture": {Source: "screen", Width: 640, Height: 360},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := bench.PlaySamples(context.Background(), bench.SessionConfig{
				Endpoint: "ws://127.0.0.1:1", Video: []bench.VideoStream{stream},
			}, nil)
			if err == nil || strings.Contains(err.Error(), "dial") {
				t.Fatalf("invalid video reached the network: %v", err)
			}
		})
	}
}

func TestConnectedConversationTimeoutIsTypedAndRetainsEvidence(t *testing.T) {
	stub := &realtimeStub{done: make(chan struct{})}
	server := httptest.NewServer(http.HandlerFunc(stub.serve))
	t.Cleanup(server.Close)

	transcript, err := bench.PlaySamples(context.Background(), bench.SessionConfig{
		Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"),
		Timeout:  150 * time.Millisecond,
		Ready:    func(context.Context) error { return nil },
		Video: []bench.VideoStream{{
			Source: "screen", Width: 2, Height: 2, Interval: 20 * time.Millisecond,
			Capture: func(context.Context) ([]byte, error) {
				return []byte{0xff, 0xd8, 0xff, 0xd9}, nil
			},
		}},
	}, nil)
	if !errors.Is(err, bench.ErrConversationTimeout) {
		t.Fatalf("timeout = %v, want ErrConversationTimeout", err)
	}
	var ready, frame bool
	for _, moment := range transcript.Moments {
		ready = ready || moment.Kind == bench.MomentReady
		frame = frame || moment.Kind == bench.MomentVideoFrame
	}
	if !ready || !frame {
		t.Fatalf("a timed-out connected run lost its evidence: %+v", transcript.Moments)
	}
}

func TestSessionJoinsAnInFlightVideoCaptureBeforeReturning(t *testing.T) {
	captureStarted := make(chan struct{})
	releaseCapture := make(chan struct{})
	var startedOnce sync.Once
	var captureCanceled atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{})
		if err != nil {
			return
		}
		defer connection.Close(websocket.StatusNormalClosure, "fixture complete")
		for {
			_, raw, err := connection.Read(request.Context())
			if err != nil {
				return
			}
			var message struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(raw, &message) != nil || message.Type != openrealtime.EventVideoSourceUpdate {
				continue
			}
			<-captureStarted
			time.Sleep(50 * time.Millisecond)
			return
		}
	}))
	t.Cleanup(server.Close)

	finished := make(chan error, 1)
	go func() {
		_, err := bench.PlaySamples(context.Background(), bench.SessionConfig{
			Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"), Timeout: 2 * time.Second,
			TrailingSilence: time.Millisecond,
			Ready:           func(context.Context) error { return nil },
			Video: []bench.VideoStream{{
				Source: "screen", Width: 2, Height: 2, Interval: time.Second,
				Capture: func(ctx context.Context) ([]byte, error) {
					startedOnce.Do(func() { close(captureStarted) })
					select {
					case <-releaseCapture:
						return nil, nil
					case <-ctx.Done():
						captureCanceled.Store(true)
						return nil, context.Cause(ctx)
					}
				},
			}},
		}, nil)
		finished <- err
	}()

	select {
	case <-captureStarted:
	case <-time.After(time.Second):
		t.Fatal("video capture never started")
	}
	select {
	case err := <-finished:
		t.Fatalf("session returned before its in-flight capture: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(releaseCapture)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("play: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session did not return after its capture finished")
	}
	if captureCanceled.Load() {
		t.Fatal("normal session shutdown canceled a shared-surface operation")
	}
}

func TestSessionProtocolFailureIsTypedAndRetainsEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{})
		if err != nil {
			return
		}
		defer connection.Close(websocket.StatusNormalClosure, "fixture complete")
		sent := false
		for {
			_, _, err := connection.Read(request.Context())
			if err != nil {
				return
			}
			if sent {
				continue
			}
			sent = true
			failure, _ := json.Marshal(map[string]any{
				"type": "error", "error": map[string]any{"message": "recogniser unavailable"},
			})
			_ = connection.Write(request.Context(), websocket.MessageText, failure)
		}
	}))
	t.Cleanup(server.Close)

	transcript, err := bench.PlaySamples(context.Background(), bench.SessionConfig{
		Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"),
		Timeout:  2 * time.Second, TrailingSilence: time.Millisecond,
	}, nil)
	if !errors.Is(err, bench.ErrSessionFailure) {
		t.Fatalf("failure = %v, want ErrSessionFailure", err)
	}
	if transcript.Failure != "recogniser unavailable" {
		t.Fatalf("session failure evidence was lost: %+v", transcript)
	}
	for _, moment := range transcript.Moments {
		if moment.Kind == bench.MomentError && moment.Text == "recogniser unavailable" {
			return
		}
	}
	t.Fatalf("session error moment was lost: %+v", transcript.Moments)
}
