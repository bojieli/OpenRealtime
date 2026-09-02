package runtime

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

type queue struct {
	id        string
	valueType element.Type
	delivery  ir.Delivery
	depth     int
	changed   *condition
	now       func() uint64
	trace     func(*queue, TraceKind, element.Envelope, int)

	mu         sync.Mutex
	items      []element.Envelope
	enqueuedAt []uint64
	head       int
	size       int
	closed     bool
	enqueued   uint64
	dequeued   uint64
	dropped    uint64
	blocked    uint64
	queueWait  uint64
	high       int
	lastID     string
}

func newQueue(
	id string,
	valueType element.Type,
	delivery ir.Delivery,
	depth int,
	changed *condition,
	now func() uint64,
	trace func(*queue, TraceKind, element.Envelope, int),
) (*queue, error) {
	if id == "" {
		return nil, fmt.Errorf("graph queue requires an ID")
	}
	if err := valueType.ValidateConcretePort(); err != nil {
		return nil, fmt.Errorf("graph queue %s: %w", id, err)
	}
	if depth <= 0 {
		return nil, fmt.Errorf("graph queue %s requires positive depth", id)
	}
	if delivery != ir.Lossless && delivery != ir.Lossy {
		return nil, fmt.Errorf("graph queue %s has invalid delivery %q", id, delivery)
	}
	if now == nil {
		return nil, fmt.Errorf("graph queue %s requires a monotonic clock", id)
	}
	return &queue{
		id: id, valueType: valueType.Clone(), delivery: delivery, depth: depth,
		changed: changed, now: now, trace: trace,
		items: make([]element.Envelope, depth), enqueuedAt: make([]uint64, depth),
	}, nil
}

func (queue *queue) send(ctx context.Context, envelope element.Envelope) (element.DeliveryResult, error) {
	return queue.sendWithLease(ctx, envelope, nil)
}

func (queue *queue) sendWithLease(
	ctx context.Context, envelope element.Envelope, lease *routeLease,
) (element.DeliveryResult, error) {
	if ctx == nil {
		return "", fmt.Errorf("send on %s: nil context", queue.id)
	}
	if err := envelope.ValidateFor(queue.valueType); err != nil {
		return "", err
	}
	reportedBackpressure := false
	for {
		queue.mu.Lock()
		if queue.closed {
			if !lease.beginCommit() {
				queue.mu.Unlock()
				return "", errBoundaryGenerationRetired
			}
			queue.mu.Unlock()
			lease.endCommit()
			return "", ErrChannelClosed
		}
		if queue.size < queue.depth {
			if !lease.beginCommit() {
				queue.mu.Unlock()
				return "", errBoundaryGenerationRetired
			}
			queue.enqueueLocked(envelope)
			occupancy := queue.size
			queue.mu.Unlock()
			lease.endCommit()
			queue.changed.signal()
			queue.emit(TraceEnqueue, envelope, occupancy)
			return element.Delivered, nil
		}
		if queue.delivery == ir.Lossy {
			if !lease.beginCommit() {
				queue.mu.Unlock()
				return "", errBoundaryGenerationRetired
			}
			queue.dropped++
			queue.lastID = envelope.ItemID
			occupancy := queue.size
			queue.mu.Unlock()
			lease.endCommit()
			queue.emit(TraceDrop, envelope, occupancy)
			return element.Dropped, nil
		}
		if !reportedBackpressure {
			queue.blocked++
			reportedBackpressure = true
		}
		wait := queue.changed.current()
		occupancy := queue.size
		queue.mu.Unlock()
		if reportedBackpressure {
			queue.emit(TraceBackpressure, envelope, occupancy)
		}
		if err := waitForQueueChange(ctx, wait, lease); err != nil {
			return "", err
		}
	}
}

func (queue *queue) receive(ctx context.Context) (element.Envelope, error) {
	return queue.receiveWithLease(ctx, nil)
}

