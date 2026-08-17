// Package clock provides monotonic clocks for live execution and deterministic replay.
package clock

import (
	"errors"
	"sync/atomic"
	"time"
)

var ErrBackwards = errors.New("monotonic clock cannot move backwards")

type Clock interface {
	NowNS() uint64
}

type System struct {
	origin time.Time
}

func NewSystem() *System {
	return &System{origin: time.Now()}
}

func (system *System) NowNS() uint64 {
	elapsed := time.Since(system.origin)
	if elapsed <= 0 {
		return 0
	}
	return uint64(elapsed.Nanoseconds())
}

type Virtual struct {
	now atomic.Uint64
}

func NewVirtual(startNS uint64) *Virtual {
	clock := &Virtual{}
	clock.now.Store(startNS)
	return clock
}

func (clock *Virtual) NowNS() uint64 {
	return clock.now.Load()
}

func (clock *Virtual) AdvanceToNS(timestampNS uint64) error {
	for {
		current := clock.now.Load()
		if timestampNS < current {
			return ErrBackwards
		}
		if current == timestampNS || clock.now.CompareAndSwap(current, timestampNS) {
			return nil
		}
	}
}

func (clock *Virtual) AdvanceNS(durationNS uint64) (uint64, error) {
	for {
		current := clock.now.Load()
		next := current + durationNS
		if next < current {
			return current, errors.New("monotonic clock overflow")
		}
		if clock.now.CompareAndSwap(current, next) {
			return next, nil
		}
	}
}
