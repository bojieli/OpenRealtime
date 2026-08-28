package interaction

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// FloorSemantics says what an act means for the conversational floor.
//
// It is carried explicitly when a controller hands an act to another model.
// The receiver should not have to reverse-engineer whether "speak-through"
// preserves the other speaker's floor or whether "interrupt" takes it from
// the spelling of the act.
type FloorSemantics string

const (
	FloorUnchanged FloorSemantics = "unchanged"
	FloorPreserve  FloorSemantics = "preserve"
	FloorTake      FloorSemantics = "take"
	FloorYield     FloorSemantics = "yield"
)

// Plan is the typed control-plane handoff from an interaction policy to the
// machinery that can act. It intentionally contains no prose to say. Policy
// and EvidenceRef make the decision auditable; Deadline says when the live
// moment expires; Confidence and Abstained preserve the decider's uncertainty
// rather than turning it into an unexplained command.
type Plan struct {
	Act         Act
	Policy      string
	EvidenceRef string
	Floor       FloorSemantics
	Deadline    time.Time
	Confidence  float64
	Abstained   bool
}

// NewPlan builds the canonical handoff for one act.
func NewPlan(act Act, policy, evidenceRef string, deadline time.Time, outcome Outcome) (Plan, error) {
	plan := Plan{
		Act: act, Policy: strings.TrimSpace(policy), EvidenceRef: strings.TrimSpace(evidenceRef),
		Floor: FloorForAct(act), Deadline: deadline, Confidence: outcome.Confidence,
		Abstained: strings.TrimSpace(outcome.Option) == "",
	}
	if err := plan.Validate(); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// FloorForAct is the lossless act-to-floor mapping shared by runtimes and
// protocol adapters.
func FloorForAct(act Act) FloorSemantics {
	switch act {
	case ActSpeakThrough:
		return FloorPreserve
	case ActAnswer, ActInterrupt:
		return FloorTake
	case ActStopSpeaking:
		return FloorYield
	default:
		return FloorUnchanged
	}
}

// Validate rejects a plan that would become ambiguous at a process boundary.
func (plan Plan) Validate() error {
	valid := false
	for _, act := range AllActs() {
		if plan.Act == act {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("unknown interaction act %q", plan.Act)
	}
	if strings.TrimSpace(plan.Policy) == "" || strings.TrimSpace(plan.EvidenceRef) == "" {
		return errors.New("an interaction plan requires policy and evidence identities")
	}
	if plan.Deadline.IsZero() {
		return errors.New("an interaction plan requires a deadline")
	}
	if plan.Floor != FloorForAct(plan.Act) {
		return fmt.Errorf("act %q requires %q floor semantics, got %q", plan.Act, FloorForAct(plan.Act), plan.Floor)
	}
	if plan.Confidence < 0 || plan.Confidence > 1 {
		return errors.New("interaction confidence must be between zero and one")
	}
	return nil
}
