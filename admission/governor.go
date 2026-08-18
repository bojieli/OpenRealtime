// Package admission provides resource-scoped, priority-aware admission for
// realtime model work.
//
// Priority is explicit provenance supplied by the runtime (for example,
// interactive ASR versus speculative preparation versus background reasoning).
// The governor never inspects prompt or transcript content. Work is admitted at
// safe points, and preemption is cooperative through lease cancellation.
package admission

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

var (
	// ErrPreempted is the cancellation cause used when higher-priority work
	// needs capacity held by a cooperative lower-priority lease.
	ErrPreempted = errors.New("resource lease preempted by higher-priority work")
	// ErrAdmissionDeadline indicates that work did not begin by its declared
	// admission deadline.
	ErrAdmissionDeadline = errors.New("resource admission deadline exceeded")
)

// Class is a runtime-declared scheduling class. The numeric ordering is part
// of the scheduler contract: larger values are admitted first.
type Class uint8

const (
	ClassBackground Class = iota + 1
	ClassSpeculative
	ClassInteractive
	ClassUrgent
)

func (class Class) String() string {
	switch class {
	case ClassBackground:
		return "background"
	case ClassSpeculative:
		return "speculative"
	case ClassInteractive:
		return "interactive"
	case ClassUrgent:
		return "urgent"
	default:
		return fmt.Sprintf("class(%d)", class)
	}
}

// Request declares resource demand independently of semantic input. Deadline
// orders queued work and bounds admission. Callers should also put the same
// deadline on the parent context when it must bound execution.
type Request struct {
	Class       Class
	Cost        int
	Preemptible bool
	Deadline    time.Time
	Label       string
}

// Config defines one resource domain, such as a co-located GPU. Capacity and
// costs are abstract integer units. ReservedInteractive prevents background
// and speculative work from consuming all capacity, while interactive and
// urgent work may use the complete capacity.
type Config struct {
	Capacity            int
	ReservedInteractive int
	Clock               func() time.Time
}

// Snapshot is safe operational telemetry; it contains no model input.
type Snapshot struct {
	Capacity            int                    `json:"capacity"`
	ReservedInteractive int                    `json:"reserved_interactive"`
	Used                int                    `json:"used"`
	LowerPriorityUsed   int                    `json:"lower_priority_used"`
	Running             map[string]int         `json:"running"`
	Waiting             map[string]int         `json:"waiting"`
	Admitted            uint64                 `json:"admitted"`
	Released            uint64                 `json:"released"`
	Preemptions         uint64                 `json:"preemptions"`
	CancelledWaiters    uint64                 `json:"cancelled_waiters"`
	DeadlineMisses      uint64                 `json:"deadline_misses"`
	Classes             map[string]ClassTiming `json:"classes,omitempty"`
}

// ClassTiming is fixed-memory admission telemetry. Wait covers request enqueue
// through lease grant. Service covers grant through the holder's Release safe
// point, so it includes provider work and cancellation acknowledgement.
type ClassTiming struct {
	Admitted                   uint64  `json:"admitted"`
	Released                   uint64  `json:"released"`
	Preemptions                uint64  `json:"preemptions"`
	WaitTotalMS                float64 `json:"wait_total_ms"`
	WaitMaxMS                  float64 `json:"wait_max_ms"`
	ServiceTotalMS             float64 `json:"service_total_ms"`
	ServiceMaxMS               float64 `json:"service_max_ms"`
	PreemptionToReleaseTotalMS float64 `json:"preemption_to_release_total_ms"`
	PreemptionToReleaseMaxMS   float64 `json:"preemption_to_release_max_ms"`
}

type waiterState uint8

const (
	waiting waiterState = iota
	granted
	cancelled
)

type waiter struct {
	sequence uint64
	request  Request
	parent   context.Context
	enqueued time.Time
	ready    chan *Lease
	state    waiterState
	lease    *Lease
}

type runningLease struct {
	lease      *Lease
	sequence   uint64
	preempting bool
	granted    time.Time
	preempted  time.Time
}

