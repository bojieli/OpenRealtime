package runtime

import (
	"slices"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
)

func TestFlowTrackerPreservesFeedbackAndBoundsRetention(t *testing.T) {
	tracker := newFlowTracker(2, 3, 1024, inspect.MaximumCausalParentsPerStage)
	envelope := element.Envelope{
		ItemID: "same", TraceID: "feedback", Sequence: 1,
		CausalParents: []string{"observation", "state-revision"},
	}
	tracker.record("feedback-edge", TraceEnqueue, envelope, 1)
	tracker.record("feedback-edge", TraceEnqueue, envelope, 2)
	tracker.record("exit-edge", TraceEnqueue, envelope, 3)
	tracker.record("too-far", TraceEnqueue, envelope, 4)
	flows, dropped := tracker.snapshot()
	flow := flows["trace:feedback"]
	if len(flow.Edges) != 3 || flow.Edges[0] != "feedback-edge" ||
		flow.Edges[1] != "feedback-edge" || flow.Edges[2] != "exit-edge" ||
		len(flow.EdgeNS) != 3 || flow.EdgeNS[0] != 1 || flow.EdgeNS[1] != 2 || flow.EdgeNS[2] != 3 ||
		len(flow.CausalStages) != 3 || flow.CausalStages[0].Item != "same" ||
		!slices.Equal(flow.CausalStages[0].Parents, envelope.CausalParents) ||
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

func TestFlowTrackerTruncatesInvalidOrRewrittenCausalLineage(t *testing.T) {
	tests := []struct {
		name   string
		second element.Envelope
	}{
		{name: "rewritten parents", second: element.Envelope{
			ItemID: "derived", TraceID: "trace", CausalParents: []string{"different"},
		}},
		{name: "self parent", second: element.Envelope{
			ItemID: "derived", TraceID: "trace", CausalParents: []string{"derived"},
		}},
		{name: "duplicate parent", second: element.Envelope{
			ItemID: "derived", TraceID: "trace", CausalParents: []string{"root", "root"},
		}},
		{name: "too many parents", second: element.Envelope{
			ItemID: "derived", TraceID: "trace", CausalParents: []string{"one", "two"},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tracker := newFlowTracker(2, 4, 32, 1)
			tracker.record("first", TraceEnqueue, element.Envelope{
				ItemID: "derived", TraceID: "trace", CausalParents: []string{"root"},
			}, 10)
			tracker.record("second", TraceEnqueue, test.second, 20)
			tracker.record("third", TraceEnqueue, element.Envelope{
				ItemID: "later", TraceID: "trace", CausalParents: []string{"derived"},
			}, 30)
			flows, dropped := tracker.snapshot()
			flow := flows["trace:trace"]
			if !flow.Truncated || len(flow.Edges) != 1 || len(flow.CausalStages) != 1 ||
				flow.CausalStages[0].Item != "derived" || dropped != 2 {
				t.Fatalf("invalid causal lineage retention = %+v, dropped=%d", flow, dropped)
			}
		})
	}
}

func TestFlowTrackerBoundsCorrelationBytesAndClampsRegressingClock(t *testing.T) {
	tracker := newFlowTracker(2, 3, 16, inspect.MaximumCausalParentsPerStage)
	tracker.record("edge", TraceEnqueue, element.Envelope{
		ItemID: "item", TraceID: "this-correlation-is-too-large",
	}, 10)
	flows, dropped := tracker.snapshot()
	if len(flows) != 0 || dropped != 1 {
		t.Fatalf("oversized correlation retention = %+v, dropped=%d", flows, dropped)
	}

	tracker.record("edge", TraceEnqueue, element.Envelope{ItemID: "item", TraceID: "ok"}, 20)
	tracker.record("edge", TraceEnqueue, element.Envelope{ItemID: "item", TraceID: "ok"}, 15)
	flows, dropped = tracker.snapshot()
	flow := flows["trace:ok"]
	if flow.FirstNS != 20 || flow.LastNS != 20 || len(flow.Edges) != 2 ||
		len(flow.EdgeNS) != 2 || flow.EdgeNS[0] != 20 || flow.EdgeNS[1] != 20 || dropped != 2 {
		t.Fatalf("regressing clock flow = %+v, dropped=%d", flow, dropped)
	}
}

func TestRecordedFlowMonotonicityRequiresAnImmutableTimingModeAndPrefix(t *testing.T) {
	before := traceCorrelation{
		edges: []string{"edge"}, edgeNS: []uint64{10},
		causalStages: []inspect.CausalStageLive{{Item: "item", Parents: []string{"root"}}},
		firstNS:      10, lastNS: 10,
	}
	if !monotonicRecordedFlow(before, inspect.FlowLive{
		Edges: []string{"edge", "edge"}, EdgeNS: []uint64{10, 20},
		CausalStages: []inspect.CausalStageLive{
			{Item: "item", Parents: []string{"root"}}, {Item: "next", Parents: []string{"item"}},
		},
		FirstNS: 10, LastNS: 20,
	}) {
		t.Fatal("valid edge and timing append was not monotonic")
	}
	if monotonicRecordedFlow(before, inspect.FlowLive{
		Edges: []string{"edge", "edge"}, EdgeNS: []uint64{11, 20},
		CausalStages: []inspect.CausalStageLive{
			{Item: "item", Parents: []string{"root"}}, {Item: "next", Parents: []string{"item"}},
		},
		FirstNS: 10, LastNS: 20,
	}) {
		t.Fatal("rewritten edge timing prefix was accepted")
	}
	if monotonicRecordedFlow(traceCorrelation{
		edges: []string{"edge"}, firstNS: 10, lastNS: 10,
	}, inspect.FlowLive{
		Edges: []string{"edge", "edge"}, EdgeNS: []uint64{10, 20}, FirstNS: 10, LastNS: 20,
	}) {
		t.Fatal("legacy flow changed timing-presence mode without rotating identity")
	}
	if monotonicRecordedFlow(before, inspect.FlowLive{
		Edges: []string{"edge", "edge"}, EdgeNS: []uint64{10, 20},
		CausalStages: []inspect.CausalStageLive{
			{Item: "item", Parents: []string{"rewritten"}}, {Item: "next", Parents: []string{"item"}},
		},
		FirstNS: 10, LastNS: 20,
	}) {
		t.Fatal("rewritten causal prefix was accepted")
	}
}

type inspectionDecisionPayload struct {
	decision element.InspectionDecision
}

func (payload inspectionDecisionPayload) InspectionDecision() element.InspectionDecision {
	return payload.decision
}

func TestNodeTelemetryClampsARegressingAuthorityDecisionClock(t *testing.T) {
	now := uint64(20)
	mounted := &Mounted{
		nodeLive: map[string]inspect.NodeLive{"authority": {State: "running"}},
		clock:    func() uint64 { return now },
	}
	telemetry := &nodeTelemetry{
		mounted: mounted, node: "authority", active: make(map[[32]byte]struct{}),
	}
	telemetry.triggerSeen.Store(true)
	telemetry.observeOutput(element.Envelope{
		ItemID: "first", Payload: inspectionDecisionPayload{decision: element.InspectionDecision{
			Kind: element.DecisionSucceeded, Operation: element.DecisionSelect,
		}},
	}, true)
	now = 10
	telemetry.observeOutput(element.Envelope{
		ItemID: "second", Payload: inspectionDecisionPayload{decision: element.InspectionDecision{
			Kind: element.DecisionCanceled, Operation: element.DecisionCancel,
		}},
	}, true)
	decision := mounted.nodeLive["authority"].AuthorityDecision
	if decision == nil || decision.AtNS != 20 || decision.Kind != element.DecisionCanceled ||
		decision.Operation != element.DecisionCancel {
		t.Fatalf("regressing authority-decision clock was not clamped: %+v", decision)
	}
}

func TestRecordedNodeCloneOwnsAuthorityDecision(t *testing.T) {
	source := inspect.TraceNodeLive{AuthorityDecision: &inspect.AuthorityDecisionLive{
		Kind: element.DecisionSucceeded, Operation: element.DecisionAuthorize, AtNS: 30,
	}}
	cloned := cloneRecordedNode(source)
	source.AuthorityDecision.Operation = element.DecisionCancel
	if cloned.AuthorityDecision == nil || cloned.AuthorityDecision.Operation != element.DecisionAuthorize {
		t.Fatalf("recorded node retained authority-decision alias: %+v", cloned.AuthorityDecision)
	}
}
