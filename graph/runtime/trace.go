package runtime

import (
	"slices"
	"sync"
)

type TraceKind string

const (
	TraceEnqueue      TraceKind = "enqueue"
	TraceDequeue      TraceKind = "dequeue"
	TraceDrop         TraceKind = "drop"
	TraceBackpressure TraceKind = "backpressure"
)

// TraceEvent contains causal and timing metadata but never the item payload.
type TraceEvent struct {
	Kind        TraceKind `json:"kind"`
	AtNS        uint64    `json:"at_ns"`
	Graph       string    `json:"graph"`
	Fingerprint string    `json:"fingerprint"`
	Channel     string    `json:"channel"`
	ItemID      string    `json:"item_id,omitempty"`
	TraceID     string    `json:"trace_id,omitempty"`
	RunID       string    `json:"run_id,omitempty"`
	Occupancy   int       `json:"occupancy"`
	Depth       int       `json:"depth"`
}

type Tracer interface {
	Record(TraceEvent)
}

type TraceFunc func(TraceEvent)

func (function TraceFunc) Record(event TraceEvent) { function(event) }

// BufferTracer retains a bounded recent trace for tests and lightweight live
// inspection. It drops the oldest trace record, never graph data.
type BufferTracer struct {
	mu       sync.Mutex
	capacity int
	events   []TraceEvent
}

func NewBufferTracer(capacity int) *BufferTracer {
	if capacity < 1 {
		capacity = 1
	}
	return &BufferTracer{capacity: capacity}
}

func (tracer *BufferTracer) Record(event TraceEvent) {
	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	if len(tracer.events) == tracer.capacity {
		copy(tracer.events, tracer.events[1:])
		tracer.events[len(tracer.events)-1] = event
		return
	}
	tracer.events = append(tracer.events, event)
}

func (tracer *BufferTracer) Events() []TraceEvent {
	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	return slices.Clone(tracer.events)
}
