package cognition

import (
	"testing"

	"github.com/bojieli/OpenRealtime/engine"
)

func testGoal(revision uint64) engine.GoalSnapshot {
	return engine.GoalSnapshot{GoalID: "goal_1", RevisionID: revision, Question: "Why?", CreatedNS: 10, DeadlineNS: 100}
}

func TestCoordinatorRejectsStaleResultAfterCancellationAndRevision(t *testing.T) {
	t.Parallel()
	coordinator := NewCoordinator()
	if err := coordinator.Begin(testGoal(1), 10); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Cancel(20, "goal revised"); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Begin(testGoal(2), 21); err != nil {
		t.Fatal(err)
	}
	accepted, err := coordinator.Accept(engine.DeliberationUpdate{
		GoalID: "goal_1", RevisionID: 1, Sequence: 0, Status: engine.DeliberationFinal, Text: "stale",
	}, 30)
	if err != nil {
		t.Fatal(err)
	}
	if accepted {
		t.Fatal("stale result was accepted")
	}
	accepted, err = coordinator.Accept(engine.DeliberationUpdate{
		GoalID: "goal_1", RevisionID: 2, Sequence: 0, Status: engine.DeliberationFinal, Text: "current",
	}, 40)
	if err != nil || !accepted {
		t.Fatalf("current result rejected: accepted=%v error=%v", accepted, err)
	}
	snapshot := coordinator.Snapshot()
	if snapshot.State != SlowCompleted || snapshot.StaleRejectCount != 1 || snapshot.LatestUpdate.Text != "current" {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
}

func TestFastDecisionTruthfulness(t *testing.T) {
	t.Parallel()
	goal := testGoal(1)
	decision := engine.FastDecision{
		GoalID: goal.GoalID, RevisionID: goal.RevisionID, Action: engine.FastAcknowledge,
		Text: "I have finished checking that.", ProgressClaim: engine.ProgressCompleted,
	}
	if err := ValidateFastDecision(goal, decision, SlowRunning); err == nil {
		t.Fatal("completed claim while running must fail")
	}
	decision.Text = "Let me work through that carefully."
	decision.ProgressClaim = engine.ProgressNone
	decision.StartSlowPath = true
	if err := ValidateFastDecision(goal, decision, SlowRunning); err != nil {
		t.Fatal(err)
	}
}
