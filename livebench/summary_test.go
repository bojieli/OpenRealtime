package livebench

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSummarizePairedManifest(t *testing.T) {
	t.Parallel()
	descriptor := Descriptor{Provider: "test", Model: "model", Architecture: "native"}
	valueA, valueB := 100.0, 80.0
	manifest := RunManifest{
		SchemaVersion: ResultSchemaVersion, Benchmark: "benchmark", Revision: "revision", Descriptor: descriptor,
		Completed: []TrialResult{
			{TrialID: "test/scenario/1/overlap/r000", Attempt: 2, Condition: "overlap", Sample: Sample{ID: "1", Scenario: "scenario"}, Session: SessionResult{Descriptor: descriptor, FirstAudioMS: &valueA, OutputAudioMS: 200}, Timing: TimingMetrics{FirstOutputMS: &valueA, OverlapSpeechMS: 40, SpeechDuringOverlap: true}},
			{TrialID: "test/scenario/1/clean/r000", Condition: "clean", Sample: Sample{ID: "1", Scenario: "scenario"}, Session: SessionResult{Descriptor: descriptor, FirstAudioMS: &valueB, OutputAudioMS: 160}, Timing: TimingMetrics{FirstOutputMS: &valueB, OverlapSpeechMS: 10}},
		},
		Attempts: []RunAttempt{
			{SampleID: "1", Scenario: "scenario", Condition: "overlap", Replicate: 0, Attempt: 1, StartedAt: time.Date(2026, 8, 18, 1, 0, 0, 0, time.UTC), FinishedAt: time.Date(2026, 8, 18, 1, 0, 1, 0, time.UTC)},
			{SampleID: "1", Scenario: "scenario", Condition: "overlap", Replicate: 0, Attempt: 2, Succeeded: true, StartedAt: time.Date(2026, 8, 18, 1, 0, 2, 0, time.UTC), FinishedAt: time.Date(2026, 8, 18, 1, 0, 3, 0, time.UTC)},
			{SampleID: "1", Scenario: "scenario", Condition: "clean", Replicate: 0, Attempt: 1, Succeeded: true},
		},
	}
	filename := filepath.Join(t.TempDir(), "manifest.json")
	if err := WriteRunManifest(filename, manifest); err != nil {
		t.Fatal(err)
	}
	report, err := SummarizeManifests([]string{filename})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Pairs) != 1 || report.Pairs[0].Pairs != 1 || report.Pairs[0].FirstAudioDeltaMS.Mean != 20 || report.Pairs[0].SpeechDuringOverlapRate != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if len(report.Conditions) != 2 || report.Conditions[1].RunAttempts != 2 || report.Conditions[1].SuccessfulRunAttempts != 1 || report.Conditions[1].FailedRunAttempts != 1 || report.Conditions[1].AttemptLedgerTrials != 1 || report.Conditions[1].RetriedTrials != 1 || report.Conditions[1].RecoveredTrials != 1 || report.Conditions[1].SpeechDuringOverlapRate != 1 {
		t.Fatalf("unexpected overlap provenance: %+v", report.Conditions)
	}
	if report.Benchmark != "benchmark" || report.Revision != "revision" || report.CollectionStartedAt == nil || report.CollectionEndedAt == nil {
		t.Fatalf("missing report provenance: %+v", report)
	}
}

func TestSummarizeRejectsMixedBenchmarkRevisions(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first := filepath.Join(root, "first.json")
	second := filepath.Join(root, "second.json")
	if err := WriteRunManifest(first, RunManifest{SchemaVersion: ResultSchemaVersion, Benchmark: "benchmark", Revision: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteRunManifest(second, RunManifest{SchemaVersion: ResultSchemaVersion, Benchmark: "benchmark", Revision: "two"}); err != nil {
		t.Fatal(err)
	}
	if _, err := SummarizeManifests([]string{first, second}); err == nil {
		t.Fatal("expected mixed revisions to be rejected")
	}
}
