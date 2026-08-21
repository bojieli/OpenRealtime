package interaction

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// A policy model is a small, fast model with a short prompt and a constrained
// output, used for judgements about a live conversation that rule-based
// machinery gets brittle at: should the agent say "mm-hm" while the user is
// still talking, and is this turn about to end.
//
// The name deliberately avoids "interaction model", which in this project and
// in the wider literature means a full-duplex model such as Moshi.
//
// Three constraints keep it cheap and safe, and all three are enforced here
// rather than left to a prompt:
//
//  1. Enumerated output, never free generation. A policy model selects among
//     declared options, so it cannot become a third cognition provider.
//  2. No tool authority, ever. Policy models sit outside the proposal/execute
//     split because they never emit calls at all.
//  3. Decision-time information only. A turn-projection model may see only
//     what was available at the instant of the decision; prompted on
//     hindsight, it yields a judgement that cannot be reproduced online.

// BackchannelChoice is the enumerated output of a backchannel policy model.
type BackchannelChoice string

const (
	// BackchannelNone says nothing, which is the right answer most of the time.
	BackchannelNone BackchannelChoice = "none"
	// BackchannelAcknowledge emits a short continuer: mm-hm, right, I see.
	BackchannelAcknowledge BackchannelChoice = "acknowledge"
	// BackchannelAffirm emits a stronger agreement token.
	BackchannelAffirm BackchannelChoice = "affirm"
)

// BackchannelDecision is one enumerated choice plus the token to emit.
type BackchannelDecision struct {
	Choice BackchannelChoice `json:"choice"`
	Token  string            `json:"token,omitempty"`
	Reason string            `json:"reason,omitempty"`
}

// Backchannel decides whether to emit a continuer while the user is speaking.
//
// Emitting one is overlap the agent chose, so the decision belongs here and
// not in the speech path, and it degrades cleanly: with no policy model
// configured it is simply off, and the system runs - it is just less alive.
type Backchannel interface {
	Named
	Decide(context.Context, Context) (BackchannelDecision, error)
}

// NoBackchannel is the rule fallback: never interject.
type NoBackchannel struct{}

func (NoBackchannel) Name() string { return "off" }

func (NoBackchannel) Decide(context.Context, Context) (BackchannelDecision, error) {
	return BackchannelDecision{Choice: BackchannelNone, Reason: "no backchannel policy configured"}, nil
}

// Projection is the enumerated output of a turn-projection policy model.
type Projection struct {
	// Ending says the user's turn is about to end.
	Ending bool `json:"ending"`
	// Confidence is the model's own reported confidence in [0,1].
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason,omitempty"`
}

// TurnProjection anticipates the end of a user turn before silence confirms
// it, which is worth latency exactly as far as it is right.
type TurnProjection interface {
	Named
	Project(Context) Projection
}

// VADOnlyProjection is the rule fallback: never project, let silence decide.
type VADOnlyProjection struct{}

func (VADOnlyProjection) Name() string { return "vad-only" }

func (VADOnlyProjection) Project(Context) Projection {
	return Projection{Reason: "no turn projection policy configured"}
}

// Decider is the narrow contract a policy-model server implements: one
// enumerated choice from a short prompt over decision-time information only.
//
// It is deliberately not continuation.Provider. A policy model does not
// continue the trajectory, does not stream, and cannot emit a tool call, and
// giving it the provider interface would make all three possible by accident.
type Decider interface {
	Name() string
	// Decide returns the index of the chosen option. Implementations must
	// reject any output that is not one of the supplied options rather than
	// coercing it to the nearest match.
	Decide(ctx context.Context, request Decision) (Outcome, error)
}

// Decision is one enumerated question.
type Decision struct {
	// Prompt is the short instruction. It must describe only what was
	// available at the decision instant.
	Prompt string `json:"prompt"`
	// Options are the permitted answers, in a stable order.
	Options []string `json:"options"`
	// Evidence is the decision-time context, already rendered to text by the
	// caller so the policy model never touches the trajectory itself.
	Evidence string `json:"evidence,omitempty"`
}

// Validate rejects a malformed question before it reaches a model.
func (decision Decision) Validate() error {
	if strings.TrimSpace(decision.Prompt) == "" {
		return errors.New("policy decision requires a prompt")
	}
	if len(decision.Options) < 2 {
		return errors.New("policy decision requires at least two enumerated options")
	}
	seen := make(map[string]struct{}, len(decision.Options))
	for index, option := range decision.Options {
		if strings.TrimSpace(option) == "" {
			return fmt.Errorf("policy option %d is empty", index)
		}
		if _, duplicate := seen[option]; duplicate {
			return fmt.Errorf("duplicate policy option %q", option)
		}
		seen[option] = struct{}{}
	}
	return nil
}

// Outcome is the chosen option and the model's confidence in it.
type Outcome struct {
	Index      int     `json:"index"`
	Option     string  `json:"option"`
	Confidence float64 `json:"confidence"`
	ElapsedNS  uint64  `json:"elapsed_ns,omitempty"`
}
