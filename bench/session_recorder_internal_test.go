package bench

import (
	"testing"
	"time"
)

func TestRecorderBeginEpisodeRetainsPreEpisodeErrorEvidence(t *testing.T) {
	recorder := &recorder{
		started: time.Now().Add(-time.Second),
		audio:   newSessionAudioRecorder(nil),
	}
	recorder.add(Moment{Kind: MomentTranscript, Text: "setup event"})
	recorder.add(Moment{Kind: MomentError, Text: "recogniser unavailable"})

	recorder.beginEpisode()
	recorder.add(Moment{Kind: MomentReady})

	transcript := recorder.snapshot()
	if len(transcript.Moments) != 2 {
		t.Fatalf("episode moments = %+v, want retained error and ready moment", transcript.Moments)
	}
	var retainedError, ready bool
	for _, moment := range transcript.Moments {
		switch {
		case moment.Kind == MomentError && moment.Text == "recogniser unavailable":
			retainedError = true
			if moment.AtMS != 0 {
				t.Fatalf("retained error time = %v ms, want episode zero", moment.AtMS)
			}
		case moment.Kind == MomentReady:
			ready = true
		case moment.Kind == MomentTranscript:
			t.Fatalf("ordinary setup moment was retained: %+v", moment)
		}
	}
	if !retainedError || !ready {
		t.Fatalf("episode moments = %+v, want retained error and ready moment", transcript.Moments)
	}
}
