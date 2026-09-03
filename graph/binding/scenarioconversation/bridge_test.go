package scenarioconversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const clientBridgeDispatchGraph = `graph scenario_client_bridge_dispatch_test {
    action.ToolLookup :: lookup;
    action.NormalizeArguments :: normalize;
    authority.TargetFence :: fence;
    action.LedgerCommit :: ledger;
    action.Dispatch :: dispatch;

    input admitted = lookup.proposal;
    lookup.declared -> normalize.action;
    output declared = normalize.normalized;
    input confirmed = fence.action;
    output authorized = fence.authorized;
    input canonical = ledger.action;
    input ledger_cancel = ledger.cancel;
    input ledger_timeout = ledger.timeout;
    output executable = ledger.executable;
    input execute = dispatch.execute;
    input dispatch_cancel = dispatch.cancel;
    input dispatch_timeout = dispatch.timeout;

    output lookup_outcome = lookup.outcome;
    output lookup_resolved = lookup.resolved;
    output normalization_outcome = normalize.outcome;
    output normalization_resolved = normalize.resolved;
    output fence_outcome = fence.outcome;
    output fence_resolved = fence.resolved;
    output ledger_transition = ledger.transition;
    output ledger_outcome = ledger.outcome;
    output ledger_resolved = ledger.resolved;
    output committed = dispatch.committed;
    output result = dispatch.result;
    output dispatch_transition = dispatch.transition;
    output audit = dispatch.audit;
    output dispatch_outcome = dispatch.outcome;
    output dispatch_resolved = dispatch.resolved;
}`

type toolCallCaptureSink struct {
	playbackClientSink
	calls []legacy.ToolCallEvent
}

func (sink *toolCallCaptureSink) ToolCalls(_ context.Context, event legacy.ToolCallEvent) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	copy := event
	copy.Calls = make([]trajectory.ToolCall, len(event.Calls))
	for index, call := range event.Calls {
		copy.Calls[index] = cloneToolCall(call)
	}
	sink.calls = append(sink.calls, copy)
	return nil
}

func TestClientBridgeRejectsDispatchWithoutExactGraphContext(t *testing.T) {
	bridge := newClientBridge()
	call := clientBridgeCall("call_1")

	_, err := bridge.Dispatch(context.Background(), call)
	if err == nil || !strings.Contains(err.Error(), "requires exact graph dispatch context") {
		t.Fatalf("Dispatch without action.Dispatch context error = %v", err)
	}
	bridge.mu.Lock()
	pending := len(bridge.calls)
	bridge.mu.Unlock()
	if pending != 0 {
		t.Fatalf("rejected dispatch retained %d call states", pending)
	}
}

func TestClientBridgeAcceptsOnlyMountedActionDispatchAuthority(t *testing.T) {
	const sessionID, runID, callID = "session_mounted", "run_mounted", "call_mounted"
	bridge := newClientBridge()
	store, admitted := clientBridgeAuthorityFixture(t, sessionID, runID, callID)
	mounted, graphCancel, graphDone := mountClientBridgeDispatchGraph(t, bridge, store)
	defer stopClientBridgeDispatchGraph(t, graphCancel, graphDone)

	sendClientBridgeEnvelope(t, mounted, "admitted", element.Envelope{
		ItemID: "admitted_item", SessionID: sessionID, RunID: runID, Payload: admitted,
	})
	declaredEnvelope := receiveClientBridgeEnvelope(t, mounted, "declared")
	declared, ok := declaredEnvelope.Payload.(actionelements.DeclaredAction)
	if !ok {
		t.Fatalf("declared payload type = %T", declaredEnvelope.Payload)
	}
	sendClientBridgeEnvelope(t, mounted, "confirmed", element.Envelope{
		ItemID: "confirmed_item", SessionID: sessionID, RunID: runID,
		CausalParents: []string{declaredEnvelope.ItemID},
		Payload:       actionelements.ConfirmedAction{Declared: declared},
	})
	authorizedEnvelope := receiveClientBridgeEnvelope(t, mounted, "authorized")
	authorized, ok := authorizedEnvelope.Payload.(actionelements.AuthorizedAction)
	if !ok {
		t.Fatalf("authorized payload type = %T", authorizedEnvelope.Payload)
	}
	canonical := actionelements.CanonicalAction{
		Authorized: authorized, ProposalItemID: "canonical_proposal",
		TrajectoryItemID: "canonical_call", StoreVersion: store.Snapshot().Version,
	}
	sendClientBridgeEnvelope(t, mounted, "canonical", element.Envelope{
		ItemID: "canonical_item", SessionID: sessionID, RunID: runID,
		CausalParents: []string{authorizedEnvelope.ItemID}, Payload: canonical,
	})
	executableEnvelope := receiveClientBridgeEnvelope(t, mounted, "executable")
	executable, ok := executableEnvelope.Payload.(actionelements.ExecutableAction)
	if !ok {
		t.Fatalf("executable payload type = %T", executableEnvelope.Payload)
	}
	sendClientBridgeEnvelope(t, mounted, "execute", element.Envelope{
		ItemID: "execute_item", SessionID: sessionID, RunID: runID,
		CausalParents: []string{executableEnvelope.ItemID}, Payload: executable,
	})
	committedEnvelope := receiveClientBridgeEnvelope(t, mounted, "committed")
	committed, ok := committedEnvelope.Payload.(actionelements.CommittedAction)
	if !ok {
		t.Fatalf("committed payload type = %T", committedEnvelope.Payload)
	}
	if err := bridge.awaitEmission(context.Background(), sessionID, runID, committed); err != nil {
		t.Fatalf("await mounted Dispatch emission: %v", err)
	}
	key := clientCallScopeKey(sessionID, runID, callID)
	bridge.mu.Lock()
	state := bridge.calls[key]
	bridge.mu.Unlock()
	if state == nil || state.sessionID != sessionID || state.runID != runID ||
		state.commitmentID != executable.CommitmentID ||
		state.canonicalCallItemID != canonical.TrajectoryItemID {
		t.Fatalf("mounted Dispatch state = %#v", state)
	}

	// The real action.Dispatch suppresses the replay before a second external
	// invocation can compete for the bridge's exact composite state.
	sendClientBridgeEnvelope(t, mounted, "execute", element.Envelope{
		ItemID: "execute_duplicate", SessionID: sessionID, RunID: runID,
		CausalParents: []string{executableEnvelope.ItemID}, Payload: executable,
	})
	duplicateEnvelope := receiveClientBridgeEnvelope(t, mounted, "dispatch_outcome")
	duplicate, ok := duplicateEnvelope.Payload.(actionelements.Outcome)
	if !ok || duplicate.Kind != actionelements.OutcomeIgnored || duplicate.Code != "duplicate_inflight" ||
		duplicate.CallID != callID {
		t.Fatalf("same-run duplicate outcome = %#v", duplicateEnvelope.Payload)
	}

	result := clientBridgeResult(callID, 7)
	if _, err := bridge.SubmitToolResult(context.Background(), sessionID, runID, result); err != nil {
		t.Fatalf("submit through mounted Dispatch: %v", err)
	}
	resultEnvelope := receiveClientBridgeEnvelope(t, mounted, "result")
	execution, ok := resultEnvelope.Payload.(actionelements.ExecutionResult)
	if !ok || execution.CompletionOrigin != actionelements.CompletionReturned ||
		execution.CommitmentID != executable.CommitmentID || !sameToolResult(execution.Result, result) {
		t.Fatalf("mounted Dispatch execution = %#v", resultEnvelope.Payload)
	}
	bridge.remove(key, state)
}

