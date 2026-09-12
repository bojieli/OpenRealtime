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

// A final transcript that lands while the agent is speaking is decided once.
// stop+speak retires the old output and admits the new request in that same
// decision; stop alone admits nothing and is not asked again when the output
// retires. There used to be a second round-trip here - stop, wait for idle,
// re-ask - and the two questions being one token is what removed it.
func TestFinalInterruptionIsDecidedOnce(t *testing.T) {
	for _, next := range []string{coreinteraction.ChoiceStopSpeak, coreinteraction.ChoiceStop} {
		t.Run(next, func(t *testing.T) {
			decider := &semanticTestDecider{descriptor: semanticTestDescriptor, answers: []string{next}}
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
			decision := receivePolicy(t, h.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
			if decision.Choice.Token() != next || !decision.Choice.Stop || decision.Event != coreinteraction.TranscriptFinal {
				t.Fatalf("decision: %+v", decision)
			}
			if next == coreinteraction.ChoiceStopSpeak {
				grant := receivePolicy(t, h.egress(t, "voice_committed")).Payload.(policyelements.SemanticGrant)
				if grant.Commit.ObservationRevision != commit.ObservationRevision || !grant.Choice.Stop || !grant.Choice.Speak {
					t.Fatalf("stop+speak grant: %+v", grant)
				}
			}
			outcome := receivePolicy(t, h.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
			_ = receivePolicy(t, h.egress(t, "state"))
			if next == coreinteraction.ChoiceStop {
				if outcome.Kind != policyelements.SemanticAdmissionSuppressed || outcome.Code != "stop" {
					t.Fatalf("stop outcome: %+v", outcome)
				}
				assertNoPolicyEnvelope(t, h.egress(t, "voice_committed"))
			} else if outcome.Kind != policyelements.SemanticAdmissionAdmitted {
				t.Fatalf("stop+speak outcome: %+v", outcome)
			}
			// The output retiring is not a new question. Nothing is re-asked.
			sendOutput(3, false)
			assertNoPolicyEnvelope(t, h.egress(t, "decision"))
			if len(decider.captured()) != 1 {
				t.Fatal("retired output re-asked the interruption")
			}
			// Neither is a replay of the same commit.
			sendPolicy(t, h.ingress(t, "committed"), input)
			_ = receivePolicy(t, h.egress(t, "outcome"))
			_ = receivePolicy(t, h.egress(t, "state"))
			if len(decider.captured()) != 1 {
				t.Fatal("terminal replay repeated the interruption")
			}
		})
	}
}
