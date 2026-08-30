package policy

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type semanticVariadicTestInput struct {
	envelope     element.Envelope
	delivered    atomic.Bool
	receiveCalls atomic.Int32
}

func BenchmarkSemanticAdmissionSituation(b *testing.B) {
	update := SessionInvocationUpdate{Invocation: continuation.Invocation{
		Instruction: "Answer only when the current evidence requires it.",
	}}
	audio := trajectory.Snapshot{Version: 1, Items: []trajectory.Item{{
		ID: "audio", Kind: trajectory.KindObservation,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "a short audio turn",
	}}}
	b.Run("audio", func(b *testing.B) {
		runner := semanticAdmissionRunner{config: SemanticAdmissionConfig{RecentLines: 12}}
		b.ReportAllocs()
		for range b.N {
			state, err := runner.situation(context.Background(), semanticRequest{}, update, audio)
			if err != nil || len(state.Seeing) != 0 {
				b.Fatalf("audio situation=%+v error=%v", state, err)
			}
		}
	})

	image := make([]byte, 25<<10)
	visual := trajectory.Snapshot{Version: 3, Items: []trajectory.Item{
		{
			ID: "old-frame", Kind: trajectory.KindObservation,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "old frame",
			Observation: &trajectory.ObservationMeta{
				Observer: "client", Source: "screen", Authority: trajectory.AuthorityUser,
				Media: []trajectory.MediaRef{{Handle: "old", MIMEType: "image/png", Source: "screen"}},
			},
		},
		{
			ID: "current-frame", Kind: trajectory.KindObservation,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "current frame",
			Observation: &trajectory.ObservationMeta{
				Observer: "client", Source: "screen", Authority: trajectory.AuthorityUser,
				Media: []trajectory.MediaRef{{Handle: "current", MIMEType: "image/png", Source: "screen"}},
			},
		},
		{ID: "tool-result", Kind: trajectory.KindToolResult, Producer: trajectory.Producer{Phase: trajectory.PhaseTool}},
	}}
	b.Run("direct_visual_25KiB", func(b *testing.B) {
		runner := semanticAdmissionRunner{
			config: SemanticAdmissionConfig{RecentLines: 12, DirectVisualInput: true},
			media: continuation.MediaResolver(func(handle string) (continuation.Media, error) {
				if handle != "current" {
					return continuation.Media{}, errors.New("resolved stale visual evidence")
				}
				return continuation.Media{MIMEType: "image/png", Bytes: image}, nil
			}),
		}
		b.SetBytes(int64(len(image)))
		b.ReportAllocs()
		for range b.N {
			state, err := runner.situation(context.Background(), semanticRequest{}, update, visual)
			if err != nil || len(state.Seeing) != 1 || len(state.Seeing[0].Bytes) != len(image) {
				b.Fatalf("visual situation=%+v error=%v", state, err)
			}
		}
	})
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
