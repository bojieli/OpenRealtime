package action_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements"
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const clientToolResultGraph = `graph client_tool_result {
    action.ClientToolResultIngress :: ingress;
    input result = ingress.result;
    input cancel = ingress.cancel;
    output accepted = ingress.accepted;
    output outcome = ingress.outcome;
}`

func TestClientToolResultIngressAttestsExactBytesThenWaitsForCanonicalSafePoint(t *testing.T) {
	rendezvous := newTestClientToolResultRendezvous()
	mounted, done, cancel := mountClientToolResult(t, rendezvous)
	defer stopClientToolResult(t, done, cancel)
	resultPort, _ := mounted.Ingress("result")
	acceptedPort, _ := mounted.Egress("accepted")
	outcomes, _ := mounted.Egress("outcome")
	result := trajectory.ToolResult{CallID: "call-a", Name: "lookup", Output: json.RawMessage(`{ "ok": true }`)}
	envelope := clientResultEnvelope("session-a", "run-a", "result-a", result)
	sendClientToolResult(t, resultPort, envelope)
	request := awaitClientToolResultRequest(t, rendezvous.started)
	acceptedEnvelope := receiveActionEnvelope(t, acceptedPort)
	accepted := acceptedEnvelope.Payload.(actionelements.ClientToolResultAccepted)
	if request.result.CallID != result.CallID || accepted.IngressItemID != "result-a" ||
		accepted.Receipt != request.receipt ||
		accepted.Receipt.ResultDigest != actionelements.ClientToolResultDigest(result) ||
		len(acceptedEnvelope.CausalParents) != 1 || acceptedEnvelope.CausalParents[0] != "result-a" {
		t.Fatalf("accepted result = %+v / %+v / request %+v", accepted, acceptedEnvelope, request)
	}
	assertNoActionEnvelope(t, outcomes)
	canonical := actionelements.ClientToolResultCanonical{Receipt: request.receipt,
		CanonicalEnvelopeItemID:   "canonical-envelope-a",
		CanonicalTrajectoryItemID: "canonical-trajectory-a", StoreVersion: 7}
	request.release <- testCanonicalRelease{canonical: canonical}
	outcomeEnvelope := receiveActionEnvelope(t, outcomes)
	outcome := outcomeEnvelope.Payload.(actionelements.Outcome)
	if outcome.Kind != actionelements.OutcomeSucceeded || outcome.Stage != "client_tool_result" ||
		outcome.Operation != "result" || outcome.CallID != "call-a" ||
		outcome.ResultDigest != request.receipt.ResultDigest || outcome.IngressItemID != "result-a" ||
		outcome.AcceptedItemID != acceptedEnvelope.ItemID ||
		outcome.CanonicalEnvelopeItemID != "canonical-envelope-a" ||
		outcome.CanonicalTrajectoryItemID != "canonical-trajectory-a" || outcome.CanonicalStoreVersion != 7 ||
		!containsActionParent(outcomeEnvelope, "result-a") ||
		!containsActionParent(outcomeEnvelope, acceptedEnvelope.ItemID) ||
		!containsActionParent(outcomeEnvelope, "canonical-envelope-a") {
		t.Fatalf("canonical result outcome = %+v / %+v", outcome, outcomeEnvelope)
	}

	sendClientToolResult(t, resultPort, envelope)
	replay := receiveActionEnvelope(t, outcomes).Payload.(actionelements.Outcome)
	if replay.Kind != actionelements.OutcomeIgnored || replay.Code != "terminal_replay" {
		t.Fatalf("client result replay = %+v", replay)
	}
}