func TestNormalizedClientCallKeepsProposalAndCompletesWithEffectiveArguments(t *testing.T) {
	const sessionID, runID, callID = "session_normalized", "run_normalized", "call_normalized"
	originalArguments := json.RawMessage(`{"order_id":"X Y Z88"}`)
	effectiveArguments := json.RawMessage(`{"order_id":"XYZ88"}`)
	proposalCall := trajectory.ToolCall{
		CallID: callID, Name: "test_tool", Arguments: originalArguments,
	}
	bridge := newClientBridge()
	store, admitted := clientBridgeProposalFixture(t, sessionID, runID, proposalCall)
	mounted, graphCancel, graphDone := mountClientBridgeDispatchGraphWithSpec(
		t, bridge, store, legacyaction.ToolSpec{
			Name: "test_tool", Description: "normalize a spoken order identifier",
			Parameters: json.RawMessage(
				`{"type":"object","properties":{"order_id":{"type":"string"}},"required":["order_id"]}`,
			),
			ArgumentNormalizers: []legacyaction.ToolArgumentNormalizer{{
				Argument: "order_id", Normalizer: legacyaction.ToolParameterCompactASCIIAlphanumericV1,
			}},
			Confirm: legacyaction.ConfirmNever,
		},
	)
	defer stopClientBridgeDispatchGraph(t, graphCancel, graphDone)

	sendClientBridgeEnvelope(t, mounted, "admitted", element.Envelope{
		ItemID: "admitted_item", SessionID: sessionID, RunID: runID, Payload: admitted,
	})
	declaredEnvelope := receiveClientBridgeEnvelope(t, mounted, "declared")
	declared, ok := declaredEnvelope.Payload.(actionelements.DeclaredAction)
	if !ok {
		t.Fatalf("normalized declaration payload type = %T", declaredEnvelope.Payload)
	}
	if string(declared.Admitted.Proposal.Call.Arguments) != string(originalArguments) {
		t.Fatalf("normalization mutated proposal bytes: %s", declared.Admitted.Proposal.Call.Arguments)
	}
	if declared.EffectiveCall == nil ||
		string(declared.EffectiveCall.Arguments) != string(effectiveArguments) ||
		declared.Normalization == nil {
		t.Fatalf("normalization did not produce the effective call and evidence: %+v", declared)
	}
	rewrites := make([]trajectory.ToolCallArgumentRewrite, len(declared.Normalization.Rewrites))
	for index, rewrite := range declared.Normalization.Rewrites {
		rewrites[index] = trajectory.ToolCallArgumentRewrite{
			Argument: rewrite.Argument, Normalizer: rewrite.Normalizer,
		}
	}
	effectiveCall := cloneToolCall(*declared.EffectiveCall)
	if err := store.Append(trajectory.Item{
		ID: "canonical_call", Kind: trajectory.KindToolCall, MonotonicNS: 3,
		CausalParentIDs: []string{"canonical_proposal"}, SourceRevision: 1, InvocationID: runID,
		Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, ToolCall: &effectiveCall,
		ToolCallDerivation: &trajectory.ToolCallDerivation{
			Kind:                     trajectory.ToolCallDerivationSchemaNormalizationV1,
			SourceArgumentsDigest:    declared.Normalization.OriginalArgumentsDigest,
			EffectiveArgumentsDigest: declared.Normalization.EffectiveArgumentsDigest,
			RegistryReference:        declared.Normalization.RegistryReference,
			RegistryDigest:           declared.Normalization.RegistryDigest,
			DeclarationDigest:        declared.Normalization.DeclarationDigest,
			Rewrites:                 rewrites,
		},
	}); err != nil {
		t.Fatalf("append normalized canonical call: %v", err)
	}

	sendClientBridgeEnvelope(t, mounted, "confirmed", element.Envelope{
		ItemID: "confirmed_item", SessionID: sessionID, RunID: runID,
		CausalParents: []string{declaredEnvelope.ItemID},
		Payload:       actionelements.ConfirmedAction{Declared: declared},
	})
	authorizedEnvelope := receiveClientBridgeEnvelope(t, mounted, "authorized")
	authorized, ok := authorizedEnvelope.Payload.(actionelements.AuthorizedAction)
	if !ok {
		t.Fatalf("normalized authorized payload type = %T", authorizedEnvelope.Payload)
	}
	canonical := actionelements.CanonicalAction{
		Authorized: authorized, ProposalItemID: "canonical_proposal",
		TrajectoryItemID: "canonical_call", StoreVersion: store.Snapshot().Version,
	}
	sendClientBridgeEnvelope(t, mounted, "canonical", element.Envelope{
		ItemID: "canonical_item", SessionID: sessionID, RunID: runID,
		CausalParents: []string{authorizedEnvelope.ItemID}, Payload: canonical,
	})
	executableEnvelope := receiveClientBridgeEnvelope(t, mounted, "executable")
	executable, ok := executableEnvelope.Payload.(actionelements.ExecutableAction)
	if !ok {
		t.Fatalf("normalized executable payload type = %T", executableEnvelope.Payload)
	}
	sendClientBridgeEnvelope(t, mounted, "execute", element.Envelope{
		ItemID: "execute_item", SessionID: sessionID, RunID: runID,
		CausalParents: []string{executableEnvelope.ItemID}, Payload: executable,
	})
	committedEnvelope := receiveClientBridgeEnvelope(t, mounted, "committed")
	committed, ok := committedEnvelope.Payload.(actionelements.CommittedAction)
	if !ok {
		t.Fatalf("normalized committed payload type = %T", committedEnvelope.Payload)
	}

	sink := &toolCallCaptureSink{}
	session := &session{
		sessionID: sessionID, sink: sink, bundle: &sessionBundle{bridge: bridge},
		calls: make(map[string]activeClientCall), terminalCalls: make(map[string]struct{}),
	}
	if err := session.publishCall(context.Background(), element.Envelope{
		ItemID: "committed_envelope", SessionID: sessionID, RunID: runID, Payload: committed,
	}); err != nil {
		t.Fatalf("publish normalized client call: %v", err)
	}
	sink.mu.Lock()
	if len(sink.calls) != 1 || len(sink.calls[0].Calls) != 1 {
		sink.mu.Unlock()
		t.Fatalf("normalized client emission count = %+v", sink.calls)
	}
	emitted := cloneToolCall(sink.calls[0].Calls[0])
	sink.mu.Unlock()
	if string(emitted.Arguments) != string(effectiveArguments) {
		t.Fatalf("client received proposal rather than effective bytes: %s", emitted.Arguments)
	}
	if string(committed.Executable.Canonical.Authorized.Confirmed.Declared.Admitted.Proposal.Call.Arguments) !=
		string(originalArguments) {
		t.Fatal("client emission mutated the retained model proposal")
	}

	result := clientBridgeResult(callID, 17)
	receipt, err := bridge.SubmitToolResult(context.Background(), sessionID, runID, result)
	if err != nil {
		t.Fatalf("submit result for normalized client call: %v", err)
	}
	resultEnvelope := receiveClientBridgeEnvelope(t, mounted, "result")
	execution, ok := resultEnvelope.Payload.(actionelements.ExecutionResult)
	if !ok || execution.CompletionOrigin != actionelements.CompletionReturned ||
		!sameToolResult(execution.Result, result) {
		t.Fatalf("normalized dispatch execution = %#v", resultEnvelope.Payload)
	}
	canonicalResult := actionelements.CanonicalResult{
		Execution: execution, TrajectoryItemID: "canonical_result", StoreVersion: store.Snapshot().Version,
	}
	if err := session.acceptCanonicalResult(element.Envelope{
		ItemID: "canonical_result_envelope", SessionID: sessionID, RunID: runID,
		Payload: canonicalResult,
	}); err != nil {
		t.Fatalf("accept normalized canonical result: %v", err)
	}
	evidence, err := bridge.WaitToolResultCanonical(context.Background(), receipt)
	if err != nil {
		t.Fatalf("wait for normalized canonical result: %v", err)
	}
	if evidence.Receipt != receipt || evidence.CanonicalTrajectoryItemID != "canonical_result" {
		t.Fatalf("normalized canonical evidence = %+v", evidence)
	}
	if _, active := session.calls[callID]; active {
		t.Fatal("canonical result left the normalized client call active")
	}
}

