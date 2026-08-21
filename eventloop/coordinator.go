package eventloop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bojieli/OpenRealtime/trajectory"
)

var (
	// ErrIdle means there is nothing committed or pending to act upon.
	ErrIdle = errors.New("event coordinator has no pending events")
	// ErrBusy means a run is already in flight and this one cannot join it.
	ErrBusy = errors.New("event coordinator is already processing a batch")
	// ErrInterrupted is the cancellation cause for an interrupting event.
	ErrInterrupted = errors.New("event processing interrupted by an urgent event")
	// ErrQueueFull means ingress capacity is exhausted.
	ErrQueueFull = errors.New("event coordinator pending queue is full")
	// ErrDeferred means the batch was committed but a gate declined to act on
	// it now. It is an ordinary outcome, not a failure: the events remain
	// committed and a later wake-up runs them.
	ErrDeferred = errors.New("event batch committed and deferred by policy")
)

// Processor advances cognition from a committed event batch. The context is
// cancelled when an interrupting event arrives; the processor must stop at its
// next safe boundary.
type Processor interface {
	Process(context.Context, Batch) error
}

type ProcessorFunc func(context.Context, Batch) error

func (function ProcessorFunc) Process(ctx context.Context, batch Batch) error {
	return function(ctx, batch)
}

// Gate decides whether committed work may be acted upon now.
//
// This is the conditional half of the invariant. A gate that declines does not
// consume the events: they stay committed and unacted, and whoever declined
// owes a Wake when its condition clears. The gate sees the batch and nothing
// else — the duplex state, admission leases, and provider backpressure it
// consults are its own, because they are interaction policy rather than event
// loop mechanism.
type Gate interface {
	AdmitRun(context.Context, Batch) (bool, string)
}

type GateFunc func(context.Context, Batch) (bool, string)

func (function GateFunc) AdmitRun(ctx context.Context, batch Batch) (bool, string) {
	return function(ctx, batch)
}

// Deferral records why committed work is waiting and what must happen next. It
// is operational telemetry: a deferral with no matching wake-up is the bug
// this type exists to make visible.
type Deferral struct {
	Reason       string `json:"reason"`
	Events       int    `json:"events"`
	Items        int    `json:"items"`
	SinceNS      uint64 `json:"since_ns"`
	StartVersion uint64 `json:"start_version"`
	EndVersion   uint64 `json:"end_version"`
}

type Config struct {
	Store     *trajectory.Store
	Processor Processor
	// Gate is optional. Without one, every committed batch runs immediately,
	// which is the correct behaviour for a runtime that has no reason to wait.
	Gate Gate
	// MaxPendingEvents is a required hard ingress bound.
	MaxPendingEvents int
	// ReservedInterruptEvents protects capacity from routine-event bursts. It
	// may be zero, but must be smaller than MaxPendingEvents.
	ReservedInterruptEvents int
	Now                     func() uint64
	NextID                  func(prefix string) string
}

type queuedEvent struct {
	sequence uint64
	event    Event
}

// Coordinator owns ingress ordering, safe-point commits, deferral bookkeeping,
// and interruption of the current processor invocation. Model output still
// commits through the continuation runner's own version-checked transaction.
type Coordinator struct {
	mu   sync.Mutex
	idMu sync.Mutex

	store     *trajectory.Store
	processor Processor
	gate      Gate
	now       func() uint64
	nextID    func(string) string

	pending      []queuedEvent
	eventIDs     map[string]struct{}
	nextSequence uint64

	// deferred holds committed batches that no run has yet acted upon. This is
	// the state that "committed but not yet acted upon" required and that the
	// loop previously could not represent at all.
	deferred     []Batch
	deferReason  string
	deferSinceNS uint64

	active       bool
	activeEpoch  uint64
	activeCancel context.CancelCauseFunc
	parallel     int

	signal     chan struct{}
	maxPending int
	reserved   int

	committedBatches atomic.Uint64
	deferredBatches  atomic.Uint64
	wakeups          atomic.Uint64
	parallelRuns     atomic.Uint64
}

func New(config Config) (*Coordinator, error) {
	if config.Store == nil {
		return nil, errors.New("event coordinator requires a trajectory store")
	}
	if config.MaxPendingEvents <= 0 {
		return nil, errors.New("event coordinator requires a positive pending-event bound")
	}
	if config.ReservedInterruptEvents < 0 || config.ReservedInterruptEvents >= config.MaxPendingEvents {
		return nil, errors.New("reserved interrupt capacity must be non-negative and smaller than the pending-event bound")
	}
	if config.Now == nil {
		origin := time.Now()
		config.Now = func() uint64 { return uint64(time.Since(origin)) }
	}
	if config.NextID == nil {
		var counter atomic.Uint64
		config.NextID = func(prefix string) string {
			return prefix + "-" + strconv.FormatUint(counter.Add(1), 10)
		}
	}
	return &Coordinator{
		store: config.Store, processor: config.Processor, gate: config.Gate,
		now: config.Now, nextID: config.NextID, eventIDs: make(map[string]struct{}),
		signal:     make(chan struct{}, 1),
		maxPending: config.MaxPendingEvents, reserved: config.ReservedInterruptEvents,
	}, nil
}

