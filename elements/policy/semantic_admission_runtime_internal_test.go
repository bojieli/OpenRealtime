package policy

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
)

type semanticVariadicTestInput struct {
	envelope     element.Envelope
	delivered    atomic.Bool
	receiveCalls atomic.Int32
}

func (*semanticVariadicTestInput) Name() string { return "committed" }
func (*semanticVariadicTestInput) Type() element.Type {
	return stateelements.ObservationCommitOutcomeType()
}
func (*semanticVariadicTestInput) Lanes() []element.Receiver { return nil }
func (input *semanticVariadicTestInput) Receive(context.Context) (element.Envelope, error) {
	input.receiveCalls.Add(1)
	return element.Envelope{}, errors.New("singular Receive used for variadic semantic commits")
}
func (input *semanticVariadicTestInput) ReceiveAny(
	ctx context.Context,
) (element.Envelope, string, error) {
	if input.delivered.CompareAndSwap(false, true) {
		return input.envelope.Clone(), "committed/audio", nil
	}
	<-ctx.Done()
	return element.Envelope{}, "", context.Cause(ctx)
}

func TestReceiveSemanticAdmissionUsesVariadicArbitrationForCommittedLanes(t *testing.T) {
	input := &semanticVariadicTestInput{envelope: element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-a",
	}}
	ctx, cancel := context.WithCancelCause(context.Background())
	inputs := make(chan semanticAdmissionInput, 1)
	failures := make(chan error, 1)
	var wait sync.WaitGroup
	wait.Add(1)
	go receiveSemanticAdmission(ctx, "committed", input, true, inputs, failures, &wait)
	select {
	case got := <-inputs:
		if got.kind != "committed" || got.envelope.ItemID != "commit-a" {
			t.Fatalf("variadic semantic input = %+v", got)
		}
	case err := <-failures:
		t.Fatalf("variadic semantic receiver failed: %v", err)
	case <-time.After(time.Second):
		t.Fatal("variadic semantic receiver did not deliver")
	}
	if calls := input.receiveCalls.Load(); calls != 0 {
		t.Fatalf("variadic semantic receiver called singular Receive %d time(s)", calls)
	}
	cancel(errors.New("test complete"))
	done := make(chan struct{})
	go func() {
		wait.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("variadic semantic receiver did not stop after cancellation")
	}
}
