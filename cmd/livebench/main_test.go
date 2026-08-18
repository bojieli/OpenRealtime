package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/livebench"
)

type flakyAdapter struct {
	descriptor livebench.Descriptor
	failures   int
	calls      int
}

func (adapter *flakyAdapter) Descriptor() livebench.Descriptor { return adapter.descriptor }

func (adapter *flakyAdapter) Run(_ context.Context, input livebench.Audio) (livebench.SessionResult, error) {
	adapter.calls++
	if adapter.calls <= adapter.failures {
		return livebench.SessionResult{}, errors.New("transient test failure")
	}
	return livebench.SessionResult{
		Descriptor: adapter.descriptor, InputDurationMS: float64(input.Duration()) / float64(time.Millisecond),
	}, nil
}

func TestRunTrialAttemptsRecoversAndRecordsProvenance(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inputPath := filepath.Join(root, "input.wav")
	inputHash, err := livebench.WriteWAV(inputPath, livebench.Audio{SampleRateHz: 16_000, PCM16: make([]byte, 320)})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := livebench.Descriptor{Provider: "test", Model: "model", OutputSampleRate: 24_000}
	adapter := &flakyAdapter{descriptor: descriptor, failures: 1}
	sample := livebench.Sample{
		Benchmark: "benchmark", Revision: "revision", Scenario: "scenario", ID: "1",
		InputPath: inputPath, InputSHA256: inputHash,
	}
	result, attempts, err := runTrialAttempts(
		t.Context(), adapter, sample,
		livebench.TrialConfig{OutputRoot: root, Condition: "overlap"},
		0, 3, time.Second, 0, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 2 || result.Attempt != 2 || len(attempts) != 2 {
		t.Fatalf("calls=%d result_attempt=%d records=%d", adapter.calls, result.Attempt, len(attempts))
	}
	if attempts[0].Succeeded || attempts[0].Error == "" || !attempts[1].Succeeded {
		t.Fatalf("unexpected attempt records: %+v", attempts)
	}
}

func TestRunTrialAttemptsStopsAtBound(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inputPath := filepath.Join(root, "input.wav")
	inputHash, err := livebench.WriteWAV(inputPath, livebench.Audio{SampleRateHz: 16_000, PCM16: make([]byte, 320)})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := livebench.Descriptor{Provider: "test", Model: "model", OutputSampleRate: 24_000}
	adapter := &flakyAdapter{descriptor: descriptor, failures: 10}
	_, attempts, err := runTrialAttempts(
		t.Context(), adapter,
		livebench.Sample{Benchmark: "benchmark", Revision: "revision", Scenario: "scenario", ID: "1", InputPath: inputPath, InputSHA256: inputHash},
		livebench.TrialConfig{OutputRoot: root, Condition: "overlap"},
		0, 2, time.Second, 0, nil,
	)
	if err == nil || adapter.calls != 2 || len(attempts) != 2 {
		t.Fatalf("err=%v calls=%d records=%d", err, adapter.calls, len(attempts))
	}
}

func TestRunTrialAttemptsContinuesAttemptNumbersAfterResume(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inputPath := filepath.Join(root, "input.wav")
	inputHash, err := livebench.WriteWAV(inputPath, livebench.Audio{SampleRateHz: 16_000, PCM16: make([]byte, 320)})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := livebench.Descriptor{Provider: "test", Model: "model", OutputSampleRate: 24_000}
	adapter := &flakyAdapter{descriptor: descriptor}
	result, attempts, err := runTrialAttempts(
		t.Context(), adapter,
		livebench.Sample{Benchmark: "benchmark", Revision: "revision", Scenario: "scenario", ID: "1", InputPath: inputPath, InputSHA256: inputHash},
		livebench.TrialConfig{OutputRoot: root, Condition: "overlap"},
		1, 3, time.Second, 0, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Attempt != 2 || len(attempts) != 1 || attempts[0].Attempt != 2 {
		t.Fatalf("result_attempt=%d records=%+v", result.Attempt, attempts)
	}
}

func TestRunTrialAttemptsDoesNotRefreshExhaustedBudget(t *testing.T) {
	t.Parallel()
	adapter := &flakyAdapter{}
	_, attempts, err := runTrialAttempts(
		t.Context(), adapter, livebench.Sample{}, livebench.TrialConfig{},
		3, 3, time.Second, 0, nil,
	)
	if err == nil || adapter.calls != 0 || len(attempts) != 0 {
		t.Fatalf("err=%v calls=%d attempts=%v", err, adapter.calls, attempts)
	}
}

func TestResumeRunManifestPreservesAttemptLedger(t *testing.T) {
	t.Parallel()
	filename := filepath.Join(t.TempDir(), "run.json")
	createdAt := time.Date(2026, time.August, 18, 1, 2, 3, 0, time.UTC)
	plan := livebench.RunManifest{
		SchemaVersion: livebench.ResultSchemaVersion,
		CreatedAt:     time.Now().UTC(),
		Benchmark:     "benchmark",
		Revision:      "revision",
		Descriptor:    livebench.Descriptor{Provider: "provider", Model: "model"},
		Conditions:    []string{"overlap"},
		Replicates:    1,
		TrialAttempts: 3,
		Samples:       []livebench.Sample{{Scenario: "scenario", ID: "1"}},
	}
	prior := plan
	prior.CreatedAt = createdAt
	prior.Attempts = []livebench.RunAttempt{{
		SampleID: "1", Scenario: "scenario", Condition: "overlap", Attempt: 1,
	}}
	prior.Completed = []livebench.TrialResult{{TrialID: "completed"}}
	prior.Failures = []livebench.RunFailure{{SampleID: "failed"}}
	if err := livebench.WriteRunManifest(filename, prior); err != nil {
		t.Fatal(err)
	}

	resumed, err := resumeRunManifest(filename, plan)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.CreatedAt.Equal(createdAt) || resumed.TrialAttempts != 3 || len(resumed.Attempts) != 1 {
		t.Fatalf("unexpected resumed manifest: %+v", resumed)
	}
	if len(resumed.Completed) != 0 || len(resumed.Failures) != 0 {
		t.Fatalf("stale outcomes were retained: completed=%d failures=%d", len(resumed.Completed), len(resumed.Failures))
	}
}

func TestResumeRunManifestRejectsChangedAttemptBudget(t *testing.T) {
	t.Parallel()
	filename := filepath.Join(t.TempDir(), "run.json")
	prior := livebench.RunManifest{
		SchemaVersion: livebench.ResultSchemaVersion, Benchmark: "benchmark", Revision: "revision",
		Descriptor: livebench.Descriptor{Provider: "provider", Model: "model"},
		Conditions: []string{"overlap"}, Replicates: 1, TrialAttempts: 2,
		Samples: []livebench.Sample{{Scenario: "scenario", ID: "1"}},
	}
	if err := livebench.WriteRunManifest(filename, prior); err != nil {
		t.Fatal(err)
	}
	planned := prior
	planned.TrialAttempts = 3
	if _, err := resumeRunManifest(filename, planned); err == nil {
		t.Fatal("expected an attempt-budget mismatch")
	}
}

func TestResumeRunManifestRejectsDifferentPlan(t *testing.T) {
	t.Parallel()
	filename := filepath.Join(t.TempDir(), "run.json")
	prior := livebench.RunManifest{
		SchemaVersion: livebench.ResultSchemaVersion, Benchmark: "benchmark", Revision: "revision",
		Descriptor: livebench.Descriptor{Provider: "provider", Model: "model"},
		Conditions: []string{"overlap"}, Replicates: 1,
		Samples: []livebench.Sample{{Scenario: "scenario", ID: "1"}},
	}
	if err := livebench.WriteRunManifest(filename, prior); err != nil {
		t.Fatal(err)
	}
	changed := prior
	changed.Samples = []livebench.Sample{{Scenario: "scenario", ID: "2"}}
	if _, err := resumeRunManifest(filename, changed); err == nil {
		t.Fatal("expected a run-plan mismatch")
	}
}

func TestRetryBackoffIsExponentialAndCapped(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		base     time.Duration
		failures int
		want     time.Duration
	}{
		{base: time.Second, failures: 1, want: time.Second},
		{base: time.Second, failures: 3, want: 4 * time.Second},
		{base: 20 * time.Second, failures: 2, want: maxRetryBackoff},
		{base: time.Hour, failures: 1, want: maxRetryBackoff},
	} {
		if got := retryBackoff(test.base, test.failures); got != test.want {
			t.Fatalf("retryBackoff(%s, %d)=%s, want %s", test.base, test.failures, got, test.want)
		}
	}
}
