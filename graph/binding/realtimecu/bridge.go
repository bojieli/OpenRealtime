package realtimecu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const maximumPendingClientCalls = 64

type sessionSettings struct {
	instruction string
	tools       []legacyaction.ToolSpec
	allowed     map[string]struct{}
}

type clientCallState struct {
	registered chan struct{}
	result     chan dispatchResult
	call       *trajectory.ToolCall
	emitted    bool
	terminal   bool
}

type dispatchResult struct {
	result trajectory.ToolResult
	err    error
}

// clientBridge is both the action.Dispatch implementation selected by the
// graph and the rendezvous used by the session adapter. The graph commits the
// executable action before the adapter is allowed to render it; the ordinary
// function_call_output returns here and unblocks action.Dispatch, which then
// emits the authenticated typed result into ToolResultCommit.
type clientBridge struct {
	mu       sync.Mutex
	target   computeruse.Target
	wanted   map[string]legacyaction.ToolSpec
	settings sessionSettings
	calls    map[string]*clientCallState
	changed  chan struct{}
	closed   bool
}

func newClientBridge(target computeruse.Target) (*clientBridge, []legacyaction.ToolSpec, error) {
	overrides := make(map[string]legacyaction.Confirm, len(computeruse.Names()))
	for _, name := range computeruse.Names() {
		overrides[name] = legacyaction.ConfirmNever
	}
	bridge := &clientBridge{
		target: target, wanted: make(map[string]legacyaction.ToolSpec),
		calls: make(map[string]*clientCallState), changed: make(chan struct{}),
	}
	specs, err := computeruse.Specs(target, bridge, overrides)
	if err != nil {
		return nil, nil, err
	}
	for _, spec := range specs {
		bridge.wanted[spec.Name] = cloneToolSpec(spec)
	}
	return bridge, specs, nil
}

func (*clientBridge) Name() string {
	return "client://openrealtime/realtime-cu/function-call-output/v1"
}

