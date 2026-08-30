package scenarioconversation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const maximumPendingClientCalls = 64

type clientCallState struct {
	key                 string
	call                *trajectory.ToolCall
	sessionID           string
	runID               string
	commitmentID        string
	canonicalCallItemID string
	terminalResult      *trajectory.ToolResult
	receipt             *actionelements.ClientToolResultReceipt
	result              chan clientDispatchResult
	emitted             bool
	terminal            bool
	canonicalSent       bool
	waitStarted         bool
	canonical           chan clientCanonicalResult
}

type clientDispatchResult struct {
	result trajectory.ToolResult
	err    error
}

type clientCanonicalResult struct {
	canonical actionelements.ClientToolResultCanonical
	err       error
}

// clientBridge is the action.Dispatch implementation selected for ordinary
// Realtime function calls. The graph crosses its ledger before Dispatch is
// invoked; the adapter waits for that committed output before rendering a
// call, and ToolResult waits for ToolResultCommit's canonical output.
type clientBridge struct {
	mu      sync.Mutex
	calls   map[string]*clientCallState
	changed chan struct{}
	closed  bool
}

func newClientBridge() *clientBridge {
	return &clientBridge{calls: make(map[string]*clientCallState), changed: make(chan struct{})}
}

func (*clientBridge) Name() string {
	return "client://openrealtime/scenario-conversation/function-call-output/v1"
}

func (bridge *clientBridge) Dispatch(
	ctx context.Context, call trajectory.ToolCall,
) (trajectory.ToolResult, error) {
	if ctx == nil {
		return trajectory.ToolResult{}, errors.New("scenario conversation client dispatch: nil context")
	}
	call = cloneToolCall(call)
	if err := validateToolCall(call); err != nil {
		return trajectory.ToolResult{}, err
	}
	scope, found := actionelements.DispatchContextFromContext(ctx)
	if !found || !canonicalIdentity(scope.SessionID) || !canonicalIdentity(scope.RunID) ||
		!canonicalIdentity(scope.CommitmentID) || !canonicalIdentity(scope.CanonicalCallItemID) {
		return trajectory.ToolResult{}, errors.New(
			"scenario conversation client dispatch requires exact graph dispatch context")
	}
	key := clientCallScopeKey(scope.SessionID, scope.RunID, call.CallID)
	bridge.mu.Lock()
	if bridge.closed {
		bridge.mu.Unlock()
		return trajectory.ToolResult{}, errors.New("scenario conversation client bridge is closed")
	}
	if len(bridge.calls) >= maximumPendingClientCalls {
		bridge.mu.Unlock()
		return trajectory.ToolResult{}, errors.New("scenario conversation client call capacity is exhausted")
	}
	if _, duplicate := bridge.calls[key]; duplicate {
		bridge.mu.Unlock()
		return trajectory.ToolResult{}, fmt.Errorf(
			"scenario conversation client call %q is duplicated in run %q", call.CallID, scope.RunID)
	}
	copy := cloneToolCall(call)
	state := &clientCallState{
		key: key, call: &copy, sessionID: scope.SessionID, runID: scope.RunID,
		commitmentID: scope.CommitmentID, canonicalCallItemID: scope.CanonicalCallItemID,
		result: make(chan clientDispatchResult, 1), canonical: make(chan clientCanonicalResult, 1),
	}
	bridge.calls[key] = state
	bridge.signalLocked()
	bridge.mu.Unlock()

	select {
	case terminal := <-state.result:
		return terminal.result, terminal.err
	case <-ctx.Done():
		// Submit linearizes by queueing the accepted result while holding the
		// bridge lock. Once queued, dispatch cancellation cannot replace those
		// exact client bytes with a host-synthesized error.
		select {
		case terminal := <-state.result:
			return terminal.result, terminal.err
		default:
		}
		accepted := bridge.cancelDispatch(key, state)
		if accepted {
			terminal := <-state.result
			return terminal.result, terminal.err
		}
		return trajectory.ToolResult{}, context.Cause(ctx)
	}
}

