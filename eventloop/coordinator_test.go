package eventloop

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestCoordinatorBatchesStructuredEventsInArrivalOrder(t *testing.T) {
	t.Parallel()
	store := trajectory.NewStore()
	var processed Batch
	coordinator, err := New(Config{
		Store: store, MaxPendingEvents: 16,
		Now: func() uint64 { return 20 },
		Processor: ProcessorFunc(func(_ context.Context, batch Batch) error {
			processed = batch
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	firstID, err := coordinator.Submit(Event{
		Type: "asr.revision", Source: "qwen3-asr", Channel: "voice",
		Priority: PriorityRoutine, Kind: trajectory.KindObservation, OccurredNS: 10,
		SourceRevision: 1, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := coordinator.Submit(Event{
		Type: "asr.endpoint", Source: "qwen3-asr", Channel: "voice",
		Priority: PriorityRoutine, Kind: trajectory.KindObservation, OccurredNS: 15,
		SourceRevision: 2, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "hello there",
	})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := coordinator.RunNext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if batch.StartVersion != 0 || batch.EndVersion != 2 || processed.EndVersion != 2 || len(batch.Items) != 2 {
		t.Fatalf("unexpected batch: %#v", batch)
	}
	if batch.Items[0].Event.EventID != firstID || batch.Items[1].Event.EventID != secondID ||
		batch.Items[0].Event.OccurredNS != 10 || batch.Items[0].MonotonicNS != 20 {
		t.Fatalf("source/commit timing was not preserved: %#v", batch.Items)
	}
	if len(batch.Items[1].CausalParentIDs) != 1 || batch.Items[1].CausalParentIDs[0] != batch.Items[0].ID {
		t.Fatalf("batch is not causally ordered: %#v", batch.Items)
	}
}

func TestTypedInterruptCancelsAtSafePointWithoutContentRouting(t *testing.T) {
	t.Parallel()
	store := trajectory.NewStore()
	started := make(chan struct{})
	var once sync.Once
	coordinator, err := New(Config{
		Store: store, MaxPendingEvents: 16, ReservedInterruptEvents: 1,
		Processor: ProcessorFunc(func(ctx context.Context, _ Batch) error {
			waited := false
			once.Do(func() {
				waited = true
				close(started)
			})
			if waited {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Submit(observationEvent("normal input", PriorityRoutine)); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		batch Batch
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		batch, runErr := coordinator.RunNext(context.Background())
		done <- outcome{batch: batch, err: runErr}
	}()
	<-started
	// The content contains no stop keyword. Its trusted event type and priority,
	// not pattern matching, carry interruption semantics.
	interrupt := observationEvent("new directed speech", PriorityInterrupt)
	interrupt.Type = "user.interrupt"
	if _, err := coordinator.Submit(interrupt); err != nil {
		t.Fatal(err)
	}
	first := <-done
	if !errors.Is(first.err, ErrInterrupted) || first.batch.EndVersion != 1 {
		t.Fatalf("active batch did not end at an interrupt safe point: batch=%#v err=%v", first.batch, first.err)
	}
	if coordinator.Pending() != 1 || store.Snapshot().Version != 1 {
		t.Fatalf("interrupt entered trajectory before safe point: pending=%d snapshot=%#v", coordinator.Pending(), store.Snapshot())
	}
	second, err := coordinator.RunNext(context.Background())
	if err != nil || second.EndVersion != 2 || second.Items[0].Event.Type != "user.interrupt" {
		t.Fatalf("interrupt was not processed next: batch=%#v err=%v", second, err)
	}
}

func TestOperationalInterruptDoesNotFabricateTrajectoryItem(t *testing.T) {
	t.Parallel()
	store := trajectory.NewStore()
	started := make(chan struct{})
	coordinator, err := New(Config{
		Store: store, MaxPendingEvents: 4, ReservedInterruptEvents: 1,
		Processor: ProcessorFunc(func(ctx context.Context, _ Batch) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Submit(observationEvent("hello", PriorityRoutine)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, runErr := coordinator.RunNext(context.Background())
		done <- runErr
	}()
	<-started
	coordinator.Interrupt(errors.New("acoustic speech start"))
	if runErr := <-done; runErr == nil {
		t.Fatal("operational interrupt did not cancel the active processor")
	}
	snapshot := store.Snapshot()
	if len(snapshot.Items) != 1 || snapshot.Items[0].Kind != trajectory.KindObservation {
		t.Fatalf("operational interrupt changed canonical trajectory: %#v", snapshot.Items)
	}
}

func TestToolResultsCrossSafePointAsOneCompleteBatch(t *testing.T) {
	t.Parallel()
	store := trajectory.NewStore()
	if err := store.AppendBatch([]trajectory.Item{
		{ID: "user", Kind: trajectory.KindObservation, MonotonicNS: 1, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "compare"},
		{ID: "call-a-item", Kind: trajectory.KindToolCall, MonotonicNS: 2, CausalParentIDs: []string{"user"}, InvocationID: "slow-1", Producer: trajectory.Producer{Phase: trajectory.PhaseSlow}, ToolCall: &trajectory.ToolCall{CallID: "call-a", Name: "read_a", Arguments: json.RawMessage(`{}`)}},
		{ID: "call-b-item", Kind: trajectory.KindToolCall, MonotonicNS: 3, CausalParentIDs: []string{"call-a-item"}, InvocationID: "slow-1", Producer: trajectory.Producer{Phase: trajectory.PhaseSlow}, ToolCall: &trajectory.ToolCall{CallID: "call-b", Name: "read_b", Arguments: json.RawMessage(`{}`)}},
	}); err != nil {
		t.Fatal(err)
	}
	var now uint64 = 3
	coordinator, err := New(Config{Store: store, MaxPendingEvents: 16, Now: func() uint64 { now++; return now }})
	if err != nil {
		t.Fatal(err)
	}
	partial := toolResultEvent([]trajectory.ToolResult{{CallID: "call-a", Name: "read_a", Output: json.RawMessage(`{"value":2}`)}})
	if _, err := coordinator.Submit(partial); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.RunNext(context.Background()); err == nil {
		t.Fatal("partial tool result event was accepted")
	}
	if store.Snapshot().Version != 3 || coordinator.Pending() != 0 {
		t.Fatalf("invalid result event changed state: pending=%d snapshot=%#v", coordinator.Pending(), store.Snapshot())
	}
	complete := toolResultEvent([]trajectory.ToolResult{
		{CallID: "call-b", Name: "read_b", Output: json.RawMessage(`{"value":1}`)},
		{CallID: "call-a", Name: "read_a", Output: json.RawMessage(`{"value":2}`)},
	})
	if _, err := coordinator.Submit(complete); err != nil {
		t.Fatal(err)
	}
	batch, err := coordinator.RunNext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Items) != 2 || batch.Items[0].ToolResult.Name != "read_a" || batch.Items[1].ToolResult.Name != "read_b" {
		t.Fatalf("results were not committed atomically in call order: %#v", batch.Items)
	}
	if len(batch.Items[0].CausalParentIDs) != 2 || batch.Items[0].Event.Type != "tool.results" {
		t.Fatalf("tool result causality/event provenance missing: %#v", batch.Items[0])
	}
}

func TestCoordinatorReservesIngressCapacityForInterrupts(t *testing.T) {
	t.Parallel()
	coordinator, err := New(Config{
		Store: trajectory.NewStore(), MaxPendingEvents: 2, ReservedInterruptEvents: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Submit(observationEvent("routine", PriorityRoutine)); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Submit(observationEvent("another routine", PriorityRoutine)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("routine event consumed interrupt reserve: %v", err)
	}
	interrupt := observationEvent("directed interruption", PriorityInterrupt)
	interrupt.Type = "user.interrupt"
	if _, err := coordinator.Submit(interrupt); err != nil {
		t.Fatalf("interrupt could not use reserved capacity: %v", err)
	}
	if _, err := coordinator.Submit(interrupt); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("full queue did not apply backpressure: %v", err)
	}
}

func observationEvent(content string, priority Priority) Event {
	return Event{
		Type: "user.input", Source: "user", Channel: "voice", Priority: priority,
		Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: content,
	}
}

func toolResultEvent(results []trajectory.ToolResult) Event {
	return Event{
		Type: "tool.results", Source: "tau-orchestrator", Channel: "tool",
		Priority: PriorityRoutine, Kind: trajectory.KindToolResult,
		InvocationID: "slow-1", ToolResults: results,
	}
}
