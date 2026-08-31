package main

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/meeting"
)

func TestMeetingCLISelectsOnlyTheDirectGraphNativeCandidate(t *testing.T) {
	var called bool
	dependencies := meetingCommandDependencies{
		run: func(_ context.Context, options meeting.Options) (bench.Result, error) {
			called = true
			want := meeting.ReferenceCell()
			if !reflect.DeepEqual(options.Cell, want) {
				t.Fatalf("Meeting CLI cell = %+v, want %+v", options.Cell, want)
			}
			return bench.Result{
				Suite: meeting.SuiteName, Cell: options.Cell, Expected: meeting.ExpectedTasks(),
			}, nil
		},
		openReview: func(
			context.Context, meetingReviewCLIConfig, string, func(string) (string, bool),
		) (*meetingReviewCLIResources, error) {
			return nil, nil
		},
	}
	var output bytes.Buffer
	if err := runMeetingWithDependencies(nil, &output, dependencies); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("Meeting CLI did not run its direct graph-native candidate")
	}

	called = false
	output.Reset()
	err := runMeetingWithDependencies([]string{"-foreground", "omni"}, &output, dependencies)
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -foreground") {
		t.Fatalf("removed Meeting compatibility selector error = %v", err)
	}
	if called {
		t.Fatal("removed Meeting compatibility selector reached the runner")
	}
}
