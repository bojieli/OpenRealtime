package bench_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/internal/testserver"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/coder/websocket"
)

func TestSessionDrivesAudioVideoAndToolsOverWebRTC(t *testing.T) {
	stack := testserver.Start(t, testserver.Config{
		Transcript: "open the launch review and present it",
		ToolName:   "meeting.read_launch_review", ToolArguments: `{}`,
		Narration: "The shared screen shows the launch review.",
	})
	canvas := image.NewRGBA(image.Rect(0, 0, 320, 180))
	canvas.Set(20, 20, color.RGBA{R: 255, G: 80, B: 20, A: 255})
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, canvas, &jpeg.Options{Quality: 75}); err != nil {
		t.Fatal(err)
	}
	frame := encoded.Bytes()
	var handled atomic.Bool
	var unexpectedTool atomic.Bool
	var captures atomic.Int32
	samples := make([]int16, 24_000)
	for index := range samples {
		samples[index] = int16(8_000 * math.Sin(2*math.Pi*220*float64(index)/24_000))
	}

	transcript, err := bench.PlaySamples(context.Background(), bench.SessionConfig{
		// A ws:// endpoint asks the driver to terminate WebRTC with the same
		// adapter used by serve; this covers production servers that expose only
		// their canonical protocol port.
		Endpoint: stack.ProtocolURL, Transport: bench.TransportWebRTC,
		Timeout: 15 * time.Second, TrailingSilence: 700 * time.Millisecond,
		Tools: []json.RawMessage{json.RawMessage(
			`{"type":"function","name":"meeting.read_launch_review","description":"Read the launch review","parameters":{"type":"object","properties":{}}}`,
		)},
		HandleTool: func(_ context.Context, request bench.ToolRequest) (json.RawMessage, error) {
			if request.Name != "meeting.read_launch_review" {
				unexpectedTool.Store(true)
				return nil, fmt.Errorf("unexpected tool %q", request.Name)
			}
			handled.Store(true)
			return json.RawMessage(`{"conversion_rate":"18.4%"}`), nil
		},
		Video: []bench.VideoStream{{
			Source: "screen", Width: 320, Height: 180, Interval: 100 * time.Millisecond,
			Capture: func(context.Context) ([]byte, error) {
				captures.Add(1)
				return frame, nil
			},
		}},
	}, samples)
	if err != nil {
		t.Fatalf("WebRTC meeting session: %v\n%+v", err, transcript.Moments)
	}
	if !handled.Load() {
		t.Fatal("the tool call did not cross the WebRTC data channel")
	}
	if unexpectedTool.Load() {
		t.Fatal("an unexpected tool crossed the WebRTC data channel")
	}
	if captures.Load() == 0 {
		t.Fatal("the screen sensor did not capture a frame")
	}
	var heard, sawFrame, heardAgent bool
	for _, moment := range transcript.Moments {
		heard = heard || moment.Kind == bench.MomentTranscript &&
			strings.Contains(moment.Text, "launch review")
		sawFrame = sawFrame || moment.Kind == bench.MomentVideoFrame
		heardAgent = heardAgent || moment.Kind == bench.MomentAgentAudio && moment.AudioMS > 0
	}
	if !heard || !sawFrame || !heardAgent {
		t.Fatalf("incomplete WebRTC evidence: transcript=%t video=%t output_audio=%t\n%+v",
			heard, sawFrame, heardAgent, transcript.Moments)
	}
	for _, moment := range transcript.Moments {
		if moment.Kind == bench.MomentReady && moment.AtMS > 100 {
			t.Fatalf("WebRTC setup leaked into the episode clock: ready at %.1f ms", moment.AtMS)
		}
	}
}

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

