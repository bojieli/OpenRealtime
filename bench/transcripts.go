package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Keeping the record a score came from.
//
// A suite reduces a conversation to a verdict and a handful of numbers, and a
// number cannot say where a millisecond went. Two explanations for one
// suite's timing were tested by rebuilding the server and re-running thirty
// recordings three times, about forty minutes an answer, because there was no
// other way to look inside an attempt. Both were refuted, and the third
// explanation - that the recordings' own annotations lead their audio - was
// visible in the first retained transcript. Retention is off unless a caller
// asks for it, and it changes nothing about how anything is scored.

// TranscriptFileName turns a case identifier into one path segment.
//
// Identifiers carry slashes, and repeated runs add a trial suffix:
// "user_interruption/1#2". Written unaltered the slash would scatter attempts
// into directories and the hash would need quoting at every shell that later
// reads them, so every character that is not a letter, a digit, a dash, or an
// underscore becomes a dash.
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

// WriteRetainedTranscript records one attempt under dir, named after its case.
//
// A failed write is returned rather than swallowed. A diagnostic directory
// that silently holds less than the run produced is worse than no directory at
// all: the absent attempt is the one that would have been looked at.
func WriteRetainedTranscript(dir, caseID string, record any) error {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("transcript directory: %w", err)
	}
	encoded, err := json.MarshalIndent(record, "", " ")
	if err != nil {
		return fmt.Errorf("encode transcript for %s: %w", caseID, err)
	}
	path := filepath.Join(dir, TranscriptFileName(caseID))
	if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
		return fmt.Errorf("write transcript for %s: %w", caseID, err)
	}
	return nil
}