func (queue *queue) receiveWithLease(
	ctx context.Context, lease *routeLease,
) (element.Envelope, error) {
	if ctx == nil {
		return element.Envelope{}, fmt.Errorf("receive on %s: nil context", queue.id)
	}
	for {
		queue.mu.Lock()
		if queue.size > 0 {
			if !lease.beginCommit() {
				queue.mu.Unlock()
				return element.Envelope{}, errBoundaryGenerationRetired
			}
			envelope := queue.dequeueLocked()
			occupancy := queue.size
			queue.mu.Unlock()
			lease.endCommit()
			queue.changed.signal()
			queue.emit(TraceDequeue, envelope, occupancy)
			return envelope, nil
		}
		if queue.closed {
			if !lease.beginCommit() {
				queue.mu.Unlock()
				return element.Envelope{}, errBoundaryGenerationRetired
			}
			queue.mu.Unlock()
			lease.endCommit()
			return element.Envelope{}, ErrChannelClosed
		}
		wait := queue.changed.current()
		queue.mu.Unlock()
		if err := waitForQueueChange(ctx, wait, lease); err != nil {
			return element.Envelope{}, err
		}
	}
}

func (queue *queue) tryReceive() (element.Envelope, bool, bool) {
	envelope, received, closed, _ := queue.tryReceiveWithLease(nil)
	return envelope, received, closed
}

func (queue *queue) tryReceiveWithLease(
	lease *routeLease,
) (element.Envelope, bool, bool, bool) {
	queue.mu.Lock()
	if queue.size > 0 {
		if !lease.beginCommit() {
			queue.mu.Unlock()
			return element.Envelope{}, false, false, true
		}
		envelope := queue.dequeueLocked()
		occupancy := queue.size
		queue.mu.Unlock()
		lease.endCommit()
		queue.changed.signal()
		queue.emit(TraceDequeue, envelope, occupancy)
		return envelope, true, false, false
	}
	closed := queue.closed
	queue.mu.Unlock()
	return element.Envelope{}, false, closed, false
}

func waitForQueueChange(ctx context.Context, changed <-chan struct{}, lease *routeLease) error {
	var retired <-chan struct{}
	if lease != nil {
		retired = lease.retiredSignal()
	}
	if err := routeWaitError(ctx, lease); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
	case <-retired:
	case <-changed:
	}
	// A queue close/capacity signal may race retirement. Recheck with stable
	// priority so a retired, provably-uncommitted operation never leaks a
	// generation-local ErrChannelClosed through the public stable wrapper.
	return routeWaitError(ctx, lease)
}

func routeWaitError(ctx context.Context, lease *routeLease) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if lease.retired() {
		return errBoundaryGenerationRetired
	}
	return nil
}

func (queue *queue) enqueueLocked(envelope element.Envelope) {
	index := (queue.head + queue.size) % queue.depth
	queue.items[index] = envelope.Clone()
	queue.enqueuedAt[index] = queue.now()
	queue.size++
	queue.enqueued++
	queue.lastID = envelope.ItemID
	if queue.size > queue.high {
		queue.high = queue.size
	}
}

func (queue *queue) dequeueLocked() element.Envelope {
	envelope := queue.items[queue.head]
	enqueuedAt := queue.enqueuedAt[queue.head]
	queue.items[queue.head] = element.Envelope{}
	queue.enqueuedAt[queue.head] = 0
	queue.head = (queue.head + 1) % queue.depth
	queue.size--
	queue.dequeued++
	queue.lastID = envelope.ItemID
	now := queue.now()
	if now >= enqueuedAt {
		queue.queueWait = saturatingAdd(queue.queueWait, now-enqueuedAt)
	}
	return envelope
}

func (queue *queue) close() {
	queue.mu.Lock()
	if queue.closed {
		queue.mu.Unlock()
		return
	}
	queue.closed = true
	queue.mu.Unlock()
	queue.changed.signal()
}

func (queue *queue) snapshot() inspect.EdgeLive {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	return inspect.EdgeLive{
		Occupancy: queue.size, HighWater: queue.high,
		Enqueued: queue.enqueued, Dequeued: queue.dequeued,
		Dropped: queue.dropped, Backpressure: queue.blocked,
		LastItemID: queue.lastID, QueueWaitNS: queue.queueWait,
	}
}

func (queue *queue) emit(kind TraceKind, envelope element.Envelope, occupancy int) {
	if queue.trace != nil {
		// The callback takes a queue only for immutable identity/depth fields;
		// occupancy was captured under the queue lock.
		queue.trace(queue, kind, envelope, occupancy)
	}
}

