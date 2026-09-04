package interaction

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// TranscriptEventKind names where a streaming transcript is in its lifecycle.
//
// A partial is a live hypothesis: later audio may extend or revise it. A final
// is the recogniser's terminal account of the utterance. Keeping that fact in
// the decision value, instead of asking a model to infer it from punctuation
// or silence, is the defining difference between this policy and the ordinary
// interaction path.
type TranscriptEventKind string

const (
	TranscriptPartial TranscriptEventKind = "partial"
	TranscriptFinal   TranscriptEventKind = "final"
)

func (kind TranscriptEventKind) validate() error {
	switch kind {
	case TranscriptPartial, TranscriptFinal:
		return nil
	default:
		return fmt.Errorf("transcript event must be partial or final, got %q", kind)
	}
}

// TranscriptEventRules is the policy for one kind of transcript event.
//
// Instruction is intentionally configuration, not a package constant. A live
// hypothesis and a settled utterance present different observation spaces and
// therefore need independently replaceable rules. Acts is an independent
// hard boundary around the model's enumerated output: a prompt cannot make an
// act available that the deployment did not permit for this event.
type TranscriptEventRules struct {
	Instruction string
	Acts        []Act
	Timeout     time.Duration
}

// TranscriptEventOptions configures the two event-specific policies.
type TranscriptEventOptions struct {
	Partial TranscriptEventRules
	Final   TranscriptEventRules
}

// TranscriptEventPolicy is the opt-in interaction path for streaming ASR.
//
// It is separate from InteractionModel rather than a branch inside it. The
// existing model and its tuned instruction therefore see exactly the same
// evidence and prompt unless a deployment explicitly selects this policy.
type TranscriptEventPolicy struct {
	decider Decider
	partial TranscriptEventRules
	final   TranscriptEventRules
}

// NewTranscriptEventPolicy validates and builds an event-aware policy.
func NewTranscriptEventPolicy(
	decider Decider, options TranscriptEventOptions,
) (*TranscriptEventPolicy, error) {
	if decider == nil {
		return nil, errors.New("a transcript-event policy requires a decider")
	}
	if err := ValidateTranscriptEventOptions(options); err != nil {
		return nil, err
	}
	partial, err := validateTranscriptRules(TranscriptPartial, options.Partial)
	if err != nil {
		return nil, err
	}
	final, err := validateTranscriptRules(TranscriptFinal, options.Final)
	if err != nil {
		return nil, err
	}
	return &TranscriptEventPolicy{decider: decider, partial: partial, final: final}, nil
}

// ValidateTranscriptEventOptions checks an event policy without opening or
// retaining a model client. Deployment configuration uses it before a live
// Decider is available; NewTranscriptEventPolicy repeats the check at the
// runtime boundary so configuration and execution cannot drift.
func ValidateTranscriptEventOptions(options TranscriptEventOptions) error {
	if _, err := validateTranscriptRules(TranscriptPartial, options.Partial); err != nil {
		return err
	}
	if _, err := validateTranscriptRules(TranscriptFinal, options.Final); err != nil {
		return err
	}
	return nil
}

func (policy *TranscriptEventPolicy) Name() string {
	return "transcript-events:" + policy.decider.Name()
}

// DecisionTimeout reports the configured deadline for one event kind.
func (policy *TranscriptEventPolicy) DecisionTimeout(kind TranscriptEventKind) time.Duration {
	rules, ok := policy.rules(kind)
	if !ok {
		return 0
	}
	return rules.Timeout
}

// AllowedActs returns the configured hard act boundary for one event kind.
// The clone lets a caller render and retain the exact decision evidence without
// gaining a handle that could mutate the policy used by later events.
func (policy *TranscriptEventPolicy) AllowedActs(kind TranscriptEventKind) []Act {
	rules, ok := policy.rules(kind)
	if !ok {
		return nil
	}
	return slices.Clone(rules.Acts)
}

