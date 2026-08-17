package baseline

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/bojieli/OpenRealtime/adapters/reference"
	openaiwire "github.com/bojieli/OpenRealtime/protocol/openai"
)

func TestEndpointedBaselineReconcilesEveryTrial(t *testing.T) {
	t.Parallel()
	root := filepath.Clean("..")
	manifest, err := reference.LoadManifest(filepath.Join(root, "tests", "fixtures", "m1-reference-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Config{
		FixturePath: filepath.Join(root, "tests", "fixtures", "m0-tone.wav"),
		Manifest:    manifest, Trials: 20, Seed: 7, FrameMS: 20, Timing: DefaultTimingModel(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Trials) != 20 || report.TimingMode != "deterministic_simulation" {
		t.Fatalf("unexpected report: %+v", report)
	}
	for _, trial := range report.Trials {
		if trial.ReconciliationErrorNS != 0 || trial.ObservedResponseLatencyNS != trial.Stages.Sum() {
			t.Fatalf("trial %d does not reconcile: %+v", trial.Index, trial)
		}
		for _, record := range trial.Trace {
			message, err := openaiwire.Decode(record.Message)
			if err != nil {
				t.Fatal(err)
			}
			if message.Type() == openaiwire.EventResponseCreated && record.MonotonicNS <= trial.EndpointNS {
				t.Fatalf("endpointed response began at %d before endpoint %d", record.MonotonicNS, trial.EndpointNS)
			}
		}
	}
	if report.Distributions["reconciliation_error_ns"].MaxNS != 0 {
		t.Fatalf("reconciliation error distribution = %+v", report.Distributions["reconciliation_error_ns"])
	}
}

func TestEndpointedBaselineIsDeterministicForSeed(t *testing.T) {
	t.Parallel()
	root := filepath.Clean("..")
	manifest, err := reference.LoadManifest(filepath.Join(root, "tests", "fixtures", "m1-reference-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	config := Config{
		FixturePath: filepath.Join(root, "tests", "fixtures", "m0-tone.wav"),
		Manifest:    manifest, Trials: 2, Seed: 91, FrameMS: 20, Timing: DefaultTimingModel(),
	}
	first, err := Run(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Run(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	for index := range first.Trials {
		if first.Trials[index].ObservedResponseLatencyNS != second.Trials[index].ObservedResponseLatencyNS {
			t.Fatalf("trial %d changed for the same seed", index)
		}
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatal("report changed for the same seed")
	}
}
