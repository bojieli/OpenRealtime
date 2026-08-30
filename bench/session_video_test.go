package bench

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/bojieli/OpenRealtime/realtimeclient"
	"github.com/coder/websocket"
)

type sessionVideoStub struct {
	send func(context.Context, any) error
}

type sessionVideoFailingJSON struct{ err error }

func (value sessionVideoFailingJSON) MarshalJSON() ([]byte, error) { return nil, value.err }

func (stub sessionVideoStub) Events() <-chan realtimeclient.Event {
	return make(chan realtimeclient.Event)
}

func (stub sessionVideoStub) Send(ctx context.Context, value any) error {
	if stub.send == nil {
		return nil
	}
	return stub.send(ctx, value)
}

func (sessionVideoStub) Err() error   { return nil }
func (sessionVideoStub) Close() error { return nil }

func newVideoTestRecorder() *recorder {
	recorder := &recorder{started: time.Now(), audio: newSessionAudioRecorder(nil)}
	recorder.beginEpisode()
	return recorder
}

func closedVideoStop() <-chan struct{} {
	stop := make(chan struct{})
	close(stop)
	return stop
}

func TestSessionVideoCaptureCopiesExactSuccessfullySentFrame(t *testing.T) {
	frame := []byte{0xff, 0xd8, 0xff, 0xd9}
	recorder := newVideoTestRecorder()
	var wirePayload string
	var wireTimestamp int64
	var sendReturnedAt float64
	client := sessionVideoStub{send: func(_ context.Context, value any) error {
		event, ok := value.(map[string]any)
		if !ok || event["type"] != openrealtime.EventVideoFrameAppend {
			t.Fatalf("sent event = %#v", value)
		}
		wirePayload, _ = event["frame"].(string)
		wireTimestamp, _ = event["timestamp_ms"].(int64)
		time.Sleep(5 * time.Millisecond)
		sendReturnedAt = recorder.at()
		return nil
	}}

	var captured SessionVideoCapture
	sink := newSessionVideoCaptureSink(func(capture SessionVideoCapture) error {
		captured = capture
		capture.Data[0] = 0
		capture.WireTimestamp = 0
		return nil
	})
	failures := make(chan error, 1)
	streamVideo(t.Context(), closedVideoStop(), client, recorder, VideoStream{
		Source: "screen", Width: 640, Height: 360, Interval: time.Hour,
		Capture: func(context.Context) ([]byte, error) { return frame, nil },
	}, sink, failures)

	select {
	case err := <-failures:
		t.Fatalf("streamVideo() error = %v", err)
	default:
	}
	decoded, err := base64.StdEncoding.DecodeString(wirePayload)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(decoded, []byte{0xff, 0xd8, 0xff, 0xd9}) ||
		!slices.Equal(frame, []byte{0xff, 0xd8, 0xff, 0xd9}) {
		t.Fatalf("callback mutation changed source/wire bytes: source=%x wire=%x", frame, decoded)
	}
	if captured.Source != "screen" || captured.Width != 640 || captured.Height != 360 ||
		captured.MediaType != "image/jpeg" || captured.WireTimestamp != wireTimestamp ||
		captured.EpisodeAtMS < sendReturnedAt ||
		!slices.Equal(captured.Data, []byte{0, 0xd8, 0xff, 0xd9}) {
		t.Fatalf("video capture = %+v, send returned at %.3f ms", captured, sendReturnedAt)
	}
	transcript := recorder.snapshot()
	if len(transcript.Moments) != 1 || transcript.Moments[0].Kind != MomentVideoFrame ||
		transcript.Moments[0].Source != "screen" {
		t.Fatalf("video moments = %+v", transcript.Moments)
	}
	encoded, err := json.Marshal(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), wirePayload) || strings.Contains(string(encoded), "image/jpeg") {
		t.Fatalf("transcript embedded retained video: %s", encoded)
	}
}