func TestClientBridgeReusesCallIDAcross513SequentialRuns(t *testing.T) {
	bridge := newClientBridge()
	const sessionID = "session_reuse"

	for index := 0; index < 513; index++ {
		runID := fmt.Sprintf("run_%03d", index)
		committed := clientBridgeCommitted(sessionID, runID, "shared_call")
		state := installClientBridgeState(t, bridge, sessionID, runID, committed)
		if err := bridge.awaitEmission(context.Background(), sessionID, runID, committed); err != nil {
			t.Fatalf("run %d await emission: %v", index, err)
		}

		result := clientBridgeResult("shared_call", index)
		receipt, err := bridge.SubmitToolResult(context.Background(), sessionID, runID, result)
		if err != nil {
			t.Fatalf("run %d submit: %v", index, err)
		}
		select {
		case dispatched := <-state.result:
			if dispatched.err != nil || !sameToolResult(dispatched.result, result) {
				t.Fatalf("run %d dispatched result = %#v, %v", index, dispatched.result, dispatched.err)
			}
		default:
			t.Fatalf("run %d accepted result was not queued for Dispatch", index)
		}

		canonical, envelopeItemID := clientBridgeCanonical(committed, result, index)
		// Canonical delivery may precede the stable API's wait without losing
		// the exact durable safe point.
		if err := bridge.canonical(canonical, envelopeItemID, nil); err != nil {
			t.Fatalf("run %d canonical before wait: %v", index, err)
		}
		evidence, err := bridge.WaitToolResultCanonical(context.Background(), receipt)
		if err != nil {
			t.Fatalf("run %d wait: %v", index, err)
		}
		if evidence.Receipt != receipt || evidence.CanonicalEnvelopeItemID != envelopeItemID ||
			evidence.CanonicalTrajectoryItemID != canonical.TrajectoryItemID ||
			evidence.StoreVersion != canonical.StoreVersion {
			t.Fatalf("run %d canonical evidence = %#v", index, evidence)
		}
		bridge.mu.Lock()
		_, retained := bridge.calls[clientCallScopeKey(sessionID, runID, "shared_call")]
		bridge.mu.Unlock()
		if retained {
			t.Fatalf("run %d retained a completed call", index)
		}
	}
}