type classTiming struct {
	admitted                 uint64
	released                 uint64
	preemptions              uint64
	waitTotal                time.Duration
	waitMax                  time.Duration
	serviceTotal             time.Duration
	serviceMax               time.Duration
	preemptionToReleaseTotal time.Duration
	preemptionToReleaseMax   time.Duration
}

// Governor owns admission state for one bounded resource domain.
type Governor struct {
	mu sync.Mutex

	capacity            int
	reservedInteractive int
	clock               func() time.Time
	nextSequence        uint64
	nextLeaseID         uint64
	used                int
	lowerPriorityUsed   int
	waiters             []*waiter
	running             map[uint64]*runningLease
	admitted            uint64
	released            uint64
	preemptions         uint64
	cancelledWaiters    uint64
	deadlineMisses      uint64
	classes             map[Class]*classTiming
}

// NewGovernor creates a deterministic safe-point scheduler.
func NewGovernor(config Config) (*Governor, error) {
	if config.Capacity <= 0 {
		return nil, errors.New("admission capacity must be positive")
	}
	if config.ReservedInteractive < 0 || config.ReservedInteractive > config.Capacity {
		return nil, errors.New("reserved interactive capacity must be between zero and total capacity")
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	return &Governor{
		capacity: config.Capacity, reservedInteractive: config.ReservedInteractive,
		clock: config.Clock, running: make(map[uint64]*runningLease),
		classes: make(map[Class]*classTiming),
	}, nil
}

// Acquire waits until request can start. A returned lease owns its capacity
// until Release; cancellation only asks the holder to stop and never pretends
// GPU work has ended before the holder acknowledges the safe point.
func (governor *Governor) Acquire(ctx context.Context, request Request) (*Lease, error) {
	if ctx == nil {
		return nil, errors.New("admission context must not be nil")
	}
	if err := governor.validateRequest(request); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	governor.mu.Lock()
	if !request.Deadline.IsZero() && !governor.clock().Before(request.Deadline) {
		governor.deadlineMisses++
		governor.mu.Unlock()
		return nil, ErrAdmissionDeadline
	}
	governor.nextSequence++
	pending := &waiter{
		sequence: governor.nextSequence, request: request, parent: ctx,
		enqueued: governor.clock(), ready: make(chan *Lease, 1), state: waiting,
	}
	governor.waiters = append(governor.waiters, pending)
	governor.scheduleLocked()
	governor.mu.Unlock()

	var deadline <-chan time.Time
	var timer *time.Timer
	if !request.Deadline.IsZero() {
		timer = time.NewTimer(time.Until(request.Deadline))
		deadline = timer.C
		defer timer.Stop()
	}
	select {
	case lease := <-pending.ready:
		return lease, nil
	case <-ctx.Done():
		return nil, governor.cancelWaiter(pending, ctx.Err(), false)
	case <-deadline:
		return nil, governor.cancelWaiter(pending, ErrAdmissionDeadline, true)
	}
}

func (governor *Governor) validateRequest(request Request) error {
	switch request.Class {
	case ClassBackground, ClassSpeculative, ClassInteractive, ClassUrgent:
	default:
		return fmt.Errorf("invalid admission class %d", request.Class)
	}
	if request.Cost <= 0 || request.Cost > governor.capacity {
		return fmt.Errorf("admission cost must be between 1 and %d", governor.capacity)
	}
	if request.Class <= ClassSpeculative && request.Cost > governor.capacity-governor.reservedInteractive {
		return errors.New("lower-priority work exceeds capacity available outside the interactive reservation")
	}
	return nil
}

func (governor *Governor) cancelWaiter(pending *waiter, cause error, deadline bool) error {
	governor.mu.Lock()
	if pending.state == granted {
		lease := pending.lease
		governor.mu.Unlock()
		lease.Release()
		return cause
	}
	if pending.state == waiting {
		pending.state = cancelled
		governor.removeWaiterLocked(pending)
		if deadline {
			governor.deadlineMisses++
		} else {
			governor.cancelledWaiters++
		}
		governor.scheduleLocked()
	}
	governor.mu.Unlock()
	return cause
}

func (governor *Governor) scheduleLocked() {
	slices.SortStableFunc(governor.waiters, compareWaiters)
	for {
		index := governor.firstAdmissibleLocked()
		if index < 0 {
			governor.requestPreemptionLocked()
			return
		}
		pending := governor.waiters[index]
		governor.waiters = slices.Delete(governor.waiters, index, index+1)
		grantedAt := governor.clock()
		governor.nextLeaseID++
		leaseContext, cancel := context.WithCancelCause(pending.parent)
		lease := &Lease{
			id: governor.nextLeaseID, request: pending.request, governor: governor,
			ctx: leaseContext, cancel: cancel,
		}
		pending.state, pending.lease = granted, lease
		governor.running[lease.id] = &runningLease{lease: lease, sequence: pending.sequence, granted: grantedAt}
		governor.used += pending.request.Cost
		if pending.request.Class <= ClassSpeculative {
			governor.lowerPriorityUsed += pending.request.Cost
		}
		governor.admitted++
		timing := governor.classTimingLocked(pending.request.Class)
		timing.admitted++
		wait := nonnegativeDuration(grantedAt.Sub(pending.enqueued))
		timing.waitTotal += wait
		timing.waitMax = max(timing.waitMax, wait)
		pending.ready <- lease
	}
}

func compareWaiters(left, right *waiter) int {
	if left.request.Class != right.request.Class {
		return int(right.request.Class) - int(left.request.Class)
	}
	leftDeadline, rightDeadline := left.request.Deadline, right.request.Deadline
	if leftDeadline.IsZero() != rightDeadline.IsZero() {
		if leftDeadline.IsZero() {
			return 1
		}
		return -1
	}
	if !leftDeadline.Equal(rightDeadline) {
		if leftDeadline.Before(rightDeadline) {
			return -1
		}
		return 1
	}
	if left.sequence < right.sequence {
		return -1
	}
	if left.sequence > right.sequence {
		return 1
	}
	return 0
}

func (governor *Governor) firstAdmissibleLocked() int {
	for index, pending := range governor.waiters {
		if pending.state != waiting {
			continue
		}
		if !pending.request.Deadline.IsZero() && !governor.clock().Before(pending.request.Deadline) {
			pending.state = cancelled
			governor.deadlineMisses++
			continue
		}
		if governor.used+pending.request.Cost > governor.capacity {
			continue
		}
		if pending.request.Class <= ClassSpeculative &&
			governor.lowerPriorityUsed+pending.request.Cost > governor.capacity-governor.reservedInteractive {
			continue
		}
		return index
	}
	governor.waiters = slices.DeleteFunc(governor.waiters, func(pending *waiter) bool { return pending.state == cancelled })
	return -1
}

func (governor *Governor) requestPreemptionLocked() {
	if len(governor.waiters) == 0 {
		return
	}
	wanted := governor.waiters[0]
	if wanted.state != waiting {
		return
	}
	needed := governor.used + wanted.request.Cost - governor.capacity
	if wanted.request.Class <= ClassSpeculative {
		needed = max(needed, governor.lowerPriorityUsed+wanted.request.Cost-(governor.capacity-governor.reservedInteractive))
	}
	if needed <= 0 {
		return
	}
	var candidates []*runningLease
	for _, running := range governor.running {
		if !running.preempting && running.lease.request.Preemptible && running.lease.request.Class < wanted.request.Class {
			candidates = append(candidates, running)
		}
	}
	slices.SortFunc(candidates, func(left, right *runningLease) int {
		if left.lease.request.Class != right.lease.request.Class {
			return int(left.lease.request.Class) - int(right.lease.request.Class)
		}
		if left.lease.request.Cost != right.lease.request.Cost {
			return right.lease.request.Cost - left.lease.request.Cost
		}
		if left.sequence > right.sequence {
			return -1
		}
		return 1
	})
	for _, candidate := range candidates {
		candidate.preempting = true
		candidate.preempted = governor.clock()
		candidate.lease.cancel(ErrPreempted)
		governor.preemptions++
		governor.classTimingLocked(candidate.lease.request.Class).preemptions++
		needed -= candidate.lease.request.Cost
		if needed <= 0 {
			break
		}
	}
}

func (governor *Governor) removeWaiterLocked(target *waiter) {
	governor.waiters = slices.DeleteFunc(governor.waiters, func(pending *waiter) bool { return pending == target })
}

func (governor *Governor) release(id uint64) {
	governor.mu.Lock()
	running := governor.running[id]
	if running == nil {
		governor.mu.Unlock()
		return
	}
	delete(governor.running, id)
	governor.used -= running.lease.request.Cost
	if running.lease.request.Class <= ClassSpeculative {
		governor.lowerPriorityUsed -= running.lease.request.Cost
	}
	governor.released++
	now := governor.clock()
	timing := governor.classTimingLocked(running.lease.request.Class)
	timing.released++
	service := nonnegativeDuration(now.Sub(running.granted))
	timing.serviceTotal += service
	timing.serviceMax = max(timing.serviceMax, service)
	if !running.preempted.IsZero() {
		delay := nonnegativeDuration(now.Sub(running.preempted))
		timing.preemptionToReleaseTotal += delay
		timing.preemptionToReleaseMax = max(timing.preemptionToReleaseMax, delay)
	}
	running.lease.cancel(context.Canceled)
	governor.scheduleLocked()
	governor.mu.Unlock()
}

// Snapshot reports queue and capacity state without exposing request content.
func (governor *Governor) Snapshot() Snapshot {
	governor.mu.Lock()
	defer governor.mu.Unlock()
	result := Snapshot{
		Capacity: governor.capacity, ReservedInteractive: governor.reservedInteractive,
		Used: governor.used, LowerPriorityUsed: governor.lowerPriorityUsed,
		Running: make(map[string]int), Waiting: make(map[string]int),
		Admitted: governor.admitted, Released: governor.released,
		Preemptions: governor.preemptions, CancelledWaiters: governor.cancelledWaiters,
		DeadlineMisses: governor.deadlineMisses, Classes: make(map[string]ClassTiming),
	}
	for _, running := range governor.running {
		result.Running[running.lease.request.Class.String()]++
	}
	for _, pending := range governor.waiters {
		if pending.state == waiting {
			result.Waiting[pending.request.Class.String()]++
		}
	}
	for class, timing := range governor.classes {
		result.Classes[class.String()] = ClassTiming{
			Admitted: timing.admitted, Released: timing.released, Preemptions: timing.preemptions,
			WaitTotalMS: milliseconds(timing.waitTotal), WaitMaxMS: milliseconds(timing.waitMax),
			ServiceTotalMS: milliseconds(timing.serviceTotal), ServiceMaxMS: milliseconds(timing.serviceMax),
			PreemptionToReleaseTotalMS: milliseconds(timing.preemptionToReleaseTotal),
			PreemptionToReleaseMaxMS:   milliseconds(timing.preemptionToReleaseMax),
		}
	}
	return result
}

func (governor *Governor) classTimingLocked(class Class) *classTiming {
	timing := governor.classes[class]
	if timing == nil {
		timing = &classTiming{}
		governor.classes[class] = timing
	}
	return timing
}

func nonnegativeDuration(value time.Duration) time.Duration {
	if value < 0 {
		return 0
	}
	return value
}

func milliseconds(value time.Duration) float64 {
	return float64(value) / float64(time.Millisecond)
}

// Lease is cooperative ownership of admitted capacity.
type Lease struct {
	id       uint64
	request  Request
	governor *Governor
	ctx      context.Context
	cancel   context.CancelCauseFunc
	once     sync.Once
}

// Context is cancelled when the caller, its deadline, or higher-priority
// preemption asks the work to stop.
func (lease *Lease) Context() context.Context { return lease.ctx }

// Request returns the immutable scheduling declaration.
func (lease *Lease) Request() Request { return lease.request }

// Release acknowledges the provider's safe point and returns capacity. It is
// idempotent and must be called even after Context is cancelled.
func (lease *Lease) Release() {
	if lease == nil || lease.governor == nil {
		return
	}
	lease.once.Do(func() { lease.governor.release(lease.id) })
}