// SetGate installs the deferral policy after construction.
//
// The gate needs the coordinator as its waker and the coordinator needs the
// gate, which is a genuine cycle rather than an accident of ordering. Breaking
// it with a setter is honest about that; the alternative is a lazily resolved
// indirection that hides when deferral actually becomes active.
func (coordinator *Coordinator) SetGate(gate Gate) {
	coordinator.mu.Lock()
	coordinator.gate = gate
	coordinator.mu.Unlock()
}

func (coordinator *Coordinator) currentGate() Gate {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	return coordinator.gate
}

// Signal fires whenever there is work to do: an event arrived, or a wake-up
// released work that a gate had deferred. It is level-triggered with a depth
// of one, so a driver that always drains to idle after receiving cannot miss
// an edge.
func (coordinator *Coordinator) Signal() <-chan struct{} { return coordinator.signal }

func (coordinator *Coordinator) notify() {
	select {
	case coordinator.signal <- struct{}{}:
	default:
	}
}

// Wake declares that a deferral condition has cleared.
//
// Every condition a gate defers on owes exactly one of these: playback
// completion, the end of a user turn, an admission lease, relief from provider
// backpressure. If deferred work exists, it signals a run. Calling it when
// nothing is waiting is a no-op, which is what lets callers wake
// unconditionally on a state transition rather than checking first.
func (coordinator *Coordinator) Wake(reason string) {
	coordinator.mu.Lock()
	waiting := len(coordinator.deferred) > 0 || len(coordinator.pending) > 0
	coordinator.mu.Unlock()
	if !waiting {
		return
	}
	coordinator.wakeups.Add(1)
	coordinator.notify()
}

// Submit validates and queues an event without waiting for cognition.
func (coordinator *Coordinator) Submit(event Event) (string, error) {
	ids, err := coordinator.SubmitBatch([]Event{event})
	if err != nil {
		return "", err
	}
	return ids[0], nil
}

// SubmitBatch validates and queues one inseparable ingress group. Either every
// event is admitted in the supplied order or none is. Media commitment
// lifecycles rely on this: their played and repair transitions must never be
// split by queue backpressure.
func (coordinator *Coordinator) SubmitBatch(events []Event) ([]string, error) {
	if len(events) == 0 {
		return nil, errors.New("event batch must not be empty")
	}
	events = slices.Clone(events)
	for index := range events {
		if err := validateEvent(events[index]); err != nil {
			return nil, fmt.Errorf("event %d: %w", index, err)
		}
		events[index] = cloneEvent(events[index])
	}
	coordinator.idMu.Lock()
	for index := range events {
		if events[index].EventID == "" {
			events[index].EventID = coordinator.nextID("event")
		}
	}
	coordinator.idMu.Unlock()
	for _, event := range events {
		if strings.TrimSpace(event.EventID) == "" {
			return nil, errors.New("event ID generator returned an empty ID")
		}
	}

	coordinator.mu.Lock()
	batchIDs := make(map[string]struct{}, len(events))
	for index, event := range events {
		limit := coordinator.maxPending
		if event.Priority == PriorityRoutine {
			limit -= coordinator.reserved
		}
		if len(coordinator.pending)+index >= limit {
			coordinator.mu.Unlock()
			return nil, ErrQueueFull
		}
		if _, duplicate := coordinator.eventIDs[event.EventID]; duplicate {
			coordinator.mu.Unlock()
			return nil, fmt.Errorf("duplicate event ID %q", event.EventID)
		}
		if _, duplicate := batchIDs[event.EventID]; duplicate {
			coordinator.mu.Unlock()
			return nil, fmt.Errorf("duplicate event ID %q", event.EventID)
		}
		batchIDs[event.EventID] = struct{}{}
	}
	ids := make([]string, len(events))
	shouldInterrupt := false
	cancel := coordinator.activeCancel
	for index, event := range events {
		coordinator.eventIDs[event.EventID] = struct{}{}
		coordinator.nextSequence++
		coordinator.pending = append(coordinator.pending, queuedEvent{sequence: coordinator.nextSequence, event: event})
		ids[index] = event.EventID
		shouldInterrupt = shouldInterrupt || event.Priority == PriorityInterrupt
	}
	coordinator.mu.Unlock()
	if shouldInterrupt && cancel != nil {
		cancel(ErrInterrupted)
	}
	coordinator.notify()
	return ids, nil
}

