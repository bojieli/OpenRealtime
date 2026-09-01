package runtime_test

import (
	"bytes"
	"slices"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestMountedFlowRetainsTheStandardSemanticCausalChain(t *testing.T) {
	mounted, _, _, _ := mountRecordedPassChain(t, bytes.Repeat([]byte{0x63}, 32), 16, 4)
	runDone := runRecordedGraph(t, mounted)
	payloads := []any{
		perception.Observation{},
		stateelements.Commit{},
		cognitionelements.Generate{},
		cognitionelements.Result{},
	}
	items := []string{"observation", "state", "policy", "model"}
	for index, payload := range payloads {
		parents := []string(nil)
		if index > 0 {
			parents = []string{items[index-1]}
		}
		sendRecordedMessage(t, mounted, element.Envelope{
			Type: element.Event(element.Named("test.Value")), ItemID: items[index],
			TraceID: "semantic-chain", CausalParents: parents, Payload: payload,
		})
	}
	live := mounted.Live()
	flow := live.Flows["trace:semantic-chain"]
	kinds := make([]element.InspectionCauseKind, len(flow.CausalStages))
	for index, stage := range flow.CausalStages {
		kinds[index] = stage.Kind
	}
	want := []element.InspectionCauseKind{
		element.CauseObservation,
		element.CauseStateRevision,
		element.CausePolicy,
		element.CauseModelRun,
	}
	if len(flow.Edges) != len(payloads) || !slices.Equal(kinds, want) {
		t.Fatalf("mounted semantic causal flow = %+v, kinds=%v", flow, kinds)
	}
	closeRecordedGraph(t, mounted, runDone)
}

func TestStandardPayloadsDeclareClosedSemanticCauseKinds(t *testing.T) {
	tests := []struct {
		name    string
		payload any
		want    element.InspectionCauseKind
	}{
		{name: "observation", payload: perception.Observation{}, want: element.CauseObservation},
		{name: "trajectory append", payload: stateelements.Append{}, want: element.CauseStateRevision},
		{name: "trajectory snapshot", payload: trajectory.Snapshot{}, want: element.CauseStateRevision},
		{name: "trajectory commit", payload: stateelements.Commit{}, want: element.CauseStateRevision},
		{name: "observation commit", payload: stateelements.ObservationCommitOutcome{}, want: element.CauseStateRevision},
		{name: "generation activation", payload: cognitionelements.Generate{}, want: element.CausePolicy},
		{name: "generation policy outcome", payload: policyelements.GenerationOutcome{}, want: element.CausePolicy},
		{name: "semantic policy decision", payload: policyelements.SemanticDecision{}, want: element.CausePolicy},
		{name: "action policy outcome", payload: actionelements.Outcome{}, want: element.CausePolicy},
		{name: "model text", payload: cognitionelements.PreparedTextDelta{}, want: element.CauseModelRun},
		{name: "model tool proposal", payload: cognitionelements.ToolProposal{}, want: element.CauseModelRun},
		{name: "model result", payload: cognitionelements.Result{}, want: element.CauseModelRun},
		{name: "model outcome", payload: cognitionelements.Outcome{}, want: element.CauseModelRun},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider, ok := test.payload.(element.InspectionCauseProvider)
			if !ok {
				t.Fatalf("%T has no semantic inspection projection", test.payload)
			}
			if got := provider.InspectionCause(); got != test.want {
				t.Fatalf("%T inspection cause = %q, want %q", test.payload, got, test.want)
			} else if err := got.Validate(); err != nil {
				t.Fatalf("%T inspection cause is not closed: %v", test.payload, err)
			}
		})
	}
}
