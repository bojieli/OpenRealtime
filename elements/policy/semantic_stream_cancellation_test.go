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

func TestSemanticAdmissionStreamCancellationSurvivesLaterRevisions(t *testing.T) {
	for _, phase := range []string{"before-arrival", "pending-context", "active-decision"} {
		t.Run(phase, func(t *testing.T) {
			entered := make(chan int, 4)
			decider := &semanticTestDecider{descriptor: semanticTestDescriptor, entered: entered}
			if phase == "active-decision" {
				decider.release = make(chan struct{})
			}
			harness := mountSemanticAdmission(t, decider, semanticConfig(8, 8, 8))
			defer harness.stop(t)
			consumeSemanticStartup(t, harness)
			installSemanticInvocation(t, harness, 1, false)
			state := func() policyelements.SemanticAdmissionState {
				t.Helper()
				return receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
			}
			outcome := func() policyelements.SemanticAdmissionOutcome {
				t.Helper()
				return receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
			}
			var history trajectory.Snapshot
			observation := func(content, stream string, revision uint64) (trajectory.Snapshot, stateelements.ObservationCommitOutcome) {
				t.Helper()
				snapshot, commit := semanticObservation(t, content, stream, revision)
				history.Items = append(history.Items, snapshot.Items[0])
				history.Version = uint64(len(history.Items))
				prefix, err := trajectory.IdentifyPrefix(history, history.Version)
				if err != nil {
					t.Fatal(err)
				}
				commit.StoreVersion = history.Version
				commit.Context = stateelements.CommittedContext{Prefix: prefix, StateItemID: fmt.Sprintf("state-%d", history.Version)}
				return history, commit
			}
			if phase != "before-arrival" {
				snapshot, commit := observation("withdraw this request", "withdrawn", 1)
				if phase == "active-decision" {
					sendSemanticContext(t, harness, "state-1", snapshot)
				}
				sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
					Type: stateelements.ObservationCommitOutcomeType(), ItemID: "initial-commit",
					SessionID: "semantic-session", Payload: commit,
				})
				initial := state()
				if phase == "active-decision" {
					awaitSemanticCall(t, entered)
					if !initial.Active {
						t.Fatalf("decision never became active: %+v", initial)
					}
				} else if initial.Pending != 1 {
					t.Fatalf("request never became pending: %+v", initial)
				}
			}
			sendPolicy(t, harness.ingress(t, "cancel"), element.Envelope{
				Type: policyelements.GenerationCancelType(), ItemID: "cancel-stream",
				SessionID: "semantic-session", Payload: policyelements.GenerationCancel{
					StreamID: "withdrawn", Reason: "request withdrawn",
				},
			})
			if phase == "pending-context" {
				if canceled := outcome(); canceled.Kind != policyelements.SemanticAdmissionCanceled {
					t.Fatalf("pending decision cancellation = %+v", canceled)
				}
			}
			if recorded := outcome(); recorded.Code != "cancel_recorded" {
				t.Fatalf("stream cancellation = %+v", recorded)
			}
			_ = state()
			if phase == "active-decision" {
				if canceled := outcome(); canceled.Code != "decision_canceled" {
					t.Fatalf("active decision cancellation = %+v", canceled)
				}
				_ = state()
				close(decider.release)
			}
			for revision := uint64(2); revision <= 3; revision++ {
				snapshot, commit := observation("delayed revision", "withdrawn", revision)
				sendSemanticContext(t, harness, commit.Context.StateItemID, snapshot)
				sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
					Type: stateelements.ObservationCommitOutcomeType(), ItemID: fmt.Sprintf("late-%d", revision),
					SessionID: "semantic-session", Payload: commit,
				})
				if current := state(); current.Pending != 0 || current.Active {
					t.Fatalf("canceled stream revision %d restarted admission: %+v", revision, current)
				}
				if canceled := outcome(); canceled.Kind != policyelements.SemanticAdmissionCanceled || canceled.Code != "canceled_before_decision" {
					t.Fatalf("canceled stream revision %d = %+v", revision, canceled)
				}
			}
			// A cancellation from another session cannot suppress local
			// work, and a fresh stream still reaches the voice consumer.
			sendPolicy(t, harness.ingress(t, "cancel"), element.Envelope{
				Type: policyelements.GenerationCancelType(), ItemID: "foreign-cancel", SessionID: "another-session",
				Payload: policyelements.GenerationCancel{StreamID: "foreign-only"},
			})
			if recorded := outcome(); recorded.Code != "cancel_recorded" {
				t.Fatalf("foreign cancel = %+v", recorded)
			}
			_ = state()
			for index, stream := range []string{"replacement", "foreign-only"} {
				snapshot, commit := observation("answer this request", stream, uint64(4+index))
				sendSemanticContext(t, harness, commit.Context.StateItemID, snapshot)
				sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
					Type: stateelements.ObservationCommitOutcomeType(), ItemID: "fresh-" + stream,
					SessionID: "semantic-session", Payload: commit,
				})
				awaitSemanticCall(t, entered)
				_ = state()
				_ = receivePolicy(t, harness.egress(t, "decision"))
				grant := receivePolicy(t, harness.egress(t, "voice_committed"))
				if admitted := outcome(); admitted.Kind != policyelements.SemanticAdmissionAdmitted || grant.SessionID != "semantic-session" {
					t.Fatalf("fresh request was suppressed: %+v, grant session %q", admitted, grant.SessionID)
				}
				_ = state()
				acknowledgeSemanticVoice(t, harness, coreinteraction.AgentOutput{Queued: true}, uint64(10+2*index), uint64(1+index))
				// The voice's answer reaches the trajectory with the next
				// context; the hold on the next stream lifts on it.
				history.Items = append(history.Items, semanticAssistantSaid("said-"+stream, "answered"))
			}
			wantCalls := 2
			if phase == "active-decision" {
				wantCalls++
			}
			if calls := len(decider.captured()); calls != wantCalls {
				t.Fatalf("decider calls = %d, want %d", calls, wantCalls)
			}
			assertNoSemanticGeneration(t, harness)
		})
	}
}