func TestClientToolResultIngressPreCancelIsCausalAndCrossSessionCannotCancel(t *testing.T) {
	rendezvous := newTestClientToolResultRendezvous()
	mounted, done, cancelRun := mountClientToolResult(t, rendezvous)
	defer stopClientToolResult(t, done, cancelRun)
	resultPort, _ := mounted.Ingress("result")
	cancelPort, _ := mounted.Ingress("cancel")
	outcomes, _ := mounted.Egress("outcome")
	result := trajectory.ToolResult{CallID: "call-a", Name: "lookup", Error: "client failed"}

	sendClientToolResult(t, cancelPort, clientResultCancelEnvelope("session-b", "run-a", "cancel-cross", result.CallID))
	crossed := receiveActionEnvelope(t, outcomes).Payload.(actionelements.Outcome)
	if crossed.Kind != actionelements.OutcomeCanceled || crossed.Code != "pre_canceled" {
		t.Fatalf("cross-session cancellation record = %+v", crossed)
	}
	sendClientToolResult(t, cancelPort, clientResultCancelEnvelope("session-a", "run-a", "cancel-exact", result.CallID))
	preCanceled := receiveActionEnvelope(t, outcomes).Payload.(actionelements.Outcome)
	if preCanceled.Kind != actionelements.OutcomeCanceled || preCanceled.Code != "pre_canceled" {
		t.Fatalf("exact pre-cancel = %+v", preCanceled)
	}
	sendClientToolResult(t, resultPort, clientResultEnvelope("session-a", "run-a", "result-a", result))
	canceledEnvelope := receiveActionEnvelope(t, outcomes)
	canceled := canceledEnvelope.Payload.(actionelements.Outcome)
	if canceled.Kind != actionelements.OutcomeCanceled || canceled.Code != "pre_canceled" ||
		!containsActionParent(canceledEnvelope, "cancel-exact") {
		t.Fatalf("pre-canceled result = %+v / %+v", canceled, canceledEnvelope)
	}
	select {
	case request := <-rendezvous.started:
		t.Fatalf("pre-canceled result reached rendezvous: %+v", request)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestClientToolResultIngressCancelAfterAtomicAcceptCannotRollbackCanonicalWait(t *testing.T) {
	rendezvous := newTestClientToolResultRendezvous()
	mounted, done, cancelRun := mountClientToolResult(t, rendezvous)
	defer stopClientToolResult(t, done, cancelRun)
	resultPort, _ := mounted.Ingress("result")
	cancelPort, _ := mounted.Ingress("cancel")
	acceptedPort, _ := mounted.Egress("accepted")
	outcomes, _ := mounted.Egress("outcome")
	result := trajectory.ToolResult{CallID: "call-a", Name: "lookup", Output: json.RawMessage(`true`)}
	sendClientToolResult(t, resultPort, clientResultEnvelope("session-a", "run-a", "result-a", result))
	request := awaitClientToolResultRequest(t, rendezvous.started)
	acceptedEnvelope := receiveActionEnvelope(t, acceptedPort)
	sendClientToolResult(t, cancelPort, clientResultCancelEnvelope("session-a", "run-a", "cancel-a", result.CallID))
	cancelEnvelope := receiveActionEnvelope(t, outcomes)
	cancelOutcome := cancelEnvelope.Payload.(actionelements.Outcome)
	if cancelOutcome.Operation != "cancel" || cancelOutcome.Code != "accepted_cannot_rollback" ||
		!containsActionParent(cancelEnvelope, "result-a") ||
		!containsActionParent(cancelEnvelope, acceptedEnvelope.ItemID) {
		t.Fatalf("post-accept cancel = %+v / %+v", cancelOutcome, cancelEnvelope)
	}
	request.release <- testCanonicalRelease{canonical: actionelements.ClientToolResultCanonical{
		Receipt: request.receipt, CanonicalEnvelopeItemID: "canonical-a",
		CanonicalTrajectoryItemID: "trajectory-a", StoreVersion: 4}}
	if completed := receiveActionEnvelope(t, outcomes).Payload.(actionelements.Outcome); completed.Kind != actionelements.OutcomeSucceeded {
		t.Fatalf("canonical completion after cancel = %+v", completed)
	}
}

func TestClientToolResultIngressRetryableRefusalAndConflictingDuplicate(t *testing.T) {
	rendezvous := newTestClientToolResultRendezvous()
	rendezvous.rejectNext = true
	mounted, done, cancel := mountClientToolResult(t, rendezvous)
	defer stopClientToolResult(t, done, cancel)
	resultPort, _ := mounted.Ingress("result")
	acceptedPort, _ := mounted.Egress("accepted")
	outcomes, _ := mounted.Egress("outcome")
	result := trajectory.ToolResult{CallID: "call-a", Name: "lookup", Output: json.RawMessage(`true`)}
	sendClientToolResult(t, resultPort, clientResultEnvelope("session-a", "run-a", "first", result))
	refused := receiveActionEnvelope(t, outcomes).Payload.(actionelements.Outcome)
	if refused.Kind != actionelements.OutcomeRejected || refused.Code != "not_accepted" {
		t.Fatalf("retryable refusal = %+v", refused)
	}
	sendClientToolResult(t, resultPort, clientResultEnvelope("session-a", "run-a", "retry", result))
	request := awaitClientToolResultRequest(t, rendezvous.started)
	_ = receiveActionEnvelope(t, acceptedPort)
	conflict := trajectory.ToolResult{CallID: "call-a", Name: "lookup", Output: json.RawMessage(`false`)}
	sendClientToolResult(t, resultPort, clientResultEnvelope("session-a", "run-a", "conflict", conflict))
	conflicted := receiveActionEnvelope(t, outcomes).Payload.(actionelements.Outcome)
	if conflicted.Kind != actionelements.OutcomeFailed || conflicted.Code != "result_conflict" {
		t.Fatalf("conflicting duplicate = %+v", conflicted)
	}
	request.release <- testCanonicalRelease{canonical: actionelements.ClientToolResultCanonical{
		Receipt: request.receipt, CanonicalEnvelopeItemID: "canonical-a",
		CanonicalTrajectoryItemID: "trajectory-a", StoreVersion: 4}}
	_ = receiveActionEnvelope(t, outcomes)
}

func TestClientToolResultIngressBindsImmutableItemEvidenceAcrossRetry(t *testing.T) {
	rendezvous := newTestClientToolResultRendezvous()
	rendezvous.rejectNext = true
	mounted, done, cancel := mountClientToolResult(t, rendezvous)
	defer stopClientToolResult(t, done, cancel)
	resultPort, _ := mounted.Ingress("result")
	outcomes, _ := mounted.Egress("outcome")
	result := trajectory.ToolResult{CallID: "call-a", Name: "lookup", Output: json.RawMessage(`true`)}
	envelope := clientResultEnvelope("session-a", "run-a", "immutable-item", result)
	envelope.TraceID = "trace-a"
	sendClientToolResult(t, resultPort, envelope)
	if refused := receiveActionEnvelope(t, outcomes).Payload.(actionelements.Outcome); refused.Code != "not_accepted" {
		t.Fatalf("initial refusal = %+v", refused)
	}
	envelope.TraceID = "trace-b"
	sendClientToolResult(t, resultPort, envelope)
	conflict := receiveActionEnvelope(t, outcomes).Payload.(actionelements.Outcome)
	if conflict.Kind != actionelements.OutcomeFailed || conflict.Code != "item_evidence_conflict" {
		t.Fatalf("mutated immutable item = %+v", conflict)
	}
	select {
	case request := <-rendezvous.started:
		t.Fatalf("mutated immutable item reached rendezvous: %+v", request)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestClientToolResultIngressRefusalEvidenceIsBoundedAndFailsClosed(t *testing.T) {
	rendezvous := newTestClientToolResultRendezvous()
	rendezvous.rejectAlways = true
	mounted, done, cancel := mountClientToolResult(t, rendezvous)
	defer stopClientToolResult(t, done, cancel)
	resultPort, _ := mounted.Ingress("result")
	outcomes, _ := mounted.Egress("outcome")
	result := trajectory.ToolResult{CallID: "call-a", Name: "lookup", Output: json.RawMessage(`true`)}
	for index := 0; index < 4096; index++ {
		sendClientToolResult(t, resultPort, clientResultEnvelope(
			"session-a", "run-a", fmt.Sprintf("refused-%d", index), result,
		))
		if outcome := receiveActionEnvelope(t, outcomes).Payload.(actionelements.Outcome); outcome.Code != "not_accepted" {
			t.Fatalf("refusal %d = %+v", index, outcome)
		}
	}
	sendClientToolResult(t, resultPort, clientResultEnvelope("session-a", "run-a", "refused-overflow", result))
	overflow := receiveActionEnvelope(t, outcomes).Payload.(actionelements.Outcome)
	if overflow.Kind != actionelements.OutcomeRejected || overflow.Code != "item_capacity" {
		t.Fatalf("bounded refusal overflow = %+v", overflow)
	}
}

func TestClientToolResultIngressUnmountDoesNotWaitForBrokenSubmit(t *testing.T) {
	rendezvous := newTestClientToolResultRendezvous()
	rendezvous.submitGate = make(chan struct{})
	rendezvous.ignoreSubmitContext = true
	mounted, done, cancelRun := mountClientToolResult(t, rendezvous)
	resultPort, _ := mounted.Ingress("result")
	cancelPort, _ := mounted.Ingress("cancel")
	outcomes, _ := mounted.Egress("outcome")
	result := trajectory.ToolResult{CallID: "call-a", Name: "lookup", Output: json.RawMessage(`true`)}
	sendClientToolResult(t, resultPort, clientResultEnvelope("session-a", "run-a", "result-a", result))
	select {
	case <-rendezvous.submitEntered:
	case <-time.After(time.Second):
		t.Fatal("blocking Submit was not entered")
	}
	sendClientToolResult(t, cancelPort, clientResultCancelEnvelope("session-a", "run-a", "cancel-a", result.CallID))
	if canceled := receiveActionEnvelope(t, outcomes).Payload.(actionelements.Outcome); canceled.Code != "cancellation_requested_before_accept" {
		t.Fatalf("cancel during Submit = %+v", canceled)
	}
	cancelRun()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("client result graph stopped with %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("client result graph waited for broken Submit")
	}
	close(rendezvous.submitGate)
}

type testCanonicalRelease struct {
	canonical actionelements.ClientToolResultCanonical
	err       error
}

type testClientToolResultRequest struct {
	result  trajectory.ToolResult
	receipt actionelements.ClientToolResultReceipt
	release chan testCanonicalRelease
}

type testClientToolResultRendezvous struct {
	mu                  sync.Mutex
	sequence            int
	requests            map[string]testClientToolResultRequest
	started             chan testClientToolResultRequest
	rejectNext          bool
	rejectAlways        bool
	submitGate          chan struct{}
	submitEntered       chan struct{}
	ignoreSubmitContext bool
}

func newTestClientToolResultRendezvous() *testClientToolResultRendezvous {
	return &testClientToolResultRendezvous{requests: make(map[string]testClientToolResultRequest),
		started: make(chan testClientToolResultRequest, 8), submitEntered: make(chan struct{}, 1)}
}

func (rendezvous *testClientToolResultRendezvous) SubmitToolResult(ctx context.Context,
	sessionID, runID string, result trajectory.ToolResult) (actionelements.ClientToolResultReceipt, error) {
	rendezvous.mu.Lock()
	if rendezvous.rejectNext || rendezvous.rejectAlways {
		rendezvous.rejectNext = false
		rendezvous.mu.Unlock()
		return actionelements.ClientToolResultReceipt{}, fmt.Errorf("%w: retry test", actionelements.ErrClientToolResultNotAccepted)
	}
	rendezvous.sequence++
	receipt := actionelements.ClientToolResultReceipt{ID: fmt.Sprintf("receipt-%d", rendezvous.sequence),
		SessionID: sessionID, RunID: runID, CallID: result.CallID, Name: result.Name,
		CommitmentID: "commitment-" + result.CallID, CanonicalCallItemID: "call-item-" + result.CallID,
		ResultDigest: actionelements.ClientToolResultDigest(result)}
	gate, ignoreContext := rendezvous.submitGate, rendezvous.ignoreSubmitContext
	rendezvous.mu.Unlock()
	if gate != nil {
		select {
		case rendezvous.submitEntered <- struct{}{}:
		default:
		}
		if ignoreContext {
			<-gate
		} else {
			select {
			case <-gate:
			case <-ctx.Done():
				return actionelements.ClientToolResultReceipt{}, fmt.Errorf("%w: %v", actionelements.ErrClientToolResultNotAccepted, context.Cause(ctx))
			}
		}
	}
	request := testClientToolResultRequest{result: result, receipt: receipt, release: make(chan testCanonicalRelease, 1)}
	rendezvous.mu.Lock()
	rendezvous.requests[receipt.ID] = request
	rendezvous.mu.Unlock()
	select {
	case rendezvous.started <- request:
	case <-ctx.Done():
		return receipt, nil
	}
	return receipt, nil
}

func (rendezvous *testClientToolResultRendezvous) WaitToolResultCanonical(ctx context.Context,
	receipt actionelements.ClientToolResultReceipt) (actionelements.ClientToolResultCanonical, error) {
	rendezvous.mu.Lock()
	request, found := rendezvous.requests[receipt.ID]
	rendezvous.mu.Unlock()
	if !found || request.receipt != receipt {
		return actionelements.ClientToolResultCanonical{}, errors.New("unknown test receipt")
	}
	select {
	case released := <-request.release:
		return released.canonical, released.err
	case <-ctx.Done():
		return actionelements.ClientToolResultCanonical{}, context.Cause(ctx)
	}
}

func mountClientToolResult(t *testing.T, rendezvous actionelements.ClientToolResultRendezvous) (*graphruntime.Mounted, <-chan error, context.CancelFunc) {
	t.Helper()
	registry, err := elements.RuntimeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(actionelements.ClientToolResultRendezvousService, rendezvous); err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: compileClientToolResult(t), Registry: registry, Services: services})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	return mounted, done, cancel
}

func compileClientToolResult(t *testing.T) ir.Graph {
	t.Helper()
	parsed, err := syntax.Parse("client-tool-result.ortg", []byte(clientToolResultGraph))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := elements.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{Catalog: catalog, ResolutionMode: resolve.Update})
	if err != nil {
		t.Fatal(err)
	}
	return compiled.Graph
}

func stopClientToolResult(t *testing.T, done <-chan error, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("client tool-result graph stopped with %v", err)
		}
	case <-time.After(time.Second):
		t.Error("client tool-result graph did not stop")
	}
}

