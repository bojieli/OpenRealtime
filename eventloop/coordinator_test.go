package eventloop_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type harness struct {
	store       *trajectory.Store
	coordinator *eventloop.Coordinator
	runs        atomic.Int64
	batches     chan eventloop.Batch
}

func newHarness(t *testing.T, gate eventloop.Gate) *harness {
	t.Helper()
	result := &harness{store: trajectory.NewStore(), batches: make(chan eventloop.Batch, 32)}
	var counter atomic.Uint64
	var clock atomic.Uint64
	coordinator, err := eventloop.New(eventloop.Config{
		Store: result.store, Gate: gate, MaxPendingEvents: 32, ReservedInterruptEvents: 4,
		Now:    func() uint64 { return clock.Add(1) },
		NextID: func(prefix string) string { return prefix + "-" + strconv.FormatUint(counter.Add(1), 10) },
		Processor: eventloop.ProcessorFunc(func(_ context.Context, batch eventloop.Batch) error {
			result.runs.Add(1)
			result.batches <- batch
			return nil
		}),
	})
	if err != nil {
		t.Fatalf("new coordinator: %v", err)
	}
	result.coordinator = coordinator
	return result
}

func observation(revision uint64, text string) eventloop.Event {
	return eventloop.Event{
		Type: "asr.endpoint", Source: "qwen3-asr", Channel: "voice",
		Priority: eventloop.PriorityRoutine, Kind: trajectory.KindObservation,
		SourceRevision: revision, Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
		Content: text,
	}
}

// The third row of the deferral table is the one with no natural trigger: the
// agent stops speaking, the user is silent, and a committed tool result sits
// unacted. Without an explicit wake-up it waits forever.
func TestDeferredWorkRunsOnWakeUp(t *testing.T) {
	var agentSpeaking atomic.Bool
	agentSpeaking.Store(true)
	test := newHarness(t, eventloop.GateFunc(func(_ context.Context, _ eventloop.Batch) (bool, string) {
		if agentSpeaking.Load() {
			return false, "agent speaking"
		}
		return true, ""
	}))

	if _, err := test.coordinator.Submit(observation(1, "what is my balance")); err != nil {
		t.Fatalf("submit: %v", err)
	}
	batch, err := test.coordinator.RunNext(context.Background())
	if !errors.Is(err, eventloop.ErrDeferred) {
		t.Fatalf("expected deferral, got %v", err)
	}
	if len(batch.Items) != 1 {
		t.Fatalf("commit must happen even when the run is deferred, got %d items", len(batch.Items))
	}
	if test.store.Snapshot().Version != 1 {
		t.Fatal("deferred event must still be committed to the trajectory")
	}
	if test.runs.Load() != 0 {
		t.Fatal("deferred batch must not reach the processor")
	}
	deferral, waiting := test.coordinator.Deferral()
	if !waiting || deferral.Reason != "agent speaking" || deferral.Events != 1 {
		t.Fatalf("expected a recorded deferral, got %+v waiting=%v", deferral, waiting)
	}

	// Playback completes. The wake-up is what starts the run the deferral owed.
	agentSpeaking.Store(false)
	test.coordinator.Wake("playback complete")
	select {
	case <-test.coordinator.Signal():
	default:
		t.Fatal("wake-up must signal the driver")
	}
	if _, err := test.coordinator.RunNext(context.Background()); err != nil {
		t.Fatalf("run after wake-up: %v", err)
	}
	if test.runs.Load() != 1 {
		t.Fatalf("expected one run after the wake-up, got %d", test.runs.Load())
	}
	if _, waiting := test.coordinator.Deferral(); waiting {
		t.Fatal("acted work must leave the deferred set")
	}
}

