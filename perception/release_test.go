package perception_test

import (
	"context"
	"sync/atomic"
	"testing"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/asrbuffer"
	"github.com/bojieli/OpenRealtime/perception"
)

// closingRecogniser is a recogniser that holds something, the way a streaming
// one holds a socket and the goroutine reading it.
type closingRecogniser struct {
	closed atomic.Int64
}

func (recogniser *closingRecogniser) Descriptor() v1.Descriptor {
	return v1.Descriptor{
		Name: "closing", Version: "1", Capabilities: v1.Capabilities{v1.CapabilityStreamingInput: true},
	}
}

func (recogniser *closingRecogniser) PushFrame(
	context.Context, v1.AudioFrame,
) ([]v1.PerceptionRevision, error) {
	return nil, nil
}

func (recogniser *closingRecogniser) Finalize(
	context.Context, uint64,
) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{RevisionID: 1, Final: true}, nil
}

func (recogniser *closingRecogniser) Close() error {
	recogniser.closed.Add(1)
	return nil
}

// One recogniser exists per utterance by design, so an abandoned utterance
// that did not release its recogniser would strand a connection for every
// utterance the session ever had.
func TestResetReleasesARecogniserThatHoldsAConnection(t *testing.T) {
	t.Parallel()
	recogniser := &closingRecogniser{}
	observer, err := perception.NewAudioObserver(perception.AudioConfig{
		Provider: func() (v1.PerceptionProvider, error) { return recogniser, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := observer.Observe(context.Background(), []perception.Frame{{
		Kind: perception.FrameAudio, Source: "microphone",
		SampleRateHz: 16_000, PCM16LE: make([]byte, 640),
	}}); err != nil {
		t.Fatal(err)
	}
	observer.Reset()
	if got := recogniser.closed.Load(); got != 1 {
		t.Fatalf("an abandoned utterance must release its recogniser, closed %d times", got)
	}

	// Resetting again must not close a recogniser that was already dropped.
	observer.Reset()
	if got := recogniser.closed.Load(); got != 1 {
		t.Fatalf("reset closed a recogniser it no longer holds: %d", got)
	}
}

// The runtime holds the cadence buffer, not the recogniser, so the buffer is
// what has to pass the release along.
func TestTheCadenceBufferPassesTheReleaseAlong(t *testing.T) {
	t.Parallel()
	recogniser := &closingRecogniser{}
	buffer, err := asrbuffer.New(asrbuffer.Config{
		Provider: recogniser, MinimumChunk: 200_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := buffer.Close(); err != nil {
		t.Fatal(err)
	}
	if got := recogniser.closed.Load(); got != 1 {
		t.Fatalf("the buffer must release the recogniser it wraps, closed %d times", got)
	}
}
