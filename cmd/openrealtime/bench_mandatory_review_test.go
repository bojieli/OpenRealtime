package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMeetingAndRealtimeCURefuseMissingReviewerBeforeAttemptOne(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func([]string, *bytes.Buffer) error
	}{
		{name: "meeting", run: func(arguments []string, output *bytes.Buffer) error {
			return runMeeting(arguments, output)
		}},
		{name: "realtime-cu", run: func(arguments []string, output *bytes.Buffer) error {
			return runRealtimeCU(arguments, output)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			working := t.TempDir()
			t.Chdir(working)
			t.Setenv("GEMINI_API_KEY", "")
			var output bytes.Buffer
			err := test.run(nil, &output)
			if err == nil || !strings.Contains(err.Error(), "Gemini review key environment is unset") {
				t.Fatalf("missing reviewer error = %v\n%s", err, output.String())
			}
			entries, readErr := os.ReadDir(filepath.Join(working, benchmarkArtifactDirectory))
			if readErr != nil || len(entries) != 1 || !entries[0].IsDir() {
				t.Fatalf("automatic diagnostic campaign entries=%v error=%v", entries, readErr)
			}
			campaign := filepath.Join(working, benchmarkArtifactDirectory, entries[0].Name())
			children, readErr := os.ReadDir(campaign)
			if readErr != nil || len(children) != 0 {
				t.Fatalf("reviewer refusal crossed attempt boundary: children=%v error=%v", children, readErr)
			}
			if strings.Contains(output.String(), "GEMINI_API_KEY") {
				t.Fatalf("reviewer credential slot leaked in output: %s", output.String())
			}
		})
	}
}

func TestGenericCandidateMissingReviewerRefusesBeforeAutomaticReservation(t *testing.T) {
	working := t.TempDir()
	t.Chdir(working)
	t.Setenv("GEMINI_API_KEY", "")
	var output bytes.Buffer
	err := runFDB(nil, &output)
	if err == nil || !strings.Contains(err.Error(), "Gemini key environment is unset") {
		t.Fatalf("generic missing reviewer error = %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(working, benchmarkArtifactDirectory)); !os.IsNotExist(statErr) {
		t.Fatalf("generic missing reviewer reserved an automatic path: %v", statErr)
	}
}
