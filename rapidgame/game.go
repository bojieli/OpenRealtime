// Package rapidgame evaluates deterministic deadline-sensitive audio games.
package rapidgame

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
)

type ConditionKind string

const (
	ConditionEndpointed ConditionKind = "endpointed"
	ConditionMicroturn  ConditionKind = "microturn_50ms"
)

type Round struct {
	ID             string
	CueAtMS        uint64
	DeadlineMS     uint64
	Prompt         string
	ExpectedAction string
}

type Condition struct {
	Kind                 ConditionKind
	BaseReactionNS       uint64
	JitterNS             uint64
	ComputeUnitsPerRound uint64
}

type Outcome struct {
	RoundID        string `json:"round_id"`
	CueAtNS        uint64 `json:"cue_at_ns"`
	ReactionNS     uint64 `json:"reaction_ns"`
	RespondedAtNS  uint64 `json:"responded_at_ns"`
	DeadlineNS     uint64 `json:"deadline_ns"`
	Action         string `json:"action"`
	Correct        bool   `json:"correct"`
	DeadlineMissed bool   `json:"deadline_missed"`
}

type Evaluation struct {
	Condition    ConditionKind `json:"condition"`
	Outcomes     []Outcome     `json:"outcomes"`
	QualityScore uint64        `json:"quality_score"`
	FailureCount uint64        `json:"failure_count"`
	ComputeUnits uint64        `json:"compute_units"`
}

func DefaultConditions() []Condition {
	return []Condition{
		{Kind: ConditionEndpointed, BaseReactionNS: 155_000_000, JitterNS: 40_000_000, ComputeUnitsPerRound: 12},
		{Kind: ConditionMicroturn, BaseReactionNS: 65_000_000, JitterNS: 20_000_000, ComputeUnitsPerRound: 20},
	}
}

func (condition Condition) Evaluate(rounds []Round, seed uint64) (Evaluation, error) {
	if err := condition.validate(); err != nil {
		return Evaluation{}, err
	}
	if len(rounds) == 0 {
		return Evaluation{}, errors.New("rapid game requires rounds")
	}
	result := Evaluation{Condition: condition.Kind, Outcomes: make([]Outcome, 0, len(rounds))}
	var successes uint64
	for index, round := range rounds {
		if round.ID == "" || round.DeadlineMS == 0 || round.Prompt == "" || round.ExpectedAction == "" {
			return Evaluation{}, fmt.Errorf("rapid game round %d is incomplete", index)
		}
		if index > 0 && round.CueAtMS <= rounds[index-1].CueAtMS {
			return Evaluation{}, errors.New("rapid game rounds must be strictly ordered")
		}
		cueAtNS, ok := multiply(round.CueAtMS, 1_000_000)
		if !ok {
			return Evaluation{}, errors.New("rapid game cue timestamp overflows")
		}
		deadlineNS, ok := multiply(round.DeadlineMS, 1_000_000)
		if !ok {
			return Evaluation{}, errors.New("rapid game deadline overflows")
		}
		reactionNS := sampleReaction(condition.BaseReactionNS, condition.JitterNS, seed, uint64(index))
		if reactionNS > math.MaxUint64-cueAtNS {
			return Evaluation{}, errors.New("rapid game response timestamp overflows")
		}
		outcome := Outcome{
			RoundID: round.ID, CueAtNS: cueAtNS, ReactionNS: reactionNS,
			RespondedAtNS: cueAtNS + reactionNS, DeadlineNS: deadlineNS,
			Action: round.ExpectedAction, Correct: true, DeadlineMissed: reactionNS > deadlineNS,
		}
		if outcome.DeadlineMissed {
			result.FailureCount++
		} else {
			successes++
		}
		result.Outcomes = append(result.Outcomes, outcome)
	}
	result.QualityScore = successes * 100 / uint64(len(rounds))
	if condition.ComputeUnitsPerRound > math.MaxUint64/uint64(len(rounds)) {
		return Evaluation{}, errors.New("rapid game compute accounting overflows")
	}
	result.ComputeUnits = condition.ComputeUnitsPerRound * uint64(len(rounds))
	return result, nil
}

func (condition Condition) validate() error {
	switch condition.Kind {
	case ConditionEndpointed, ConditionMicroturn:
	default:
		return fmt.Errorf("unsupported rapid game condition %q", condition.Kind)
	}
	if condition.BaseReactionNS < condition.JitterNS || condition.ComputeUnitsPerRound == 0 {
		return errors.New("rapid game condition requires bounded reaction time and compute")
	}
	return nil
}

func sampleReaction(base, jitter, seed, stream uint64) uint64 {
	if jitter == 0 {
		return base
	}
	random := rand.New(rand.NewPCG(seed^0xd6e8feb86659fd93, stream^0xa5a3564e27f8862b))
	return base - jitter + random.Uint64N(jitter*2+1)
}

func multiply(left, right uint64) (uint64, bool) {
	if left != 0 && right > math.MaxUint64/left {
		return 0, false
	}
	return left * right, true
}
