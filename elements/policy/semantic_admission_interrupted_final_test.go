package policy_test

import (
	"fmt"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestFinalInterruptionIsReconsideredAfterOutputRetires(t *testing.T) {
	for _, next := range []coreinteraction.Act{coreinteraction.ActAnswer, coreinteraction.ActStaySilent} {
		t.Run(string(next), func(t *testing.T) {
			decider := &semanticTestDecider{descriptor: semanticTestDescriptor, answers: []string{string(coreinteraction.ActStopSpeaking), string(next)}}
			h := mountSemanticAdmission(t, decider, semanticConfig(8, 8, 8))
			defer h.stop(t)
			consumeSemanticStartup(t, h)
			installSemanticInvocation(t, h, 1, false)
			sendOutput := func(revision uint64, active bool) {
				output := coreinteraction.AgentOutput{Revision: revision, Active: active}
				if active {
					output.Audible = true
					output.Saying = "An old answer."
					output.InFlight = "playback=1"
				}
				sendPolicy(t, h.ingress(t, "agent_output"), element.Envelope{Type: coreinteraction.AgentOutputType(), ItemID: fmt.Sprintf("output-%d", revision), SessionID: "semantic-session", Payload: output})
				_ = receivePolicy(t, h.egress(t, "state"))
			}
			sendOutput(2, true)
			item := semanticTranscriptObservation("new-question", "asr.endpoint", 1, "Wait, what is the capital of Japan?")
			snapshot := trajectory.Snapshot{Version: 1, Items: []trajectory.Item{item}}
			prefix, err := trajectory.IdentifyPrefix(snapshot, 1)
			if err != nil {
				t.Fatal(err)
			}
			sendSemanticContext(t, h, "state-1", snapshot)
			commit := semanticCommittedOutcome(item, "question", prefix, "state-1", 1)
			input := element.Envelope{Type: stateelements.ObservationCommitOutcomeType(), ItemID: "final-question", SessionID: "semantic-session", Payload: commit}
			sendPolicy(t, h.ingress(t, "committed"), input)
			_ = receivePolicy(t, h.egress(t, "state"))
			stop := receivePolicy(t, h.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
			if stop.Act != coreinteraction.ActStopSpeaking {
				t.Fatalf("first decision: %+v", stop)
			}
			_ = receivePolicy(t, h.egress(t, "outcome"))
			_ = receivePolicy(t, h.egress(t, "state"))
			assertNoPolicyEnvelope(t, h.egress(t, "voice_committed"))
			sendOutput(1, false) // A stale idle observation cannot release the new request.
			if len(decider.captured()) != 1 {
				t.Fatal("stale output state released interruption")
			}
			sendOutput(3, false)
			decision := receivePolicy(t, h.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
			if decision.Act != next {
				t.Fatalf("final utterance lost after cancellation: %+v", decision)
			}
			_ = receivePolicy(t, h.egress(t, "outcome"))
			_ = receivePolicy(t, h.egress(t, "state"))
			if next == coreinteraction.ActAnswer {
				grant := receivePolicy(t, h.egress(t, "voice_committed")).Payload.(policyelements.SemanticGrant)
				if grant.Commit.ObservationRevision != commit.ObservationRevision {
					t.Fatal("reconsideration changed the observation")
				}
			} else {
				assertNoPolicyEnvelope(t, h.egress(t, "voice_committed"))
			}
			sendPolicy(t, h.ingress(t, "committed"), input)
			_ = receivePolicy(t, h.egress(t, "outcome"))
			_ = receivePolicy(t, h.egress(t, "state"))
			if len(decider.captured()) != 2 {
				t.Fatal("terminal replay repeated the interruption")
			}
		})
	}
}
