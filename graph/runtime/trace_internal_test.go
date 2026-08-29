package runtime

import (
	"testing"

	"github.com/bojieli/OpenRealtime/element"
)

func TestFlowTrackerPreservesFeedbackAndBoundsRetention(t *testing.T) {
	tracker := newFlowTracker(2, 3)
	envelope := element.Envelope{ItemID: "same", TraceID: "feedback", Sequence: 1}
	tracker.record("feedback-edge", TraceEnqueue, envelope, 1)
	tracker.record("feedback-edge", TraceEnqueue, envelope, 2)
	tracker.record("exit-edge", TraceEnqueue, envelope, 3)
	tracker.record("too-far", TraceEnqueue, envelope, 4)
	flows, dropped := tracker.snapshot()
	flow := flows["trace:feedback"]
	if len(flow.Edges) != 3 || flow.Edges[0] != "feedback-edge" ||
		flow.Edges[1] != "feedback-edge" || flow.Edges[2] != "exit-edge" ||
		!flow.Truncated || dropped != 1 {
		t.Fatalf("bounded feedback flow = %+v, dropped=%d", flow, dropped)
	}

	tracker.record("edge", TraceEnqueue, element.Envelope{ItemID: "two", RunID: "two"}, 5)
	tracker.record("edge", TraceEnqueue, element.Envelope{ItemID: "three", OpportunityID: "three"}, 6)
	flows, dropped = tracker.snapshot()
	if _, retained := flows["trace:feedback"]; retained || len(flows) != 2 || dropped != 2 {
		t.Fatalf("flow eviction = %+v, dropped=%d", flows, dropped)
	}

	// Boundaries and non-enqueue trace records never become selected Graph IR
	// routes, and therefore do not consume retention.
	tracker.record("boundary:input", TraceEnqueue, element.Envelope{ItemID: "boundary"}, 7)
	tracker.record("edge", TraceDequeue, element.Envelope{ItemID: "dequeue"}, 8)
	after, afterDropped := tracker.snapshot()
	if len(after) != len(flows) || afterDropped != dropped {
		t.Fatalf("non-route records changed flow retention: %+v dropped=%d", after, afterDropped)
	}
}
