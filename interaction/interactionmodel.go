package interaction

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

// InertialAct is what an agent does when nothing has changed: it carries on.
//
// Inertia is the default for every state, which is what makes it a rule rather
// than a table of special cases. An agent mid-sentence keeps talking, a silent
// one stays silent, one deliberating keeps deliberating. Nothing about the
// agent's own situation ever makes acting necessary; only evidence does.
func InertialAct(state Situation) Act {
	if state.AgentSpeaking {
		return ActKeepSpeaking
	}
	return ActStaySilent
}

// Decidable reports whether there is anything here to decide on.
//
// A model is never asked a question it cannot answer. When somebody starts
// making noise the recogniser has produced nothing yet, and acoustic onset by
// itself does not distinguish a correction from a cough from the next item on
// a phone menu - so there is no answer to give, and the honest response is to
// carry on rather than to guess.
//
// This is also why a door slam no longer interrupts the agent. A barge-in
// triggered by voice activity has to guess; one triggered by content does not,
// because noise produces no content and so produces no decision.
func (state Situation) Decidable() bool {
	// Quiet is the one input that is not an arrival, and it is admitted only
	// where somebody has asked for something that happens on its own. See
	// Situation.Quiet: a policy about time makes the passing of time evidence,
	// and without such a policy the quiet decides nothing.
	// A frame the model can look at is evidence in its own right. Requiring a
	// description first is what put a cloud narration on the critical path of
	// every visual turn, and it is the one input here that does not need
	// turning into words before it can be judged.
	return state.Seen != "" || len(state.Seeing) > 0 || v1.CarriesSpeech(state.Heard) ||
		(state.Quiet && len(state.Pins) > 0)
}

// InteractionModel decides what an agent does in an instant.
//
// It replaces four separate predicates - endpointing, backchannel, turn
// projection, and overlap - which each answered one narrow question from one
// narrow input and could not disagree coherently because none of them saw the
// whole question. More importantly, none of them could read the conversation,
// which meant no policy anyone stated out loud could reach them.
type InteractionModel struct {
	decider Decider
}

// NewInteractionModel builds the policy over a decider.
func NewInteractionModel(decider Decider) (*InteractionModel, error) {
	if decider == nil {
		return nil, fmt.Errorf("an interaction model requires a decider")
	}
	return &InteractionModel{decider: decider}, nil
}

func (model *InteractionModel) Name() string { return "model:" + model.decider.Name() }

// DecisionTimeout reports the decider's own live deadline when it declares
// one. Zero means the caller did not expose a bound; evidence retains that as
// unknown rather than inventing a default.
func (model *InteractionModel) DecisionTimeout() time.Duration {
	if bounded, ok := model.decider.(interface{ DecisionTimeout() time.Duration }); ok {
		return bounded.DecisionTimeout()
	}
	return 0
}

// Decide chooses one act.
//
// The cheap answer comes first and gives it most of the time: with no evidence
// there is nothing to decide, and skipping the call is free rather than a
// tuning choice, because the input the model would have seen is the same input
// it saw last time. What survives is asked.
func (model *InteractionModel) Decide(ctx context.Context, state Situation) (Act, Outcome, error) {
	if !state.Decidable() {
		return InertialAct(state), Outcome{Option: string(InertialAct(state))}, nil
	}
	acts := state.AvailableActs()
	options := make([]string, len(acts))
	for index, act := range acts {
		options[index] = string(act)
	}
	outcome, err := model.decider.Decide(ctx, Decision{
		Prompt: Instruction, Options: options, Evidence: state.Render(),
		Images: state.Seeing,
	})
	if err != nil {
		// A decision that could not be taken is not a decision to do something
		// drastic. Falling back to inertia keeps a failing policy model silent
		// rather than making it interrupt people.
		return InertialAct(state), Outcome{}, err
	}
	chosen := Act(strings.TrimSpace(outcome.Option))
	for _, act := range acts {
		if act == chosen {
			return chosen, outcome, nil
		}
	}
	return InertialAct(state), outcome, fmt.Errorf("interaction model chose %q, which is not available here", outcome.Option)
}
