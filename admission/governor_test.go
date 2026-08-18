package admission

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestReservationLeavesImmediateInteractiveCapacity(t *testing.T) {
	t.Parallel()
	governor, err := NewGovernor(Config{Capacity: 10, ReservedInteractive: 4})
	if err != nil {
		t.Fatal(err)
	}
	background, err := governor.Acquire(context.Background(), Request{Class: ClassBackground, Cost: 6})
	if err != nil {
		t.Fatal(err)
	}
	defer background.Release()
	interactive, err := governor.Acquire(context.Background(), Request{Class: ClassInteractive, Cost: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer interactive.Release()
	snapshot := governor.Snapshot()
	if snapshot.Used != 10 || snapshot.LowerPriorityUsed != 6 || snapshot.Running["interactive"] != 1 {
		t.Fatalf("reservation was not enforced: %+v", snapshot)
	}
}

func TestPreemptionWaitsForCooperativeSafePoint(t *testing.T) {
	t.Parallel()
	governor, err := NewGovernor(Config{Capacity: 4})
	if err != nil {
		t.Fatal(err)
	}
	background, err := governor.Acquire(context.Background(), Request{
		Class: ClassBackground, Cost: 4, Preemptible: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan *Lease, 1)
	failed := make(chan error, 1)
	go func() {
		lease, acquireErr := governor.Acquire(context.Background(), Request{Class: ClassInteractive, Cost: 4})
		if acquireErr != nil {
			failed <- acquireErr
			return
		}
		acquired <- lease
	}()
	waitFor(t, func() bool { return errors.Is(context.Cause(background.Context()), ErrPreempted) })
	select {
	case lease := <-acquired:
		lease.Release()
		t.Fatal("interactive work was admitted before the cancelled holder released capacity")
	default:
	}
	background.Release()
	select {
	case lease := <-acquired:
		lease.Release()
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("interactive work was not admitted after the safe point")
	}
	if snapshot := governor.Snapshot(); snapshot.Preemptions != 1 || snapshot.Used != 0 {
		t.Fatalf("unexpected governor state: %+v", snapshot)
	}
}

func TestQueueUsesClassThenEarliestDeadline(t *testing.T) {
	t.Parallel()
	governor, err := NewGovernor(Config{Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := governor.Acquire(context.Background(), Request{Class: ClassInteractive, Cost: 1})
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		name  string
		lease *Lease
		err   error
	}
	results := make(chan outcome, 2)
	now := time.Now()
	for _, item := range []struct {
		name     string
		deadline time.Time
	}{{"later", now.Add(5 * time.Second)}, {"earlier", now.Add(4 * time.Second)}} {
		item := item
		go func() {
			lease, acquireErr := governor.Acquire(context.Background(), Request{
				Class: ClassBackground, Cost: 1, Deadline: item.deadline,
			})
			results <- outcome{name: item.name, lease: lease, err: acquireErr}
		}()
	}
	waitFor(t, func() bool { return governor.Snapshot().Waiting["background"] == 2 })
	blocker.Release()
	first := <-results
	if first.err != nil || first.name != "earlier" {
		t.Fatalf("first admitted outcome = %+v", first)
	}
	first.lease.Release()
	second := <-results
	if second.err != nil || second.name != "later" {
		t.Fatalf("second admitted outcome = %+v", second)
	}
	second.lease.Release()
}

func TestCancelledWaiterDoesNotLeakCapacity(t *testing.T) {
	t.Parallel()
	governor, err := NewGovernor(Config{Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := governor.Acquire(context.Background(), Request{Class: ClassInteractive, Cost: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, acquireErr := governor.Acquire(ctx, Request{Class: ClassBackground, Cost: 1})
		done <- acquireErr
	}()
	waitFor(t, func() bool { return governor.Snapshot().Waiting["background"] == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire returned %v", err)
	}
	blocker.Release()
	snapshot := governor.Snapshot()
	if snapshot.Used != 0 || snapshot.CancelledWaiters != 1 {
		t.Fatalf("cancelled waiter leaked: %+v", snapshot)
	}
}

func TestClassTimingSeparatesWaitAndService(t *testing.T) {
	t.Parallel()
	var elapsed atomic.Int64
	origin := time.Unix(1, 0)
	clock := func() time.Time { return origin.Add(time.Duration(elapsed.Load())) }
	governor, err := NewGovernor(Config{Capacity: 1, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := governor.Acquire(context.Background(), Request{Class: ClassInteractive, Cost: 1})
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan *Lease, 1)
	go func() {
		lease, acquireErr := governor.Acquire(context.Background(), Request{Class: ClassBackground, Cost: 1})
		if acquireErr != nil {
			return
		}
		acquired <- lease
	}()
	waitFor(t, func() bool { return governor.Snapshot().Waiting["background"] == 1 })
	elapsed.Store(int64(10 * time.Millisecond))
	blocker.Release()
	background := <-acquired
	elapsed.Store(int64(30 * time.Millisecond))
	background.Release()
	timing := governor.Snapshot().Classes["background"]
	if timing.Admitted != 1 || timing.Released != 1 || timing.WaitTotalMS != 10 ||
		timing.WaitMaxMS != 10 || timing.ServiceTotalMS != 20 || timing.ServiceMaxMS != 20 {
		t.Fatalf("unexpected class timing: %+v", timing)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(time.Millisecond)
	}
}
