package interaction

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/session"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// Deferral decides whether committed work may be acted upon now.
//
// It is the conditional half of the event loop's invariant. Deferring never
// discards: the events stay committed, and the wake-up that this policy's
// condition implies is wired automatically by Bind, so a new deferral
// condition cannot be added without its wake-up.
type Deferral interface {
	Named
	// Admit reports whether a run may start now, and why not when it may not.
	Admit(Waiting) (bool, string)
	// Conditions lists the duplex transitions that can clear this policy's
	// deferrals. Bind subscribes to exactly these, which is what makes "every
	// deferral has a wake-up" structural rather than a discipline.
	Conditions() []session.TransitionKind
}

// Waiting is what a deferral policy decides about.
type Waiting struct {
	Duplex session.Snapshot `json:"duplex"`
	// Observation, ToolResult, and Repair report what the committed batch
	// contains. A policy decides on kinds, never on content.
	Observation bool `json:"observation"`
	ToolResult  bool `json:"tool_result"`
	Repair      bool `json:"repair"`
	// Parallel marks a batch the loop may run alongside work in flight.
	Parallel bool `json:"parallel"`
	// Backpressure is the deployment's optional signal that providers are
	// under load. It is unconstrained by default: throttling a conversation to
	// save tokens is a deployment decision, not something a runtime assumes.
	Backpressure bool `json:"backpressure,omitempty"`
}

// WaitingFrom reduces a committed batch to what a deferral policy may see.
func WaitingFrom(batch eventloop.Batch, state session.Snapshot) Waiting {
	return Waiting{
		Duplex:      state,
		Observation: batch.Contains(trajectory.KindObservation),
		ToolResult:  batch.Contains(trajectory.KindToolResult),
		Repair:      batch.Contains(trajectory.KindRepair),
		Parallel:    batch.Triage == eventloop.TriageParallel,
	}
}

// DeferralOptions configures the shipped duplex-state policy.
type DeferralOptions struct {
	// AllowWhileUserSpeaking runs immediately even while the user is talking.
	// The default is to wait: answering into someone's sentence is worse than
	// answering a moment later, and the endpoint will start the run.
	AllowWhileUserSpeaking bool
	// AllowWhileAgentSpeaking runs immediately even while agent audio is
	// reaching the user. The default is to wait for playback to complete.
	AllowWhileAgentSpeaking bool
	// DeferUnderBackpressure adds provider load as a deferral condition. It is
	// off by default and left to the deployment.
	DeferUnderBackpressure bool
}

type duplexDeferral struct {
	options DeferralOptions
}

// NewDuplexDeferral defers by duplex state: not while the user is speaking,
// not while the agent is being heard.
func NewDuplexDeferral(options DeferralOptions) Deferral {
	return duplexDeferral{options: options}
}

func (policy duplexDeferral) Name() string {
	parts := []string{"duplex"}
	if policy.options.AllowWhileUserSpeaking {
		parts = append(parts, "allow-over-user")
	}
	if policy.options.AllowWhileAgentSpeaking {
		parts = append(parts, "allow-over-agent")
	}
	if policy.options.DeferUnderBackpressure {
		parts = append(parts, "backpressure")
	}
	return strings.Join(parts, "+")
}

func (policy duplexDeferral) Conditions() []session.TransitionKind {
	conditions := make([]session.TransitionKind, 0, 2)
	if !policy.options.AllowWhileUserSpeaking {
		conditions = append(conditions, session.UserSpeechStopped)
	}
	if !policy.options.AllowWhileAgentSpeaking {
		conditions = append(conditions, session.AgentAudioStopped)
	}
	return conditions
}

func (policy duplexDeferral) Admit(waiting Waiting) (bool, string) {
	// A parallel batch is admitted by construction: the loop only offers one
	// when it can be handled without disturbing the work in flight, and making
	// it wait for silence would defeat the branch entirely.
	if waiting.Parallel {
		return true, ""
	}
	if policy.options.DeferUnderBackpressure && waiting.Backpressure {
		return false, "provider backpressure"
	}
	if waiting.Duplex.UserSpeaking && !policy.options.AllowWhileUserSpeaking {
		return false, "user is speaking"
	}
	if waiting.Duplex.AgentSpeaking && !policy.options.AllowWhileAgentSpeaking {
		return false, "agent audio is reaching the user"
	}
	return true, ""
}

// AlwaysRun admits everything immediately. It is correct for a runtime with no
// audio at all - a text client, or a benchmark harness driving turns - where
// there is nothing to talk over.
type AlwaysRun struct{}

func (AlwaysRun) Name() string                         { return "always" }
func (AlwaysRun) Admit(Waiting) (bool, string)         { return true, "" }
func (AlwaysRun) Conditions() []session.TransitionKind { return nil }

// Gate adapts a Deferral policy to the event loop, and wires the wake-ups the
// policy's own conditions imply.
type Gate struct {
	policy      Deferral
	duplex      Duplexer
	waker       Waker
	unsubscribe func()

	mu           sync.Mutex
	backpressure bool
}

// Waker is the part of the event loop a gate needs: something to tell that a
// deferral condition has cleared.
type Waker interface {
	Wake(reason string)
}

// Duplexer is the part of the session core a gate needs.
type Duplexer interface {
	Snapshot() session.Snapshot
	Observe(session.Observer) func()
}

// Bind connects a deferral policy to the duplex state and the event loop.
//
// It subscribes to exactly the transitions the policy declared, so a policy
// that defers on a condition it never named would deadlock loudly in tests
// rather than silently in production - which is the failure this arrangement
// exists to make impossible.
func Bind(policy Deferral, duplex Duplexer, waker Waker) (*Gate, error) {
	if policy == nil {
		return nil, fmt.Errorf("deferral policy is required")
	}
	if duplex == nil {
		return nil, fmt.Errorf("deferral gate requires the duplex state")
	}
	if waker == nil {
		return nil, fmt.Errorf("deferral gate requires a waker")
	}
	gate := &Gate{policy: policy, duplex: duplex, waker: waker}
	wanted := make(map[session.TransitionKind]struct{}, 4)
	for _, condition := range policy.Conditions() {
		wanted[condition] = struct{}{}
	}
	if len(wanted) > 0 {
		gate.unsubscribe = duplex.Observe(func(transition session.Transition) {
			if _, matched := wanted[transition.Kind]; !matched {
				return
			}
			waker.Wake(transition.Reason())
		})
	}
	return gate, nil
}

// Close removes the gate's subscription.
func (gate *Gate) Close() {
	if gate.unsubscribe != nil {
		gate.unsubscribe()
		gate.unsubscribe = nil
	}
}

// SetBackpressure reports provider load. Relief wakes deferred work, which is
// the wake-up that the backpressure condition owes.
func (gate *Gate) SetBackpressure(under bool) {
	gate.mu.Lock()
	changed := gate.backpressure != under
	gate.backpressure = under
	gate.mu.Unlock()
	if changed && !under {
		gate.waker.Wake("provider backpressure relieved")
	}
}

// Name reports the bound policy.
func (gate *Gate) Name() string { return gate.policy.Name() }

// AdmitRun implements eventloop.Gate.
func (gate *Gate) AdmitRun(_ context.Context, batch eventloop.Batch) (bool, string) {
	waiting := WaitingFrom(batch, gate.duplex.Snapshot())
	gate.mu.Lock()
	waiting.Backpressure = gate.backpressure
	gate.mu.Unlock()
	return gate.policy.Admit(waiting)
}

var _ eventloop.Gate = (*Gate)(nil)
