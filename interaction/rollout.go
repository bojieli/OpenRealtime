package interaction

import (
	"fmt"
	"strings"

	"github.com/bojieli/OpenRealtime/trajectory"
)

// StepKind names what a rollout step asks the runtime to do.
type StepKind string

const (
	// StepFast runs the fast cognition provider. Its output is what the user
	// hears, and it is the only step that speaks.
	StepFast StepKind = "fast"
	// StepSlow runs the slow cognition provider. Its output appends and is
	// never voiced directly.
	StepSlow StepKind = "slow"
)

// The signals a cognition phase raises when it finishes.
//
// Each opens a safe point so the loop reconsiders, and neither appends
// anything: what they refer to is already in the trajectory. Travelling as
// events is the whole point - it is what puts the decision of when to act back
// in the one place that owns it, instead of in whichever step happened to
// finish.
const (
	// SignalEscalated says the fast turn handed the work on. What it starts is
	// deliberation, which is never heard, so the gate admits it at once.
	SignalEscalated = "cognition.escalated"
	// SignalBackgroundResult says a slow continuation finished and left a
	// result nobody has spoken from. What it starts is a spoken turn, so it
	// passes the gate exactly as any other spoken turn does.
	SignalBackgroundResult = "cognition.background_result"
)

// Step is one action in a rollout plan.
type Step struct {
	Kind   StepKind `json:"kind"`
	Reason string   `json:"reason,omitempty"`
}

// Phase is the cognition phase a step invokes.
func (step Step) Phase() trajectory.Phase {
	if step.Kind == StepSlow {
		return trajectory.PhaseSlow
	}
	return trajectory.PhaseFast
}

// Cause is what opened the rollout: what arrived at the safe point.
//
// Every field is a fact about the committed batch, never about content. One
// batch causes at most one spoken turn and at most one deliberation, and
// anything either of them produces arrives later as its own batch - which is
// what keeps this a policy over events rather than a private loop.
type Cause struct {
	Observation bool `json:"observation"`
	ToolResult  bool `json:"tool_result"`
	// PendingRepair is set when the batch left an unresolved audible-repair
	// obligation, which is a reason to deliberate even with nothing else new.
	PendingRepair bool `json:"pending_repair"`
	// Escalated is set when the fast turn handed the work on: it named a
	// capability it cannot execute, or said the turn needs deliberation.
	//
	// It is why slow does not run on every observation. A turn fast can answer
	// outright is answered once, and the failure where a simple question is
	// processed twice - and heard twice - cannot arise.
	Escalated bool `json:"escalated"`
	// BackgroundResult is set when a slow continuation finished and left a
	// written result nobody has spoken from yet.
	BackgroundResult bool `json:"background_result"`
	// SlowInvocations is how many slow continuations this turn has already
	// run, so a rollout can stop rather than loop.
	SlowInvocations int `json:"slow_invocations"`
	// Parallel marks a batch the loop admitted alongside work in flight. A
	// rollout must keep such a plan short and must not touch tools.
	Parallel bool `json:"parallel"`
}

// RolloutInput is a decision instant plus what caused it.
type RolloutInput struct {
	Context
	Cause Cause
}

// Rollout decides when each cognition provider fires, whether slow runs at
// all, and what fills the gap.
//
// The providers themselves are cognition; this arrangement of them is
// interaction. The distinction is not cosmetic: this is exactly what factor F2
// varies, and a policy welded into the engine could not be varied at all.
type Rollout interface {
	Named
	Plan(RolloutInput) []Step
}

// RolloutOptions configures the shipped fast/slow arrangement.
type RolloutOptions struct {
	// MaxSlowInvocations bounds slow continuations per turn. Zero selects 8.
	MaxSlowInvocations int
	// ToolResultProgress lets a completed tool result trigger a short fast
	// utterance. It is not a second answer - it is a status report, and the
	// fast provider can give one because it shares the trajectory and knows
	// what came back. Whether it does is one of the levers F2 varies.
	ToolResultProgress bool
}

type fastThenSlowRollout struct {
	options RolloutOptions
}

// NewFastThenSlowRollout is the reference arrangement: fast answers now, and
// deliberates only when fast asks it to.
func NewFastThenSlowRollout(options RolloutOptions) Rollout {
	if options.MaxSlowInvocations <= 0 {
		options.MaxSlowInvocations = 8
	}
	return fastThenSlowRollout{options: options}
}