// RunNext commits everything pending and, if policy admits it, acts on that
// batch together with anything a previous gate deferred.
//
// The two halves are deliberately not separable from the caller's side: commit
// happens on every call, whatever the gate decides, which is what makes commit
// unconditional in practice rather than only in principle. When the gate
// declines, the committed batch is returned alongside ErrDeferred so a caller
// can observe what it committed without being able to lose it.
func (coordinator *Coordinator) RunNext(parent context.Context) (Batch, error) {
	if parent == nil {
		return Batch{}, errors.New("event processor context must not be nil")
	}
	committed, commitErr := coordinator.Commit()
	if commitErr != nil && !errors.Is(commitErr, ErrIdle) {
		return Batch{}, commitErr
	}
	return coordinator.run(parent, committed)
}

// Commit drains the pending ingress queue and appends it atomically. It never
// consults the gate: an external event enters the trajectory the moment it
// arrives, whatever the conversational state.
func (coordinator *Coordinator) Commit() (Batch, error) {
	coordinator.mu.Lock()
	if len(coordinator.pending) == 0 {
		coordinator.mu.Unlock()
		return Batch{}, ErrIdle
	}
	queued := slices.Clone(coordinator.pending)
	coordinator.pending = nil
	coordinator.mu.Unlock()

	batch, err := coordinator.commit(queued)
	if err != nil {
		if errors.Is(err, trajectory.ErrVersionConflict) {
			coordinator.requeue(queued)
		} else {
			coordinator.forget(queued)
		}
		return Batch{}, err
	}
	coordinator.committedBatches.Add(1)
	coordinator.mu.Lock()
	coordinator.deferred = append(coordinator.deferred, batch)
	coordinator.mu.Unlock()
	return batch, nil
}

func (coordinator *Coordinator) run(parent context.Context, committed Batch) (Batch, error) {
	coordinator.mu.Lock()
	if len(coordinator.deferred) == 0 {
		coordinator.mu.Unlock()
		return Batch{}, ErrIdle
	}
	work := merge(coordinator.deferred)
	parallelBranch := work.Triage == TriageParallel && coordinator.active
	if coordinator.active && !parallelBranch {
		coordinator.mu.Unlock()
		return committed, ErrBusy
	}
	coordinator.mu.Unlock()

	if gate := coordinator.currentGate(); gate != nil {
		admitted, reason := gate.AdmitRun(parent, work)
		if !admitted {
			coordinator.markDeferred(reason)
			return committed, fmt.Errorf("%w: %s", ErrDeferred, reason)
		}
	}

	coordinator.mu.Lock()
	// Re-check under the lock: another run may have claimed the work while the
	// gate was deciding.
	if len(coordinator.deferred) == 0 {
		coordinator.mu.Unlock()
		return Batch{}, ErrIdle
	}
	work = merge(coordinator.deferred)
	parallelBranch = work.Triage == TriageParallel && coordinator.active
	if coordinator.active && !parallelBranch {
		coordinator.mu.Unlock()
		return committed, ErrBusy
	}
	coordinator.deferred = nil
	coordinator.deferReason = ""
	coordinator.deferSinceNS = 0
	ctx, cancel := context.WithCancelCause(parent)
	var epoch uint64
	if parallelBranch {
		coordinator.parallel++
		coordinator.parallelRuns.Add(1)
	} else {
		coordinator.active = true
		coordinator.activeEpoch++
		epoch = coordinator.activeEpoch
		coordinator.activeCancel = cancel
	}
	coordinator.mu.Unlock()

	var processErr error
	if coordinator.processor != nil {
		processErr = coordinator.processor.Process(ctx, work)
	}
	if cause := context.Cause(ctx); cause != nil {
		processErr = errors.Join(processErr, cause)
	}
	if parallelBranch {
		coordinator.mu.Lock()
		coordinator.parallel--
		coordinator.mu.Unlock()
	} else {
		coordinator.finish(epoch)
	}
	cancel(nil)
	return work, processErr
}

// markDeferred returns work to the deferred set and records why.
func (coordinator *Coordinator) markDeferred(reason string) {
	if strings.TrimSpace(reason) == "" {
		reason = "unspecified policy deferral"
	}
	coordinator.deferredBatches.Add(1)
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	coordinator.deferReason = reason
	if coordinator.deferSinceNS == 0 {
		coordinator.deferSinceNS = coordinator.now()
	}
}