func TestClientBridgeKeepsSessionAndRunScopesIsolated(t *testing.T) {
	bridge := newClientBridge()
	type scopedCall struct {
		sessionID string
		runID     string
		index     int
	}
	scopes := []scopedCall{
		{sessionID: "session_a", runID: "run_shared", index: 1},
		{sessionID: "session_b", runID: "run_shared", index: 2},
		{sessionID: "session_a", runID: "run_other", index: 3},
	}
	states := make(map[string]*clientCallState, len(scopes))
	committed := make(map[string]actionelements.CommittedAction, len(scopes))
	for _, scope := range scopes {
		key := clientCallScopeKey(scope.sessionID, scope.runID, "shared_call")
		committed[key] = clientBridgeCommitted(scope.sessionID, scope.runID, "shared_call")
		states[key] = installClientBridgeState(t, bridge, scope.sessionID, scope.runID, committed[key])
		if err := bridge.awaitEmission(context.Background(), scope.sessionID, scope.runID, committed[key]); err != nil {
			t.Fatalf("await emission for %q/%q: %v", scope.sessionID, scope.runID, err)
		}
	}

	wrong := clientBridgeResult("shared_call", 99)
	if _, err := bridge.SubmitToolResult(context.Background(), "session_b", "run_other", wrong); err == nil ||
		!errors.Is(err, actionelements.ErrClientToolResultNotAccepted) {
		t.Fatalf("cross-scope submit error = %v", err)
	}

	for _, scope := range scopes {
		key := clientCallScopeKey(scope.sessionID, scope.runID, "shared_call")
		result := clientBridgeResult("shared_call", scope.index)
		receipt, err := bridge.SubmitToolResult(context.Background(), scope.sessionID, scope.runID, result)
		if err != nil {
			t.Fatalf("submit for %q/%q: %v", scope.sessionID, scope.runID, err)
		}
		dispatched := <-states[key].result
		if dispatched.err != nil || !sameToolResult(dispatched.result, result) {
			t.Fatalf("dispatch for %q/%q = %#v, %v", scope.sessionID, scope.runID,
				dispatched.result, dispatched.err)
		}
		canonical, envelopeItemID := clientBridgeCanonical(committed[key], result, scope.index)
		if err := bridge.canonical(canonical, envelopeItemID, nil); err != nil {
			t.Fatalf("canonical for %q/%q: %v", scope.sessionID, scope.runID, err)
		}
		if _, err := bridge.WaitToolResultCanonical(context.Background(), receipt); err != nil {
			t.Fatalf("wait for %q/%q: %v", scope.sessionID, scope.runID, err)
		}
	}
}