type envelopeObserver func(element.Envelope)

type sender struct {
	queue   *queue
	observe envelopeObserver
}

func (sender *sender) ID() string         { return sender.queue.id }
func (sender *sender) Type() element.Type { return sender.queue.valueType.Clone() }
func (sender *sender) Send(ctx context.Context, envelope element.Envelope) (element.DeliveryResult, error) {
	return sender.sendWithLease(ctx, envelope, nil)
}

func (sender *sender) sendWithLease(
	ctx context.Context, envelope element.Envelope, lease *routeLease,
) (element.DeliveryResult, error) {
	result, err := sender.queue.sendWithLease(ctx, envelope, lease)
	if err == nil && sender.observe != nil {
		sender.observe(envelope)
	}
	return result, err
}

type receiver struct {
	queue   *queue
	observe envelopeObserver
}

func (receiver *receiver) ID() string         { return receiver.queue.id }
func (receiver *receiver) Type() element.Type { return receiver.queue.valueType.Clone() }
func (receiver *receiver) Receive(ctx context.Context) (element.Envelope, error) {
	return receiver.receiveWithLease(ctx, nil)
}

func (receiver *receiver) receiveWithLease(
	ctx context.Context, lease *routeLease,
) (element.Envelope, error) {
	envelope, err := receiver.queue.receiveWithLease(ctx, lease)
	if err == nil && receiver.observe != nil {
		receiver.observe(envelope)
	}
	return envelope, err
}

type outputPort struct {
	name    string
	typ     element.Type
	queues  []*queue
	changed *condition
	observe envelopeObserver
}

func (port *outputPort) Name() string       { return port.name }
func (port *outputPort) Type() element.Type { return port.typ.Clone() }
func (port *outputPort) Lanes() []element.Sender {
	result := make([]element.Sender, len(port.queues))
	for index, queue := range port.queues {
		result[index] = &sender{queue: queue, observe: port.observe}
	}
	return result
}

func (port *outputPort) Broadcast(ctx context.Context, envelope element.Envelope) (element.SendResult, error) {
	return port.broadcastWithLease(ctx, envelope, nil)
}

func (port *outputPort) broadcastWithLease(
	ctx context.Context, envelope element.Envelope, lease *routeLease,
) (element.SendResult, error) {
	if ctx == nil {
		return element.SendResult{}, fmt.Errorf("broadcast on %s: nil context", port.name)
	}
	if err := envelope.ValidateFor(port.typ); err != nil {
		return element.SendResult{}, err
	}
	if len(port.queues) == 0 {
		if !lease.beginCommit() {
			return element.SendResult{}, errBoundaryGenerationRetired
		}
		if port.observe != nil {
			port.observe(envelope)
		}
		lease.endCommit()
		return element.SendResult{}, nil
	}
	queues := append([]*queue(nil), port.queues...)
	sort.Slice(queues, func(left, right int) bool { return queues[left].id < queues[right].id })
	blocked := make(map[*queue]bool)
	for {
		for _, queue := range queues {
			queue.mu.Lock()
		}
		var fullLossless []*queue
		for _, queue := range queues {
			if queue.closed {
				if !lease.beginCommit() {
					unlockQueues(queues)
					return element.SendResult{}, errBoundaryGenerationRetired
				}
				unlockQueues(queues)
				lease.endCommit()
				return element.SendResult{}, fmt.Errorf("broadcast on %s lane %s: %w", port.name, queue.id, ErrChannelClosed)
			}
			if queue.delivery == ir.Lossless && queue.size == queue.depth {
				fullLossless = append(fullLossless, queue)
				if !blocked[queue] {
					queue.blocked++
					blocked[queue] = true
				}
			}
		}
		if len(fullLossless) > 0 {
			wait := port.changed.current()
			occupancies := make(map[*queue]int, len(fullLossless))
			for _, queue := range fullLossless {
				occupancies[queue] = queue.size
			}
			unlockQueues(queues)
			for _, queue := range fullLossless {
				if blocked[queue] {
					queue.emit(TraceBackpressure, envelope, occupancies[queue])
				}
			}
			if err := waitForQueueChange(ctx, wait, lease); err != nil {
				return element.SendResult{}, err
			}
			continue
		}

		if !lease.beginCommit() {
			unlockQueues(queues)
			return element.SendResult{}, errBoundaryGenerationRetired
		}
		result := element.SendResult{}
		type emitted struct {
			queue     *queue
			kind      TraceKind
			occupancy int
		}
		emittedEvents := make([]emitted, 0, len(queues))
		for _, queue := range queues {
			if queue.size == queue.depth { // Only a lossy queue can be full here.
				queue.dropped++
				queue.lastID = envelope.ItemID
				result.Dropped++
				emittedEvents = append(emittedEvents, emitted{queue: queue, kind: TraceDrop, occupancy: queue.size})
				continue
			}
			queue.enqueueLocked(envelope)
			result.Delivered++
			emittedEvents = append(emittedEvents, emitted{queue: queue, kind: TraceEnqueue, occupancy: queue.size})
		}
		unlockQueues(queues)
		lease.endCommit()
		port.changed.signal()
		for _, event := range emittedEvents {
			event.queue.emit(event.kind, envelope, event.occupancy)
		}
		if port.observe != nil {
			port.observe(envelope)
		}
		return result, nil
	}
}