// Deferral describes the committed work currently waiting for a wake-up.
func (coordinator *Coordinator) Deferral() (Deferral, bool) {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if len(coordinator.deferred) == 0 {
		return Deferral{}, false
	}
	work := merge(coordinator.deferred)
	return Deferral{
		Reason: coordinator.deferReason, Events: len(work.Events), Items: len(work.Items),
		SinceNS: coordinator.deferSinceNS, StartVersion: work.StartVersion, EndVersion: work.EndVersion,
	}, true
}

// Metrics is operational telemetry with no model input in it.
type Metrics struct {
	CommittedBatches uint64 `json:"committed_batches"`
	DeferredBatches  uint64 `json:"deferred_batches"`
	Wakeups          uint64 `json:"wakeups"`
	ParallelRuns     uint64 `json:"parallel_runs"`
	PendingEvents    int    `json:"pending_events"`
	UnactedBatches   int    `json:"unacted_batches"`
}

func (coordinator *Coordinator) Metrics() Metrics {
	coordinator.mu.Lock()
	pending, unacted := len(coordinator.pending), len(coordinator.deferred)
	coordinator.mu.Unlock()
	return Metrics{
		CommittedBatches: coordinator.committedBatches.Load(),
		DeferredBatches:  coordinator.deferredBatches.Load(),
		Wakeups:          coordinator.wakeups.Load(),
		ParallelRuns:     coordinator.parallelRuns.Load(),
		PendingEvents:    pending, UnactedBatches: unacted,
	}
}

func (coordinator *Coordinator) Pending() int {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	return len(coordinator.pending)
}

// Unacted reports how many committed batches are waiting to be acted upon.
func (coordinator *Coordinator) Unacted() int {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	return len(coordinator.deferred)
}

// Runnable reports whether a call to RunNext would do anything.
func (coordinator *Coordinator) Runnable() bool {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	return len(coordinator.pending) > 0 || len(coordinator.deferred) > 0
}

func (coordinator *Coordinator) Active() bool {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	return coordinator.active
}

// Interrupt cooperatively cancels the processor currently running at a safe
// point without fabricating a model-visible trajectory item. It is used for
// operational signals such as the acoustic start of user speech: the final
// transcript is submitted later as the authoritative observation event.
func (coordinator *Coordinator) Interrupt(cause error) {
	if cause == nil {
		cause = ErrInterrupted
	}
	coordinator.mu.Lock()
	cancel := coordinator.activeCancel
	coordinator.mu.Unlock()
	if cancel != nil {
		cancel(cause)
	}
}

func (coordinator *Coordinator) finish(epoch uint64) {
	coordinator.mu.Lock()
	waiting := len(coordinator.deferred) > 0 || len(coordinator.pending) > 0
	if coordinator.active && coordinator.activeEpoch == epoch {
		coordinator.active = false
		coordinator.activeCancel = nil
	}
	coordinator.mu.Unlock()
	if waiting {
		coordinator.notify()
	}
}

func (coordinator *Coordinator) requeue(events []queuedEvent) {
	coordinator.mu.Lock()
	coordinator.pending = append(slices.Clone(events), coordinator.pending...)
	coordinator.mu.Unlock()
	coordinator.notify()
}

func (coordinator *Coordinator) forget(events []queuedEvent) {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	for _, event := range events {
		delete(coordinator.eventIDs, event.event.EventID)
	}
}

func (coordinator *Coordinator) commit(queued []queuedEvent) (Batch, error) {
	slices.SortStableFunc(queued, func(left, right queuedEvent) int {
		switch {
		case left.sequence < right.sequence:
			return -1
		case left.sequence > right.sequence:
			return 1
		default:
			return 0
		}
	})
	events := make([]Event, len(queued))
	triage := TriageQueue
	for index := range queued {
		events[index] = cloneEvent(queued[index].event)
		switch TriageOf(events[index].Priority) {
		case TriageCancel:
			triage = TriageCancel
		case TriageParallel:
			if triage != TriageCancel {
				triage = TriageParallel
			}
		}
	}
	snapshot := coordinator.store.Snapshot()
	items, err := coordinator.compile(snapshot, events)
	if err != nil {
		return Batch{}, err
	}
	coordinator.idMu.Lock()
	batchID := coordinator.nextID("batch")
	coordinator.idMu.Unlock()
	markBatch(batchID, items)
	if err := coordinator.store.AppendBatchAt(snapshot.Version, items); err != nil {
		return Batch{}, fmt.Errorf("commit external event batch: %w", err)
	}
	committed := coordinator.store.Snapshot()
	return Batch{
		Events: events, Items: slices.Clone(committed.Items[snapshot.Version:]),
		StartVersion: snapshot.Version, EndVersion: committed.Version, Triage: triage,
	}, nil
}