func TestClientBridgeRejectsCommittedContextDriftAndSameRunDuplicates(t *testing.T) {
	bridge := newClientBridge()
	const sessionID, runID = "session_exact", "run_exact"
	committed := clientBridgeCommitted(sessionID, runID, "call_exact")
	state := installClientBridgeState(t, bridge, sessionID, runID, committed)

	tests := []struct {
		name   string
		mutate func(*actionelements.CommittedAction)
	}{
		{name: "commitment", mutate: func(value *actionelements.CommittedAction) {
			value.Executable.CommitmentID = "commitment_tampered"
		}},
		{name: "canonical call item", mutate: func(value *actionelements.CommittedAction) {
			value.Executable.Canonical.TrajectoryItemID = "call_item_tampered"
		}},
		{name: "call arguments", mutate: func(value *actionelements.CommittedAction) {
			value.Executable.Canonical.Authorized.Confirmed.Declared.Admitted.Proposal.Call.Arguments =
				json.RawMessage(`{"value":"tampered"}`)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			drifted := committed
			test.mutate(&drifted)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := bridge.awaitEmission(ctx, sessionID, runID, drifted); err == nil ||
				!strings.Contains(err.Error(), "drifted from dispatch context") {
				t.Fatalf("drift error = %v", err)
			}
		})
	}

	if err := bridge.awaitEmission(context.Background(), sessionID, runID, committed); err != nil {
		t.Fatalf("exact committed action: %v", err)
	}
	if err := bridge.awaitEmission(context.Background(), sessionID, runID, committed); err == nil ||
		!strings.Contains(err.Error(), "not emit-ready") {
		t.Fatalf("duplicate committed emission error = %v", err)
	}
	result := clientBridgeResult("call_exact", 1)
	if _, err := bridge.SubmitToolResult(context.Background(), sessionID, runID, result); err != nil {
		t.Fatalf("first result: %v", err)
	}
	if _, err := bridge.SubmitToolResult(context.Background(), sessionID, runID, result); err == nil ||
		!errors.Is(err, actionelements.ErrClientToolResultNotAccepted) {
		t.Fatalf("duplicate result error = %v", err)
	}
	select {
	case dispatched := <-state.result:
		if dispatched.err != nil || !sameToolResult(dispatched.result, result) {
			t.Fatalf("queued result = %#v, %v", dispatched.result, dispatched.err)
		}
	default:
		t.Fatal("first accepted result was not queued")
	}
}

func TestClientBridgeRejectsDuplicateCanonicalWaiter(t *testing.T) {
	bridge := newClientBridge()
	const sessionID, runID = "session_wait", "run_wait"
	committed := clientBridgeCommitted(sessionID, runID, "call_wait")
	state := installClientBridgeState(t, bridge, sessionID, runID, committed)
	if err := bridge.awaitEmission(context.Background(), sessionID, runID, committed); err != nil {
		t.Fatal(err)
	}
	result := clientBridgeResult("call_wait", 1)
	receipt, err := bridge.SubmitToolResult(context.Background(), sessionID, runID, result)
	if err != nil {
		t.Fatal(err)
	}
	<-state.result

	type waitResult struct {
		canonical actionelements.ClientToolResultCanonical
		err       error
	}
	first := make(chan waitResult, 1)
	go func() {
		canonical, waitErr := bridge.WaitToolResultCanonical(context.Background(), receipt)
		first <- waitResult{canonical: canonical, err: waitErr}
	}()
	waitForClientBridge(t, func() bool {
		bridge.mu.Lock()
		defer bridge.mu.Unlock()
		return state.waitStarted
	})

	if _, err := bridge.WaitToolResultCanonical(context.Background(), receipt); err == nil ||
		!strings.Contains(err.Error(), "already has a waiter") {
		t.Fatalf("duplicate wait error = %v", err)
	}
	canonical, envelopeItemID := clientBridgeCanonical(committed, result, 1)
	if err := bridge.canonical(canonical, envelopeItemID, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case completed := <-first:
		if completed.err != nil || completed.canonical.Receipt != receipt {
			t.Fatalf("first wait = %#v, %v", completed.canonical, completed.err)
		}
	case <-time.After(time.Second):
		t.Fatal("first canonical wait did not complete")
	}
}