func unlockQueues(queues []*queue) {
	for index := len(queues) - 1; index >= 0; index-- {
		queues[index].mu.Unlock()
	}
}

type inputPort struct {
	name    string
	typ     element.Type
	queues  []*queue
	changed *condition
	observe envelopeObserver

	mu     sync.Mutex
	cursor int
}

func (port *inputPort) Name() string       { return port.name }
func (port *inputPort) Type() element.Type { return port.typ.Clone() }
func (port *inputPort) Lanes() []element.Receiver {
	result := make([]element.Receiver, len(port.queues))
	for index, queue := range port.queues {
		result[index] = &receiver{queue: queue, observe: port.observe}
	}
	return result
}

func (port *inputPort) Receive(ctx context.Context) (element.Envelope, error) {
	return port.receiveWithLease(ctx, nil)
}

func (port *inputPort) receiveWithLease(
	ctx context.Context, lease *routeLease,
) (element.Envelope, error) {
	if len(port.queues) == 0 {
		return element.Envelope{}, fmt.Errorf("input %s: %w", port.name, ErrPortUnbound)
	}
	if len(port.queues) != 1 {
		return element.Envelope{}, fmt.Errorf("input %s has %d lanes: %w", port.name, len(port.queues), ErrPortCardinality)
	}
	envelope, err := port.queues[0].receiveWithLease(ctx, lease)
	if err == nil && port.observe != nil {
		port.observe(envelope)
	}
	return envelope, err
}

func (port *inputPort) ReceiveAny(ctx context.Context) (element.Envelope, string, error) {
	return port.receiveAnyWithLease(ctx, nil)
}

func (port *inputPort) receiveAnyWithLease(
	ctx context.Context, lease *routeLease,
) (element.Envelope, string, error) {
	if ctx == nil {
		return element.Envelope{}, "", fmt.Errorf("receive on %s: nil context", port.name)
	}
	if len(port.queues) == 0 {
		return element.Envelope{}, "", fmt.Errorf("input %s: %w", port.name, ErrPortUnbound)
	}
	for {
		wait := port.changed.current()
		port.mu.Lock()
		start := port.cursor
		allClosed := true
		for offset := range port.queues {
			index := (start + offset) % len(port.queues)
			envelope, received, closed, retired := port.queues[index].tryReceiveWithLease(lease)
			if retired {
				port.mu.Unlock()
				return element.Envelope{}, "", errBoundaryGenerationRetired
			}
			if !closed {
				allClosed = false
			}
			if received {
				port.cursor = (index + 1) % len(port.queues)
				lane := port.queues[index].id
				port.mu.Unlock()
				if port.observe != nil {
					port.observe(envelope)
				}
				return envelope, lane, nil
			}
		}
		port.mu.Unlock()
		if allClosed {
			if !lease.beginCommit() {
				return element.Envelope{}, "", errBoundaryGenerationRetired
			}
			lease.endCommit()
			return element.Envelope{}, "", ErrChannelClosed
		}
		if err := waitForQueueChange(ctx, wait, lease); err != nil {
			return element.Envelope{}, "", err
		}
	}
}