// Decide chooses an act under the rules for exactly this event kind.
func (policy *TranscriptEventPolicy) Decide(
	ctx context.Context, kind TranscriptEventKind, state Situation,
) (Act, Outcome, error) {
	rules, ok := policy.rules(kind)
	if !ok {
		return InertialAct(state), Outcome{}, kind.validate()
	}
	state.TranscriptEvent = kind
	state.AllowedActs = slices.Clone(rules.Acts)
	acts := state.AvailableActs()
	if len(acts) == 0 {
		return InertialAct(state), Outcome{}, fmt.Errorf(
			"the %s transcript rules leave no act available in this situation", kind)
	}
	if len(acts) == 1 {
		return acts[0], Outcome{Index: 0, Option: string(acts[0])}, nil
	}
	if rules.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, rules.Timeout)
		defer cancel()
	}
	// Active output presents a narrower decision than a general transcript
	// event: is this speech directed at the agent, a listener backchannel, or
	// speech to somebody else? Reuse the model-backed overlap classifier for
	// that distinction. Its dedicated prompt proved materially more reliable
	// on incomplete vocatives and floor-taking openers than asking the broader
	// event prompt to rediscover the same taxonomy. Deliberately triggered
	// output remains on the event policy because only it sees the protected
	// same-stream continuation contract.
	if state.AgentSpeaking && !state.AgentOutputProtected {
		classifier, err := NewModelOverlapClassifier(policy.decider)
		if err != nil {
			return InertialAct(state), Outcome{}, err
		}
		evidence := classifier.Classify(ctx, Context{
			Revision: Revision{
				ID: 1, StableText: state.Heard, Final: kind == TranscriptFinal,
			},
			Situation: &state,
		})
		chosen := Act("")
		switch evidence {
		case OverlapDirected:
			chosen = ActStopSpeaking
		case OverlapBackchannel, OverlapSide:
			chosen = ActKeepSpeaking
		}
		if index := slices.Index(acts, chosen); index >= 0 {
			return chosen, Outcome{Index: index, Option: string(chosen)}, nil
		}
	}
	options := make([]string, len(acts))
	for index, act := range acts {
		options[index] = string(act)
	}
	outcome, err := policy.decider.Decide(ctx, Decision{
		Prompt: rules.Instruction, Options: options, Evidence: state.Render(),
		Images: state.Seeing,
	})
	if err != nil {
		return InertialAct(state), Outcome{}, err
	}
	chosen := Act(strings.TrimSpace(outcome.Option))
	if slices.Contains(acts, chosen) {
		return chosen, outcome, nil
	}
	return InertialAct(state), outcome, fmt.Errorf(
		"transcript-event policy chose %q, which is not available here", outcome.Option)
}

func (policy *TranscriptEventPolicy) rules(kind TranscriptEventKind) (TranscriptEventRules, bool) {
	switch kind {
	case TranscriptPartial:
		return policy.partial, true
	case TranscriptFinal:
		return policy.final, true
	default:
		return TranscriptEventRules{}, false
	}
}

func validateTranscriptRules(
	kind TranscriptEventKind, rules TranscriptEventRules,
) (TranscriptEventRules, error) {
	if err := kind.validate(); err != nil {
		return TranscriptEventRules{}, err
	}
	if strings.TrimSpace(rules.Instruction) == "" {
		return TranscriptEventRules{}, fmt.Errorf("%s transcript rules require an instruction", kind)
	}
	if len(rules.Acts) < 2 {
		return TranscriptEventRules{}, fmt.Errorf(
			"%s transcript rules require at least two acts", kind)
	}
	permitted := map[TranscriptEventKind]map[Act]bool{
		TranscriptPartial: {
			ActStaySilent: true, ActSpeakThrough: true, ActInterrupt: true,
			ActActSilently: true, ActKeepSpeaking: true, ActStopSpeaking: true,
		},
		TranscriptFinal: {
			ActStaySilent: true, ActAnswer: true, ActActSilently: true,
			ActKeepSpeaking: true, ActStopSpeaking: true,
		},
	}
	seen := make(map[Act]struct{}, len(rules.Acts))
	for _, act := range rules.Acts {
		if !permitted[kind][act] {
			return TranscriptEventRules{}, fmt.Errorf(
				"act %q is not valid for a %s transcript event", act, kind)
		}
		if _, duplicate := seen[act]; duplicate {
			return TranscriptEventRules{}, fmt.Errorf(
				"%s transcript rules contain duplicate act %q", kind, act)
		}
		seen[act] = struct{}{}
	}
	rules.Instruction = strings.TrimSpace(rules.Instruction)
	rules.Acts = slices.Clone(rules.Acts)
	return rules, nil
}
