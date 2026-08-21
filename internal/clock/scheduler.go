package clock

import (
	"container/heap"
	"sync"
	"time"
)

// Timer is a scheduled callback that has not necessarily fired yet.
type Timer interface {
	// Stop cancels the timer and reports whether it stopped it before it ran.
	Stop() bool
}

// Scheduler is a clock that can also schedule future work. Components that
// must act when nothing external happens - a playout horizon passing, a
// deadline expiring - take a Scheduler so tests can drive time explicitly
// rather than sleeping.
type Scheduler interface {
	Clock
	AfterFunc(time.Duration, func()) Timer
}

type systemTimer struct{ timer *time.Timer }

func (timer systemTimer) Stop() bool { return timer.timer.Stop() }

// AfterFunc schedules callback on the wall clock.
func (system *System) AfterFunc(delay time.Duration, callback func()) Timer {
	if delay < 0 {
		delay = 0
	}
	return systemTimer{timer: time.AfterFunc(delay, callback)}
}

type manualTimer struct {
	scheduler *Manual
	sequence  uint64
	deadline  uint64
	callback  func()
	stopped   bool
	index     int
}

func (timer *manualTimer) Stop() bool {
	timer.scheduler.mu.Lock()
	defer timer.scheduler.mu.Unlock()
	if timer.stopped || timer.index < 0 {
		return false
	}
	heap.Remove(&timer.scheduler.queue, timer.index)
	timer.stopped = true
	return true
}

type timerQueue []*manualTimer

func (queue timerQueue) Len() int { return len(queue) }
func (queue timerQueue) Less(left, right int) bool {
	if queue[left].deadline == queue[right].deadline {
		return queue[left].sequence < queue[right].sequence
	}
	return queue[left].deadline < queue[right].deadline
}
func (queue timerQueue) Swap(left, right int) {
	queue[left], queue[right] = queue[right], queue[left]
	queue[left].index, queue[right].index = left, right
}
func (queue *timerQueue) Push(value any) {
	timer := value.(*manualTimer)
	timer.index = len(*queue)
	*queue = append(*queue, timer)
}
func (queue *timerQueue) Pop() any {
	old := *queue
	last := old[len(old)-1]
	last.index = -1
	*queue = old[:len(old)-1]
	return last
}

// Manual is a deterministic Scheduler for tests. Advancing it fires every
// timer whose deadline has passed, in deadline then scheduling order, with the
// clock already set to that timer's deadline so a callback that reads the time
// sees the moment it was scheduled for rather than the moment advancing ended.
type Manual struct {
	mu       sync.Mutex
	now      uint64
	sequence uint64
	queue    timerQueue
}

func NewManual(startNS uint64) *Manual {
	return &Manual{now: startNS}
}

func (manual *Manual) NowNS() uint64 {
	manual.mu.Lock()
	defer manual.mu.Unlock()
	return manual.now
}

func (manual *Manual) AfterFunc(delay time.Duration, callback func()) Timer {
	if delay < 0 {
		delay = 0
	}
	manual.mu.Lock()
	defer manual.mu.Unlock()
	manual.sequence++
	timer := &manualTimer{
		scheduler: manual, sequence: manual.sequence,
		deadline: manual.now + uint64(delay.Nanoseconds()), callback: callback,
	}
	heap.Push(&manual.queue, timer)
	return timer
}

// AdvanceNS moves the clock forward and fires everything due.
func (manual *Manual) AdvanceNS(durationNS uint64) {
	manual.mu.Lock()
	target := manual.now + durationNS
	manual.mu.Unlock()
	manual.AdvanceToNS(target)
}

// AdvanceToNS moves the clock to an absolute time and fires everything due.
func (manual *Manual) AdvanceToNS(timestampNS uint64) {
	for {
		manual.mu.Lock()
		if timestampNS < manual.now {
			manual.mu.Unlock()
			return
		}
		if manual.queue.Len() == 0 || manual.queue[0].deadline > timestampNS {
			manual.now = timestampNS
			manual.mu.Unlock()
			return
		}
		timer := heap.Pop(&manual.queue).(*manualTimer)
		timer.stopped = true
		manual.now = timer.deadline
		manual.mu.Unlock()
		timer.callback()
	}
}

var (
	_ Scheduler = (*System)(nil)
	_ Scheduler = (*Manual)(nil)
)
