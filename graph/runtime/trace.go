package runtime

import (
	"slices"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
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

type trackedFlow struct {
	flow inspect.FlowLive
}

// flowTracker retains bounded payload-free routes by envelope correlation.
// It records internal Graph IR edges only; boundary queues are useful for
// queue telemetry but are not selected graph paths.
type flowTracker struct {
	mu       sync.Mutex
	maxFlows int
	maxEdges int
	maxKey   int
	order    []string
	flows    map[string]*trackedFlow
	dropped  uint64
}

func newFlowTracker(maxFlows, maxEdges, maxCorrelationBytes int) *flowTracker {
	if maxFlows < 1 {
		maxFlows = 1
	}
	if maxEdges < 1 {
		maxEdges = 1
	}
	if maxCorrelationBytes < 1 {
		maxCorrelationBytes = 1024
	}
	return &flowTracker{
		maxFlows: maxFlows, maxEdges: maxEdges, maxKey: maxCorrelationBytes,
		flows: make(map[string]*trackedFlow),
	}
}

func (tracker *flowTracker) record(
	channel string, kind TraceKind, envelope element.Envelope, atNS uint64,
) {
	if tracker == nil || kind != TraceEnqueue || strings.HasPrefix(channel, "boundary:") {
		return
	}
	key := flowKey(envelope)
	if key == "" {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if len(key) > tracker.maxKey {
		tracker.dropped++
		return
	}
	tracked := tracker.flows[key]
	if tracked == nil {
		if len(tracker.order) == tracker.maxFlows {
			oldest := tracker.order[0]
			tracker.order = tracker.order[1:]
			delete(tracker.flows, oldest)
			tracker.dropped++
		}
		tracked = &trackedFlow{
			flow: inspect.FlowLive{Correlation: key, FirstNS: atNS},
		}
		tracker.flows[key] = tracked
		tracker.order = append(tracker.order, key)
	} else if atNS < tracked.flow.LastNS {
		atNS = tracked.flow.LastNS
		tracker.dropped++
	}
	tracked.flow.LastNS = atNS
	// Enqueue is emitted exactly once per actual traversal. Do not de-duplicate
	// by item or sequence: a feedback loop may deliberately send the same
	// envelope through the same edge again, and that repetition is evidence.
	if len(tracked.flow.Edges) == tracker.maxEdges {
		tracked.flow.Truncated = true
		tracker.dropped++
		return
	}
	tracked.flow.Edges = append(tracked.flow.Edges, channel)
}

func (tracker *flowTracker) snapshot() (map[string]inspect.FlowLive, uint64) {
	if tracker == nil {
		return nil, 0
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	result := make(map[string]inspect.FlowLive, len(tracker.flows))
	for key, tracked := range tracker.flows {
		result[key] = tracked.flow.Clone()
	}
	return result, tracker.dropped
}

func flowKey(envelope element.Envelope) string {
	for _, value := range []struct{ prefix, value string }{
		{"trace:", envelope.TraceID}, {"run:", envelope.RunID},
		{"opportunity:", envelope.OpportunityID}, {"item:", envelope.ItemID},
	} {
		if value.value != "" {
			return value.prefix + value.value
		}
	}
	return ""
}
