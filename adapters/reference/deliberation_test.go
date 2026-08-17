package reference

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bojieli/OpenRealtime/engine"
)

func TestDifficultWorkloadAndCognitionAdapters(t *testing.T) {
	t.Parallel()
	workload, err := LoadDifficultWorkload(filepath.Join("..", "..", "tests", "fixtures", "m4-difficult-workload.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workload.Tasks) != 3 {
		t.Fatalf("tasks = %d, want 3", len(workload.Tasks))
	}
	task := workload.Tasks[0]
	goal := engine.GoalSnapshot{GoalID: task.ID, RevisionID: 1, Question: task.Question}
	decision, err := NewFast(task, FastModeAcknowledge).Decide(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != engine.FastAcknowledge || !decision.StartSlowPath {
		t.Fatalf("unexpected decision: %+v", decision)
	}
	var updates []engine.DeliberationUpdate
	if err := NewDeliberation(task, DeliberationComplete).Deliberate(context.Background(), goal, func(update engine.DeliberationUpdate) error {
		updates = append(updates, update)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(updates) != 2 || updates[1].Status != engine.DeliberationFinal || updates[1].QualityScore != task.SlowQualityScore {
		t.Fatalf("unexpected updates: %+v", updates)
	}
}
