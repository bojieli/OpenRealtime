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
	if err := episode.Surface().ClickElement(ctx, violet); err != nil {
		t.Fatal(err)
	}
	result, err = episode.Result(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.ActionsAfterCompletion != 1 {
		t.Fatalf("repeated post-success click was not retained: %+v", result)
	}
}

func TestOwnedBrowserEnvironmentReportsStructuredBeforeCondition(t *testing.T) {
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
	for _, test := range []struct {
		name, mode, button string
		cue                time.Duration
		camera             bool
	}{
		{name: "camera", mode: "camera", button: "emergency-stop", cue: 9800 * time.Millisecond, camera: true},
		{name: "dashboard", mode: "dashboard", button: "acknowledge", cue: 8600 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			axes := []Axis{AxisVision}
			if test.camera {
				axes = append(axes, AxisCamera)
			}
			episode, err := environment.Episode(ctx, Case{
				Task: Task{
					ID: "test-before-condition-" + test.name, Category: "test",
					PageMode: test.mode, AudioAsset: test.name + ".wav", Axes: axes,
					CueAt: test.cue, Deadline: time.Second, MaxActions: 1, Camera: test.camera,
					Groundings: []Grounding{GroundingPixel},
				},
				Grounding: GroundingPixel,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := episode.Ready(ctx); err != nil {
				t.Fatal(err)
			}
			var clicked bool
			script := `(() => {
				document.getElementById('` + test.button + `').click();
				return true;
			})()`
			if err := episode.Surface().Evaluate(ctx, script, &clicked); err != nil {
				t.Fatal(err)
			}
			if !clicked {
				t.Fatalf("browser fixture did not click %s", test.button)
			}
			result, err := episode.Result(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Complete || result.Success || result.Code != PageResultCodeBeforeCondition {
				t.Fatalf("early %s action did not report structured before-condition result: %+v", test.name, result)
			}
		})
	}
}