// Deferral must accumulate rather than replace: three events deferred across
// three commits are all handed to the run that eventually happens.
func TestDeferredBatchesAccumulateAndMergeInCommitOrder(t *testing.T) {
	var admit atomic.Bool
	test := newHarness(t, eventloop.GateFunc(func(_ context.Context, _ eventloop.Batch) (bool, string) {
		if admit.Load() {
			return true, ""
		}
		return false, "user speaking"
	}))
	for index := 1; index <= 3; index++ {
		if _, err := test.coordinator.Submit(observation(uint64(index), fmt.Sprintf("part %d", index))); err != nil {
			t.Fatalf("submit %d: %v", index, err)
		}
		if _, err := test.coordinator.RunNext(context.Background()); !errors.Is(err, eventloop.ErrDeferred) {
			t.Fatalf("expected deferral %d, got %v", index, err)
		}
	}
	if got := test.coordinator.Unacted(); got != 3 {
		t.Fatalf("expected three unacted batches, got %d", got)
	}
	admit.Store(true)
	test.coordinator.Wake("endpoint")
	batch, err := test.coordinator.RunNext(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(batch.Events) != 3 || len(batch.Items) != 3 {
		t.Fatalf("expected all deferred work in one run, got %d events %d items", len(batch.Events), len(batch.Items))
	}
	if !batch.Deferred {
		t.Fatal("merged batch must declare that it carried deferred work")
	}
	for index, item := range batch.Items {
		if item.Content != fmt.Sprintf("part %d", index+1) {
			t.Fatalf("item %d out of commit order: %q", index, item.Content)
		}
	}
}

func TestCommitIsUnconditionalUnderEveryGateAnswer(t *testing.T) {
	test := newHarness(t, eventloop.GateFunc(func(_ context.Context, _ eventloop.Batch) (bool, string) {
		return false, "never admits"
	}))
	for index := 1; index <= 5; index++ {
		if _, err := test.coordinator.Submit(observation(uint64(index), "text")); err != nil {
			t.Fatalf("submit: %v", err)
		}
		if _, err := test.coordinator.RunNext(context.Background()); !errors.Is(err, eventloop.ErrDeferred) {
			t.Fatalf("expected deferral, got %v", err)
		}
	}
	if version := test.store.Snapshot().Version; version != 5 {
		t.Fatalf("every committed event must be in the log, got version %d", version)
	}
	if test.runs.Load() != 0 {
		t.Fatal("no run should have happened")
	}
}

func TestWakeWithoutDeferredWorkIsANoOp(t *testing.T) {
	test := newHarness(t, nil)
	test.coordinator.Wake("playback complete")
	select {
	case <-test.coordinator.Signal():
		t.Fatal("wake-up with nothing waiting must not signal")
	default:
	}
	if test.coordinator.Metrics().Wakeups != 0 {
		t.Fatal("a no-op wake-up must not be counted")
	}
}

func TestBatchMarkersAppearOnlyForRealBatches(t *testing.T) {
	test := newHarness(t, nil)
	if _, err := test.coordinator.SubmitBatch([]eventloop.Event{
		observation(1, "first"), observation(2, "second"),
	}); err != nil {
		t.Fatalf("submit batch: %v", err)
	}
	batch, err := test.coordinator.RunNext(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(batch.Items) != 2 {
		t.Fatalf("expected two items, got %d", len(batch.Items))
	}
	for index, item := range batch.Items {
		if item.Event.BatchSize != 2 || item.Event.BatchIndex != index || item.Event.BatchID == "" {
			t.Fatalf("item %d is not legible as part of a batch: %+v", index, item.Event)
		}
	}

	if _, err := test.coordinator.Submit(observation(3, "alone")); err != nil {
		t.Fatalf("submit single: %v", err)
	}
	single, err := test.coordinator.RunNext(context.Background())
	if err != nil {
		t.Fatalf("run single: %v", err)
	}
	if single.Items[0].Event.BatchID != "" || single.Items[0].Event.BatchSize != 0 {
		t.Fatalf("one event is not a batch: %+v", single.Items[0].Event)
	}
}

// A parallel-triaged event answers a quick question without waiting for the
// work in flight and without cancelling it.
func TestParallelEventRunsAlongsideWorkInFlight(t *testing.T) {
	store := trajectory.NewStore()
	release := make(chan struct{})
	// Buffered: the processor signals without blocking, so an unbuffered
	// channel would lose the signal whenever it runs before the test parks.
	started := make(chan struct{}, 1)
	var mainRuns, parallelRuns atomic.Int64
	var counter, clock atomic.Uint64
	coordinator, err := eventloop.New(eventloop.Config{
		Store: store, MaxPendingEvents: 16, ReservedInterruptEvents: 2,
		Now:    func() uint64 { return clock.Add(1) },
		NextID: func(prefix string) string { return prefix + "-" + strconv.FormatUint(counter.Add(1), 10) },
		Processor: eventloop.ProcessorFunc(func(ctx context.Context, batch eventloop.Batch) error {
			if batch.Triage == eventloop.TriageParallel {
				parallelRuns.Add(1)
				return nil
			}
			mainRuns.Add(1)
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		}),
	})
	if err != nil {
		t.Fatalf("new coordinator: %v", err)
	}

	if _, err := coordinator.Submit(observation(1, "do the long thing")); err != nil {
		t.Fatalf("submit: %v", err)
	}
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		if _, err := coordinator.RunNext(context.Background()); err != nil {
			t.Errorf("main run: %v", err)
		}
	}()
	<-started

	quick := observation(2, "what time is it")
	quick.Priority = eventloop.PriorityParallel
	if _, err := coordinator.Submit(quick); err != nil {
		t.Fatalf("submit parallel: %v", err)
	}
	if _, err := coordinator.RunNext(context.Background()); err != nil {
		t.Fatalf("parallel run: %v", err)
	}
	if parallelRuns.Load() != 1 {
		t.Fatalf("expected the parallel branch to run, got %d", parallelRuns.Load())
	}
	if mainRuns.Load() != 1 {
		t.Fatal("the parallel branch must not have started a second main run")
	}
	close(release)
	wait.Wait()
	if coordinator.Metrics().ParallelRuns != 1 {
		t.Fatal("parallel runs must be counted")
	}
}

func TestRoutineEventWaitsForWorkInFlight(t *testing.T) {
	store := trajectory.NewStore()
	release := make(chan struct{})
	// Buffered: the processor signals without blocking, so an unbuffered
	// channel would lose the signal whenever it runs before the test parks.
	started := make(chan struct{}, 1)
	var counter, clock atomic.Uint64
	coordinator, err := eventloop.New(eventloop.Config{
		Store: store, MaxPendingEvents: 16,
		Now:    func() uint64 { return clock.Add(1) },
		NextID: func(prefix string) string { return prefix + "-" + strconv.FormatUint(counter.Add(1), 10) },
		Processor: eventloop.ProcessorFunc(func(ctx context.Context, _ eventloop.Batch) error {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return nil
		}),
	})
	if err != nil {
		t.Fatalf("new coordinator: %v", err)
	}
	if _, err := coordinator.Submit(observation(1, "first")); err != nil {
		t.Fatalf("submit: %v", err)
	}
	go func() { _, _ = coordinator.RunNext(context.Background()) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("processor never started")
	}
	if _, err := coordinator.Submit(observation(2, "second")); err != nil {
		t.Fatalf("submit second: %v", err)
	}
	if _, err := coordinator.RunNext(context.Background()); !errors.Is(err, eventloop.ErrBusy) {
		t.Fatalf("expected ErrBusy for a routine event, got %v", err)
	}
	if version := store.Snapshot().Version; version != 2 {
		t.Fatalf("the busy event must still be committed, got version %d", version)
	}
	close(release)
}

func TestToolPlaceholderEventCommitsAgainstItsCall(t *testing.T) {
	test := newHarness(t, nil)
	if err := test.store.AppendBatch([]trajectory.Item{
		{
			ID: "obs-1", Kind: trajectory.KindObservation, MonotonicNS: 1, SourceRevision: 1,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "pay the invoice",
		},
		{
			ID: "call-1", Kind: trajectory.KindToolCall, MonotonicNS: 2, CausalParentIDs: []string{"obs-1"},
			SourceRevision: 1, InvocationID: "inv-1", Producer: trajectory.Producer{Phase: trajectory.PhaseSlow},
			ToolCall: &trajectory.ToolCall{CallID: "c1", Name: "pay", Arguments: json.RawMessage(`{"id":"1"}`)},
		},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := test.coordinator.Submit(eventloop.Event{
		Type: "tool.interrupted", Source: "runtime", Channel: "tool",
		Priority: eventloop.PriorityRoutine, Kind: trajectory.KindToolPlaceholder, InvocationID: "inv-1",
		ToolPlaceholder: &trajectory.ToolPlaceholder{CallID: "c1", Name: "pay", Reason: "interrupted"},
	}); err != nil {
		t.Fatalf("submit placeholder: %v", err)
	}
	batch, err := test.coordinator.RunNext(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !batch.Contains(trajectory.KindToolPlaceholder) {
		t.Fatal("expected a committed placeholder")
	}
	if pending := trajectory.UnresolvedToolCalls(test.store.Snapshot()); len(pending) != 0 {
		t.Fatalf("placeholder must account for the outstanding call, got %d", len(pending))
	}
}

func TestObserverAuthorityObservationCommitsThroughIngress(t *testing.T) {
	test := newHarness(t, nil)
	event := observation(1, "A confirmation dialog appeared.")
	event.Type = "video.observation"
	event.Source = "video-observer"
	event.Producer = trajectory.Producer{Phase: trajectory.PhaseObserver, Provider: "video"}
	event.Observation = &trajectory.ObservationMeta{
		Observer: "video", Source: "screen", Authority: trajectory.AuthorityObserver,
		Media: []trajectory.MediaRef{{Handle: "frame-1", MIMEType: "image/jpeg", Width: 1280, Height: 720}},
	}
	if _, err := test.coordinator.Submit(event); err != nil {
		t.Fatalf("submit: %v", err)
	}
	batch, err := test.coordinator.RunNext(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := trajectory.AuthorityOf(batch.Items[0]); got != trajectory.AuthorityObserver {
		t.Fatalf("expected observer authority through ingress, got %q", got)
	}
}

func TestInterruptCancelsTheRunInFlight(t *testing.T) {
	store := trajectory.NewStore()
	// Buffered: the processor signals without blocking, so an unbuffered
	// channel would lose the signal whenever it runs before the test parks.
	started := make(chan struct{}, 1)
	var counter, clock atomic.Uint64
	coordinator, err := eventloop.New(eventloop.Config{
		Store: store, MaxPendingEvents: 16, ReservedInterruptEvents: 4,
		Now:    func() uint64 { return clock.Add(1) },
		NextID: func(prefix string) string { return prefix + "-" + strconv.FormatUint(counter.Add(1), 10) },
		Processor: eventloop.ProcessorFunc(func(ctx context.Context, _ eventloop.Batch) error {
			close(started)
			<-ctx.Done()
			return context.Cause(ctx)
		}),
	})
	if err != nil {
		t.Fatalf("new coordinator: %v", err)
	}
	if _, err := coordinator.Submit(observation(1, "long")); err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := coordinator.RunNext(context.Background())
		done <- err
	}()
	<-started
	urgent := observation(2, "stop")
	urgent.Priority = eventloop.PriorityInterrupt
	if _, err := coordinator.Submit(urgent); err != nil {
		t.Fatalf("submit interrupt: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, eventloop.ErrInterrupted) {
			t.Fatalf("expected interruption, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("interrupt did not cancel the run")
	}
}

func TestQueueFullReservesInterruptCapacity(t *testing.T) {
	store := trajectory.NewStore()
	coordinator, err := eventloop.New(eventloop.Config{
		Store: store, MaxPendingEvents: 4, ReservedInterruptEvents: 2,
	})
	if err != nil {
		t.Fatalf("new coordinator: %v", err)
	}
	for index := 0; index < 2; index++ {
		if _, err := coordinator.Submit(observation(uint64(index+1), "text")); err != nil {
			t.Fatalf("submit %d: %v", index, err)
		}
	}
	if _, err := coordinator.Submit(observation(3, "text")); !errors.Is(err, eventloop.ErrQueueFull) {
		t.Fatalf("expected routine backpressure, got %v", err)
	}
	urgent := observation(3, "text")
	urgent.Priority = eventloop.PriorityInterrupt
	if _, err := coordinator.Submit(urgent); err != nil {
		t.Fatalf("reserved interrupt capacity must remain: %v", err)
	}
}