func clientResultEnvelope(session, run, item string, result trajectory.ToolResult) element.Envelope {
	return element.Envelope{Type: actionelements.ClientToolResultType(), ItemID: item, SessionID: session,
		RunID: run, OpportunityID: result.CallID, CancellationScope: run, Payload: result}
}

func clientResultCancelEnvelope(session, run, item, callID string) element.Envelope {
	return element.Envelope{Type: actionelements.InterruptType(), ItemID: item, SessionID: session,
		RunID: run, OpportunityID: callID, CancellationScope: run,
		Payload: actionelements.Interrupt{CallID: callID, Reason: "test cancellation"}}
}

func sendClientToolResult(t *testing.T, port element.OutputPort, envelope element.Envelope) {
	t.Helper()
	result, err := port.Broadcast(context.Background(), envelope)
	if err != nil || result.Delivered != 1 || result.Dropped != 0 {
		t.Fatalf("send client tool result = %+v, %v", result, err)
	}
}

func receiveActionEnvelope(t *testing.T, port element.InputPort) element.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	envelope, err := port.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func assertNoActionEnvelope(t *testing.T, port element.InputPort) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if envelope, err := port.Receive(ctx); err == nil {
		t.Fatalf("unexpected action envelope %+v", envelope)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func awaitClientToolResultRequest(t *testing.T, requests <-chan testClientToolResultRequest) testClientToolResultRequest {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case <-time.After(time.Second):
		t.Fatal("client tool-result rendezvous was not invoked")
		return testClientToolResultRequest{}
	}
}

func containsActionParent(envelope element.Envelope, itemID string) bool {
	for _, parent := range envelope.CausalParents {
		if parent == itemID {
			return true
		}
	}
	return false
}
