package cognition

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/engine"
)

type SlowState string

const (
	SlowIdle      SlowState = "idle"
	SlowRunning   SlowState = "running"
	SlowCompleted SlowState = "completed"
	SlowFailed    SlowState = "failed"
	SlowCancelled SlowState = "cancelled"
)

type Event struct {
	AtNS       uint64                    `json:"at_ns"`
	Type       string                    `json:"type"`
	GoalID     string                    `json:"goal_id"`
	RevisionID uint64                    `json:"revision_id"`
	Sequence   uint64                    `json:"sequence,omitempty"`
	Status     engine.DeliberationStatus `json:"status,omitempty"`
	Reason     string                    `json:"reason,omitempty"`
}

type Snapshot struct {
	Goal             engine.GoalSnapshot        `json:"goal"`
	State            SlowState                  `json:"state"`
	NextSequence     uint64                     `json:"next_sequence"`
	LatestUpdate     *engine.DeliberationUpdate `json:"latest_update,omitempty"`
	StaleRejectCount uint64                     `json:"stale_reject_count"`
	DeadlineMissed   bool                       `json:"deadline_missed"`
	Events           []Event                    `json:"events"`
}

type Coordinator struct {
	mu sync.Mutex

	goal             engine.GoalSnapshot
	state            SlowState
	nextSequence     uint64
	latest           *engine.DeliberationUpdate
	staleRejectCount uint64
	deadlineMissed   bool
	events           []Event
	hasTime          bool
	lastNS           uint64
}

func NewCoordinator() *Coordinator { return &Coordinator{state: SlowIdle} }

func (coordinator *Coordinator) Begin(goal engine.GoalSnapshot, atNS uint64) error {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if err := validateGoal(goal); err != nil {
		return err
	}
	if err := coordinator.checkTime(atNS); err != nil {
		return err
	}
	if coordinator.state == SlowRunning {
		return errors.New("cannot begin a goal while deliberation is running")
	}
	coordinator.goal = goal
	coordinator.state = SlowRunning
	coordinator.nextSequence = 0
	coordinator.latest = nil
	coordinator.deadlineMissed = false
	coordinator.record(Event{AtNS: atNS, Type: "deliberation.started", GoalID: goal.GoalID, RevisionID: goal.RevisionID})
	return nil
}

func (coordinator *Coordinator) Accept(update engine.DeliberationUpdate, atNS uint64) (bool, error) {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if err := coordinator.checkTime(atNS); err != nil {
		return false, err
	}
	if update.GoalID != coordinator.goal.GoalID || update.RevisionID != coordinator.goal.RevisionID || coordinator.state != SlowRunning {
		coordinator.staleRejectCount++
		coordinator.record(Event{
			AtNS: atNS, Type: "deliberation.stale_rejected", GoalID: update.GoalID,
			RevisionID: update.RevisionID, Sequence: update.Sequence, Status: update.Status,
		})
		return false, nil
	}
	if update.Sequence != coordinator.nextSequence {
		return false, fmt.Errorf("expected deliberation sequence %d, got %d", coordinator.nextSequence, update.Sequence)
	}
	if err := validateUpdate(update); err != nil {
		return false, err
	}
	copy := update
	coordinator.latest = &copy
	coordinator.nextSequence++
	coordinator.deadlineMissed = coordinator.goal.DeadlineNS != 0 && atNS > coordinator.goal.DeadlineNS
	coordinator.record(Event{
		AtNS: atNS, Type: "deliberation.update", GoalID: update.GoalID,
		RevisionID: update.RevisionID, Sequence: update.Sequence, Status: update.Status,
	})
	switch update.Status {
	case engine.DeliberationFinal:
		coordinator.state = SlowCompleted
	case engine.DeliberationFailed:
		coordinator.state = SlowFailed
	}
	return true, nil
}

func (coordinator *Coordinator) Cancel(atNS uint64, reason string) error {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if err := coordinator.checkTime(atNS); err != nil {
		return err
	}
	if coordinator.state != SlowRunning {
		return errors.New("no running deliberation to cancel")
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("deliberation cancellation requires a reason")
	}
	coordinator.state = SlowCancelled
	coordinator.record(Event{
		AtNS: atNS, Type: "deliberation.cancelled", GoalID: coordinator.goal.GoalID,
		RevisionID: coordinator.goal.RevisionID, Reason: reason,
	})
	return nil
}

func (coordinator *Coordinator) Snapshot() Snapshot {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	var latest *engine.DeliberationUpdate
	if coordinator.latest != nil {
		copy := *coordinator.latest
		latest = &copy
	}
	return Snapshot{
		Goal: coordinator.goal, State: coordinator.state, NextSequence: coordinator.nextSequence,
		LatestUpdate: latest, StaleRejectCount: coordinator.staleRejectCount,
		DeadlineMissed: coordinator.deadlineMissed, Events: slices.Clone(coordinator.events),
	}
}

func (coordinator *Coordinator) ValidateClosed() error {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.state == SlowRunning {
		return errors.New("deliberation is still running")
	}
	if coordinator.state == SlowIdle {
		return errors.New("deliberation never started")
	}
	return nil
}

func (coordinator *Coordinator) checkTime(atNS uint64) error {
	if coordinator.hasTime && atNS < coordinator.lastNS {
		return errors.New("deliberation time moved backwards")
	}
	return nil
}

func (coordinator *Coordinator) record(event Event) {
	coordinator.hasTime = true
	coordinator.lastNS = event.AtNS
	coordinator.events = append(coordinator.events, event)
}

func validateGoal(goal engine.GoalSnapshot) error {
	if goal.GoalID == "" || goal.RevisionID == 0 || strings.TrimSpace(goal.Question) == "" {
		return errors.New("goal ID, revision, and question are required")
	}
	if goal.DeadlineNS != 0 && goal.DeadlineNS < goal.CreatedNS {
		return errors.New("goal deadline cannot precede creation")
	}
	return nil
}

func validateUpdate(update engine.DeliberationUpdate) error {
	switch update.Status {
	case engine.DeliberationProgress:
		if strings.TrimSpace(update.Text) == "" || update.Error != "" {
			return errors.New("progress update requires text and no error")
		}
	case engine.DeliberationFinal:
		if strings.TrimSpace(update.Text) == "" || update.Error != "" {
			return errors.New("final update requires answer text and no error")
		}
	case engine.DeliberationFailed:
		if strings.TrimSpace(update.Error) == "" || update.Text != "" {
			return errors.New("failed update requires an error and no answer text")
		}
	default:
		return errors.New("unknown deliberation update status")
	}
	return nil
}
