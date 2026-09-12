package policy_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/element"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
)

func semanticPlaybackRelease(output coreinteraction.AgentOutput) element.Envelope {
	return element.Envelope{
		Type: speechelements.PlaybackReleaseType(), ItemID: "overlap-release",
		SessionID: "semantic-session", RunID: "completed-run", SourceID: "utterance",
		CancellationScope: "utterance", Sequence: 1,
		CausalParents: []string{"sink-receipt", "release-output-state"},
		Payload: speechelements.PlaybackRelease{
			Receipt: speechelements.PlaybackReceipt{
				Kind: speechelements.PlaybackReleased, Sequence: 1,
				Utterance: action.Utterance{ID: "utterance", Text: "One."},
				Outcome:   action.Outcome{Completed: true, PlayedMS: 20},
			},
			AgentOutput: output, AgentOutputItemID: "release-output-state",
		},
	}
}

func TestSemanticAdmissionPlaybackReleaseAppliesStateBeforeNextRequest(t *testing.T) {
	active := func(revision uint64) coreinteraction.AgentOutput {
		return coreinteraction.AgentOutput{Revision: revision, Active: true, Queued: true, InFlight: "another sentence remains queued"}
	}
	for _, tc := range []struct {
		name              string
		current, released coreinteraction.AgentOutput
		want              coreinteraction.Choice
	}{
		{"completed-output", active(1), coreinteraction.AgentOutput{Revision: 2}, coreinteraction.Choice{Speak: true}},
		{"newer-active-output", active(3), coreinteraction.AgentOutput{Revision: 2}, coreinteraction.Choice{Speaking: true}},
		{"queued-output", coreinteraction.AgentOutput{Revision: 1}, active(2), coreinteraction.Choice{Speaking: true}},
		{"same-revision", active(2), active(2), coreinteraction.Choice{Speaking: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decider := &semanticTestDecider{descriptor: semanticTestDescriptor, answers: []string{tc.want.Token()}}
			h := mountSemanticAdmission(t, decider, semanticConfig(8, 8, 8))
			defer h.stop(t)
			consumeSemanticStartup(t, h)
			installSemanticInvocation(t, h, 1, false)
			snapshot, _ := semanticObservation(t, "Continue the count.", "speech", 1)
			sendSemanticContext(t, h, "context-1", snapshot)
			sendPolicy(t, h.ingress(t, "agent_output"), element.Envelope{
				Type: coreinteraction.AgentOutputType(), ItemID: "earlier-output", SessionID: "semantic-session", Payload: tc.current,
			})
			_ = receivePolicy(t, h.egress(t, "state"))
			release := semanticPlaybackRelease(tc.released)
			sendPolicy(t, h.ingress(t, "release"), release)
			forwarded := receivePolicy(t, h.egress(t, "safe_release"))
			if !forwarded.Type.Equal(speechelements.PlaybackReceiptType()) ||
				!reflect.DeepEqual(forwarded.Payload, release.Payload.(speechelements.PlaybackRelease).Receipt) ||
				forwarded.SessionID != release.SessionID || forwarded.RunID != release.RunID ||
				forwarded.CancellationScope != release.CancellationScope ||
				!slices.Contains(forwarded.CausalParents, release.ItemID) {
				t.Fatalf("policy completion changed receipt or lineage: %+v", forwarded)
			}
			_ = receivePolicy(t, h.egress(t, "state"))
			// A delayed older observation must not undo the state carried by
			// completion, including reactivation of already completed speech.
			sendPolicy(t, h.ingress(t, "agent_output"), element.Envelope{
				Type: coreinteraction.AgentOutputType(), ItemID: "delayed-output", SessionID: "semantic-session", Payload: active(1),
			})
			_ = receivePolicy(t, h.egress(t, "state"))
			version := snapshot.Version
			sendPolicy(t, h.ingress(t, "create"), element.Envelope{
				Type: policyelements.ResponseCreateType(), ItemID: "next-request", SessionID: "semantic-session",
				Payload: policyelements.ResponseCreate{ResponseID: "next-response", ExpectedContextVersion: &version, ExpectedContextItemID: "context-1"},
			})
			_ = receivePolicy(t, h.egress(t, "state"))
			decision := receivePolicy(t, h.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
			outcome := receivePolicy(t, h.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
			if decision.Choice != tc.want || outcome.Kind == policyelements.SemanticAdmissionFailed {
				t.Fatalf("next request sampled stale output: %+v / %+v", decision, outcome)
			}
			if tc.want.Speak {
				_ = receivePolicy(t, h.egress(t, "voice_create"))
			} else {
				assertNoPolicyEnvelope(t, h.egress(t, "voice_create"))
				captured := decider.captured()
				if len(captured) != 1 || !strings.Contains(captured[0].Evidence, "another sentence remains queued") {
					t.Fatalf("pending output disappeared from policy: %+v", captured)
				}
			}
		})
	}
}

func TestSemanticAdmissionPlaybackReleaseRejectsConflictingStateAndLineage(t *testing.T) {
	for _, defect := range []string{"state-conflict", "state-parent", "session", "utterance", "pending-receipt", "zero-revision"} {
		t.Run(defect, func(t *testing.T) {
			decider := &semanticTestDecider{descriptor: semanticTestDescriptor}
			h := mountSemanticAdmission(t, decider, semanticConfig(8, 8, 8))
			defer h.cancel()
			consumeSemanticStartup(t, h)
			snapshot, _ := semanticObservation(t, "Continue.", "speech", 1)
			sendSemanticContext(t, h, "context-1", snapshot)
			sendPolicy(t, h.ingress(t, "agent_output"), element.Envelope{
				Type: coreinteraction.AgentOutputType(), ItemID: "current-output", SessionID: "semantic-session",
				Payload: coreinteraction.AgentOutput{Revision: 2, Active: true, Queued: true},
			})
			_ = receivePolicy(t, h.egress(t, "state"))
			release := semanticPlaybackRelease(coreinteraction.AgentOutput{Revision: 3})
			payload := release.Payload.(speechelements.PlaybackRelease)
			switch defect {
			case "state-conflict":
				payload.AgentOutput.Revision = 2
			case "state-parent":
				release.CausalParents = []string{"sink-receipt"}
			case "session":
				release.SessionID = "other-session"
			case "utterance":
				release.CancellationScope = "other-utterance"
			case "pending-receipt":
				payload.Receipt.Kind = speechelements.PlaybackReserved
			case "zero-revision":
				payload.AgentOutput.Revision = 0
			}
			release.Payload = payload
			sendPolicy(t, h.ingress(t, "release"), release)
			select {
			case err := <-h.done:
				if err == nil {
					t.Fatal("invalid completion did not fail closed")
				}
			case <-time.After(time.Second):
				t.Fatal("invalid completion did not stop the graph")
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if forwarded, err := h.egress(t, "safe_release").Receive(ctx); !errors.Is(err, graphruntime.ErrChannelClosed) {
				t.Fatalf("invalid completion escaped policy: %+v / %v", forwarded, err)
			}
		})
	}
}
