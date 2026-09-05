package fdb

import "github.com/bojieli/OpenRealtime/bench"

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
	Case         string   `json:"case"`
	Recording    string   `json:"recording"`
	Trial        int      `json:"trial"`
	Category     Category `json:"category"`
	EventStartMS float64  `json:"event_start_ms"`
	EventEndMS   float64  `json:"event_end_ms"`
	// EventAudibleAfterMS is how long after the annotation the event's speech
	// actually begins, which is where the scorer's windows open.
	EventAudibleAfterMS float64           `json:"event_audible_after_ms"`
	ShouldYield         bool              `json:"should_yield"`
	Outcome             bench.TaskOutcome `json:"outcome"`
	Transcript          bench.Transcript  `json:"transcript"`
}

// WriteTranscript records one attempt under dir, named after the case.
func WriteTranscript(dir string, record TranscriptRecord) error {
	// The graph execution evidence is deliberately dropped. It is fifty
	// kilobytes of deployment identity per attempt, it is already carried in
	// the result, and it says nothing about when anything happened.
	record.Outcome.Execution = nil
	record.Transcript.Execution = nil
	return bench.WriteRetainedTranscript(dir, record.Case, record)
}

// TranscriptFileName turns a case identifier into one path segment.
func TranscriptFileName(caseID string) string { return bench.TranscriptFileName(caseID) }
