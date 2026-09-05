package bench

import (
	"context"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/realtimeclient"
)

// The endpoint reports both when it noticed speech and where in the audio it
// says the speech began. Only the second answers how long detection took: the
// first also contains however long the notice spent coming back. A suite that
// keeps one of them cannot tell a slow detector from a slow wire, and the
// interruption category is decided inside a window of one second.
func TestSpeechActivityRetainsTheEndpointsOwnStreamPosition(t *testing.T) {
	recorder := &recorder{started: time.Now().Add(-2 * time.Second), audio: newSessionAudioRecorder(nil)}
	recorder.handle(context.Background(), nil, SessionConfig{}, realtimeclient.Event{
		Type: "input_audio_buffer.speech_started",
		Raw: []byte(`{"type":"input_audio_buffer.speech_started",` +
			`"audio_start_ms":6740,"item_id":"item_1"}`),
	})
	recorder.handle(context.Background(), nil, SessionConfig{}, realtimeclient.Event{
		Type: "input_audio_buffer.speech_stopped",
		Raw: []byte(`{"type":"input_audio_buffer.speech_stopped",` +
			`"audio_end_ms":9310,"item_id":"item_1"}`),
	})

	transcript := recorder.snapshot()
	if len(transcript.Moments) != 2 {
		t.Fatalf("moments = %+v, want a start and a stop", transcript.Moments)
	}
	start, stop := transcript.Moments[0], transcript.Moments[1]
	if start.Kind != MomentSpeechStarted || stop.Kind != MomentSpeechStopped {
		t.Fatalf("kinds = %q, %q", start.Kind, stop.Kind)
	}
	if start.StreamAtMS != 6740 {
		t.Fatalf("speech start stream position = %v ms, want the endpoint's audio_start_ms", start.StreamAtMS)
	}
	if stop.StreamAtMS != 9310 {
		t.Fatalf("speech stop stream position = %v ms, want the endpoint's audio_end_ms", stop.StreamAtMS)
	}
	if start.AtMS == start.StreamAtMS {
		t.Fatal("arrival time and stream position are the same number, so nothing separates detection from reporting")
	}
}

// An endpoint that omits the field must not be given an invented one.
func TestSpeechActivityWithoutAStreamPositionStaysZero(t *testing.T) {
	recorder := &recorder{started: time.Now(), audio: newSessionAudioRecorder(nil)}
	recorder.handle(context.Background(), nil, SessionConfig{}, realtimeclient.Event{
		Type: "input_audio_buffer.speech_started",
		Raw:  []byte(`{"type":"input_audio_buffer.speech_started"}`),
	})
	moments := recorder.snapshot().Moments
	if len(moments) != 1 || moments[0].StreamAtMS != 0 {
		t.Fatalf("moments = %+v, want one moment with no invented position", moments)
	}
}