func TestConcurrentToolDoesNotStopCollectingMeetingEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{})
		if err != nil {
			return
		}
		defer connection.Close(websocket.StatusNormalClosure, "test complete")
		for {
			_, raw, err := connection.Read(request.Context())
			if err != nil {
				return
			}
			var message map[string]any
			if json.Unmarshal(raw, &message) != nil || message["type"] != "session.update" {
				continue
			}
			tool, _ := json.Marshal(map[string]any{
				"type": "response.function_call_arguments.done", "call_id": "analysis_1",
				"name": "meeting.analyze", "arguments": `{}`,
			})
			_ = connection.Write(request.Context(), websocket.MessageText, tool)
			time.Sleep(30 * time.Millisecond)
			observation, _ := json.Marshal(map[string]any{
				"type": openrealtime.EventObservationAdded, "observer": "video",
				"source": "shared-screen", "text": "the risks slide is visible",
			})
			_ = connection.Write(request.Context(), websocket.MessageText, observation)
		}
	}))
	t.Cleanup(server.Close)

	transcript, err := bench.PlaySamples(context.Background(), bench.SessionConfig{
		Endpoint:        "ws" + strings.TrimPrefix(server.URL, "http"),
		Timeout:         5 * time.Second,
		WorkingTimeout:  2 * time.Second,
		TrailingSilence: time.Millisecond,
		ConcurrentTools: true,
		HandleTool: func(context.Context, bench.ToolRequest) (json.RawMessage, error) {
			time.Sleep(250 * time.Millisecond)
			return json.RawMessage(`{"analysis":"complete"}`), nil
		},
	}, nil)
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	observationAt, resultAt := -1.0, -1.0
	for _, moment := range transcript.Moments {
		switch moment.Kind {
		case bench.MomentObservation:
			observationAt = moment.AtMS
		case bench.MomentToolResult:
			resultAt = moment.AtMS
		}
	}
	if observationAt < 0 || resultAt < 0 || observationAt >= resultAt {
		t.Fatalf("events were serialized behind the tool: observation=%.0f result=%.0f moments=%+v",
			observationAt, resultAt, transcript.Moments)
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

func TestSessionCapturesNegotiatedRuntimeEvidence(t *testing.T) {
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
			var message map[string]any
			if json.Unmarshal(raw, &message) != nil || message["type"] != "session.update" {
				continue
			}
			updated, _ := json.Marshal(map[string]any{"type": "session.updated", "session": map[string]any{}})
			_ = connection.Write(request.Context(), websocket.MessageText, updated)
			evidence, _ := json.Marshal(map[string]any{
				"type": openrealtime.EventDebug, "category": "session", "name": "session.updated",
				"attributes": map[string]any{"runtime": map[string]any{
					"binding": "sidecar", "profile": "voice",
					"ownership": map[string]any{
						"perception": "model", "fast_cognition": "model", "slow_cognition": "engine",
						"action": "model", "interaction": "engine", "floor": "model",
					},
					"stack": map[string]any{
						"audio_input": true, "audio_output": true, "turn_generation": true,
						"native_floor": true, "native_interaction": true, "interaction_acts": true,
					},
					"interaction": map[string]any{
						"evidence": "transcript", "transport": "sidecar", "protocol_version": 2,
						"act_handoff": "typed",
					},
					"tools": map[string]any{
						"fast": "propose", "slow": "execute", "authorization": "engine",
						"execution": "engine-or-client",
					},
				}},
			})
			_ = connection.Write(request.Context(), websocket.MessageText, evidence)
		}
	}))
	t.Cleanup(server.Close)

	transcript, err := bench.PlaySamples(context.Background(), bench.SessionConfig{
		Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"), Timeout: 5 * time.Second,
		TrailingSilence: time.Millisecond, CaptureRuntimeEvidence: true,
	}, nil)
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	if transcript.Runtime == nil || transcript.Runtime.Binding != "sidecar" ||
		transcript.Runtime.Interaction.ActHandoff != "typed" ||
		transcript.Runtime.Tools.Authorization != "engine" {
		t.Fatalf("runtime evidence was not retained: %+v", transcript.Runtime)
	}
}
