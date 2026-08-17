package livebench

import (
	"path/filepath"
	"testing"
)

func TestSummarizePairedManifest(t *testing.T) {
	t.Parallel()
	descriptor := Descriptor{Provider: "test", Model: "model", Architecture: "native"}
	valueA, valueB := 100.0, 80.0
	manifest := RunManifest{
		SchemaVersion: ResultSchemaVersion, Descriptor: descriptor,
		Completed: []TrialResult{
			{TrialID: "test/scenario/1/overlap/r000", Condition: "overlap", Sample: Sample{ID: "1", Scenario: "scenario"}, Session: SessionResult{Descriptor: descriptor, FirstAudioMS: &valueA, OutputAudioMS: 200}, Timing: TimingMetrics{FirstOutputMS: &valueA, OverlapSpeechMS: 40, SpeechDuringOverlap: true}},
			{TrialID: "test/scenario/1/clean/r000", Condition: "clean", Sample: Sample{ID: "1", Scenario: "scenario"}, Session: SessionResult{Descriptor: descriptor, FirstAudioMS: &valueB, OutputAudioMS: 160}, Timing: TimingMetrics{FirstOutputMS: &valueB, OverlapSpeechMS: 10}},
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
}
