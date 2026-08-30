package scenarioconversation

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestClientToolResultOutcomeRequiresEveryCanonicalReceiptField(t *testing.T) {
	result := trajectory.ToolResult{CallID: "call-a", Name: "lookup", Output: []byte(`{"ok":true}`)}
	digest := actionelements.ClientToolResultDigest(result)
	baseOutcome := actionelements.Outcome{
		Kind: actionelements.OutcomeSucceeded, Stage: "client_tool_result", Operation: "result",
		CallID: result.CallID, ResultDigest: digest, IngressItemID: "api-result-a",
		AcceptedItemID: "accepted-a", CanonicalEnvelopeItemID: "canonical-envelope-a",
		CanonicalTrajectoryItemID: "canonical-trajectory-a", CanonicalStoreVersion: 7,
	}
	baseEnvelope := element.Envelope{
		Type: actionelements.OutcomeType(), ItemID: "outcome-a", SessionID: "session-a", RunID: "run-a",
		OpportunityID: result.CallID, CancellationScope: "run-a",
		CausalParents: []string{"api-result-a", "accepted-a", "canonical-envelope-a"},
	}

	tests := []struct {
		name string
		edit func(*element.Envelope, *actionelements.Outcome)
	}{
		{name: "digest", edit: func(_ *element.Envelope, outcome *actionelements.Outcome) { outcome.ResultDigest += "00" }},
		{name: "ingress", edit: func(_ *element.Envelope, outcome *actionelements.Outcome) { outcome.IngressItemID = "other-ingress" }},
		{name: "accepted", edit: func(_ *element.Envelope, outcome *actionelements.Outcome) { outcome.AcceptedItemID = "" }},
		{name: "canonical envelope", edit: func(_ *element.Envelope, outcome *actionelements.Outcome) { outcome.CanonicalEnvelopeItemID = "" }},
		{name: "canonical trajectory", edit: func(_ *element.Envelope, outcome *actionelements.Outcome) { outcome.CanonicalTrajectoryItemID = "" }},
		{name: "store version", edit: func(_ *element.Envelope, outcome *actionelements.Outcome) { outcome.CanonicalStoreVersion = 0 }},
		{name: "run", edit: func(envelope *element.Envelope, _ *actionelements.Outcome) { envelope.RunID = "other-run" }},
		{name: "scope", edit: func(envelope *element.Envelope, _ *actionelements.Outcome) { envelope.CancellationScope = "other-run" }},
		{name: "opportunity", edit: func(envelope *element.Envelope, _ *actionelements.Outcome) { envelope.OpportunityID = "other-call" }},
		{name: "accepted parent", edit: func(envelope *element.Envelope, _ *actionelements.Outcome) {
			envelope.CausalParents = slices.DeleteFunc(envelope.CausalParents, func(value string) bool { return value == "accepted-a" })
		}},
		{name: "canonical parent", edit: func(envelope *element.Envelope, _ *actionelements.Outcome) {
			envelope.CausalParents = slices.DeleteFunc(envelope.CausalParents, func(value string) bool { return value == "canonical-envelope-a" })
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session, pending := toolResultOutcomeSession(digest, false)
			envelope := baseEnvelope.Clone()
			outcome := baseOutcome
			test.edit(&envelope, &outcome)
			envelope.Payload = outcome
			if err := session.acceptClientToolResultOutcome(envelope); err == nil {
				t.Fatal("drifted tool-result receipt was accepted")
			}
			if session.pendingOps[pending.requestID] != pending || pending.completed {
				t.Fatal("invalid receipt consumed the exact pending operation")
			}
		})
	}

	t.Run("exact", func(t *testing.T) {
		session, pending := toolResultOutcomeSession(digest, false)
		envelope := baseEnvelope.Clone()
		envelope.Payload = baseOutcome
		if err := session.acceptClientToolResultOutcome(envelope); err != nil {
			t.Fatal(err)
		}
		if session.pendingOps[pending.requestID] != nil || !pending.completed {
			t.Fatal("exact receipt did not retire its operation")
		}
		if ack := <-pending.result; ack.err != nil {
			t.Fatalf("exact receipt ACK = %v", ack.err)
		}
	})
}