func TestSessionVideoCaptureSerializesAcrossStreamWorkers(t *testing.T) {
	recorder := newVideoTestRecorder()
	client := sessionVideoStub{}
	var active atomic.Int32
	var calls atomic.Int32
	var overlapped atomic.Bool
	var sourcesMu sync.Mutex
	var sources []string
	sink := newSessionVideoCaptureSink(func(capture SessionVideoCapture) error {
		if active.Add(1) != 1 {
			overlapped.Store(true)
		}
		time.Sleep(20 * time.Millisecond)
		sourcesMu.Lock()
		sources = append(sources, capture.Source)
		sourcesMu.Unlock()
		active.Add(-1)
		calls.Add(1)
		return nil
	})
	failures := make(chan error, 2)
	streams := []VideoStream{
		{Source: "screen", Width: 2, Height: 2, Interval: time.Hour,
			Capture: func(context.Context) ([]byte, error) {
				return []byte{0xff, 0xd8, 0xff, 0xd9}, nil
			}},
		{Source: "camera", Width: 2, Height: 2, Interval: time.Hour,
			Capture: func(context.Context) ([]byte, error) {
				return []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, nil
			}},
	}
	var workers sync.WaitGroup
	for _, stream := range streams {
		stream := stream
		workers.Add(1)
		go func() {
			defer workers.Done()
			streamVideo(t.Context(), closedVideoStop(), client, recorder, stream, sink, failures)
		}()
	}
	workers.Wait()
	if overlapped.Load() || active.Load() != 0 || calls.Load() != 2 {
		t.Fatalf("serialized callback state: overlap=%t active=%d calls=%d",
			overlapped.Load(), active.Load(), calls.Load())
	}
	select {
	case err := <-failures:
		t.Fatalf("streamVideo() error = %v", err)
	default:
	}
	sourcesMu.Lock()
	defer sourcesMu.Unlock()
	if len(sources) != 2 || !slices.Contains(sources, "screen") || !slices.Contains(sources, "camera") {
		t.Fatalf("captured sources = %v", sources)
	}
}

func TestSessionVideoCaptureRejectsUnsupportedAndUnsentFrames(t *testing.T) {
	sendFailure := errors.New("wire rejected frame")
	for _, test := range []struct {
		name      string
		frame     []byte
		sendError error
		match     string
		wantSends int32
	}{
		{name: "unsupported MIME", frame: []byte("not an image"),
			match: "unsupported video frame media type", wantSends: 0},
		{name: "send failure", frame: []byte{0xff, 0xd8, 0xff, 0xd9},
			sendError: sendFailure, match: "send video source", wantSends: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var sends atomic.Int32
			var captures atomic.Int32
			client := sessionVideoStub{send: func(context.Context, any) error {
				sends.Add(1)
				return test.sendError
			}}
			failures := make(chan error, 1)
			streamVideo(t.Context(), closedVideoStop(), client, newVideoTestRecorder(), VideoStream{
				Source: "screen", Width: 2, Height: 2, Interval: time.Hour,
				Capture: func(context.Context) ([]byte, error) { return test.frame, nil },
			}, newSessionVideoCaptureSink(func(SessionVideoCapture) error {
				captures.Add(1)
				return nil
			}), failures)
			err := <-failures
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("streamVideo() error = %v, want %q", err, test.match)
			}
			if test.sendError != nil && !errors.Is(err, test.sendError) {
				t.Fatalf("streamVideo() error = %v, want wrapped send error", err)
			}
			if sends.Load() != test.wantSends || captures.Load() != 0 {
				t.Fatalf("unsent frame calls: sends=%d captures=%d", sends.Load(), captures.Load())
			}
		})
	}
}

