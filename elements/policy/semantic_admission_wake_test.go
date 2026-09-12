package policy_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// A hold that only time can lift - the generation finished and left nothing
// in the trajectory - lifts on its own, without another input to occasion
// the look. Measured, the turn after a failed generation waited for the next
// input, which in a quiet room never came.
func TestSemanticAdmissionHeldRequestProceedsWhenTheGraceExpires(t *testing.T) {
	semanticTestNow = func() uint64 { return uint64(time.Now().UnixNano()) }
	t.Cleanup(func() { semanticTestNow = nil })
	descriptor := semanticTestDescriptor
	// The acknowledged output leaves the voice active, so the second step is
	// decided in the speaking state.
	decider := &semanticTestDecider{descriptor: descriptor, answers: []string{"speak", "keep+speak"}}
	config, err := json.Marshal(policyelements.SemanticAdmissionConfig{
		Decider: "semantic-primary", RecentLines: 12, MaxPending: 8, TerminalMemory: 8, CancelMemory: 8, StandingMemory: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := mountSemanticAdmissionRegistered(t, descriptor, decider, config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	harness := policyHarness{mounted: mounted, done: done, cancel: cancel}
	defer harness.stop(t)
	consumeSemanticStartup(t, harness)
	sendPolicy(t, harness.ingress(t, "update"), element.Envelope{
		Type: policyelements.SessionInvocationUpdateType(), ItemID: "settings-1",
		SessionID: "semantic-session", Payload: policyelements.SessionInvocationUpdate{
			Revision: 1, Invocation: continuation.Invocation{Instruction: "Answer.", MaxOutputTokens: 128},
		},
	})
	_ = receivePolicy(t, harness.egress(t, "state"))

	var items []trajectory.Item
	commit := func(stream, text string) {
		t.Helper()
		version := uint64(len(items) + 1)
		observation := semanticEndpointObservation(stream, version, version, text)
		items = append(items, observation)
		snapshot := trajectory.Snapshot{Version: version, Items: slices.Clone(items)}
		identity, err := trajectory.IdentifyPrefix(snapshot, snapshot.Version)
		if err != nil {
			t.Fatal(err)
		}
		sendSemanticContext(t, harness, "state-"+stream, snapshot)
		sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
			Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-" + stream, SessionID: "semantic-session",
			Payload: semanticCommittedOutcome(observation, stream, identity, "state-"+stream, version),
		})
		_ = receivePolicy(t, harness.egress(t, "state"))
	}
	commit("first", "What is the capital of France?")
	first := receivePolicy(t, harness.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
	_ = receivePolicy(t, harness.egress(t, "voice_committed"))
	_ = receivePolicy(t, harness.egress(t, "outcome"))
	_ = receivePolicy(t, harness.egress(t, "state"))
	if !first.Choice.Speak {
		t.Fatalf("first decision = %+v, want the voice admitted", first)
	}
	// The generation starts and ends, and nothing of it reaches the
	// trajectory: a provider failure. The next request is held.
	acknowledgeSemanticVoice(t, harness, coreinteraction.AgentOutput{Queued: true}, 2, 1)
	commit("second", "And of Spain?")
	started := time.Now()
	waited, cancelWait := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancelWait()
	envelope, err := harness.egress(t, "decision").Receive(waited)
	if err != nil {
		t.Fatalf("the held request never proceeded once the grace expired: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the held request proceeded only after %s, want soon after the grace", elapsed)
	}
	t.Logf("held request proceeded after %s", time.Since(started))
	second := envelope.Payload.(policyelements.SemanticDecision)
	if second.StreamID != "second" || !second.Choice.Speak {
		t.Fatalf("second decision = %+v", second)
	}
}
