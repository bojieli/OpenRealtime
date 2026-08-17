// Package cognition coordinates deadline-bounded foreground and asynchronous slow cognition.
package cognition

import (
	"errors"
	"strings"

	"github.com/bojieli/OpenRealtime/engine"
)

func ValidateFastDecision(
	goal engine.GoalSnapshot,
	decision engine.FastDecision,
	slowState SlowState,
) error {
	if goal.GoalID == "" || goal.RevisionID == 0 || strings.TrimSpace(goal.Question) == "" {
		return errors.New("fast decision requires a valid goal snapshot")
	}
	if decision.GoalID != goal.GoalID || decision.RevisionID != goal.RevisionID {
		return errors.New("fast decision does not match the current goal revision")
	}
	switch decision.Action {
	case engine.FastListen, engine.FastYield:
		if strings.TrimSpace(decision.Text) != "" {
			return errors.New("listen/yield decisions must not fabricate response text")
		}
	case engine.FastAcknowledge, engine.FastAnswer, engine.FastDefer:
		if strings.TrimSpace(decision.Text) == "" {
			return errors.New("audible fast decision text must not be empty")
		}
	default:
		return errors.New("unknown fast decision action")
	}
	switch decision.ProgressClaim {
	case engine.ProgressNone:
		return nil
	case engine.ProgressWorking:
		if slowState != SlowRunning {
			return errors.New("working claim is untruthful unless deliberation is running")
		}
	case engine.ProgressCompleted:
		if slowState != SlowCompleted {
			return errors.New("completed claim is untruthful unless deliberation completed")
		}
	default:
		return errors.New("unknown progress claim")
	}
	return nil
}
