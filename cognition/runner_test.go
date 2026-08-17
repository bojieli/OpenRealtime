package cognition

import (
	"context"
	"testing"

	"github.com/bojieli/OpenRealtime/engine"
	"github.com/bojieli/OpenRealtime/internal/clock"
)

type gatedProvider struct {
	oldGate chan struct{}
}

type contextProvider struct{}

func (*contextProvider) Name() string { return "test.context_deliberation" }
func (*contextProvider) Capabilities() engine.Capabilities {
	return engine.Capabilities{engine.CapabilityCancellation: true}
}
func (*contextProvider) Deliberate(ctx context.Context, _ engine.GoalSnapshot, _ func(engine.DeliberationUpdate) error) error {
	<-ctx.Done()
	return ctx.Err()
}

func (provider *gatedProvider) Name() string { return "test.gated_deliberation" }

func (provider *gatedProvider) Capabilities() engine.Capabilities {
	return engine.Capabilities{engine.CapabilityDeterministic: true}
}

func (provider *gatedProvider) Deliberate(
	_ context.Context,
	goal engine.GoalSnapshot,
	consume func(engine.DeliberationUpdate) error,
) error {
	if goal.RevisionID == 1 {
		<-provider.oldGate
	}
	return consume(engine.DeliberationUpdate{
		GoalID: goal.GoalID, RevisionID: goal.RevisionID, Sequence: 0,
		Status: engine.DeliberationFinal, Text: "answer",
	})
}

func TestRunnerAllowsRevisionToReplaceNoncancellableStaleTask(t *testing.T) {
	t.Parallel()
	monotonicClock := clock.NewVirtual(10)
	coordinator := NewCoordinator()
	provider := &gatedProvider{oldGate: make(chan struct{})}
	runner, err := NewRunner(provider, coordinator, monotonicClock)
	if err != nil {
		t.Fatal(err)
	}
	oldTask, err := runner.Start(context.Background(), testGoal(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := monotonicClock.AdvanceToNS(20); err != nil {
		t.Fatal(err)
	}
	if err := runner.CancelCurrent("revision changed"); err != nil {
		t.Fatal(err)
	}
	if err := monotonicClock.AdvanceToNS(21); err != nil {
		t.Fatal(err)
	}
	currentTask, err := runner.Start(context.Background(), testGoal(2))
	if err != nil {
		t.Fatal(err)
	}
	if err := <-currentTask.Done; err != nil {
		t.Fatal(err)
	}
	if err := monotonicClock.AdvanceToNS(30); err != nil {
		t.Fatal(err)
	}
	close(provider.oldGate)
	if err := <-oldTask.Done; err != nil {
		t.Fatal(err)
	}
	snapshot := coordinator.Snapshot()
	if snapshot.State != SlowCompleted || snapshot.Goal.RevisionID != 2 || snapshot.StaleRejectCount != 1 {
		t.Fatalf("unexpected runner snapshot: %+v", snapshot)
	}
}

func TestRunnerTurnsParentCancellationIntoClosedState(t *testing.T) {
	t.Parallel()
	coordinator := NewCoordinator()
	runner, err := NewRunner(&contextProvider{}, coordinator, clock.NewVirtual(10))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	task, err := runner.Start(ctx, testGoal(1))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-task.Done; err == nil {
		t.Fatal("cancelled provider task must return its context error")
	}
	snapshot := coordinator.Snapshot()
	if snapshot.State != SlowCancelled {
		t.Fatalf("parent cancellation left state %s", snapshot.State)
	}
	if err := coordinator.ValidateClosed(); err != nil {
		t.Fatal(err)
	}
}
