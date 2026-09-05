package perception_test

import (
	"fmt"
	"testing"

	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	coreperception "github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestFinalObservationGateCanceledStreamRejectsNewCauses(t *testing.T) {
	for _, operation := range []string{"cancel", "observe", "flush"} {
		t.Run(operation, func(t *testing.T) {
			mounted, done, cancel := mountFinalObservationGate(t)
			defer stopASR(t, mounted, done, cancel)
			observations, _ := mounted.Ingress("observations")
			flush, _ := mounted.Ingress("flush")
			outcomes, _ := mounted.Egress("outcome")
			admitted, _ := mounted.Egress("admitted")
			finals, _ := mounted.Egress("finals")
			sendGate(t, flush, gateFlushEnvelope("session-a", "withdrawn", "cancel-cause", "cancel-outcome", perceptionelements.Outcome{
				Kind: perceptionelements.OutcomeCanceled, Operation: operation, StreamID: "withdrawn", Code: "canceled",
			}))
			_ = receive(t, outcomes)
			for index, final := range []bool{false, true, true} {
				cause := fmt.Sprintf("late-cause-%d", index)
				observation := coreperception.Observation{
					Text: "withdrawn text", Observer: "asr", Source: "microphone", Authority: trajectory.AuthorityUser,
					Revision: uint64(index + 1), Provisional: !final, Final: final,
				}
				// Both independent-lane orderings must preserve the cancellation.
				if index == 2 {
					sendGate(t, flush, gateFlushEnvelope("session-a", "withdrawn", cause, cause+"-flush", perceptionelements.Outcome{Kind: perceptionelements.OutcomeSucceeded, Operation: "flush", StreamID: "withdrawn", ObservationCount: 1}))
					_ = receive(t, outcomes)
				}
				sendGate(t, observations, gateObservationEnvelope("session-a", "withdrawn", cause, cause+"-text", observation))
				if outcome := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome); outcome.Kind != perceptionelements.FinalObservationCanceled {
					t.Fatalf("new cause on canceled stream was admitted or retained: %+v", outcome)
				}
				if index == 1 {
					sendGate(t, flush, gateFlushEnvelope("session-a", "withdrawn", cause, cause+"-flush", perceptionelements.Outcome{Kind: perceptionelements.OutcomeSucceeded, Operation: "flush", StreamID: "withdrawn", ObservationCount: 1}))
					_ = receive(t, outcomes)
				}
			}
			assertNoGateEnvelope(t, admitted)
			assertNoGateEnvelope(t, finals)
			for index, control := range []struct{ session, stream string }{{"session-a", "fresh"}, {"session-b", "withdrawn"}} {
				cause := fmt.Sprintf("fresh-%d", index)
				sendGate(t, observations, gateObservationEnvelope(control.session, control.stream, cause, cause+"-text", coreperception.Observation{
					Text: "fresh text", Observer: "asr", Source: "microphone", Authority: trajectory.AuthorityUser,
					Revision: 1, Final: true,
				}))
				_ = receive(t, outcomes)
				sendGate(t, flush, gateFlushEnvelope(control.session, control.stream, cause, cause+"-flush", perceptionelements.Outcome{Kind: perceptionelements.OutcomeSucceeded, Operation: "flush", StreamID: control.stream, ObservationCount: 1}))
				if outcome := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome); outcome.Kind != perceptionelements.FinalObservationEmitted {
					t.Fatalf("fresh final was suppressed: %+v", outcome)
				}
				_ = receive(t, admitted)
				_ = receive(t, finals)
			}
		})
	}
}
