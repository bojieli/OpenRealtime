package cognition

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/bojieli/OpenRealtime/engine"
	"github.com/bojieli/OpenRealtime/internal/clock"
)

type TaskHandle struct {
	ID   uint64
	Done <-chan error
}

type Runner struct {
	mu sync.Mutex

	provider    engine.DeliberationProvider
	coordinator *Coordinator
	clock       clock.Clock
	nextTaskID  uint64
	currentID   uint64
	cancels     map[uint64]context.CancelCauseFunc
}

func NewRunner(
	provider engine.DeliberationProvider,
	coordinator *Coordinator,
	monotonicClock clock.Clock,
) (*Runner, error) {
	if provider == nil || coordinator == nil || monotonicClock == nil {
		return nil, errors.New("deliberation runner requires provider, coordinator, and clock")
	}
	if provider.Name() == "" {
		return nil, errors.New("deliberation provider name must not be empty")
	}
	return &Runner{
		provider: provider, coordinator: coordinator, clock: monotonicClock,
		cancels: make(map[uint64]context.CancelCauseFunc),
	}, nil
}

func (runner *Runner) Start(parent context.Context, goal engine.GoalSnapshot) (TaskHandle, error) {
	if parent == nil {
		return TaskHandle{}, errors.New("deliberation parent context must not be nil")
	}
	if err := parent.Err(); err != nil {
		return TaskHandle{}, fmt.Errorf("deliberation parent context is already done: %w", err)
	}
	if err := runner.coordinator.Begin(goal, runner.clock.NowNS()); err != nil {
		return TaskHandle{}, err
	}
	ctx, cancel := context.WithCancelCause(parent)
	runner.mu.Lock()
	runner.nextTaskID++
	taskID := runner.nextTaskID
	runner.currentID = taskID
	runner.cancels[taskID] = cancel
	runner.mu.Unlock()
	done := make(chan error, 1)
	go runner.run(ctx, taskID, goal, done)
	return TaskHandle{ID: taskID, Done: done}, nil
}

func (runner *Runner) CancelCurrent(reason string) error {
	if reason == "" {
		return errors.New("deliberation cancellation requires a reason")
	}
	runner.mu.Lock()
	taskID := runner.currentID
	cancel := runner.cancels[taskID]
	runner.mu.Unlock()
	if cancel == nil {
		return errors.New("no current deliberation task")
	}
	if err := runner.coordinator.Cancel(runner.clock.NowNS(), reason); err != nil {
		return err
	}
	cancel(errors.New(reason))
	return nil
}

func (runner *Runner) run(
	ctx context.Context,
	taskID uint64,
	goal engine.GoalSnapshot,
	done chan<- error,
) {
	defer close(done)
	var nextSequence uint64
	err := runner.provider.Deliberate(ctx, goal, func(update engine.DeliberationUpdate) error {
		if update.Sequence >= nextSequence {
			nextSequence = update.Sequence + 1
		}
		_, acceptErr := runner.coordinator.Accept(update, runner.clock.NowNS())
		return acceptErr
	})
	if err == nil {
		snapshot := runner.coordinator.Snapshot()
		if snapshot.Goal.GoalID == goal.GoalID && snapshot.Goal.RevisionID == goal.RevisionID && snapshot.State == SlowRunning {
			err = fmt.Errorf("provider %s returned without a terminal update", runner.provider.Name())
		}
	}
	if err != nil {
		snapshot := runner.coordinator.Snapshot()
		isCurrentRunning := snapshot.Goal.GoalID == goal.GoalID &&
			snapshot.Goal.RevisionID == goal.RevisionID && snapshot.State == SlowRunning
		if cause := context.Cause(ctx); cause != nil && isCurrentRunning {
			if cancelErr := runner.coordinator.Cancel(runner.clock.NowNS(), cause.Error()); cancelErr != nil {
				err = errors.Join(err, cancelErr)
			}
		} else if cause == nil && isCurrentRunning {
			_, acceptErr := runner.coordinator.Accept(engine.DeliberationUpdate{
				GoalID: goal.GoalID, RevisionID: goal.RevisionID, Sequence: nextSequence,
				Status: engine.DeliberationFailed, Error: err.Error(),
			}, runner.clock.NowNS())
			if acceptErr != nil {
				err = errors.Join(err, acceptErr)
			}
		}
	}
	runner.mu.Lock()
	delete(runner.cancels, taskID)
	if runner.currentID == taskID {
		runner.currentID = 0
	}
	runner.mu.Unlock()
	done <- err
}
