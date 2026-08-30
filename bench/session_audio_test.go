package bench

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestSessionAudioRecorderUsesOnePlayoutClockAndReturnsOwnedCopies(t *testing.T) {
	recorder := newSessionAudioRecorder([]int16{1, 2, 3})
	recorder.beginEpisode()
	recorder.addAgent(100, make([]int16, 2_400))
	// This delta arrived halfway through the preceding 100 ms chunk. A single
	// speaker queues it after that chunk instead of mixing it over itself.
	recorder.addAgent(150, []int16{7, 8})

	capture := recorder.snapshot()
	if capture.SampleRateHz != 24_000 || !slices.Equal(capture.RoomPCM16, []int16{1, 2, 3}) {
		t.Fatalf("capture input = %+v", capture)
	}
	if len(capture.Agent) != 2 || capture.Agent[0].AtMS != 100 || capture.Agent[1].AtMS != 200 {
		t.Fatalf("agent playout chunks = %+v", capture.Agent)
	}
	capture.RoomPCM16[0] = 99
	capture.Agent[1].PCM16[0] = 99
	again := recorder.snapshot()
	if again.RoomPCM16[0] != 1 || again.Agent[1].PCM16[0] != 7 {
		t.Fatal("capture recipient mutated the recorder's owned PCM")
	}
}

func TestSessionAudioCaptureRunsOnFailureAndJoinsSinkErrors(t *testing.T) {
	sinkFailure := errors.New("review sink refused the capture")
	called := 0
	_, err := PlaySamples(t.Context(), SessionConfig{
		CaptureAudio: func(capture SessionAudioCapture) error {
			called++
			if capture.SampleRateHz != 24_000 ||
				!slices.Equal(capture.RoomPCM16, []int16{11, 12}) || len(capture.Agent) != 0 {
				t.Fatalf("failure capture = %+v", capture)
			}
			return sinkFailure
		},
	}, []int16{11, 12})
	if called != 1 {
		t.Fatalf("capture callback calls = %d, want 1", called)
	}
	if err == nil || !errors.Is(err, sinkFailure) ||
		!strings.Contains(err.Error(), "a session needs an endpoint") {
		t.Fatalf("joined failure = %v", err)
	}
}
