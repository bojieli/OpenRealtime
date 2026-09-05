package cascade

import (
	"context"
	"sync"
)

// waitableLane is a single-holder flag whose release wakes its waiters.
//
// It replaces an atomic.Bool that one path had to poll. Polling a flag on a
// fixed tick is two costs at once: a wake-up every tick for as long as the
// lane is busy, and up to a whole tick of latency added to a path that is
// already waiting on a model. Here the tick was five milliseconds and the
// thing waited for is a screen action a person is watching for, so the latency
// was the expensive half.
//
// The zero value is an unheld lane.
type waitableLane struct {
	mu   sync.Mutex
	held bool
	// released is non-nil exactly while the lane is held, and closing it is
	// the wake-up. A fresh channel per acquisition is what lets a waiter that
	// observed "held" sleep on that specific occupancy: the channel it is
	// holding is the one that gets closed, so it cannot miss a release that
	// happens between the observation and the sleep.
	released chan struct{}
}

// Load reports whether the lane is held.
func (lane *waitableLane) Load() bool {
	lane.mu.Lock()
	defer lane.mu.Unlock()
	return lane.held
}

// CompareAndSwap claims or releases the lane only from the expected state.
//
// It keeps the shape the atomic had, because the claim sites are two
// goroutines deciding the same revision and the compare is the decision:
// whichever succeeds owns the micro-turn, and the other must not spend a
// second model call on the same pixels.
func (lane *waitableLane) CompareAndSwap(old, new bool) bool {
	lane.mu.Lock()
	defer lane.mu.Unlock()
	if lane.held != old {
		return false
	}
	lane.set(new)
	return true
}

// Store sets the lane unconditionally.
func (lane *waitableLane) Store(held bool) {
	lane.mu.Lock()
	defer lane.mu.Unlock()
	lane.set(held)
}

func (lane *waitableLane) set(held bool) {
	if lane.held == held {
		return
	}
	lane.held = held
	if held {
		lane.released = make(chan struct{})
		return
	}
	close(lane.released)
	lane.released = nil
}

// Wait blocks until the lane is free, or ctx ends.
//
// The loop is not a poll. Each pass sleeps on the release of the occupancy it
// actually observed and re-checks only because a different goroutine may have
// claimed the lane again in between, which is a real race here: the floor
// handoff and the fixed-cadence trigger both claim this lane.
func (lane *waitableLane) Wait(ctx context.Context) error {
	for {
		lane.mu.Lock()
		if !lane.held {
			lane.mu.Unlock()
			return nil
		}
		released := lane.released
		lane.mu.Unlock()
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-released:
		}
	}
}
