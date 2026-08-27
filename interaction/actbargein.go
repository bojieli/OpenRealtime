package interaction

import (
	"context"
	"errors"
	"time"
)

// ActBargeInOptions configures overlap decisions owned by the whole-act
// interaction model.
type ActBargeInOptions struct {
	// Timeout bounds one model decision. Overlap is a live moment; a verdict
	// which arrives after either speaker stopped is no longer actionable.
	Timeout time.Duration
}

// NewActBargeIn gives overlap cancellation to the same enumerated-act policy
// which owns the rest of full interaction. At acoustic onset there are no
// words to classify, so InteractionModel returns inertia (keep-speaking). Once
// transcript evidence arrives it chooses keep-speaking or stop-speaking from
// the ordinary situation vocabulary.
func NewActBargeIn(model *InteractionModel, options ActBargeInOptions) (BargeIn, error) {
	if model == nil {
		return nil, errors.New("act barge-in requires an interaction model")
	}
	if options.Timeout <= 0 {
		options.Timeout = model.DecisionTimeout()
		if options.Timeout <= 0 {
			options.Timeout = 150 * time.Millisecond
		}
	}
	return &actBargeIn{model: model, timeout: options.Timeout}, nil
}

type actBargeIn struct {
	model   *InteractionModel
	timeout time.Duration
}

func (policy *actBargeIn) Name() string { return "act:" + policy.model.Name() }

func (policy *actBargeIn) Decide(input BargeInInput) BargeInDecision {
	if input.Context.Situation == nil {
		return BargeInDecision{Reason: "interaction situation is not available"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), policy.timeout)
	defer cancel()
	act, _, err := policy.model.Decide(ctx, *input.Context.Situation)
	if err != nil {
		return BargeInDecision{Reason: "interaction model unavailable; keep speaking"}
	}
	if act == ActStopSpeaking {
		return BargeInDecision{Cancel: true, Reason: "the interaction model chose stop-speaking"}
	}
	return BargeInDecision{Reason: "the interaction model chose " + string(act)}
}
