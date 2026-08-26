// Package realtimecu owns OpenRealtime's audiovisual computer-use benchmark.
//
// The tasks, browser pages, audio, runner, scoring, and report conversion all
// live in this repository. External suites remain useful independent checks,
// but no release or capability claim in this package depends on one.
package realtimecu

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Axis names a capability a task isolates or composes.
type Axis string

const (
	AxisAudio         Axis = "audio"
	AxisVision        Axis = "visual_temporal"
	AxisInteraction   Axis = "realtime_interaction"
	AxisCamera        Axis = "camera"
	AxisAuthorization Axis = "authorization"
)

// Grounding is how a visual action names its target.
type Grounding string

const (
	GroundingPixel     Grounding = "pixel"
	GroundingSetOfMark Grounding = "set_of_mark"
)

func ParseGrounding(value string) (Grounding, error) {
	grounding := Grounding(strings.ToLower(strings.TrimSpace(value)))
	switch grounding {
	case GroundingPixel, GroundingSetOfMark:
		return grounding, nil
	default:
		return "", fmt.Errorf("unsupported grounding %q: use pixel or set_of_mark", value)
	}
}

// Task is one repository-owned, deterministically scored browser episode.
type Task struct {
	ID         string
	Category   string
	Difficulty string
	Axes       []Axis
	PageMode   string
	AudioAsset string
	// CueAt is when the condition worth reacting to becomes observable after
	// the environment clock starts. For an audio-only instruction it is the
	// end of the authored utterance; for a transient visual event it is the
	// page's declared event time.
	CueAt    time.Duration
	Deadline time.Duration
	// MaxActions prevents a failing model from clicking indefinitely.
	MaxActions int
	Camera     bool
	Groundings []Grounding
}

func (task Task) Validate() error {
	if strings.TrimSpace(task.ID) == "" || strings.TrimSpace(task.Category) == "" ||
		strings.TrimSpace(task.PageMode) == "" || strings.TrimSpace(task.AudioAsset) == "" {
		return errors.New("a realtime computer-use task requires identity, category, page, and audio")
	}
	if len(task.Axes) == 0 || len(task.Groundings) == 0 {
		return fmt.Errorf("task %q requires capability axes and grounding modes", task.ID)
	}
	if task.CueAt < 0 || task.Deadline <= 0 || task.MaxActions <= 0 {
		return fmt.Errorf("task %q requires a non-negative cue, deadline, and action budget", task.ID)
	}
	for _, grounding := range task.Groundings {
		if grounding != GroundingPixel && grounding != GroundingSetOfMark {
			return fmt.Errorf("task %q has unsupported grounding %q", task.ID, grounding)
		}
	}
	return nil
}

func (task Task) HasAxis(axis Axis) bool { return slices.Contains(task.Axes, axis) }

// Suite returns the complete v1 task set.
//
// Each task is intentionally small enough to diagnose. The suite composes the
// axes across tasks instead of hiding every failure inside one elaborate page:
// a missed transient, a bad coordinate, a misheard colour, and an unsafe click
// should be four different rows.
func Suite() []Task {
	both := []Grounding{GroundingPixel, GroundingSetOfMark}
	return []Task{
		{
			ID: "static-control", Category: "control", Difficulty: "easy",
			Axes: []Axis{AxisAudio}, PageMode: "static", AudioAsset: "static.wav",
			CueAt: 3800 * time.Millisecond, Deadline: 6 * time.Second, MaxActions: 2,
			Groundings: both,
		},
		{
			ID: "transient-deployment-alert", Category: "transient-ui", Difficulty: "hard",
			Axes: []Axis{AxisAudio, AxisVision, AxisInteraction}, PageMode: "transient",
			AudioAsset: "transient.wav", CueAt: 6 * time.Second,
			Deadline: 1400 * time.Millisecond, MaxActions: 3, Groundings: both,
		},
		{
			ID: "live-temperature-threshold", Category: "dashboard", Difficulty: "medium",
			Axes: []Axis{AxisAudio, AxisVision, AxisInteraction}, PageMode: "dashboard",
			AudioAsset: "dashboard.wav", CueAt: 8600 * time.Millisecond,
			Deadline: 2500 * time.Millisecond, MaxActions: 3, Groundings: both,
		},
		{
			ID: "moving-target", Category: "game", Difficulty: "hard",
			Axes: []Axis{AxisAudio, AxisVision, AxisInteraction}, PageMode: "game",
			AudioAsset: "game.wav", CueAt: 4 * time.Second,
			Deadline: 1600 * time.Millisecond, MaxActions: 5, Groundings: both,
		},
		{
			ID: "spoken-colour-choice", Category: "audio-visual", Difficulty: "easy",
			Axes: []Axis{AxisAudio, AxisVision}, PageMode: "choice", AudioAsset: "choice.wav",
			CueAt: 3300 * time.Millisecond, Deadline: 5 * time.Second, MaxActions: 3,
			Groundings: both,
		},
		{
			ID: "camera-smoke-stop", Category: "physical-camera", Difficulty: "hard",
			Axes: []Axis{AxisAudio, AxisVision, AxisInteraction, AxisCamera}, PageMode: "camera",
			AudioAsset: "camera.wav", CueAt: 9800 * time.Millisecond,
			Deadline: 2200 * time.Millisecond, MaxActions: 3, Camera: true, Groundings: both,
		},
		{
			ID: "untrusted-payment-prompt", Category: "authorization", Difficulty: "medium",
			Axes: []Axis{AxisAudio, AxisVision, AxisAuthorization}, PageMode: "authorization",
			AudioAsset: "authorization.wav", CueAt: 5940 * time.Millisecond,
			Deadline: 5 * time.Second, MaxActions: 2, Groundings: both,
		},
		{
			ID: "typed-incident-code", Category: "multi-step", Difficulty: "medium",
			Axes: []Axis{AxisAudio, AxisVision}, PageMode: "form", AudioAsset: "form.wav",
			CueAt: 4500 * time.Millisecond, Deadline: 8 * time.Second, MaxActions: 5,
			Groundings: both,
		},
	}
}

// Select filters tasks without changing the declared size of the suite.
func Select(categories []string, groundings []Grounding) ([]Case, error) {
	wantedCategories := make(map[string]struct{}, len(categories))
	for _, category := range categories {
		if name := strings.TrimSpace(category); name != "" {
			wantedCategories[name] = struct{}{}
		}
	}
	wantedGroundings := make(map[Grounding]struct{}, len(groundings))
	for _, grounding := range groundings {
		wantedGroundings[grounding] = struct{}{}
	}
	var cases []Case
	for _, task := range Suite() {
		if err := task.Validate(); err != nil {
			return nil, err
		}
		if len(wantedCategories) > 0 {
			if _, selected := wantedCategories[task.Category]; !selected {
				continue
			}
		}
		for _, grounding := range task.Groundings {
			if len(wantedGroundings) > 0 {
				if _, selected := wantedGroundings[grounding]; !selected {
					continue
				}
			}
			cases = append(cases, Case{Task: task, Grounding: grounding})
		}
	}
	if len(cases) == 0 {
		return nil, errors.New("the selection contains no realtime computer-use cases")
	}
	return cases, nil
}

// Case is one task under one grounding condition.
type Case struct {
	Task      Task
	Grounding Grounding
}

func (item Case) ID() string { return item.Task.ID + "/" + string(item.Grounding) }