func (bridge *clientBridge) Update(settings legacy.Settings) error {
	allowed := make(map[string]struct{}, len(settings.Tools))
	tools := make([]legacyaction.ToolSpec, len(settings.Tools))
	for index, supplied := range settings.Tools {
		wanted, found := bridge.wanted[supplied.Name]
		if !found {
			return fmt.Errorf("realtime-CU session declared non-standard tool %q", supplied.Name)
		}
		if supplied.Description != wanted.Description || supplied.Confirm != legacyaction.ConfirmNever ||
			supplied.Target != bridge.target.Name || supplied.Background != wanted.Background ||
			!jsonEqual(supplied.Parameters, wanted.Parameters) {
			return fmt.Errorf("realtime-CU tool %q declaration drifted from the exact target-bound schema", supplied.Name)
		}
		if _, duplicate := allowed[supplied.Name]; duplicate {
			return fmt.Errorf("realtime-CU session repeated tool %q", supplied.Name)
		}
		allowed[supplied.Name] = struct{}{}
		tools[index] = cloneToolSpec(supplied)
	}
	if len(tools) == 0 {
		return errors.New("realtime-CU session requires at least one declared computer-use tool")
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if bridge.closed {
		return errors.New("realtime-CU client action bridge is closed")
	}
	if len(bridge.calls) != 0 {
		return errors.New("realtime-CU tool declarations cannot change while an action is pending")
	}
	bridge.settings = sessionSettings{
		instruction: strings.TrimSpace(settings.Instruction), tools: tools, allowed: allowed,
	}
	return nil
}

func (bridge *clientBridge) SnapshotSettings() sessionSettings {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	result := sessionSettings{instruction: bridge.settings.instruction}
	result.tools = make([]legacyaction.ToolSpec, len(bridge.settings.tools))
	for index, spec := range bridge.settings.tools {
		result.tools[index] = cloneToolSpec(spec)
	}
	result.allowed = make(map[string]struct{}, len(bridge.settings.allowed))
	for name := range bridge.settings.allowed {
		result.allowed[name] = struct{}{}
	}
	return result
}

func (bridge *clientBridge) Dispatch(
	ctx context.Context, call trajectory.ToolCall,
) (trajectory.ToolResult, error) {
	if ctx == nil {
		return trajectory.ToolResult{}, errors.New("realtime-CU client dispatch: nil context")
	}
	call = cloneToolCall(call)
	if err := validateClientCall(call); err != nil {
		return trajectory.ToolResult{}, err
	}
	bridge.mu.Lock()
	if bridge.closed {
		bridge.mu.Unlock()
		return trajectory.ToolResult{}, errors.New("realtime-CU client action bridge is closed")
	}
	if _, allowed := bridge.settings.allowed[call.Name]; !allowed {
		bridge.mu.Unlock()
		return trajectory.ToolResult{}, fmt.Errorf("realtime-CU client did not declare tool %q", call.Name)
	}
	state := bridge.calls[call.CallID]
	if state == nil {
		if len(bridge.calls) >= maximumPendingClientCalls {
			bridge.mu.Unlock()
			return trajectory.ToolResult{}, errors.New("realtime-CU client action capacity is exhausted")
		}
		state = newClientCallState()
		bridge.calls[call.CallID] = state
	}
	if state.call != nil || state.terminal {
		bridge.mu.Unlock()
		return trajectory.ToolResult{}, fmt.Errorf("realtime-CU client call %q is duplicated", call.CallID)
	}
	copy := cloneToolCall(call)
	state.call = &copy
	close(state.registered)
	bridge.signalLocked()
	bridge.mu.Unlock()

	select {
	case terminal := <-state.result:
		bridge.remove(call.CallID, state)
		return terminal.result, terminal.err
	case <-ctx.Done():
		bridge.fail(call.CallID, state, context.Cause(ctx))
		return trajectory.ToolResult{}, context.Cause(ctx)
	}
}

func (bridge *clientBridge) AwaitEmission(
	ctx context.Context, call trajectory.ToolCall,
) error {
	if ctx == nil {
		return errors.New("realtime-CU client emission: nil context")
	}
	if err := validateClientCall(call); err != nil {
		return err
	}
	for {
		bridge.mu.Lock()
		if bridge.closed {
			bridge.mu.Unlock()
			return errors.New("realtime-CU client action bridge is closed")
		}
		state := bridge.calls[call.CallID]
		changed := bridge.changed
		if state != nil && state.call != nil {
			if !sameToolCall(*state.call, call) {
				bridge.mu.Unlock()
				return fmt.Errorf("realtime-CU committed call %q drifted from dispatcher input", call.CallID)
			}
			if state.emitted || state.terminal {
				bridge.mu.Unlock()
				return fmt.Errorf("realtime-CU committed call %q is not emit-ready", call.CallID)
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

func (bridge *clientBridge) Complete(result trajectory.ToolResult) error {
	result = cloneToolResult(result)
	if result.CallID == "" || result.Name == "" ||
		(len(result.Output) == 0) == (result.Error == "") ||
		(len(result.Output) != 0 && !json.Valid(result.Output)) {
		return errors.New("realtime-CU client returned an invalid terminal tool result")
	}
	bridge.mu.Lock()
	state := bridge.calls[result.CallID]
	if bridge.closed || state == nil || state.call == nil || !state.emitted || state.terminal {
		bridge.mu.Unlock()
		return fmt.Errorf("realtime-CU client result %q has no emitted graph-authorized call", result.CallID)
	}
	if state.call.Name != result.Name {
		bridge.mu.Unlock()
		return fmt.Errorf("realtime-CU client result %q names %q, want %q", result.CallID, result.Name, state.call.Name)
	}
	state.terminal = true
	bridge.mu.Unlock()
	state.result <- dispatchResult{result: result}
	return nil
}

func (bridge *clientBridge) FailEmission(callID string, err error) {
	bridge.mu.Lock()
	state := bridge.calls[callID]
	if state == nil || state.terminal {
		bridge.mu.Unlock()
		return
	}
	state.terminal = true
	bridge.mu.Unlock()
	state.result <- dispatchResult{err: err}
}

func (bridge *clientBridge) Close(cause error) {
	bridge.mu.Lock()
	if bridge.closed {
		bridge.mu.Unlock()
		return
	}
	bridge.closed = true
	if cause == nil {
		cause = errors.New("realtime-CU client action bridge closed")
	}
	pending := make([]*clientCallState, 0, len(bridge.calls))
	for _, state := range bridge.calls {
		if !state.terminal {
			state.terminal = true
			pending = append(pending, state)
		}
	}
	bridge.signalLocked()
	bridge.mu.Unlock()
	for _, state := range pending {
		state.result <- dispatchResult{err: cause}
	}
}

func newClientCallState() *clientCallState {
	return &clientCallState{registered: make(chan struct{}), result: make(chan dispatchResult, 1)}
}

func (bridge *clientBridge) signalLocked() {
	close(bridge.changed)
	bridge.changed = make(chan struct{})
}

func (bridge *clientBridge) remove(callID string, state *clientCallState) {
	bridge.mu.Lock()
	if bridge.calls[callID] == state {
		delete(bridge.calls, callID)
	}
	bridge.signalLocked()
	bridge.mu.Unlock()
}

func (bridge *clientBridge) fail(callID string, state *clientCallState, err error) {
	bridge.mu.Lock()
	if bridge.calls[callID] == state {
		state.terminal = true
		delete(bridge.calls, callID)
	}
	bridge.signalLocked()
	bridge.mu.Unlock()
	_ = err
}

type sessionModel struct {
	provider continuation.Provider
	bridge   *clientBridge
	base     continuation.Descriptor
}

func (model *sessionModel) Descriptor() continuation.Descriptor { return model.base }

func (model *sessionModel) Continue(
	ctx context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	settings := model.bridge.SnapshotSettings()
	baseInstruction := strings.TrimSpace(request.Invocation.Instruction)
	if settings.instruction != "" {
		baseInstruction += "\n\nSession instruction (lower priority than the graph's authority rules):\n" + settings.instruction
	}
	request.Invocation.Instruction = baseInstruction
	request.Invocation.Tools = make([]continuation.ToolDefinition, len(settings.tools))
	for index, spec := range settings.tools {
		request.Invocation.Tools[index] = continuation.ToolDefinition{
			Name: spec.Name, Description: spec.Description,
			Parameters: slices.Clone(spec.Parameters), Background: spec.Background,
		}
	}
	return model.provider.Continue(ctx, request, emit)
}

func (model *sessionModel) Close() error {
	if closer, ok := model.provider.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func cloneToolSpec(spec legacyaction.ToolSpec) legacyaction.ToolSpec {
	spec.Parameters = slices.Clone(spec.Parameters)
	return spec
}

func cloneToolCall(call trajectory.ToolCall) trajectory.ToolCall {
	call.Arguments = slices.Clone(call.Arguments)
	return call
}

func cloneToolResult(result trajectory.ToolResult) trajectory.ToolResult {
	result.Output = slices.Clone(result.Output)
	return result
}

func validateClientCall(call trajectory.ToolCall) error {
	if !canonical(call.CallID) || !canonical(call.Name) || !json.Valid(call.Arguments) {
		return errors.New("realtime-CU graph produced an invalid client call")
	}
	return nil
}

func sameToolCall(left, right trajectory.ToolCall) bool {
	return left.CallID == right.CallID && left.Name == right.Name &&
		jsonEqual(left.Arguments, right.Arguments)
}

func jsonEqual(left, right json.RawMessage) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}
