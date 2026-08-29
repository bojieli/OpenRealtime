// Package meeting owns the deterministic OpenRealtime meeting-assistant
// evaluation. It composes recorded speech, a changing shared screen, computer
// actions, spoken output, and asynchronous document reasoning in the same
// session.
package meeting

import (
	"embed"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

//go:embed testdata/audio/*.wav
var audioAssets embed.FS

//go:embed testdata/fixtures.json
var fixtureManifest []byte

const SuiteName = "openrealtime-meeting-assistant-v1"

const (
	ToolReadLaunchReview    = "meeting.read_launch_review"
	ToolAnalyzeLaunchReview = "meeting.analyze_launch_review"
)

// Cue is one authored event against the recording/environment clock. Start is
// used for interruption and overlap; End is used when the agent needs the full
// spoken instruction before acting.
type Cue struct {
	Name     string
	Start    time.Duration
	End      time.Duration
	Deadline time.Duration
}

// Task is one complete meeting episode.
type Task struct {
	ID         string
	Category   string
	Difficulty string
	PageMode   string
	AudioAsset string
	Cues       []Cue
	MaxActions int
}

func (task Task) Validate() error {
	if strings.TrimSpace(task.ID) == "" || strings.TrimSpace(task.Category) == "" ||
		strings.TrimSpace(task.PageMode) == "" || strings.TrimSpace(task.AudioAsset) == "" {
		return errors.New("a meeting task requires identity, category, page mode, and recorded audio")
	}
	if task.MaxActions <= 0 || len(task.Cues) == 0 {
		return fmt.Errorf("task %q requires an action budget and at least one cue", task.ID)
	}
	seen := map[string]bool{}
	for _, cue := range task.Cues {
		if strings.TrimSpace(cue.Name) == "" || cue.Start < 0 || cue.End < cue.Start || cue.Deadline <= 0 {
			return fmt.Errorf("task %q has an invalid cue %+v", task.ID, cue)
		}
		if seen[cue.Name] {
			return fmt.Errorf("task %q repeats cue %q", task.ID, cue.Name)
		}
		seen[cue.Name] = true
	}
	return nil
}

func (task Task) Cue(name string) Cue {
	for _, cue := range task.Cues {
		if cue.Name == name {
			return cue
		}
	}
	return Cue{}
}

func (task Task) Primary() Cue { return task.Cues[0] }

// Suite returns four diagnostic meeting scenarios. Audio offsets are fixed by
// testdata/fixtures.json and the checked-in recordings; they are not inferred
// from model transcripts.
func Suite() []Task {
	return []Task{
		{
			ID: "open-share-present", Category: "composed-meeting", Difficulty: "medium",
			PageMode: "open-present", AudioAsset: "open-present.wav", MaxActions: 6,
			Cues: []Cue{{Name: "request", Start: 0, End: 5155 * time.Millisecond, Deadline: 5 * time.Second}},
		},
		{
			ID: "follow-up-during-analysis", Category: "concurrent-work", Difficulty: "hard",
			PageMode: "follow-up", AudioAsset: "follow-up.wav", MaxActions: 6,
			Cues: []Cue{
				{Name: "analysis-request", Start: 0, End: 4737 * time.Millisecond, Deadline: 8 * time.Second},
				{Name: "follow-up", Start: 8737 * time.Millisecond, End: 10920 * time.Millisecond, Deadline: 2 * time.Second},
			},
		},
		{
			ID: "visual-alert-during-presentation", Category: "audiovisual-overlap", Difficulty: "hard",
			PageMode: "visual-alert", AudioAsset: "visual-alert.wav", MaxActions: 4,
			Cues: []Cue{
				{Name: "presentation-request", Start: 0, End: 8034 * time.Millisecond, Deadline: 5 * time.Second},
				{Name: "deployment-alert", Start: 10500 * time.Millisecond, End: 10500 * time.Millisecond, Deadline: 1400 * time.Millisecond},
			},
		},
		{
			ID: "spoken-navigation-correction", Category: "correction", Difficulty: "medium",
			PageMode: "correction", AudioAsset: "correction.wav", MaxActions: 6,
			Cues: []Cue{
				{Name: "summary-request", Start: 0, End: 3437 * time.Millisecond, Deadline: 3 * time.Second},
				{Name: "correction", Start: 6437 * time.Millisecond, End: 8991 * time.Millisecond, Deadline: 2 * time.Second},
			},
		},
	}
}

func ExpectedTasks() int { return len(Suite()) }

// Select restricts a diagnostic run. Run always declares ExpectedTasks, so a
// filtered smoke can never become a reportable full-suite claim.
func Select(categories []string) ([]Task, error) {
	wanted := map[string]bool{}
	for _, category := range categories {
		if trimmed := strings.TrimSpace(category); trimmed != "" {
			wanted[trimmed] = true
		}
	}
	var selected []Task
	for _, task := range Suite() {
		if err := task.Validate(); err != nil {
			return nil, err
		}
		if len(wanted) == 0 || wanted[task.Category] {
			selected = append(selected, task)
		}
	}
	if len(selected) == 0 {
		return nil, errors.New("the selection contains no meeting-assistant tasks")
	}
	return slices.Clone(selected), nil
}
