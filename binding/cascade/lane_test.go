package cascade

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestTheVisualLaneWakesItsWaiterInsteadOfPollingIt is a latency test, and
// everything about its shape is there to make it decide rather than flake.
//
// The lane used to be an atomic.Bool that this waiter polled every five
// milliseconds. Two things follow from that and only the second is
// measurable. A single release is somewhere between zero and five
// milliseconds late, which no one assertion can separate from a wake-up. But
// a waiter that is already parked on the tick cannot return before the next
// one, so the time from release to return, summed over many releases, is
// either milliseconds each or microseconds each.
//
// So each cycle parks the waiter first and measures only what happens after
// the release. The park has to be a real wait rather than a check that the
// lane is held: the releasing goroutine set that flag itself, so observing it
// says nothing about whether anybody is waiting on it yet, and an earlier
// version of this test passed against the polling implementation for exactly
// that reason - it released before the waiter ever blocked, so the tick it
// was supposed to catch was never entered.
func TestTheVisualLaneWakesItsWaiterInsteadOfPollingIt(t *testing.T) {
	t.Parallel()
	const (
		cycles = 100
		// Comfortably longer than starting a goroutine and reaching the lane,
		// and shorter than the five millisecond tick, so a polling waiter is
		// parked with its first tick still ahead of it.
		park = 2 * time.Millisecond
	)
	var lane waitableLane
	var afterRelease time.Duration

	for range cycles {
		if !lane.CompareAndSwap(false, true) {
			t.Fatal("the lane was already held at the start of a cycle")
		}
		waiting := make(chan error, 1)
		go func() { waiting <- lane.Wait(context.Background()) }()
		time.Sleep(park)

		released := time.Now()
		lane.Store(false)
		if err := <-waiting; err != nil {
			t.Fatal(err)
		}
		afterRelease += time.Since(released)
	}

	// A parked poller returns on its next tick, so a hundred releases cost a
	// few hundred milliseconds. A wake-up costs a scheduling round trip, so
	// they cost single-digit milliseconds in total. Fifty is far above one and
	// far below the other.
	if afterRelease > 50*time.Millisecond {
		t.Fatalf("%d releases took %s to reach the waiter, which is tick latency rather than a wake-up",
			cycles, afterRelease)
	}
}

// TestTheVisualLaneWaitReturnsTheCancellationCause keeps the contract the
// polling loop had: a waiter whose session ends learns why, rather than
// waiting for a lane that will never be released.
func TestTheVisualLaneWaitReturnsTheCancellationCause(t *testing.T) {
	t.Parallel()
	var lane waitableLane
	lane.Store(true)
	cause := errors.New("session ended")
	ctx, cancel := context.WithCancelCause(context.Background())
	waiting := make(chan error, 1)
	go func() { waiting <- lane.Wait(ctx) }()
	cancel(cause)
	if err := <-waiting; !errors.Is(err, cause) {
		t.Fatalf("Wait() error = %v, want %v", err, cause)
	}
}

// TestTheVisualLaneAdmitsOneHolder is the property the claim sites depend on.
// The floor handoff and the fixed-cadence trigger can decide the same revision
// on adjacent goroutines, and exactly one of them must own the micro-turn.
func TestTheVisualLaneAdmitsOneHolder(t *testing.T) {
	t.Parallel()
	var lane waitableLane
	var claimed, ready sync.WaitGroup
	const claimants = 64
	var won int64
	var mu sync.Mutex
	ready.Add(claimants)
	claimed.Add(claimants)
	release := make(chan struct{})
	for range claimants {
		go func() {
			ready.Done()
			<-release
			if lane.CompareAndSwap(false, true) {
				mu.Lock()
				won++
				mu.Unlock()
			}
			claimed.Done()
		}()
	}
	ready.Wait()
	close(release)
	claimed.Wait()
	if won != 1 {
		t.Fatalf("%d of %d claimants took the lane, want exactly 1", won, claimants)
	}
	if !lane.Load() {
		t.Fatal("the lane is not held after a successful claim")
	}
}

// TestTheVisualLaneWaitReturnsImmediatelyWhenFree covers the fast path the
// caller relies on: the common case is an unoccupied lane, and it must not
// cost a scheduling round trip.
func TestTheVisualLaneWaitReturnsImmediatelyWhenFree(t *testing.T) {
	t.Parallel()
	var lane waitableLane
	if err := lane.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	lane.Store(true)
	lane.Store(false)
	if err := lane.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