func TestSessionVideoCaptureSinkErrorAfterSendWinsProtocolSuccessAndDoesNotOverlapAudioCapture(t *testing.T) {
	frameReceived := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{})
		if err != nil {
			return
		}
		defer connection.Close(websocket.StatusNormalClosure, "fixture complete")
		responded := false
		for {
			_, raw, err := connection.Read(request.Context())
			if err != nil {
				return
			}
			var event struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(raw, &event) == nil && event.Type == openrealtime.EventVideoFrameAppend {
				select {
				case frameReceived <- struct{}{}:
				default:
				}
				if !responded {
					responded = true
					for _, response := range []map[string]any{
						{"type": "response.created"},
						{"type": "response.done"},
					} {
						encoded, _ := json.Marshal(response)
						if connection.Write(request.Context(), websocket.MessageText, encoded) != nil {
							return
						}
					}
				}
			}
		}
	}))
	t.Cleanup(server.Close)

	sinkFailure := errors.New("review video sink failed")
	var videoCalls atomic.Int32
	var audioCalls atomic.Int32
	var callbackActive atomic.Int32
	var callbacksOverlapped atomic.Bool
	transcript, err := PlaySamples(t.Context(), SessionConfig{
		Endpoint:          "ws" + strings.TrimPrefix(server.URL, "http"),
		Timeout:           2 * time.Second,
		TrailingSilence:   time.Millisecond,
		PostPlaybackQuiet: time.Millisecond,
		CaptureAudio: func(SessionAudioCapture) error {
			if callbackActive.Add(1) != 1 {
				callbacksOverlapped.Store(true)
			}
			audioCalls.Add(1)
			callbackActive.Add(-1)
			return nil
		},
		CaptureVideo: func(SessionVideoCapture) error {
			if callbackActive.Add(1) != 1 {
				callbacksOverlapped.Store(true)
			}
			videoCalls.Add(1)
			// Let the conversation collector reach its quiet edge first. The
			// joined video worker must still publish this finalization failure.
			time.Sleep(300 * time.Millisecond)
			callbackActive.Add(-1)
			return sinkFailure
		},
		Video: []VideoStream{{
			Source: "screen", Width: 2, Height: 2, Interval: time.Hour,
			Capture: func(context.Context) ([]byte, error) {
				return []byte{0xff, 0xd8, 0xff, 0xd9}, nil
			},
		}},
	}, nil)
	if err == nil || !errors.Is(err, sinkFailure) ||
		!strings.Contains(err.Error(), "capture sent video source") {
		t.Fatalf("PlaySamples() error = %v", err)
	}
	if videoCalls.Load() != 1 || audioCalls.Load() != 1 ||
		callbackActive.Load() != 0 || callbacksOverlapped.Load() {
		t.Fatalf("media callback state: video=%d audio=%d active=%d overlap=%t",
			videoCalls.Load(), audioCalls.Load(), callbackActive.Load(), callbacksOverlapped.Load())
	}
	select {
	case <-frameReceived:
	case <-time.After(time.Second):
		t.Fatal("sink ran for a frame the server never received")
	}
	var sentMoment bool
	for _, moment := range transcript.Moments {
		sentMoment = sentMoment || moment.Kind == MomentVideoFrame
	}
	if !sentMoment {
		t.Fatalf("sent frame missing from transcript: %+v", transcript.Moments)
	}
}

func TestSessionScheduledEncodingFailurePrecedesVideoSideEffects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{})
		if err != nil {
			return
		}
		defer connection.Close(websocket.StatusNormalClosure, "fixture complete")
		for {
			if _, _, err := connection.Read(request.Context()); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)

	sinkFailure := errors.New("video retention failed")
	scheduledFailure := errors.New("scheduled event encoding failed")
	var videoCalls atomic.Int32
	_, err := PlaySamples(t.Context(), SessionConfig{
		Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"), Timeout: 2 * time.Second,
		Realtime: true,
		CaptureVideo: func(SessionVideoCapture) error {
			videoCalls.Add(1)
			return sinkFailure
		},
		Video: []VideoStream{{
			Source: "screen", Width: 2, Height: 2, Interval: time.Hour,
			Capture: func(context.Context) ([]byte, error) {
				return []byte{0xff, 0xd8, 0xff, 0xd9}, nil
			},
		}},
		Scheduled: []ScheduledEvent{{
			AtMS: 100, Name: "fail-encoding",
			Event: map[string]any{"invalid": sessionVideoFailingJSON{err: scheduledFailure}},
		}},
	}, make([]int16, 4800))
	if err == nil || errors.Is(err, sinkFailure) || !errors.Is(err, scheduledFailure) {
		t.Fatalf("PlaySamples() error = %v, want only preflight scheduled failure", err)
	}
	if videoCalls.Load() != 0 {
		t.Fatalf("video capture calls = %d, want zero before preflight succeeds", videoCalls.Load())
	}
}
