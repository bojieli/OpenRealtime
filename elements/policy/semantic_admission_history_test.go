package policy_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/spoken"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// The decider is the consumer of policy context. A real canonical playback
// transition must change that context even though assistant content is immutable.
func TestSemanticAdmissionHistoryDoesNotClaimUnplayedSpeechWasHeard(t *testing.T) {
	for _, tc := range []struct {
		name       string
		visibility trajectory.Visibility
		mark       *spoken.Mark
		want       string
	}{
		{"partial-playback", trajectory.VisibilityPlayed, &spoken.Mark{Spoken: "Find the order", Cut: "number", Pending: "number and purchase email.", Measured: true}, "agent: Find the order\nplayback stopped during: \"number\"\nprepared (not heard): number and purchase email."},
		{"canceled-before-playback", trajectory.VisibilityCancelled, nil, "canceled (not said): Find the order number and purchase email."},
		{"queued", trajectory.VisibilityQueued, nil, "prepared (not yet said): Find the order number and purchase email."},
		{"fully-played", trajectory.VisibilityPlayed, &spoken.Mark{Spoken: "Find the order number and purchase email.", Measured: true}, "agent: Find the order number and purchase email."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := trajectory.NewStore()
			if err := store.AppendBatch([]trajectory.Item{
				{ID: "question", Kind: trajectory.KindObservation, Content: "Explain the refund process.", MonotonicNS: 1, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}},
				{ID: "draft", Kind: trajectory.KindAssistant, Content: "Find the order number and purchase email.", MonotonicNS: 2, Visibility: trajectory.VisibilityPrepared, Producer: trajectory.Producer{Phase: trajectory.PhaseFast}},
				{ID: "playback", Kind: trajectory.KindAssistantState, MonotonicNS: 3, Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, AssistantState: &trajectory.AssistantState{AssistantItemID: "draft", Visibility: tc.visibility, Heard: tc.mark}},
				{ID: "resume", Kind: trajectory.KindObservation, Content: "Please continue where you stopped.", MonotonicNS: 4, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}},
			}); err != nil {
				t.Fatal(err)
			}
			snapshot := store.Snapshot()
			before := store.Snapshot()
			decider := &semanticTestDecider{descriptor: semanticTestDescriptor, acts: []coreinteraction.Act{coreinteraction.ActAnswer}}
			h := mountSemanticAdmission(t, decider, semanticConfig(8, 8, 8))
			defer h.stop(t)
			consumeSemanticStartup(t, h)
			installSemanticInvocation(t, h, 1, false)
			sendSemanticContext(t, h, "published-history", snapshot)
			version := snapshot.Version
			sendPolicy(t, h.ingress(t, "create"), element.Envelope{Type: policyelements.ResponseCreateType(), ItemID: "resume-create", SessionID: "semantic-session", Payload: policyelements.ResponseCreate{ResponseID: "resumed", ExpectedContextVersion: &version, ExpectedContextItemID: "published-history"}})
			_ = receivePolicy(t, h.egress(t, "state"))
			decision := receivePolicy(t, h.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
			_ = receivePolicy(t, h.egress(t, "voice_create"))
			_ = receivePolicy(t, h.egress(t, "outcome"))
			calls := decider.captured()
			if decision.Act != coreinteraction.ActAnswer || len(calls) != 1 {
				t.Fatalf("resume failed: %+v / %+v", decision, calls)
			}
			if !strings.Contains(calls[0].Evidence, tc.want) {
				t.Fatalf("policy did not receive exact playback history: %s", calls[0].Evidence)
			}
			if tc.name != "fully-played" && strings.Contains(calls[0].Evidence, "agent: Find the order number and purchase email.") {
				t.Fatal("policy was told the unplayed draft was spoken")
			}
			if !reflect.DeepEqual(snapshot, before) || !reflect.DeepEqual(store.Snapshot(), before) {
				t.Fatal("policy rewrote canonical content")
			}
		})
	}
}
