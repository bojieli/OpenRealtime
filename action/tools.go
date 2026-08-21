package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/trajectory"
)

// Dispatcher executes one authoritative call.
//
// Implementations must be idempotent by call ID: recovery may retry a
// committed call, and a retry that duplicated the external effect would make
// the commit boundary meaningless in exactly the situation it matters most.
type Dispatcher interface {
	Name() string
	Dispatch(context.Context, trajectory.ToolCall) (trajectory.ToolResult, error)
}

// DispatcherFunc adapts a function to Dispatcher.
type DispatcherFunc func(context.Context, trajectory.ToolCall) (trajectory.ToolResult, error)

func (function DispatcherFunc) Name() string { return "func" }
func (function DispatcherFunc) Dispatch(ctx context.Context, call trajectory.ToolCall) (trajectory.ToolResult, error) {
	return function(ctx, call)
}

// ConfirmationRequest is what a confirmer is asked to authorize. It carries
// the declared requirement and the call, and nothing that would let a
// confirmer be talked into an answer by the content it is authorizing.
type ConfirmationRequest struct {
	Call    trajectory.ToolCall `json:"call"`
	Confirm Confirm             `json:"confirm"`
	// Target is the declared context an action applies to - a browser context
	// or a virtual display - so a human sees what the click will land on.
	Target string `json:"target,omitempty"`
}

// Confirmer authorizes actions whose declared requirement calls for it.
type Confirmer interface {
	Confirm(context.Context, ConfirmationRequest) (bool, error)
}

// ConfirmerFunc adapts a function to Confirmer.
type ConfirmerFunc func(context.Context, ConfirmationRequest) (bool, error)

func (function ConfirmerFunc) Confirm(ctx context.Context, request ConfirmationRequest) (bool, error) {
	return function(ctx, request)
}

// DenyAll refuses every action that requires confirmation. It is the correct
// default for an unattended deployment: an action nobody can authorize should
// not happen because nobody was asked.
type DenyAll struct{}

func (DenyAll) Confirm(context.Context, ConfirmationRequest) (bool, error) { return false, nil }

// PolicyDecision is how a deployment answers the "policy" requirement.
type PolicyDecision func(trajectory.ToolCall) bool

var (
	// ErrNoAuthority means a call was never committed as an executable call.
	// A proposal from the fast provider lands here, which is the first
	// cognition boundary enforced at the point of effect.
	ErrNoAuthority = errors.New("call has no execution authority in the trajectory")
	// ErrNotConfirmed means a declared confirmation requirement was refused.
	ErrNotConfirmed = errors.New("action was not confirmed")
	// ErrUnknownTool names a call for a tool that was never declared.
	ErrUnknownTool = errors.New("unknown tool")
)

// ToolSpec is a declared tool: its schema, its confirmation requirement, and
// which dispatcher runs it.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Confirm     Confirm         `json:"confirm,omitempty"`
	// Target names the declared context this tool acts on, when it acts on
	// one. A computer-use tool always has one; an ordinary function need not.
	Target     string     `json:"target,omitempty"`
	Dispatcher Dispatcher `json:"-"`
}

// Registry is the declared action surface of a session.
type Registry struct {
	mu    sync.RWMutex
	specs map[string]ToolSpec
	order []string
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{specs: make(map[string]ToolSpec)}
}

