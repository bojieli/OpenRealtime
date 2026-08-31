package cascade_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/eventloop"
)

// interruptingSink is a client whose turn boundary reports the interruption the
// runtime itself raised. That is what happens on an ordinary barge-in: the
// interaction plane cancels in-flight cognition, and the cancellation reaches
// the deferred TurnEnd as the cause.
type interruptingSink struct {
	*recordingSink
}

func (sink *interruptingSink) TurnEnd(ctx context.Context, outcome binding.TurnOutcome) error {
	_ = sink.recordingSink.TurnEnd(ctx, outcome)
	return fmt.Errorf("user resumed before the response began: %w", eventloop.ErrInterrupted)
}

// Interrupting the agent is the interaction plane doing its job, not a fault in
// the session. The event-loop driver already knows that -- drain() treats
// ErrInterrupted and context.Canceled as ordinary -- but the turn boundary did
// not, so every barge-in reached the client as a session error.
//
// A full-duplex caller talks over the agent constantly, so this fired on almost
// every turn and ended the call each time.
func TestABargeInInterruptIsNotASessionFailure(t *testing.T) {
	inner := &recordingSink{}
	sink := &interruptingSink{recordingSink: inner}
	bind, err := cascade.New(cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) {
			return &scriptedASR{final: "what is my balance"}, nil
		},
		Fast:   newFast(),
		Slow:   newSlow(),
		Speech: toneSpeech{chunks: 1},
	})
	if err != nil {
		t.Fatalf("new cascade: %v", err)
	}
	runtime, err := bind.Start(context.Background(), binding.Options{
		Sink: sink, SessionID: "test",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background(), nil) })

	speak(t, runtime, 3)
	waitFor(t, func() bool { return len(runtime.Trajectory().Items) > 0 }, "nothing was ever heard")
	time.Sleep(300 * time.Millisecond)

	inner.mu.Lock()
	failures := append([]binding.ErrorEvent(nil), inner.failures...)
	inner.mu.Unlock()
	for _, failure := range failures {
		t.Fatalf("an interruption was reported to the client: %s %s", failure.Code, failure.Message)
	}
}
