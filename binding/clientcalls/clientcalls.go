// Package clientcalls tracks tool calls a client is executing.
//
// The Realtime protocol puts tool execution on the client: the server emits
// the call, the client runs it, and the result comes back as an ordinary
// conversation item. So between those two moments a session is holding an
// invocation it does not control, and the model is looking at a call with no
// result.
//
// A batch is committed only when every call in it has a result, because a
// partial batch would leave the model unable to tell whether the rest is
// coming. That is correct, and on its own it means a client that crashes,
// disconnects mid-call, or simply never answers leaves the invocation
// outstanding for the life of the session - the conversation keeps working,
// but that branch of it never advances again, and nothing anywhere says so.
//
// So an invocation has a deadline. When it passes, the calls that were never
// answered get results saying so, the batch commits, and the session carries
// on with a model that knows the tools failed rather than one still waiting.
// A timeout is not a good outcome, but it is an outcome, and a stuck
// invocation is not.
//
// Three bindings need this and had three copies of the bookkeeping. It is one
// package because the deadline is the part that is easy to get subtly wrong,
// and because a fix that lands in one copy of something is a fix that has not
// landed.
package clientcalls

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// DefaultTimeout bounds an invocation when a binding does not say otherwise.
//
// It matches the provider request timeout, because the two answer the same
// question - how long the session waits on something outside it before
// deciding it is not coming - and answering it differently in two places
// would be a distinction without a reason.
//
// A client with work that legitimately runs longer than this returns a result
// saying the work started and reports the outcome as a message when it
// finishes. That keeps the model's view accurate at every moment, which
// holding the batch open does not.
const DefaultTimeout = 2 * time.Minute

// Config configures a tracker.
type Config struct {
	// Timeout bounds one invocation. Zero selects DefaultTimeout; a negative
	// value disables the deadline, which is what a harness driving tool
	// results by hand wants and is never right for a session facing a real
	// client.
	Timeout time.Duration
	// Scheduler supplies the deadline. Nil selects the system clock.
	Scheduler clock.Scheduler
	// Commit appends a complete batch, in call order. It is called with no
	// lock held, so it may re-enter the tracker.
	Commit func(invocationID string, results []trajectory.ToolResult) error
	// Expired reports an invocation the client never finished answering,
	// after its synthesised results have been committed. It exists so the
	// session can tell the client what happened rather than leaving the
	// timeout as something only the model can see.
	Expired func(invocationID string, unanswered []trajectory.ToolCall)
}

// Tracker holds invocations a client has not finished answering.
type Tracker struct {
	config Config

	mu      sync.Mutex
	pending map[string]*invocation
	owner   map[string]string
	closed  bool
}

type invocation struct {
	calls      []trajectory.ToolCall
	results    map[string]trajectory.ToolResult
	dispatched bool
	deadline   clock.Timer
}

// New creates a tracker.
func New(config Config) (*Tracker, error) {
	if config.Commit == nil {
		return nil, errors.New("a client-call tracker requires somewhere to commit a completed batch")
	}
	if config.Timeout == 0 {
		config.Timeout = DefaultTimeout
	}
	if config.Scheduler == nil {
		config.Scheduler = clock.NewSystem()
	}
	return &Tracker{
		config:  config,
		pending: make(map[string]*invocation),
		owner:   make(map[string]string),
	}, nil
}

// Track registers an invocation and starts its deadline.
//
// Every call in the batch is registered, including any this process is
// executing itself: a split batch commits as one thing, so the local results
// wait alongside the client's and the deadline covers the whole of it.
func (tracker *Tracker) Track(invocationID string, calls []trajectory.ToolCall) error {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.closed {
		return errors.New("the session is closed")
	}
	if _, duplicate := tracker.pending[invocationID]; duplicate {
		return fmt.Errorf("duplicate tool invocation %q", invocationID)
	}
	pending := &invocation{
		calls:   append([]trajectory.ToolCall(nil), calls...),
		results: make(map[string]trajectory.ToolResult, len(calls)),
	}
	tracker.pending[invocationID] = pending
	for _, call := range calls {
		tracker.owner[call.CallID] = invocationID
	}
	if tracker.config.Timeout > 0 {
		pending.deadline = tracker.config.Scheduler.AfterFunc(tracker.config.Timeout, func() {
			tracker.expire(invocationID)
		})
	}
	return nil
}

