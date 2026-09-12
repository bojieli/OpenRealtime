package policy_test

import (
	"fmt"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	"github.com/bojieli/OpenRealtime/interaction"
)

func TestGenerationStreamCancellationSurvivesLaterRevisions(t *testing.T) {
	for _, dynamic := range []bool{false, true} {
		name := "GenerateOnObservation"
		if dynamic {
			name = "SessionInvocation"
		}
		t.Run(name, func(t *testing.T) {
			var harness policyHarness
			if dynamic {
				harness = mountSessionInvocation(t)
			} else {
				harness = mountPolicy(t, validPolicyConfig("foreground"))
			}
			defer harness.stop(t)
			_ = receivePolicy(t, harness.egress(t, "state"))
			if dynamic {
				installSessionInvocation(t, harness, 1, "Answer the current request.", nil)
			}
			outcome := func() (kind, code string) {
				t.Helper()
				value := receivePolicy(t, harness.egress(t, "outcome")).Payload
				_ = receivePolicy(t, harness.egress(t, "state"))
				switch value := value.(type) {
				case policyelements.GenerationOutcome:
					return string(value.Kind), value.Code
				case policyelements.SessionInvocationOutcome:
					return string(value.Kind), value.Code
				default:
					t.Fatalf("unexpected generation outcome %T", value)
					return "", ""
				}
			}
			sendPolicy(t, harness.ingress(t, "cancel"), element.Envelope{
				Type: policyelements.GenerationCancelType(), ItemID: "cancel-stream",
				SessionID: "session-policy", Payload: policyelements.GenerationCancel{
					StreamID: "canceled-request", Reason: "request withdrawn",
				},
			})
			if kind, code := outcome(); kind != "ignored" || code != "cancel_recorded" {
				t.Fatalf("cancellation = %s/%s", kind, code)
			}
			var previousRunID string
			for index, test := range []struct {
				session, stream string
				want            string
				cancelPrevious  bool
			}{
				{"session-policy", "canceled-request", "canceled", false},
				{"session-policy", "canceled-request", "canceled", false},
				{"different-session", "canceled-request", "emitted", false},
				{"session-policy", "fresh-request", "emitted", false},
				{"session-policy", "canceled-request", "canceled", false},
				{"session-policy", "fresh-request", "emitted", true},
			} {
				if test.cancelPrevious {
					sendPolicy(t, harness.ingress(t, "cancel"), element.Envelope{
						Type: policyelements.GenerationCancelType(), ItemID: "cancel-one-generation",
						SessionID: test.session, Payload: policyelements.GenerationCancel{
							GenerationID: previousRunID, Reason: "stop only this response",
						},
					})
					if kind, code := outcome(); kind != "ignored" || code != "cancel_recorded" {
						t.Fatalf("generation cancellation = %s/%s", kind, code)
					}
				}
				id := fmt.Sprintf("revision-%d", index+1)
				commit := committedObservation(t, test.stream, id, id+"-item", id+"-state", uint64(index+1), uint64(index+1))
				commit.ObservationRevision = uint64(index + 1)
				envelope := commitEnvelope(id+"-commit", test.session, commit)
				if dynamic {
					envelope.Type = policyelements.SemanticGrantType()
					envelope.Payload = semanticGrant(commit, interaction.Choice{Speak: true})
				}
				sendPolicy(t, harness.ingress(t, "committed"), envelope)
				if kind, code := outcome(); kind != test.want {
					t.Fatalf("%s on %s/%s = %s/%s, want %s", id, test.session, test.stream, kind, code, test.want)
				}
				if test.want == "emitted" {
					trigger := receivePolicy(t, harness.egress(t, "trigger"))
					authority := receivePolicy(t, harness.egress(t, "authority"))
					if trigger.SessionID != test.session || authority.RunID != trigger.RunID {
						t.Fatalf("fresh activation changed identity: trigger=%+v authority=%+v", trigger, authority)
					}
					previousRunID = trigger.RunID
				}
			}
			assertNoPolicyEnvelope(t, harness.egress(t, "trigger"))
			assertNoPolicyEnvelope(t, harness.egress(t, "authority"))
		})
	}
}