func TestClientBridgeCancelAndAcceptanceLinearizeExactlyOnce(t *testing.T) {
	t.Run("cancel before acceptance", func(t *testing.T) {
		bridge := newClientBridge()
		committed := clientBridgeCommitted("session_cancel", "run_cancel_first", "call_cancel")
		state := installClientBridgeState(t, bridge, "session_cancel", "run_cancel_first", committed)
		if err := bridge.awaitEmission(context.Background(), "session_cancel", "run_cancel_first", committed); err != nil {
			t.Fatal(err)
		}
		if bridge.cancelDispatch(state.key, state) {
			t.Fatal("unaccepted state reported an accepted client result")
		}
		if _, err := bridge.SubmitToolResult(context.Background(), "session_cancel", "run_cancel_first",
			clientBridgeResult("call_cancel", 1)); err == nil ||
			!errors.Is(err, actionelements.ErrClientToolResultNotAccepted) {
			t.Fatalf("submit after cancellation error = %v", err)
		}
	})

	t.Run("acceptance before cancel", func(t *testing.T) {
		bridge := newClientBridge()
		committed := clientBridgeCommitted("session_cancel", "run_accept_first", "call_cancel")
		state := installClientBridgeState(t, bridge, "session_cancel", "run_accept_first", committed)
		if err := bridge.awaitEmission(context.Background(), "session_cancel", "run_accept_first", committed); err != nil {
			t.Fatal(err)
		}
		result := clientBridgeResult("call_cancel", 2)
		if _, err := bridge.SubmitToolResult(context.Background(), "session_cancel", "run_accept_first", result); err != nil {
			t.Fatal(err)
		}
		if !bridge.cancelDispatch(state.key, state) {
			t.Fatal("accepted result lost to dispatch cancellation")
		}
		queued := <-state.result
		if queued.err != nil || !sameToolResult(queued.result, result) {
			t.Fatalf("accepted terminal result = %#v, %v", queued.result, queued.err)
		}
	})

	t.Run("concurrent", func(t *testing.T) {
		for index := 0; index < 256; index++ {
			bridge := newClientBridge()
			runID := fmt.Sprintf("run_race_%03d", index)
			committed := clientBridgeCommitted("session_cancel", runID, "call_cancel")
			state := installClientBridgeState(t, bridge, "session_cancel", runID, committed)
			if err := bridge.awaitEmission(context.Background(), "session_cancel", runID, committed); err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			var receipt actionelements.ClientToolResultReceipt
			var submitErr error
			var cancelAccepted bool
			var workers sync.WaitGroup
			workers.Add(2)
			go func() {
				defer workers.Done()
				<-start
				receipt, submitErr = bridge.SubmitToolResult(context.Background(), "session_cancel", runID,
					clientBridgeResult("call_cancel", index))
			}()
			go func() {
				defer workers.Done()
				<-start
				cancelAccepted = bridge.cancelDispatch(state.key, state)
			}()
			close(start)
			workers.Wait()

			if submitErr == nil {
				if receipt.RunID != runID || !cancelAccepted {
					t.Fatalf("iteration %d accepted receipt = %#v, cancel accepted = %v", index,
						receipt, cancelAccepted)
				}
				select {
				case terminal := <-state.result:
					if terminal.err != nil || terminal.result.CallID != "call_cancel" {
						t.Fatalf("iteration %d terminal = %#v, %v", index, terminal.result, terminal.err)
					}
				default:
					t.Fatalf("iteration %d accepted without queued result", index)
				}
				bridge.remove(state.key, state)
				continue
			}
			if !errors.Is(submitErr, actionelements.ErrClientToolResultNotAccepted) || cancelAccepted {
				t.Fatalf("iteration %d submit error = %v, cancel accepted = %v", index,
					submitErr, cancelAccepted)
			}
			bridge.mu.Lock()
			_, retained := bridge.calls[state.key]
			bridge.mu.Unlock()
			if retained {
				t.Fatalf("iteration %d canceled call was retained", index)
			}
		}
	})
}

func TestClientBridgeDispatcherErrorCleanupNeedsNoClientResult(t *testing.T) {
	bridge := newClientBridge()
	committed := clientBridgeCommitted("session_error", "run_error", "call_error")
	state := installClientBridgeState(t, bridge, "session_error", "run_error", committed)
	if err := bridge.awaitEmission(context.Background(), "session_error", "run_error", committed); err != nil {
		t.Fatal(err)
	}

	// action.Dispatch returns dispatcher_error on its result edge. Its canceled
	// dispatcher context removes this client-only rendezvous; the graph join
	// handles the signed dispatcher error without fabricating a client receipt.
	if bridge.cancelDispatch(state.key, state) {
		t.Fatal("dispatcher error cleanup reported a client acceptance")
	}
	bridge.mu.Lock()
	_, retained := bridge.calls[state.key]
	bridge.mu.Unlock()
	if retained {
		t.Fatal("dispatcher error retained client rendezvous state")
	}
	if _, err := bridge.SubmitToolResult(context.Background(), "session_error", "run_error",
		clientBridgeResult("call_error", 1)); err == nil ||
		!errors.Is(err, actionelements.ErrClientToolResultNotAccepted) {
		t.Fatalf("client result after dispatcher error = %v", err)
	}
}

