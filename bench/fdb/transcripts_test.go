package fdb_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/fdb"
)

func TestARepeatedAttemptDoesNotOverwriteItsSibling(t *testing.T) {
	first := fdb.TranscriptFileName("user_interruption/1#1")
	second := fdb.TranscriptFileName("user_interruption/1#2")
	if first == second {
		t.Fatalf("two trials of one recording share the file name %q", first)
	}
	for _, name := range []string{first, second} {
		if filepath.Base(name) != name {
			t.Fatalf("case identifier escaped its directory: %q", name)
		}
	}
}

func TestARetainedTranscriptKeepsTheTimedRecordAndDropsTheDeploymentBlob(t *testing.T) {
	dir := t.TempDir()
	record := fdb.TranscriptRecord{
		Case: "user_interruption/7#2", Recording: "user_interruption/7", Trial: 2,
		Category: fdb.Interruption, EventStartMS: 4200, EventEndMS: 5100,
		ShouldYield: true,
		Outcome: bench.TaskOutcome{
			ID: "user_interruption/7#2", Completed: true,
			Execution: &bench.ExecutionEvidence{Kind: "graph-native"},
		},
		Transcript: bench.Transcript{
			Moments: []bench.Moment{
				{AtMS: 4310, Kind: bench.MomentSpeechStarted},
				{AtMS: 4820, Kind: bench.MomentAgentAudio, AudioMS: 40},
			},
			Execution: &bench.ExecutionEvidence{Kind: "graph-native"},
		},
	}
	if err := fdb.WriteTranscript(dir, record); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, fdb.TranscriptFileName(record.Case)))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var decoded fdb.TranscriptRecord
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Transcript.Moments) != 2 {
		t.Fatalf("moments dropped: %d", len(decoded.Transcript.Moments))
	}
	if decoded.Transcript.Moments[0].Kind != bench.MomentSpeechStarted {
		t.Fatalf("the endpoint's own detection time did not survive: %+v", decoded.Transcript.Moments[0])
	}
	if decoded.EventStartMS != 4200 {
		t.Fatalf("annotation lost, so the file cannot be read without the dataset: %v", decoded.EventStartMS)
	}
	if decoded.Transcript.Execution != nil || decoded.Outcome.Execution != nil {
		t.Fatal("the fifty-kilobyte deployment identity was retained per attempt")
	}
}

func TestWritingATranscriptRefusesRatherThanSilentlyKeepingLess(t *testing.T) {
	file := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(file, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fdb.WriteTranscript(file, fdb.TranscriptRecord{Case: "a/1"}); err == nil {
		t.Fatal("an unwritable directory reported success, so the missing attempt looks like one that never ran")
	}
	if err := fdb.WriteTranscript("", fdb.TranscriptRecord{Case: "a/1"}); err != nil {
		t.Fatalf("retention is opt-in and an empty directory must stay silent: %v", err)
	}
}
