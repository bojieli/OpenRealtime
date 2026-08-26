package realtimecu

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestOwnedBrowserEnvironmentSupportsMarkedGroundingEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("chromium"); err != nil {
		t.Skip("chromium is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	environment, err := NewEnvironment(ctx, EnvironmentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer environment.Close()
	episode, err := environment.Episode(ctx, Case{
		Task: Task{
			ID: "test", Category: "test", PageMode: "choice", AudioAsset: "choice.wav",
			Axes: []Axis{AxisVision}, CueAt: 0, Deadline: time.Second, MaxActions: 1,
			Groundings: []Grounding{GroundingSetOfMark},
		},
		Grounding: GroundingSetOfMark,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := episode.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	frame, marks, err := episode.Surface().CaptureMarked(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(frame) == 0 || len(marks) != 4 {
		t.Fatalf("expected a marked frame with four choices, got %d bytes and %+v", len(frame), marks)
	}
	var violet string
	for _, mark := range marks {
		if mark.Name == "Violet" {
			violet = mark.ID
		}
	}
	if violet == "" {
		t.Fatalf("the evaluator could not locate the violet mark in %+v", marks)
	}
	if err := episode.Surface().ClickElement(ctx, violet); err != nil {
		t.Fatal(err)
	}
	result, err := episode.Result(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || !result.Complete {
		t.Fatalf("marked click did not reach the page: %+v", result)
	}
}
