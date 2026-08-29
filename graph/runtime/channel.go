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
			queue.mu.Unlock()
			return "", ErrChannelClosed
		}
		if queue.size < queue.depth {
			queue.enqueueLocked(envelope)
			occupancy := queue.size
			queue.mu.Unlock()
			queue.changed.signal()
			queue.emit(TraceEnqueue, envelope, occupancy)
			return element.Delivered, nil
		}
		if queue.delivery == ir.Lossy {
			queue.dropped++
			queue.lastID = envelope.ItemID
			occupancy := queue.size
			queue.mu.Unlock()
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
		select {
		case <-ctx.Done():
			return "", context.Cause(ctx)
		case <-wait:
		}
	}
}

func (queue *queue) receive(ctx context.Context) (element.Envelope, error) {
	if ctx == nil {
		return element.Envelope{}, fmt.Errorf("receive on %s: nil context", queue.id)
	}
	for {
		queue.mu.Lock()
		if queue.size > 0 {
			envelope := queue.dequeueLocked()
			occupancy := queue.size
			queue.mu.Unlock()
			queue.changed.signal()
			queue.emit(TraceDequeue, envelope, occupancy)
			return envelope, nil
		}
		if queue.closed {
			queue.mu.Unlock()
			return element.Envelope{}, ErrChannelClosed
		}
		wait := queue.changed.current()
		queue.mu.Unlock()
		select {
		case <-ctx.Done():
			return element.Envelope{}, context.Cause(ctx)
		case <-wait:
		}
	}
}

func (queue *queue) tryReceive() (element.Envelope, bool, bool) {
	queue.mu.Lock()
	if queue.size > 0 {
		envelope := queue.dequeueLocked()
		occupancy := queue.size
		queue.mu.Unlock()
		queue.changed.signal()
		queue.emit(TraceDequeue, envelope, occupancy)
		return envelope, true, false
	}
	closed := queue.closed
	queue.mu.Unlock()
	return element.Envelope{}, false, closed
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

type sender struct{ queue *queue }

func (sender *sender) ID() string         { return sender.queue.id }
func (sender *sender) Type() element.Type { return sender.queue.valueType.Clone() }
func (sender *sender) Send(ctx context.Context, envelope element.Envelope) (element.DeliveryResult, error) {
	return sender.queue.send(ctx, envelope)
}

type receiver struct{ queue *queue }

func (receiver *receiver) ID() string         { return receiver.queue.id }
func (receiver *receiver) Type() element.Type { return receiver.queue.valueType.Clone() }
func (receiver *receiver) Receive(ctx context.Context) (element.Envelope, error) {
	return receiver.queue.receive(ctx)
}

type outputPort struct {
	name    string
	typ     element.Type
	queues  []*queue
	changed *condition
}

func (port *outputPort) Name() string       { return port.name }
func (port *outputPort) Type() element.Type { return port.typ.Clone() }
func (port *outputPort) Lanes() []element.Sender {
	result := make([]element.Sender, len(port.queues))
	for index, queue := range port.queues {
		result[index] = &sender{queue: queue}
	}
	return result
}

func (port *outputPort) Broadcast(ctx context.Context, envelope element.Envelope) (element.SendResult, error) {
	if ctx == nil {
		return element.SendResult{}, fmt.Errorf("broadcast on %s: nil context", port.name)
	}
	if err := envelope.ValidateFor(port.typ); err != nil {
		return element.SendResult{}, err
	}
	if len(port.queues) == 0 {
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
				unlockQueues(queues)
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
			select {
			case <-ctx.Done():
				return element.SendResult{}, context.Cause(ctx)
			case <-wait:
				continue
			}
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
		port.changed.signal()
		for _, event := range emittedEvents {
			event.queue.emit(event.kind, envelope, event.occupancy)
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

	mu     sync.Mutex
	cursor int
}

func (port *inputPort) Name() string       { return port.name }
func (port *inputPort) Type() element.Type { return port.typ.Clone() }
func (port *inputPort) Lanes() []element.Receiver {
	result := make([]element.Receiver, len(port.queues))
	for index, queue := range port.queues {
		result[index] = &receiver{queue: queue}
	}
	return result
}

func (port *inputPort) Receive(ctx context.Context) (element.Envelope, error) {
	if len(port.queues) == 0 {
		return element.Envelope{}, fmt.Errorf("input %s: %w", port.name, ErrPortUnbound)
	}
	if len(port.queues) != 1 {
		return element.Envelope{}, fmt.Errorf("input %s has %d lanes: %w", port.name, len(port.queues), ErrPortCardinality)
	}
	return port.queues[0].receive(ctx)
}

func (port *inputPort) ReceiveAny(ctx context.Context) (element.Envelope, string, error) {
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
			envelope, received, closed := port.queues[index].tryReceive()
			if !closed {
				allClosed = false
			}
			if received {
				port.cursor = (index + 1) % len(port.queues)
				lane := port.queues[index].id
				port.mu.Unlock()
				return envelope, lane, nil
			}
		}
		port.mu.Unlock()
		if allClosed {
			return element.Envelope{}, "", ErrChannelClosed
		}
		select {
		case <-ctx.Done():
			return element.Envelope{}, "", context.Cause(ctx)
		case <-wait:
		}
	}
}
