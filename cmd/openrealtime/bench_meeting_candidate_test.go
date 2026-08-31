package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/meeting"
)

func TestMeetingCLISelectsOnlyTheDirectGraphNativeCandidate(t *testing.T) {
	t.Chdir(t.TempDir())
	var runCalled, reviewCalled bool
	stop := errors.New("fixture stops after review setup")
	dependencies := meetingCommandDependencies{
		run: func(context.Context, meeting.Options) (bench.Result, error) {
			runCalled = true
			return bench.Result{}, nil
		},
		openReview: func(
			_ context.Context, config meetingReviewCLIConfig, _ string, _ func(string) (string, bool),
		) (*meetingReviewCLIResources, error) {
			reviewCalled = true
			if filepath.Base(config.Directory) != "review" {
				t.Fatalf("automatic Meeting review destination = %q", config.Directory)
			}
			info, err := os.Lstat(filepath.Dir(config.Directory))
			if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
				t.Fatalf("automatic Meeting campaign = %v, %v", info, err)
			}
			return nil, stop
		},
	}
	var output bytes.Buffer
	if err := runMeetingWithDependencies(nil, &output, dependencies); !errors.Is(err, stop) {
		t.Fatalf("Meeting pre-attempt stop = %v", err)
	}
	if !reviewCalled || runCalled {
		t.Fatalf("Meeting review/run calls = %v/%v", reviewCalled, runCalled)
	}

	runCalled, reviewCalled = false, false
	output.Reset()
	err := runMeetingWithDependencies([]string{"-foreground", "omni"}, &output, dependencies)
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -foreground") {
		t.Fatalf("removed Meeting compatibility selector error = %v", err)
	}
	if runCalled || reviewCalled {
		t.Fatal("removed Meeting compatibility selector reached review or runner")
	}
}