func (rollout fastThenSlowRollout) Name() string {
	name := "fast+slow"
	if rollout.options.ToolResultProgress {
		name += "+progress"
	}
	return name
}

// Plan answers one committed batch, and never re-plans what it produces.
//
// There is exactly one kind of spoken turn, and the fast provider is it. A
// question is answered by a fast turn; a finished background result is spoken
// by a fast turn reading the same trajectory. Nothing here asks a provider to
// recite what another provider wrote, because a step that could would be a
// second answer to a question that already had one.
func (rollout fastThenSlowRollout) Plan(input RolloutInput) []Step {
	if input.Cause.Parallel {
		// A parallel branch answers the question that arrived and nothing
		// more. It must not start slow work: the point of the branch is that
		// the long work in flight is undisturbed.
		if input.Cause.Observation {
			return []Step{{Kind: StepFast, Reason: "parallel question"}}
		}
		return nil
	}
	var steps []Step
	switch {
	case input.Cause.Observation:
		steps = append(steps, Step{Kind: StepFast, Reason: "answer now"})
	case input.Cause.BackgroundResult:
		steps = append(steps, Step{Kind: StepFast, Reason: "the background reasoner finished"})
	case input.Cause.ToolResult && rollout.options.ToolResultProgress:
		steps = append(steps, Step{Kind: StepFast, Reason: "report progress"})
	}
	if input.Cause.SlowInvocations >= rollout.options.MaxSlowInvocations {
		return steps
	}
	// Slow runs when it was asked for, when a result it is waiting on came
	// back, or when a correction is owed. An observation alone is not a
	// reason: whether the turn needs deliberation is fast's to judge, and it
	// has just judged it.
	if input.Cause.Escalated || input.Cause.ToolResult || input.Cause.PendingRepair {
		steps = append(steps, Step{Kind: StepSlow, Reason: "reason and act"})
	}
	return steps
}

type fastOnlyRollout struct{}

// NewFastOnlyRollout runs the fast provider and nothing else. It is the
// control condition that isolates what the background reasoner is worth.
func NewFastOnlyRollout() Rollout { return fastOnlyRollout{} }

func (fastOnlyRollout) Name() string { return "fast-only" }

func (fastOnlyRollout) Plan(input RolloutInput) []Step {
	if input.Cause.Observation {
		return []Step{{Kind: StepFast, Reason: "answer now"}}
	}
	return nil
}

type slowOnlyRollout struct {
	options RolloutOptions
}

// NewEndpointedSlowOnlyRollout deliberates before saying anything. It is what
// a conventional agent does: correct, and with the dead air that this project
// exists to remove.
func NewEndpointedSlowOnlyRollout(options RolloutOptions) Rollout {
	if options.MaxSlowInvocations <= 0 {
		options.MaxSlowInvocations = 8
	}
	return slowOnlyRollout{options: options}
}

func (slowOnlyRollout) Name() string { return "endpointed-slow-only" }

func (rollout slowOnlyRollout) Plan(input RolloutInput) []Step {
	if input.Cause.Parallel {
		return nil
	}
	// The control condition differs in one place: an observation goes straight
	// to deliberation instead of being answered first. What speaks afterwards
	// is the same fast turn, for the same reason.
	if input.Cause.BackgroundResult {
		return []Step{{Kind: StepFast, Reason: "the background reasoner finished"}}
	}
	if input.Cause.SlowInvocations >= rollout.options.MaxSlowInvocations {
		return nil
	}
	if input.Cause.Observation || input.Cause.ToolResult || input.Cause.PendingRepair {
		return []Step{{Kind: StepSlow, Reason: "reason and act"}}
	}
	return nil
}

// ParseRollout resolves a configured level of factor F2.
func ParseRollout(value string, options RolloutOptions) (Rollout, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "fast-only":
		return NewFastOnlyRollout(), nil
	case "fast+slow", "fast-slow", "":
		return NewFastThenSlowRollout(options), nil
	case "endpointed-slow-only", "slow-only":
		return NewEndpointedSlowOnlyRollout(options), nil
	default:
		return nil, fmt.Errorf("rollout must be fast-only, fast+slow, or endpointed-slow-only, got %q", value)
	}
}
