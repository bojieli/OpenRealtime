package scenarioconversation

import (
	"context"
	"errors"
	"testing"
	"time"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/spoken"
)

type recordingDebugSink struct {
	legacy.Sink
	events []legacy.DebugEvent
}

func (sink *recordingDebugSink) Debug(_ context.Context, event legacy.DebugEvent) error {
	sink.events = append(sink.events, event)
	return nil
}

// TestWordTimingFailuresAreReported covers the silence that hid two other
// defects: the room pointed word timings at a server that never returns word
// timestamps, and nothing anywhere said so. spoken.TrackerConfig carries Report
// for exactly this, the legacy cascade set it, and this binding did not.
func TestWordTimingFailuresAreReported(t *testing.T) {
	sink := &recordingDebugSink{}
	config := wordTimingTrackerConfig(context.Background(), sink, stubAligner{}, 900*time.Millisecond)
	if config.Report == nil {
		t.Fatal("alignment failures are swallowed: no Report on the tracker configuration")
	}
	config.Report(errors.New("the transcription endpoint returned no word timestamps"))
	if len(sink.events) != 1 {
		t.Fatalf("expected one debug event, got %d", len(sink.events))
	}
	event := sink.events[0]
	if event.Name != "speech.word_timing_failed" || event.Phase != "error" {
		t.Fatalf("unexpected event: %+v", event)
	}
	if got, _ := event.Attributes["error"].(string); got == "" {
		t.Fatalf("event carries no error text: %+v", event)
	}
}

// A sink that is not a DebugSink must still produce a usable configuration
// rather than a nil Report that panics when a listen fails.
func TestWordTimingConfigSurvivesASinkWithoutDebug(t *testing.T) {
	config := wordTimingTrackerConfig(context.Background(), nil, stubAligner{}, time.Second)
	if config.Aligner == nil || config.Interval != time.Second {
		t.Fatalf("configuration lost its aligner or interval: %+v", config)
	}
}

type stubAligner struct{}

func (stubAligner) Words(context.Context, spoken.Audio) ([]spoken.Word, error) { return nil, nil }
