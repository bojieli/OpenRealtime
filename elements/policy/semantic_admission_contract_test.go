package policy_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// The session instruction's own rules are standing policies: read once, on
// the first decision that sees the instruction, in force before that decision
// is taken, and not the room's to lift.
func TestSemanticAdmissionReadsTheOperatorsRulesOnceAndKeepsThem(t *testing.T) {
	descriptor := semanticTestDescriptor
	descriptor.StandingExtraction = true
	const rule = "when a recorded menu offers the option the user wants, press that key"
	decider := &semanticTestDecider{
		descriptor: descriptor, answers: []string{"listen"},
		contractAnswer: "pin conversation " + rule,
		// The operator's rule is read for counting, restriction, and scope;
		// the first final sets nothing, and the second tries to lift the
		// operator's rule.
		generationAnswers: []string{"no", "no", "standing", "none", "revoke " + rule},
	}
	config, err := json.Marshal(policyelements.SemanticAdmissionConfig{
		Decider: "semantic-primary", StandingExtraction: true,
		RecentLines: 12, MaxPending: 8, TerminalMemory: 8, CancelMemory: 8, StandingMemory: 8,
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
			Revision: 1, Contract: "You are calling a support line for the user. When a recorded menu offers " +
				"an option that matches what the user wants, press that key.",
			Invocation: continuation.Invocation{Instruction: "Act only when this turn requires it.", MaxOutputTokens: 128},
		},
	})
	_ = receivePolicy(t, harness.egress(t, "state"))

	var items []trajectory.Item
	decide := func(itemID, text string) policyelements.SemanticDecision {
		t.Helper()
		version := uint64(len(items) + 1)
		observation := semanticEndpointObservation(itemID, version, version, text)
		items = append(items, observation)
		snapshot := trajectory.Snapshot{Version: version, Items: slices.Clone(items)}
		identity, err := trajectory.IdentifyPrefix(snapshot, snapshot.Version)
		if err != nil {
			t.Fatal(err)
		}
		sendSemanticContext(t, harness, "state-"+itemID, snapshot)
		sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
			Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-" + itemID, SessionID: "semantic-session",
			Payload: semanticCommittedOutcome(observation, itemID, identity, "state-"+itemID, version),
		})
		_ = receivePolicy(t, harness.egress(t, "state"))
		decision := receivePolicy(t, harness.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
		_ = receivePolicy(t, harness.egress(t, "outcome"))
		_ = receivePolicy(t, harness.egress(t, "state"))
		return decision
	}
	first := decide("call", "Call them and find out where my order has got to.")
	if first.StandingBefore != 1 || first.StandingAfter != 1 || first.Standing == nil ||
		len(first.Standing.Pinned) != 1 || !strings.Contains(first.Standing.Pinned[0], rule) ||
		!strings.Contains(first.Evidence, "Standing instructions:\n- "+rule) {
		t.Fatalf("first decision did not read the operator's rule before deciding: %+v", first)
	}
	second := decide("stop", "Stop pressing keys, never mind about the order.")
	if second.StandingBefore != 1 || second.StandingAfter != 1 || second.StandingRevoked != 0 {
		t.Fatalf("the operator's rule was lifted or re-read: %+v", second)
	}
	decider.mu.Lock()
	reads := decider.contractReads
	decider.mu.Unlock()
	if reads != 1 {
		t.Fatalf("the contract was read %d times, want once", reads)
	}
}