func installClientBridgeState(t *testing.T, bridge *clientBridge, sessionID, runID string,
	committed actionelements.CommittedAction,
) *clientCallState {
	t.Helper()
	call := cloneToolCall(committed.Executable.Canonical.Authorized.Confirmed.Declared.Admitted.Proposal.Call)
	key := clientCallScopeKey(sessionID, runID, call.CallID)
	state := &clientCallState{
		key: key, call: &call, sessionID: sessionID, runID: runID,
		commitmentID:        committed.Executable.CommitmentID,
		canonicalCallItemID: committed.Executable.Canonical.TrajectoryItemID,
		result:              make(chan clientDispatchResult, 1),
		canonical:           make(chan clientCanonicalResult, 1),
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if _, duplicate := bridge.calls[key]; duplicate {
		t.Fatalf("test setup duplicated bridge key %q", key)
	}
	bridge.calls[key] = state
	bridge.signalLocked()
	return state
}

func clientBridgeCall(callID string) trajectory.ToolCall {
	return trajectory.ToolCall{CallID: callID, Name: "test_tool", Arguments: json.RawMessage(`{"value":1}`)}
}

func clientBridgeResult(callID string, value int) trajectory.ToolResult {
	return trajectory.ToolResult{CallID: callID, Name: "test_tool",
		Output: json.RawMessage(fmt.Sprintf(`{"value":%d}`, value))}
}

func clientBridgeCommitted(sessionID, runID, callID string) actionelements.CommittedAction {
	value := actionelements.CommittedAction{}
	value.Executable.CommitmentID = "commitment_" + sessionID + "_" + runID + "_" + callID
	value.Executable.Canonical.TrajectoryItemID = "canonical_call_" + sessionID + "_" + runID + "_" + callID
	admitted := &value.Executable.Canonical.Authorized.Confirmed.Declared.Admitted
	admitted.SessionID = sessionID
	admitted.ModelRunID = runID
	admitted.Proposal.Call = clientBridgeCall(callID)
	return value
}

func clientBridgeCanonical(committed actionelements.CommittedAction, result trajectory.ToolResult,
	sequence int,
) (actionelements.CanonicalResult, string) {
	return actionelements.CanonicalResult{
		Execution: actionelements.ExecutionResult{
			Executable: committed.Executable, CompletionOrigin: actionelements.CompletionReturned,
			CallID: result.CallID, Name: result.Name, CommitmentID: committed.Executable.CommitmentID,
			Result: cloneToolResult(result),
		},
		TrajectoryItemID: fmt.Sprintf("canonical_result_%s_%03d", result.CallID, sequence),
		StoreVersion:     uint64(sequence + 1),
	}, fmt.Sprintf("canonical_envelope_%s_%03d", result.CallID, sequence)
}

func waitForClientBridge(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for client bridge state")
		}
		time.Sleep(time.Millisecond)
	}
}

func clientBridgeAuthorityFixture(t *testing.T, sessionID, runID, callID string,
) (*trajectory.Store, actionelements.AdmittedProposal) {
	t.Helper()
	call := clientBridgeCall(callID)
	producer := trajectory.Producer{
		Phase: trajectory.PhaseFast, Provider: "bridge-test", Model: "bridge-test-model",
		ReasoningEffort: string(continuation.EffortMinimal),
		SpeechAuthority: string(continuation.SpeechAuthoritySilent),
	}
	store := trajectory.NewStore()
	if err := store.AppendBatch([]trajectory.Item{
		{
			ID: "authority_observation", Kind: trajectory.KindObservation, MonotonicNS: 1,
			SourceRevision: 1, Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
			Content: "run the mounted tool",
			Event: &trajectory.EventMetadata{EventID: "authority_trigger", Type: "input_text",
				Source: "user", Channel: "text", OccurredNS: 1},
		},
		{
			ID: "canonical_proposal", Kind: trajectory.KindToolProposal, MonotonicNS: 2,
			CausalParentIDs: []string{"authority_observation"}, SourceRevision: 1,
			InvocationID: runID, Producer: producer, ToolCall: clientBridgeCallPointer(call),
		},
		{
			ID: "canonical_call", Kind: trajectory.KindToolCall, MonotonicNS: 3,
			CausalParentIDs: []string{"canonical_proposal"}, SourceRevision: 1,
			InvocationID: runID, Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime},
			ToolCall: clientBridgeCallPointer(call),
		},
	}); err != nil {
		t.Fatal(err)
	}
	return store, actionelements.AdmittedProposal{
		Proposal: cognitionelements.ToolProposal{
			Call: cloneToolCall(call), Declared: true,
			ProviderAuthority: continuation.ToolAuthorityPropose,
		},
		ProposalItemID: "proposal_envelope", CandidateItemID: "candidate_envelope",
		ResultItemID: "result_envelope", ModelRunID: runID, SessionID: sessionID,
		ActivationItemID: "activation_item", ActivationCauseItemID: "activation_cause",
		Authority: trajectory.AuthorityUser, AuthorityItemID: "authority_observation",
		ObservationTriggerItemID: "authority_trigger", SourceRevision: 1,
		ContextVersion: 1, ContextEnvelopeItemID: "context_envelope",
		ContextTailItem: "authority_observation", ProviderReference: "bridge-test",
		ModelResultDigest: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		ModelProducer:     producer,
	}
}

func clientBridgeProposalFixture(t *testing.T, sessionID, runID string, call trajectory.ToolCall,
) (*trajectory.Store, actionelements.AdmittedProposal) {
	t.Helper()
	producer := trajectory.Producer{
		Phase: trajectory.PhaseFast, Provider: "bridge-test", Model: "bridge-test-model",
		ReasoningEffort: string(continuation.EffortMinimal),
		SpeechAuthority: string(continuation.SpeechAuthoritySilent),
	}
	store := trajectory.NewStore()
	if err := store.AppendBatch([]trajectory.Item{
		{
			ID: "authority_observation", Kind: trajectory.KindObservation, MonotonicNS: 1,
			SourceRevision: 1, Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
			Content: "run the mounted tool",
			Event: &trajectory.EventMetadata{EventID: "authority_trigger", Type: "input_text",
				Source: "user", Channel: "text", OccurredNS: 1},
		},
		{
			ID: "canonical_proposal", Kind: trajectory.KindToolProposal, MonotonicNS: 2,
			CausalParentIDs: []string{"authority_observation"}, SourceRevision: 1,
			InvocationID: runID, Producer: producer, ToolCall: clientBridgeCallPointer(call),
		},
	}); err != nil {
		t.Fatal(err)
	}
	return store, actionelements.AdmittedProposal{
		Proposal: cognitionelements.ToolProposal{
			Call: cloneToolCall(call), Declared: true,
			ProviderAuthority: continuation.ToolAuthorityPropose,
		},
		ProposalItemID: "proposal_envelope", CandidateItemID: "candidate_envelope",
		ResultItemID: "result_envelope", ModelRunID: runID, SessionID: sessionID,
		ActivationItemID: "activation_item", ActivationCauseItemID: "activation_cause",
		Authority: trajectory.AuthorityUser, AuthorityItemID: "authority_observation",
		ObservationTriggerItemID: "authority_trigger", SourceRevision: 1,
		ContextVersion: 1, ContextEnvelopeItemID: "context_envelope",
		ContextTailItem: "authority_observation", ProviderReference: "bridge-test",
		ModelResultDigest: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		ModelProducer:     producer,
	}
}