// Declare adds a tool. Redeclaring a name replaces it, which is what a session
// update does when a client changes its tool list.
func (registry *Registry) Declare(spec ToolSpec) error {
	if strings.TrimSpace(spec.Name) == "" || strings.TrimSpace(spec.Description) == "" {
		return errors.New("tool declaration requires a name and description")
	}
	confirm, err := ParseConfirm(string(spec.Confirm))
	if err != nil {
		return fmt.Errorf("tool %q: %w", spec.Name, err)
	}
	spec.Confirm = confirm
	if len(spec.Parameters) == 0 || !json.Valid(spec.Parameters) {
		return fmt.Errorf("tool %q parameters must be valid JSON", spec.Name)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(spec.Parameters, &object); err != nil || object == nil {
		return fmt.Errorf("tool %q parameters must be one JSON object", spec.Name)
	}
	spec.Parameters = slices.Clone(spec.Parameters)

	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.specs[spec.Name]; !exists {
		registry.order = append(registry.order, spec.Name)
	}
	registry.specs[spec.Name] = spec
	return nil
}

// Replace declares a complete tool set, dropping anything not in it.
func (registry *Registry) Replace(specs []ToolSpec) error {
	replacement := NewRegistry()
	for _, spec := range specs {
		if err := replacement.Declare(spec); err != nil {
			return err
		}
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.specs, registry.order = replacement.specs, replacement.order
	return nil
}

// Lookup returns one declared tool.
func (registry *Registry) Lookup(name string) (ToolSpec, bool) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	spec, exists := registry.specs[name]
	return spec, exists
}

// Specs returns every declared tool in declaration order.
func (registry *Registry) Specs() []ToolSpec {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	result := make([]ToolSpec, 0, len(registry.order))
	for _, name := range registry.order {
		spec := registry.specs[name]
		spec.Parameters = slices.Clone(spec.Parameters)
		result = append(result, spec)
	}
	return result
}

// ToolsConfig configures dispatch.
type ToolsConfig struct {
	Registry *Registry
	Ledger   *Ledger
	// Store is the canonical trajectory. Dispatch reads it to verify that a
	// call was committed with execution authority, which is why authority
	// cannot be granted by whoever is calling Dispatch.
	Store *trajectory.Store
	// Confirmer authorizes actions whose declared requirement calls for it.
	// Nil means DenyAll.
	Confirmer Confirmer
	// Policy answers the "policy" confirmation requirement. Nil means the
	// requirement is treated as "always", which is the safe reading of a
	// deployment that declared a policy and then supplied none.
	Policy PolicyDecision
	// Audit receives every dispatch attempt and outcome. Every executed action
	// is already a trajectory item with causal parents; this is the
	// operational mirror of that, for a deployment that wants one.
	Audit func(Record)
}

// Record is one dispatch attempt.
type Record struct {
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Target    string `json:"target,omitempty"`
	Confirmed bool   `json:"confirmed"`
	Executed  bool   `json:"executed"`
	Error     string `json:"error,omitempty"`
	ElapsedNS uint64 `json:"elapsed_ns,omitempty"`
}

// Tools dispatches authoritative calls through one boundary.
type Tools struct {
	config ToolsConfig

	mu       sync.Mutex
	results  map[string]trajectory.ToolResult
	inFlight map[string]struct{}
}

// NewTools creates the dispatch path.
func NewTools(config ToolsConfig) (*Tools, error) {
	if config.Registry == nil {
		return nil, errors.New("tool dispatch requires a registry")
	}
	if config.Ledger == nil {
		return nil, errors.New("tool dispatch requires the irreversibility ledger")
	}
	if config.Store == nil {
		return nil, errors.New("tool dispatch requires the canonical trajectory")
	}
	if config.Confirmer == nil {
		config.Confirmer = DenyAll{}
	}
	return &Tools{
		config: config, results: make(map[string]trajectory.ToolResult),
		inFlight: make(map[string]struct{}),
	}, nil
}

// Dispatch executes one call after checking that it has authority and, where
// declared, confirmation.
//
// Idempotency is enforced here rather than hoped for: a call ID that has
// already produced a result returns that result without touching the
// dispatcher again.
func (tools *Tools) Dispatch(ctx context.Context, call trajectory.ToolCall) (trajectory.ToolResult, error) {
	spec, declared := tools.config.Registry.Lookup(call.Name)
	if !declared {
		return trajectory.ToolResult{}, fmt.Errorf("%w: %s", ErrUnknownTool, call.Name)
	}
	if !tools.authoritative(call) {
		return trajectory.ToolResult{}, fmt.Errorf("%w: %s", ErrNoAuthority, call.CallID)
	}

	tools.mu.Lock()
	if result, done := tools.results[call.CallID]; done {
		tools.mu.Unlock()
		return result, nil
	}
	if _, running := tools.inFlight[call.CallID]; running {
		tools.mu.Unlock()
		return trajectory.ToolResult{}, fmt.Errorf("%w: %s is already dispatching", ErrDuplicateCall, call.CallID)
	}
	tools.inFlight[call.CallID] = struct{}{}
	tools.mu.Unlock()
	defer func() {
		tools.mu.Lock()
		delete(tools.inFlight, call.CallID)
		tools.mu.Unlock()
	}()

	kind := KindToolCall
	if strings.HasPrefix(call.Name, "computer.") {
		kind = KindComputerAction
	}
	commitmentID := "action_" + call.CallID
	if err := tools.config.Ledger.Prepare(Commitment{
		ID: commitmentID, Kind: kind, CallID: call.CallID, Confirm: spec.Confirm,
	}); err != nil && !errors.Is(err, ErrDuplicateCall) {
		return trajectory.ToolResult{}, err
	}

	confirmed, err := tools.confirm(ctx, call, spec)
	if err != nil {
		tools.record(Record{CallID: call.CallID, Name: call.Name, Target: spec.Target, Error: err.Error()})
		_, _ = tools.config.Ledger.Cancel(commitmentID, err.Error())
		return trajectory.ToolResult{}, err
	}
	if !confirmed {
		tools.record(Record{CallID: call.CallID, Name: call.Name, Target: spec.Target, Error: ErrNotConfirmed.Error()})
		_, _ = tools.config.Ledger.Cancel(commitmentID, "not confirmed")
		return trajectory.ToolResult{}, fmt.Errorf("%w: %s", ErrNotConfirmed, call.Name)
	}

	if err := tools.config.Ledger.Queue(commitmentID); err != nil && !errors.Is(err, ErrInvalidTransition) {
		return trajectory.ToolResult{}, err
	}
	dispatcher := spec.Dispatcher
	if dispatcher == nil {
		return trajectory.ToolResult{}, fmt.Errorf("tool %q has no dispatcher", call.Name)
	}
	// The crossing is recorded before the call runs. A dispatcher that starts
	// a side effect and then fails has still crossed the boundary, and a
	// ledger that only learned about successes would say otherwise.
	if err := tools.config.Ledger.Emit(commitmentID); err != nil {
		return trajectory.ToolResult{}, err
	}
	result, dispatchErr := dispatcher.Dispatch(ctx, call)
	if dispatchErr != nil {
		result = trajectory.ToolResult{CallID: call.CallID, Name: call.Name, Error: dispatchErr.Error()}
	}
	if result.CallID != call.CallID || result.Name != call.Name {
		mismatch := fmt.Errorf("tool %q returned mismatched identity", call.Name)
		_ = tools.config.Ledger.Complete(commitmentID, 0)
		tools.record(Record{CallID: call.CallID, Name: call.Name, Target: spec.Target, Confirmed: true, Executed: true, Error: mismatch.Error()})
		return trajectory.ToolResult{}, mismatch
	}
	_ = tools.config.Ledger.Complete(commitmentID, 0)
	tools.mu.Lock()
	tools.results[call.CallID] = result
	tools.mu.Unlock()
	tools.record(Record{
		CallID: call.CallID, Name: call.Name, Target: spec.Target,
		Confirmed: true, Executed: true, Error: result.Error,
	})
	return result, nil
}

// DispatchAll runs a complete call batch, preserving call order in the
// results. A batch that fails part way still returns what it completed: those
// effects happened and the trajectory must learn about them.
func (tools *Tools) DispatchAll(ctx context.Context, calls []trajectory.ToolCall) ([]trajectory.ToolResult, error) {
	results := make([]trajectory.ToolResult, 0, len(calls))
	for _, call := range calls {
		result, err := tools.Dispatch(ctx, call)
		if err != nil {
			// A refused or unauthorized call is still an outcome the model
			// must see: silence would leave the call unsatisfied forever.
			results = append(results, trajectory.ToolResult{
				CallID: call.CallID, Name: call.Name, Error: err.Error(),
			})
			continue
		}
		results = append(results, result)
	}
	return results, nil
}

func (tools *Tools) confirm(ctx context.Context, call trajectory.ToolCall, spec ToolSpec) (bool, error) {
	switch spec.Confirm {
	case ConfirmNever:
		return true, nil
	case ConfirmPolicy:
		if tools.config.Policy == nil {
			break
		}
		if tools.config.Policy(call) {
			return true, nil
		}
	}
	return tools.config.Confirmer.Confirm(ctx, ConfirmationRequest{
		Call: call, Confirm: spec.Confirm, Target: spec.Target,
	})
}

// authoritative reports whether the call was committed to the trajectory as an
// executable call. A tool_proposal never satisfies this, which is how the fast
// provider's structural inability to act is enforced at the point of effect
// rather than by convention.
func (tools *Tools) authoritative(call trajectory.ToolCall) bool {
	for _, item := range tools.config.Store.Snapshot().Items {
		if item.Kind != trajectory.KindToolCall || item.ToolCall == nil {
			continue
		}
		if item.ToolCall.CallID == call.CallID && item.ToolCall.Name == call.Name {
			return true
		}
	}
	return false
}

func (tools *Tools) record(record Record) {
	if tools.config.Audit != nil {
		tools.config.Audit(record)
	}
}