func (bridge *clientBridge) awaitEmission(
	ctx context.Context, sessionID, runID string, committed actionelements.CommittedAction,
) error {
	call := cloneToolCall(committed.Executable.Canonical.Authorized.Confirmed.Declared.Admitted.Proposal.Call)
	if ctx == nil {
		return errors.New("scenario conversation client emission: nil context")
	}
	if err := validateToolCall(call); err != nil {
		return err
	}
	if !canonicalIdentity(sessionID) || !canonicalIdentity(runID) ||
		!canonicalIdentity(committed.Executable.CommitmentID) ||
		!canonicalIdentity(committed.Executable.Canonical.TrajectoryItemID) {
		return errors.New("scenario conversation client emission requires canonical session and run IDs")
	}
	key := clientCallScopeKey(sessionID, runID, call.CallID)
	for {
		bridge.mu.Lock()
		if bridge.closed {
			bridge.mu.Unlock()
			return errors.New("scenario conversation client bridge is closed")
		}
		state := bridge.calls[key]
		changed := bridge.changed
		if state != nil && state.call != nil {
			if !sameToolCall(*state.call, call) || state.sessionID != sessionID || state.runID != runID ||
				state.commitmentID != committed.Executable.CommitmentID ||
				state.canonicalCallItemID != committed.Executable.Canonical.TrajectoryItemID {
				bridge.mu.Unlock()
				return fmt.Errorf("scenario conversation committed call %q drifted from dispatch context", call.CallID)
			}
			if state.emitted || state.terminal {
				bridge.mu.Unlock()
				return fmt.Errorf("scenario conversation committed call %q is not emit-ready", call.CallID)
			}
			state.emitted = true
			bridge.mu.Unlock()
			return nil
		}
		bridge.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

func (bridge *clientBridge) SubmitToolResult(
	ctx context.Context, sessionID, runID string, result trajectory.ToolResult,
) (actionelements.ClientToolResultReceipt, error) {
	if ctx == nil {
		return actionelements.ClientToolResultReceipt{}, fmt.Errorf("%w: nil context",
			actionelements.ErrClientToolResultNotAccepted)
	}
	if err := context.Cause(ctx); err != nil {
		return actionelements.ClientToolResultReceipt{}, fmt.Errorf("%w: %v",
			actionelements.ErrClientToolResultNotAccepted, err)
	}
	result = cloneToolResult(result)
	if err := validateToolResult(result); err != nil {
		return actionelements.ClientToolResultReceipt{}, fmt.Errorf("%w: %v",
			actionelements.ErrClientToolResultNotAccepted, err)
	}
	if !canonicalIdentity(sessionID) || !canonicalIdentity(runID) {
		return actionelements.ClientToolResultReceipt{}, fmt.Errorf("%w: canonical session and run IDs required",
			actionelements.ErrClientToolResultNotAccepted)
	}
	key := clientCallScopeKey(sessionID, runID, result.CallID)
	bridge.mu.Lock()
	if err := context.Cause(ctx); err != nil {
		bridge.mu.Unlock()
		return actionelements.ClientToolResultReceipt{}, fmt.Errorf("%w: %v",
			actionelements.ErrClientToolResultNotAccepted, err)
	}
	state := bridge.calls[key]
	if bridge.closed || state == nil || state.call == nil || !state.emitted || state.terminal {
		bridge.mu.Unlock()
		return actionelements.ClientToolResultReceipt{}, fmt.Errorf(
			"%w: client result %q has no emitted graph-authorized call",
			actionelements.ErrClientToolResultNotAccepted, result.CallID)
	}
	if state.sessionID != sessionID || state.runID != runID {
		bridge.mu.Unlock()
		return actionelements.ClientToolResultReceipt{}, fmt.Errorf(
			"%w: client result %q crossed its session or run",
			actionelements.ErrClientToolResultNotAccepted, result.CallID)
	}
	if state.call.Name != result.Name {
		bridge.mu.Unlock()
		return actionelements.ClientToolResultReceipt{}, fmt.Errorf(
			"%w: client result %q names %q, want %q",
			actionelements.ErrClientToolResultNotAccepted, result.CallID, result.Name, state.call.Name)
	}
	digest := actionelements.ClientToolResultDigest(result)
	receipt := actionelements.ClientToolResultReceipt{
		ID:        clientToolResultReceiptID(sessionID, runID, state.commitmentID, result.CallID, digest),
		SessionID: sessionID, RunID: runID, CallID: result.CallID, Name: result.Name,
		CommitmentID: state.commitmentID, CanonicalCallItemID: state.canonicalCallItemID,
		ResultDigest: digest,
	}
	state.terminal = true
	terminalResult := cloneToolResult(result)
	state.terminalResult = &terminalResult
	state.receipt = &receipt
	// Queue while holding the lock: this is the atomic accepted boundary that
	// Dispatch's cancellation branch observes before it may synthesize error.
	state.result <- clientDispatchResult{result: result}
	bridge.mu.Unlock()
	return receipt, nil
}

func (bridge *clientBridge) WaitToolResultCanonical(
	ctx context.Context, receipt actionelements.ClientToolResultReceipt,
) (actionelements.ClientToolResultCanonical, error) {
	if ctx == nil {
		return actionelements.ClientToolResultCanonical{}, errors.New("scenario conversation canonical wait: nil context")
	}
	bridge.mu.Lock()
	key := clientCallScopeKey(receipt.SessionID, receipt.RunID, receipt.CallID)
	state := bridge.calls[key]
	if state == nil || state.receipt == nil || *state.receipt != receipt || !state.terminal {
		bridge.mu.Unlock()
		return actionelements.ClientToolResultCanonical{}, errors.New("scenario conversation canonical wait has no exact accepted receipt")
	}
	if state.waitStarted {
		bridge.mu.Unlock()
		return actionelements.ClientToolResultCanonical{}, errors.New("scenario conversation canonical receipt already has a waiter")
	}
	state.waitStarted = true
	canonical := state.canonical
	bridge.mu.Unlock()
	select {
	case result := <-canonical:
		bridge.remove(key, state)
		return result.canonical, result.err
	case <-ctx.Done():
		return actionelements.ClientToolResultCanonical{}, context.Cause(ctx)
	}
}

func (bridge *clientBridge) canonical(
	canonical actionelements.CanonicalResult, envelopeItemID string, err error,
) error {
	result := cloneToolResult(canonical.Execution.Result)
	admitted := canonical.Execution.Executable.Canonical.Authorized.Confirmed.Declared.Admitted
	key := clientCallScopeKey(admitted.SessionID, admitted.ModelRunID, result.CallID)
	bridge.mu.Lock()
	state := bridge.calls[key]
	if state == nil || state.call == nil || state.terminalResult == nil ||
		state.receipt == nil || !state.emitted || !state.terminal {
		bridge.mu.Unlock()
		return fmt.Errorf("scenario conversation canonical result %q has no matching client completion", result.CallID)
	}
	if state.sessionID != admitted.SessionID || state.runID != admitted.ModelRunID ||
		state.call.Name != result.Name {
		bridge.mu.Unlock()
		return fmt.Errorf("scenario conversation canonical result %q names %q, want %q",
			result.CallID, result.Name, state.call.Name)
	}
	if !sameToolResult(*state.terminalResult, result) {
		bridge.mu.Unlock()
		return fmt.Errorf("scenario conversation canonical result %q drifted from the client completion",
			result.CallID)
	}
	if canonical.Execution.CommitmentID != state.commitmentID ||
		canonical.Execution.Executable.Canonical.TrajectoryItemID != state.canonicalCallItemID ||
		canonical.TrajectoryItemID == "" || canonical.StoreVersion == 0 ||
		!canonicalIdentity(envelopeItemID) {
		bridge.mu.Unlock()
		return fmt.Errorf("scenario conversation canonical result %q drifted from accepted authority", result.CallID)
	}
	if state.canonicalSent {
		bridge.mu.Unlock()
		return fmt.Errorf("scenario conversation canonical result %q was delivered more than once", result.CallID)
	}
	state.canonicalSent = true
	receipt := *state.receipt
	response := clientCanonicalResult{err: err, canonical: actionelements.ClientToolResultCanonical{
		Receipt: receipt, CanonicalEnvelopeItemID: envelopeItemID,
		CanonicalTrajectoryItemID: canonical.TrajectoryItemID, StoreVersion: canonical.StoreVersion,
	}}
	state.canonical <- response
	bridge.mu.Unlock()
	return nil
}

func (bridge *clientBridge) canonicalFailure(
	sessionID, runID, callID, commitmentID string, err error,
) error {
	if err == nil {
		err = errors.New("scenario conversation canonical tool-result commit failed")
	}
	key := clientCallScopeKey(sessionID, runID, callID)
	bridge.mu.Lock()
	state := bridge.calls[key]
	if state == nil || state.call == nil || state.terminalResult == nil ||
		state.receipt == nil || !state.emitted || !state.terminal ||
		state.sessionID != sessionID || state.runID != runID || state.commitmentID != commitmentID {
		bridge.mu.Unlock()
		return fmt.Errorf("scenario conversation canonical failure %q has no matching client completion", callID)
	}
	if state.canonicalSent {
		bridge.mu.Unlock()
		return fmt.Errorf("scenario conversation canonical failure %q was delivered more than once", callID)
	}
	state.canonicalSent = true
	state.canonical <- clientCanonicalResult{err: err}
	bridge.mu.Unlock()
	return nil
}

func (bridge *clientBridge) failEmission(sessionID, runID, callID string, err error) {
	key := clientCallScopeKey(sessionID, runID, callID)
	bridge.mu.Lock()
	state := bridge.calls[key]
	if state == nil || state.terminal {
		bridge.mu.Unlock()
		return
	}
	state.terminal = true
	delete(bridge.calls, key)
	bridge.signalLocked()
	bridge.mu.Unlock()
	state.result <- clientDispatchResult{err: err}
}

func (bridge *clientBridge) Close(cause error) {
	bridge.mu.Lock()
	if bridge.closed {
		bridge.mu.Unlock()
		return
	}
	bridge.closed = true
	if cause == nil {
		cause = errors.New("scenario conversation client bridge closed")
	}
	pending := make([]*clientCallState, 0, len(bridge.calls))
	for _, state := range bridge.calls {
		if !state.terminal {
			state.terminal = true
			pending = append(pending, state)
		} else if !state.canonicalSent {
			state.canonicalSent = true
			select {
			case state.canonical <- clientCanonicalResult{err: cause}:
			default:
			}
		}
	}
	bridge.signalLocked()
	bridge.mu.Unlock()
	for _, state := range pending {
		state.result <- clientDispatchResult{err: cause}
	}
}

func (bridge *clientBridge) signalLocked() {
	close(bridge.changed)
	bridge.changed = make(chan struct{})
}

func (bridge *clientBridge) remove(key string, state *clientCallState) {
	bridge.mu.Lock()
	if bridge.calls[key] == state {
		delete(bridge.calls, key)
	}
	bridge.signalLocked()
	bridge.mu.Unlock()
}

func (bridge *clientBridge) cancelDispatch(key string, state *clientCallState) bool {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if bridge.calls[key] == state && state.receipt != nil && state.terminalResult != nil {
		return true
	}
	if bridge.calls[key] == state {
		state.terminal = true
		delete(bridge.calls, key)
	}
	bridge.signalLocked()
	return false
}

func clientCallScopeKey(sessionID, runID, callID string) string {
	return sessionID + "\x00" + runID + "\x00" + callID
}

func cloneToolCall(call trajectory.ToolCall) trajectory.ToolCall {
	call.Arguments = slices.Clone(call.Arguments)
	return call
}

func cloneToolResult(result trajectory.ToolResult) trajectory.ToolResult {
	result.Output = slices.Clone(result.Output)
	return result
}

func validateToolCall(call trajectory.ToolCall) error {
	if !canonicalIdentity(call.CallID) || !canonicalIdentity(call.Name) || !json.Valid(call.Arguments) {
		return errors.New("scenario conversation graph produced an invalid client call")
	}
	var arguments map[string]json.RawMessage
	if json.Unmarshal(call.Arguments, &arguments) != nil || arguments == nil {
		return errors.New("scenario conversation graph produced non-object tool arguments")
	}
	return nil
}

func validateToolResult(result trajectory.ToolResult) error {
	if !canonicalIdentity(result.CallID) || !canonicalIdentity(result.Name) ||
		(len(result.Output) == 0) == (result.Error == "") ||
		(len(result.Output) != 0 && !json.Valid(result.Output)) ||
		len(result.Output)+len(result.Error) > maximumAdapterTextBytes {
		return errors.New("scenario conversation client returned an invalid terminal tool result")
	}
	return nil
}

func sameToolCall(left, right trajectory.ToolCall) bool {
	if left.CallID != right.CallID || left.Name != right.Name {
		return false
	}
	var leftValue, rightValue any
	return json.Unmarshal(left.Arguments, &leftValue) == nil &&
		json.Unmarshal(right.Arguments, &rightValue) == nil &&
		reflect.DeepEqual(leftValue, rightValue)
}

func sameToolResult(left, right trajectory.ToolResult) bool {
	return left.CallID == right.CallID && left.Name == right.Name && left.Error == right.Error &&
		bytes.Equal(left.Output, right.Output)
}

func clientToolResultReceiptID(sessionID, runID, commitmentID, callID, resultDigest string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("openrealtime.scenario-conversation.client-result-receipt/v1\x00"))
	for _, value := range []string{sessionID, runID, commitmentID, callID, resultDigest} {
		_, _ = fmt.Fprintf(hash, "%d:%s", len(value), value)
	}
	return "receipt:" + hex.EncodeToString(hash.Sum(nil))
}

var _ legacyaction.Dispatcher = (*clientBridge)(nil)
var _ actionelements.ClientToolResultRendezvous = (*clientBridge)(nil)