func clientBridgeCallPointer(call trajectory.ToolCall) *trajectory.ToolCall {
	copy := cloneToolCall(call)
	return &copy
}

func mountClientBridgeDispatchGraph(t *testing.T, bridge *clientBridge, store *trajectory.Store,
) (*graphruntime.Mounted, context.CancelFunc, <-chan error) {
	return mountClientBridgeDispatchGraphWithSpec(t, bridge, store, legacyaction.ToolSpec{
		Name: "test_tool", Description: "mounted bridge test tool",
		Parameters: json.RawMessage(`{"type":"object"}`), Confirm: legacyaction.ConfirmNever,
	})
}

func mountClientBridgeDispatchGraphWithSpec(t *testing.T, bridge *clientBridge,
	store *trajectory.Store, spec legacyaction.ToolSpec,
) (*graphruntime.Mounted, context.CancelFunc, <-chan error) {
	t.Helper()
	tools := actionelements.NewToolRegistries()
	spec.Dispatcher = bridge
	if err := tools.Register("tools", []legacyaction.ToolSpec{spec}); err != nil {
		t.Fatal(err)
	}
	targets := actionelements.NewTargetRegistries()
	if err := targets.Register("target", computeruse.Target{
		Name: "target", Sources: []string{"screen"}, Width: 100, Height: 100,
	}); err != nil {
		t.Fatal(err)
	}
	ledgers := actionelements.NewLedgerRegistries()
	if err := ledgers.Register("main", legacyaction.NewLedger()); err != nil {
		t.Fatal(err)
	}
	services := graphruntime.NewServiceSet()
	for name, service := range map[string]any{
		actionelements.TrajectoryStoreService:      store,
		actionelements.ToolRegistryService:         tools,
		actionelements.TargetRegistryService:       targets,
		actionelements.LedgerRegistryService:       ledgers,
		actionelements.ConfirmationRegistryService: actionelements.NewConfirmationProviders(),
	} {
		if _, err := services.Set(name, service); err != nil {
			t.Fatal(err)
		}
	}
	catalog := resolve.NewCatalog()
	if err := actionelements.RegisterDescriptors(catalog); err != nil {
		t.Fatal(err)
	}
	parsed, err := syntax.Parse("scenario-client-bridge-dispatch-test.ortg", []byte(clientBridgeDispatchGraph))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := graphruntime.NewRegistry()
	if err := actionelements.RegisterFactories(registry); err != nil {
		t.Fatal(err)
	}
	var now atomic.Uint64
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: compiled.Graph, Registry: registry, Services: services,
		Values: map[string]json.RawMessage{
			"lookup":   json.RawMessage(`{"registry":"tools"}`),
			"fence":    json.RawMessage(`{"target":"target"}`),
			"ledger":   json.RawMessage(`{"ledger":"main"}`),
			"dispatch": json.RawMessage(`{"registry":"tools","ledger":"main"}`),
		},
		Now: func() uint64 { return now.Add(1) }, ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	return mounted, cancel, done
}

func sendClientBridgeEnvelope(t *testing.T, mounted *graphruntime.Mounted, portName string,
	envelope element.Envelope,
) {
	t.Helper()
	port, err := mounted.Ingress(portName)
	if err != nil {
		t.Fatal(err)
	}
	envelope.Type = port.Type()
	if envelope.TraceID == "" {
		envelope.TraceID = envelope.ItemID
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	delivery, err := port.Broadcast(ctx, envelope)
	if err != nil || delivery.Delivered != 1 || delivery.Dropped != 0 {
		t.Fatalf("send %s delivery = %+v, %v", portName, delivery, err)
	}
}

func receiveClientBridgeEnvelope(t *testing.T, mounted *graphruntime.Mounted, portName string,
) element.Envelope {
	t.Helper()
	port, err := mounted.Egress(portName)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	envelope, err := port.Receive(ctx)
	if err != nil {
		t.Fatalf("receive %s: %v", portName, err)
	}
	return envelope
}

func stopClientBridgeDispatchGraph(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("mounted bridge dispatch graph stopped with %v", err)
		}
	case <-time.After(time.Second):
		t.Error("mounted bridge dispatch graph did not stop")
	}
}
