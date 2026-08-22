package interaction

import (
	"errors"
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
	// StepVoice runs the fast provider for the sole purpose of voicing what
	// slow has already committed. It exists because slow cannot speak: the
	// short extra hop is what buys the removal of the race where slow
	// contradicts something fast already said.
	StepVoice StepKind = "voice"
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
type Cause struct {
	Observation bool `json:"observation"`
	ToolResult  bool `json:"tool_result"`
	// PendingRepair is set when the batch left an unresolved audible-repair
	// obligation, which is a reason to run slow even with nothing else new.
	PendingRepair bool `json:"pending_repair"`
	// SlowCommitted is set when the previous step in this turn was a slow
	// continuation that produced assistant content nobody has voiced yet.
	SlowCommitted bool `json:"slow_committed"`
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
	// VoiceSlowOutput runs a fast step to voice what slow committed. Turning
	// it off is what "slow-only, written" looks like, which is a legitimate
	// configuration for a text client and a useful control condition.
	VoiceSlowOutput bool
}

type fastThenSlowRollout struct {
	options RolloutOptions
}

// NewFastThenSlowRollout is the reference arrangement: fast answers now, slow
// reasons and acts, and a fast step voices what slow produced.
func NewFastThenSlowRollout(options RolloutOptions) Rollout {
	if options.MaxSlowInvocations <= 0 {
		options.MaxSlowInvocations = 8
	}
	options.VoiceSlowOutput = true
	return fastThenSlowRollout{options: options}
}

func (rollout fastThenSlowRollout) Name() string {
	name := "fast+slow"
	if rollout.options.ToolResultProgress {
		name += "+progress"
	}
	return name
}

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
	if input.Cause.SlowCommitted {
		return []Step{{Kind: StepVoice, Reason: "voice the slow continuation"}}
	}
	var steps []Step
	if input.Cause.Observation {
		steps = append(steps, Step{Kind: StepFast, Reason: "answer now"})
	}
	if input.Cause.ToolResult && rollout.options.ToolResultProgress {
		steps = append(steps, Step{Kind: StepFast, Reason: "report progress"})
	}
	if input.Cause.SlowInvocations >= rollout.options.MaxSlowInvocations {
		return steps
	}
	if input.Cause.Observation || input.Cause.ToolResult || input.Cause.PendingRepair {
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

// NewEndpointedSlowOnlyRollout runs only the slow provider and voices its
// output. It is what a conventional agent does: correct, and with the dead air
// that this project exists to remove.
func NewEndpointedSlowOnlyRollout(options RolloutOptions) Rollout {
	if options.MaxSlowInvocations <= 0 {
		options.MaxSlowInvocations = 8
	}
	// The voice step is the only way anything this rollout produces reaches
	// the client: the slow provider is silent by construction, so its answer
	// is committed to the trajectory and delivered by the fast step that reads
	// it back. Without that step there is no output path at all - not quieter
	// speech, no response - and a client that asked for one waits forever.
	//
	// Forced rather than defaulted, exactly as the fast+slow rollout forces
	// it, because no caller has a reason for a rollout whose entire output is
	// discarded. Tool execution is unaffected either way: the plan only
	// consults this once slow has committed text.
	options.VoiceSlowOutput = true
	return slowOnlyRollout{options: options}
}

func (slowOnlyRollout) Name() string { return "endpointed-slow-only" }

func (rollout slowOnlyRollout) Plan(input RolloutInput) []Step {
	if input.Cause.Parallel {
		return nil
	}
	if input.Cause.SlowCommitted {
		if !rollout.options.VoiceSlowOutput {
			return nil
		}
		return []Step{{Kind: StepVoice, Reason: "voice the slow continuation"}}
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

// ErrSlowMaySpeak reports a provider arrangement that would let the slow
// provider be heard directly. It is returned by ValidateArrangement rather
// than being silently corrected: a runtime that quietly muted a provider would
// be running a different arrangement than the one it was configured with.
var ErrSlowMaySpeak = errors.New("slow provider must not have voice authority")

// ErrFastMayExecute reports a fast provider that could cause a side effect.
var ErrFastMayExecute = errors.New("fast provider must not have executable-tool authority")
