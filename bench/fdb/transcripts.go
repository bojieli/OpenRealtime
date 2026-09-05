package fdb

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bojieli/OpenRealtime/bench"
)

// Why a run can keep its transcripts.
//
// The score this suite produces is one number per recording, and a number
// cannot say where a millisecond went. The interruption category is decided
// inside a one-second window, so a recording that fails by eighty milliseconds
// and a recording that never yields at all arrive here as the same word. The
// difference between them lives in the timed record - when the endpoint said it
// heard speech, when the last audio delta left - and that record is discarded
// as soon as the outcome is scored.
//
// Two hypotheses about the interruption floor were tested by rebuilding the
// server twice and re-running thirty recordings three times each, roughly forty
// minutes per answer, because there was no other way to look inside a single
// attempt. Both were refuted. Retaining the transcript answers that class of
// question from one run, offline, without a rebuild.

// TranscriptRecord is one attempt's timed record together with the annotation
// it was scored against, so the file can be read without the dataset beside it.
//
// The graph execution evidence is deliberately dropped. It is fifty kilobytes
// of deployment identity per attempt, it is already carried in the result, and
// it says nothing about when anything happened.
type TranscriptRecord struct {
	Case         string            `json:"case"`
	Recording    string            `json:"recording"`
	Trial        int               `json:"trial"`
	Category     Category          `json:"category"`
	EventStartMS float64           `json:"event_start_ms"`
	EventEndMS   float64           `json:"event_end_ms"`
	ShouldYield  bool              `json:"should_yield"`
	Outcome      bench.TaskOutcome `json:"outcome"`
	Transcript   bench.Transcript  `json:"transcript"`
}

// WriteTranscript records one attempt under dir, named after the case.
//
// A failed write is returned rather than swallowed. A diagnostic directory that
// silently holds less than the run produced is worse than no directory at all:
// the absent attempt is exactly the one that would have been looked at.
func WriteTranscript(dir string, record TranscriptRecord) error {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("transcript directory: %w", err)
	}
	record.Outcome.Execution = nil
	record.Transcript.Execution = nil
	encoded, err := json.MarshalIndent(record, "", " ")
	if err != nil {
		return fmt.Errorf("encode transcript for %s: %w", record.Case, err)
	}
	name := TranscriptFileName(record.Case)
	if err := os.WriteFile(filepath.Join(dir, name), append(encoded, '\n'), 0o644); err != nil {
		return fmt.Errorf("write transcript for %s: %w", record.Case, err)
	}
	return nil
}

// TranscriptFileName turns a case identifier into one path segment.
//
// Case identifiers carry a category, a slash, an index, and - when the run
// repeats - a trial suffix: "user_interruption/1#2". Written unaltered the
// slash would scatter attempts into per-category directories and the hash
// would need quoting at every shell that later reads them, so every character
// that is not a letter, a digit, or a dash becomes a dash.
func TranscriptFileName(caseID string) string {
	var builder strings.Builder
	for _, symbol := range caseID {
		switch {
		case symbol >= 'a' && symbol <= 'z',
			symbol >= 'A' && symbol <= 'Z',
			symbol >= '0' && symbol <= '9',
			symbol == '-', symbol == '_':
			builder.WriteRune(symbol)
		default:
			builder.WriteRune('-')
		}
	}
	trimmed := strings.Trim(builder.String(), "-")
	if trimmed == "" {
		trimmed = "attempt"
	}
	return trimmed + ".json"
}