// Hold records results this process produced for a split batch.
//
// They are not enough to complete anything on their own: the invocation is
// waiting on the client's half, and holding these is what lets both commit
// together when it arrives.
func (tracker *Tracker) Hold(invocationID string, results []trajectory.ToolResult) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	pending, exists := tracker.pending[invocationID]
	if !exists {
		return
	}
	for _, result := range results {
		pending.results[result.CallID] = result
	}
}

// Result records one client-executed result and commits the batch when it is
// the last one outstanding.
func (tracker *Tracker) Result(result trajectory.ToolResult) error {
	if strings.TrimSpace(result.CallID) == "" {
		return errors.New("a tool result requires a call ID")
	}
	tracker.mu.Lock()
	invocationID, known := tracker.owner[result.CallID]
	if !known {
		tracker.mu.Unlock()
		return fmt.Errorf("tool result references unknown call %q", result.CallID)
	}
	pending := tracker.pending[invocationID]
	if pending == nil {
		tracker.mu.Unlock()
		return fmt.Errorf("tool result references a completed invocation %q", invocationID)
	}
	if _, duplicate := pending.results[result.CallID]; duplicate {
		tracker.mu.Unlock()
		return fmt.Errorf("duplicate tool result for call %q", result.CallID)
	}
	for _, call := range pending.calls {
		if call.CallID == result.CallID {
			result.Name = call.Name
		}
	}
	pending.results[result.CallID] = result
	ordered, complete := pending.readyLocked()
	tracker.mu.Unlock()
	if !complete {
		return nil
	}

	if err := tracker.config.Commit(invocationID, ordered); err != nil {
		// The batch did not land, so the invocation is still outstanding and
		// a later result - or the deadline - must be able to try again.
		tracker.mu.Lock()
		if current := tracker.pending[invocationID]; current == pending {
			current.dispatched = false
		}
		tracker.mu.Unlock()
		return err
	}
	tracker.forget(invocationID, pending)
	return nil
}

// expire commits whatever the client did answer, plus a result for everything
// it did not, and reports what happened.
func (tracker *Tracker) expire(invocationID string) {
	tracker.mu.Lock()
	pending, exists := tracker.pending[invocationID]
	if !exists || pending.dispatched {
		tracker.mu.Unlock()
		return
	}
	var unanswered []trajectory.ToolCall
	for _, call := range pending.calls {
		if _, answered := pending.results[call.CallID]; answered {
			continue
		}
		unanswered = append(unanswered, call)
		pending.results[call.CallID] = trajectory.ToolResult{
			CallID: call.CallID, Name: call.Name,
			Error: fmt.Sprintf("the client did not return a result within %s", tracker.config.Timeout),
		}
	}
	ordered, complete := pending.readyLocked()
	tracker.mu.Unlock()
	if !complete {
		return
	}
	if err := tracker.config.Commit(invocationID, ordered); err != nil {
		tracker.mu.Lock()
		if current := tracker.pending[invocationID]; current == pending {
			current.dispatched = false
		}
		tracker.mu.Unlock()
		return
	}
	tracker.forget(invocationID, pending)
	if tracker.config.Expired != nil && len(unanswered) > 0 {
		tracker.config.Expired(invocationID, unanswered)
	}
}

// readyLocked reports the batch in call order once every call has a result,
// and claims it so two callers cannot commit the same invocation.
func (pending *invocation) readyLocked() ([]trajectory.ToolResult, bool) {
	if pending.dispatched || len(pending.results) != len(pending.calls) {
		return nil, false
	}
	pending.dispatched = true
	ordered := make([]trajectory.ToolResult, 0, len(pending.calls))
	for _, call := range pending.calls {
		ordered = append(ordered, pending.results[call.CallID])
	}
	return ordered, true
}

func (tracker *Tracker) forget(invocationID string, pending *invocation) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if current := tracker.pending[invocationID]; current != pending {
		return
	}
	if pending.deadline != nil {
		pending.deadline.Stop()
	}
	delete(tracker.pending, invocationID)
	for _, call := range pending.calls {
		delete(tracker.owner, call.CallID)
	}
}

// Close stops every outstanding deadline.
//
// It does not commit anything: the session is ending, and a batch appended to
// a trajectory nobody will read again is not worth the risk of running a
// commit against a runtime that is shutting down.
func (tracker *Tracker) Close() {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.closed = true
	for _, pending := range tracker.pending {
		if pending.deadline != nil {
			pending.deadline.Stop()
		}
	}
	clear(tracker.pending)
	clear(tracker.owner)
}

// Outstanding reports how many invocations are waiting on a client. It exists
// for tests and for operational visibility into a session that is stuck.
func (tracker *Tracker) Outstanding() int {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return len(tracker.pending)
}
