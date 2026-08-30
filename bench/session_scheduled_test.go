package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestSessionScheduledCaptureUsesExactSuccessfulWireValueAndOwnsBytes(t *testing.T) {
	received := make(chan []byte, 1)
	server := scheduledCaptureServer(t, func(raw []byte) {
		select {
		case received <- append([]byte(nil), raw...):
		default:
		}
	})
	event := map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": "scheduled"}},
		},
	}
	wantJSON, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var captured SessionScheduledCapture
	transcript, err := PlaySamples(t.Context(), SessionConfig{
		Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"),
		Timeout:  2 * time.Second, TrailingSilence: time.Millisecond,
		PostPlaybackQuiet: time.Millisecond,
		Scheduled:         []ScheduledEvent{{AtMS: 0, Name: "fixture.input.1", Event: event}},
		CaptureScheduled: func(value SessionScheduledCapture) error {
			captured = value
			captured.EventJSON = append([]byte(nil), value.EventJSON...)
			for index := range value.EventJSON {
				value.EventJSON[index] ^= 0xff
			}
			return nil
		},
	}, []int16{1})
	if err != nil {
		t.Fatalf("PlaySamples() error = %v", err)
	}
	select {
	case raw := <-received:
		if !bytes.Equal(raw, wantJSON) {
			t.Fatalf("wire event = %s, want %s", raw, wantJSON)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not receive scheduled event")
	}
	if captured.AtMS != 0 || captured.Name != "fixture.input.1" ||
		captured.EventType != "conversation.item.create" ||
		!bytes.Equal(captured.EventJSON, wantJSON) {
		t.Fatalf("scheduled capture = %+v", captured)
	}
	if !hasScheduledMoment(transcript, "fixture.input.1") {
		t.Fatalf("successful scheduled capture transcript = %+v", transcript.Moments)
	}
}

func TestSessionScheduledCaptureFailureIsFailClosedAfterSend(t *testing.T) {
	received := make(chan struct{}, 1)
	server := scheduledCaptureServer(t, func([]byte) {
		select {
		case received <- struct{}{}:
		default:
		}
	})
	want := errors.New("scheduled evidence sink failed")
	transcript, err := PlaySamples(t.Context(), SessionConfig{
		Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"),
		Timeout:  2 * time.Second, TrailingSilence: time.Millisecond,
		Scheduled: []ScheduledEvent{{
			AtMS: 0, Name: "fixture.input.1",
			Event: map[string]any{"type": "conversation.item.create"},
		}},
		CaptureScheduled: func(SessionScheduledCapture) error { return want },
	}, []int16{1})
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("PlaySamples() error = %v, want capture failure", err)
	}
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("callback failure happened before successful send")
	}
	if hasScheduledMoment(transcript, "fixture.input.1") {
		t.Fatalf("failed capture emitted successful moment: %+v", transcript.Moments)
	}
}

func TestSessionScheduledSendCancellationNeverInvokesCapture(t *testing.T) {
	server := scheduledCaptureServer(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	var calls atomic.Int32
	transcript, err := PlaySamples(ctx, SessionConfig{
		Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"),
		Timeout:  2 * time.Second, TrailingSilence: time.Millisecond,
		Ready: func(context.Context) error {
			cancel()
			return nil
		},
		Scheduled: []ScheduledEvent{{
			AtMS: 0, Name: "fixture.input.1",
			Event: map[string]any{"type": "conversation.item.create"},
		}},
		CaptureScheduled: func(SessionScheduledCapture) error {
			calls.Add(1)
			return nil
		},
	}, []int16{1})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("PlaySamples() error = %v, want cancellation", err)
	}
	if calls.Load() != 0 || hasScheduledMoment(transcript, "fixture.input.1") {
		t.Fatalf("canceled send calls=%d moments=%+v", calls.Load(), transcript.Moments)
	}
}

func TestSessionScheduledCancellationDuringCallbackDoesNotPublishSuccess(t *testing.T) {
	server := scheduledCaptureServer(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	var calls atomic.Int32
	transcript, err := PlaySamples(ctx, SessionConfig{
		Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"),
		Timeout:  2 * time.Second, TrailingSilence: time.Millisecond,
		Scheduled: []ScheduledEvent{{
			AtMS: 0, Name: "fixture.input.1",
			Event: map[string]any{"type": "conversation.item.create"},
		}},
		CaptureScheduled: func(SessionScheduledCapture) error {
			calls.Add(1)
			cancel()
			return nil
		},
	}, []int16{1})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("PlaySamples() error = %v, want cancellation", err)
	}
	if calls.Load() != 1 || hasScheduledMoment(transcript, "fixture.input.1") {
		t.Fatalf("callback cancellation calls=%d moments=%+v", calls.Load(), transcript.Moments)
	}
}

func TestSessionScheduledPreflightRejectsIdentityAndByteBoundsBeforeDial(t *testing.T) {
	var dials atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		dials.Add(1)
	}))
	t.Cleanup(server.Close)
	tests := []struct {
		name   string
		events []ScheduledEvent
		want   string
	}{
		{
			name: "duplicate identity",
			events: []ScheduledEvent{
				{AtMS: 0, Name: "fixture.input.1", Event: map[string]any{"type": "first"}},
				{AtMS: 1, Name: "fixture.input.1", Event: map[string]any{"type": "second"}},
			},
			want: "repeated",
		},
		{
			name: "missing type",
			events: []ScheduledEvent{{
				AtMS: 0, Name: "fixture.input.1", Event: map[string]any{"payload": "value"},
			}},
			want: "protocol type",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			_, err := PlaySamples(t.Context(), SessionConfig{
				Endpoint:  "ws" + strings.TrimPrefix(server.URL, "http"),
				Scheduled: test.events,
				CaptureScheduled: func(SessionScheduledCapture) error {
					calls.Add(1)
					return nil
				},
			}, []int16{1})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("PlaySamples() error = %v, want %q", err, test.want)
			}
			if calls.Load() != 0 {
				t.Fatalf("rejected preflight invoked capture %d times", calls.Load())
			}
		})
	}
	if dials.Load() != 0 {
		t.Fatalf("rejected scheduled events opened %d connection(s)", dials.Load())
	}
}

func TestSessionScheduledEncodedByteBoundIsClosed(t *testing.T) {
	for _, size := range []int{0, maximumScheduledEventBytes + 1} {
		if err := validateScheduledEventBytes("fixture.input.1", size); err == nil ||
			!strings.Contains(err.Error(), "encoded bytes") {
			t.Fatalf("scheduled size %d error = %v", size, err)
		}
	}
	for _, size := range []int{1, maximumScheduledEventBytes} {
		if err := validateScheduledEventBytes("fixture.input.1", size); err != nil {
			t.Fatalf("scheduled size %d error = %v", size, err)
		}
	}
}

func scheduledCaptureServer(t *testing.T, receive func([]byte)) *httptest.Server {
	t.Helper()
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
			var event struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(raw, &event) == nil && event.Type == "conversation.item.create" && receive != nil {
				receive(raw)
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func hasScheduledMoment(transcript Transcript, name string) bool {
	for _, moment := range transcript.Moments {
		if moment.Kind == MomentScheduled && moment.Name == name {
			return true
		}
	}
	return false
}
