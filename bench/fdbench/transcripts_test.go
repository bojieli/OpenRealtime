package fdbench_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/fdbench"
)

// A premature start and a missed turn are the same word in a score, and both
// of them are claims about where a turn ended. Keeping the timed record beside
// the turn boundaries is what lets that be checked against the audio instead
// of by running the conversation again.
func TestARetainedConversationKeepsItsTurnsAndItsTiming(t *testing.T) {
	dir := t.TempDir()
	record := fdbench.TranscriptRecord{
		Case:      "chattts-single-round-combine-easy/conversation_100",
		Condition: "chattts-single-round-combine-easy",
		Turns: []fdbench.Turn{
			{StartMS: 450, EndMS: 3723}, {StartMS: 13179, EndMS: 16356},
		},
		LatencyBudget: 2000,
		Outcome: bench.TaskOutcome{
			ID: "conversation_100", Completed: true,
			Execution: &bench.ExecutionEvidence{Kind: "graph-native"},
		},
		Transcript: bench.Transcript{
			Moments: []bench.Moment{
				{AtMS: 3800, Kind: bench.MomentAgentAudio, AudioMS: 40},
			},
			Execution: &bench.ExecutionEvidence{Kind: "graph-native"},
		},
	}
	record.Outcome.Execution, record.Transcript.Execution = nil, nil
	if err := bench.WriteRetainedTranscript(dir, record.Case, record); err != nil {
		t.Fatalf("write: %v", err)
	}
	name := bench.TranscriptFileName(record.Case)
	if filepath.Base(name) != name {
		t.Fatalf("case identifier escaped its directory: %q", name)
	}
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var decoded fdbench.TranscriptRecord
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Turns) != 2 || decoded.Turns[1].EndMS != 16356 {
		t.Fatalf("turn boundaries lost: %+v", decoded.Turns)
	}
	if len(decoded.Transcript.Moments) != 1 || decoded.Transcript.Moments[0].AtMS != 3800 {
		t.Fatalf("timing lost: %+v", decoded.Transcript.Moments)
	}
	if decoded.LatencyBudget != 2000 {
		t.Fatalf("the budget the score used was not retained: %v", decoded.LatencyBudget)
	}
}