func TestAbandonedToolResultOperationConsumesValidatedLateCanonicalACK(t *testing.T) {
	result := trajectory.ToolResult{CallID: "call-a", Name: "lookup", Output: []byte(`true`)}
	digest := actionelements.ClientToolResultDigest(result)
	session, pending := toolResultOutcomeSession(digest, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := session.awaitOperation(ctx, pending.requestID, pending); !errors.Is(err, context.Canceled) {
		t.Fatalf("await cancellation = %v", err)
	}
	if !pending.abandoned || session.pendingOps[pending.requestID] != pending {
		t.Fatal("caller timeout did not leave a bounded exact tombstone")
	}
	outcome := actionelements.Outcome{
		Kind: actionelements.OutcomeSucceeded, Stage: "client_tool_result", Operation: "result",
		CallID: "call-a", ResultDigest: digest, IngressItemID: pending.requestID,
		AcceptedItemID: "accepted-a", CanonicalEnvelopeItemID: "canonical-a",
		CanonicalTrajectoryItemID: "trajectory-a", CanonicalStoreVersion: 3,
	}
	envelope := element.Envelope{
		Type: actionelements.OutcomeType(), ItemID: "outcome-a", SessionID: "session-a", RunID: "run-a",
		OpportunityID: "call-a", CancellationScope: "run-a",
		CausalParents: []string{pending.requestID, "accepted-a", "canonical-a"}, Payload: outcome,
	}
	if err := session.acceptClientToolResultOutcome(envelope); err != nil {
		t.Fatal(err)
	}
	if session.pendingOps[pending.requestID] != nil || !pending.completed || len(pending.result) != 0 {
		t.Fatal("late canonical ACK was not consumed without reviving the timed-out caller")
	}
}

func TestClientToolResultCancelOutcomeUsesActiveOrExactTerminalRun(t *testing.T) {
	active := activeClientCall{runID: "run-a", call: trajectory.ToolCall{CallID: "call-a", Name: "lookup"}}
	session := &session{
		sessionID: "session-a", calls: map[string]activeClientCall{},
		terminalCalls: map[string]struct{}{
			clientCallScopeKey("session-a", "run-a", "call-a"): {},
		},
	}
	_ = active
	outcome := actionelements.Outcome{
		Kind: actionelements.OutcomeIgnored, Stage: "client_tool_result", Operation: "cancel",
		CallID: "call-a", Code: "already_terminal",
	}
	envelope := element.Envelope{
		Type: actionelements.OutcomeType(), ItemID: "cancel-outcome", SessionID: "session-a", RunID: "run-a",
		OpportunityID: "call-a", CancellationScope: "run-a", Payload: outcome,
	}
	if err := session.acceptClientToolResultOutcome(envelope); err != nil {
		t.Fatal(err)
	}
	envelope.RunID, envelope.CancellationScope = "run-b", "run-b"
	if err := session.acceptClientToolResultOutcome(envelope); err == nil {
		t.Fatal("cross-run cancellation outcome was accepted")
	}
}

func toolResultOutcomeSession(digest string, abandoned bool) (*session, *pendingOperation) {
	pending := &pendingOperation{
		requestID: "api-result-a", operation: "tool_result", generation: "run-a",
		callID: "call-a", name: "lookup", digest: digest, abandoned: abandoned,
		result: make(chan operationAck, 1),
	}
	return &session{
		sessionID: "session-a", pendingOps: map[string]*pendingOperation{pending.requestID: pending},
		calls: make(map[string]activeClientCall), terminalCalls: make(map[string]struct{}),
	}, pending
}
