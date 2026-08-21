package dynacu_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/dynacu"
)

// Verify runs before anything expensive and fails closed. Every case here is a
// way a run produces numbers that look fine and mean nothing, and finding out
// after six hours of browser automation is the expensive way to find out.
func TestVerifyRefusesAnEnvironmentThatWouldMeasureSomethingElse(t *testing.T) {
	for _, test := range []struct {
		name    string
		config  dynacu.Config
		expects string
	}{
		{
			name:    "no checkout",
			config:  dynacu.Config{Endpoint: "ws://127.0.0.1:8765/v1/realtime"},
			expects: "prepared AOI checkout",
		},
		{
			name:    "no endpoint",
			config:  dynacu.Config{AOIDir: t.TempDir()},
			expects: "endpoint is required",
		},
		{
			name: "not a checkout",
			config: dynacu.Config{
				AOIDir: t.TempDir(), Endpoint: "ws://127.0.0.1:8765/v1/realtime",
			},
			expects: "not a Git checkout",
		},
		{
			name: "a remote endpoint with no credential",
			config: dynacu.Config{
				AOIDir: t.TempDir(), Endpoint: "wss://example.invalid/v1/realtime",
			},
			expects: "not a Git checkout",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := test.config
			err := config.Verify(context.Background())
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !strings.Contains(err.Error(), test.expects) {
				t.Fatalf("expected %q, got %v", test.expects, err)
			}
		})
	}
}

// A run that covers less than the declared suite is incomplete however well it
// scores: five tasks from one category answer a question the published number
// does not ask.
func TestARestrictedRunIsNeverComplete(t *testing.T) {
	for _, config := range []dynacu.Config{
		{Category: "A_podcast"},
		{Difficulty: "easy"},
		{TaskIDs: []string{"A-E1"}},
		{Limit: 5},
	} {
		if !config.Restricted() {
			t.Fatalf("%+v should be restricted", config)
		}
	}
	if (dynacu.Config{}).Restricted() {
		t.Fatal("an unrestricted run covers the suite")
	}
}

// The report is built from what the driver wrote, so an interrupted run is
// reported with whatever it completed rather than lost.
func TestTheReportIsBuiltFromWhatTheSuiteSaid(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "records.jsonl")
	if err := os.WriteFile(output, []byte(strings.Join([]string{
		`{"task_id":"A-E1","category":"A_podcast","difficulty":"easy","success":true,` +
			`"result_val":"guest_name_correct","steps_taken":2,"total_time_s":31.4,"final_score":1}`,
		`{"task_id":"A-E2","category":"A_podcast","difficulty":"easy","success":false,` +
			`"result_val":"incorrect","steps_taken":8,"total_time_s":95.1}`,
		`{"task_id":"S-E1","category":"S_static","difficulty":"easy","success":false,` +
			`"error":"INVALID: 4 model-call failures"}`,
	}, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write records: %v", err)
	}

	result, err := dynacu.Report(dynacu.Config{Output: output, Cell: bench.Reference()})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if result.Suite != "dynacu-bench" {
		t.Fatalf("unexpected suite %q", result.Suite)
	}
	if result.Expected != dynacu.TaskCount {
		t.Fatalf("a cell is measured against the declared suite, got %d", result.Expected)
	}
	if len(result.Tasks) != 3 {
		t.Fatalf("expected three rows, got %d", len(result.Tasks))
	}
	if result.Summary.Complete {
		t.Fatal("three of a hundred and fifty tasks is not a complete cell")
	}

	// An invalid task is not a failed one. The suite marks a task INVALID when
	// every model call failed, and scoring that zero is how infrastructure
	// trouble becomes a published capability claim.
	breakdown := dynacu.Breakdown(result)
	if got := breakdown["A_podcast"]; got.Completed != 2 || got.Passed != 1 || got.Rate != 0.5 {
		t.Fatalf("podcast: %+v", got)
	}
	if got := breakdown["S_static"]; got.Invalid != 1 || got.Completed != 0 || got.Rate != 0 {
		t.Fatalf("static: %+v", got)
	}
}

// A resumed run rewrites a task it retried, and the last word wins rather than
// the row appearing twice.
func TestAResumedRunKeepsOneRowPerTask(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "records.jsonl")
	if err := os.WriteFile(output, []byte(strings.Join([]string{
		`{"task_id":"A-E1","category":"A_podcast","success":false,"error":"INVALID: endpoint down"}`,
		`{"task_id":"A-E1","category":"A_podcast","success":true,"result_val":"guest_name_correct"}`,
	}, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write records: %v", err)
	}
	result, err := dynacu.Report(dynacu.Config{Output: output, Cell: bench.Reference()})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(result.Tasks) != 1 {
		t.Fatalf("expected one row, got %d", len(result.Tasks))
	}
	if !result.Tasks[0].Completed || !result.Tasks[0].Passed {
		t.Fatalf("the retry is the outcome: %+v", result.Tasks[0])
	}
}

// The suite is 150 tasks and this runner says so, because a cell that ran
// fewer is incomplete and a report that divided by whatever ran would not say
// so.
func TestTheDeclaredSuiteIsWhatTheRunnerMeasures(t *testing.T) {
	if dynacu.TaskCount != dynacu.DynamicTasks+dynacu.StaticTasks {
		t.Fatalf("%d dynamic plus %d static is not %d",
			dynacu.DynamicTasks, dynacu.StaticTasks, dynacu.TaskCount)
	}
	if len(dynacu.Categories) != 11 {
		t.Fatalf("ten dynamic categories and a static control, got %d", len(dynacu.Categories))
	}
	if len(dynacu.PinnedRevision) != 40 {
		t.Fatalf("the pinned revision must be a full commit hash, got %q", dynacu.PinnedRevision)
	}
}
