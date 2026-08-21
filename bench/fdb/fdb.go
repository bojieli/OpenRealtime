// Package fdb runs the Full-Duplex Bench overlap suite.
//
// Every recording has the same shape: the user says something, the agent
// begins answering, and then a second event happens while the agent is still
// talking. What the agent should do about that second event is the whole test,
// and it differs by category:
//
//	user_interruption  the user takes the floor    → stop, and answer the new thing
//	user_backchannel   the user says "mm-hm"       → keep going
//	background_speech  someone else's speech       → keep going, do not respond
//	talking_to_other   the user addresses someone  → keep going, do not respond
//
// Two of those want the agent to yield and two want it to hold, which is why
// a system that scores well by always yielding is not a system that handles
// overlap - and why this suite is worth running as four categories rather than
// one number.
//
// Scoring is mechanical, from the timed record: whether the agent was making
// sound in the window after the event, and how quickly it stopped. No judge
// model is involved, so the numbers are reproducible and say exactly what they
// measured.
package fdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
)

// Category is one of the four overlap conditions.
type Category string

const (
	Interruption     Category = "user_interruption"
	Backchannel      Category = "user_backchannel"
	BackgroundSpeech Category = "background_speech"
	TalkingToOther   Category = "talking_to_other"
)

// Categories lists them in the order the report uses.
func Categories() []Category {
	return []Category{Interruption, Backchannel, BackgroundSpeech, TalkingToOther}
}

// ShouldYield reports what the agent is supposed to do about the event.
func (category Category) ShouldYield() bool { return category == Interruption }

// Sample is one recording.
type Sample struct {
	ID        string
	Category  Category
	AudioPath string
	// ContextText is the user's first turn, the one the agent answers.
	ContextText string
	// EventText is what happens over the agent's answer.
	EventText string
	// EventStartMS and EventEndMS are when it happens, from the recording's
	// own annotations rather than from anything this harness detects.
	EventStartMS float64
	EventEndMS   float64
}

type metadata struct {
	ContextText     string    `json:"context_text"`
	CurrentTurnText string    `json:"current_turn_text"`
	BackchannelText string    `json:"backchannel_text"`
	BackgroundText  string    `json:"background_text"`
	Timestamps      []float64 `json:"timestamps"`
}

// Load reads the dataset.
func Load(root string, categories []Category, limit int) ([]Sample, error) {
	if len(categories) == 0 {
		categories = Categories()
	}
	var samples []Sample
	for _, category := range categories {
		directory := filepath.Join(root, string(category))
		entries, err := os.ReadDir(directory)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", directory, err)
		}
		var identifiers []int
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			if identifier, err := strconv.Atoi(entry.Name()); err == nil {
				identifiers = append(identifiers, identifier)
			}
		}
		sort.Ints(identifiers)
		for _, identifier := range identifiers {
			sample, err := loadSample(directory, category, identifier)
			if err != nil {
				return nil, err
			}
			samples = append(samples, sample)
			if limit > 0 && len(samples) >= limit {
				return samples, nil
			}
		}
	}
	return samples, nil
}

func loadSample(directory string, category Category, identifier int) (Sample, error) {
	path := filepath.Join(directory, strconv.Itoa(identifier))
	payload, err := os.ReadFile(filepath.Join(path, "metadata.json"))
	if err != nil {
		return Sample{}, fmt.Errorf("read metadata for %s/%d: %w", category, identifier, err)
	}
	var decoded metadata
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return Sample{}, err
	}
	if len(decoded.Timestamps) < 2 {
		return Sample{}, fmt.Errorf("%s/%d has no event timestamps", category, identifier)
	}
	audioPath := filepath.Join(path, "input.wav")
	if _, err := os.Stat(audioPath); err != nil {
		return Sample{}, fmt.Errorf("%s/%d has no input audio: %w", category, identifier, err)
	}
	return Sample{
		ID: fmt.Sprintf("%s/%d", category, identifier), Category: category, AudioPath: audioPath,
		ContextText:  decoded.ContextText,
		EventText:    firstNonEmpty(decoded.CurrentTurnText, decoded.BackchannelText, decoded.BackgroundText),
		EventStartMS: decoded.Timestamps[0] * 1000,
		EventEndMS:   decoded.Timestamps[1] * 1000,
	}, nil
}

// Options configures a run.
type Options struct {
	Root       string
	Endpoint   string
	Token      string
	Model      string
	Cell       bench.Cell
	Categories []Category
	// Limit bounds how many recordings run. A limited run is explicitly not a
	// complete cell, and the report says so.
	Limit int
	// YieldWindow is how long the agent has to stop after an interruption
	// before it counts as not having stopped. Zero selects 1 s.
	YieldWindow time.Duration
	// HoldWindow is how long after a non-interruption event the agent is
	// expected to still be speaking. Zero selects 1 s.
	HoldWindow time.Duration
	Timeout    time.Duration
	Progress   func(string)
}

// Run executes the suite.
func Run(ctx context.Context, options Options) (bench.Result, error) {
	if options.YieldWindow <= 0 {
		options.YieldWindow = time.Second
	}
	if options.HoldWindow <= 0 {
		options.HoldWindow = time.Second
	}
	if options.Timeout <= 0 {
		options.Timeout = 3 * time.Minute
	}
	samples, err := Load(options.Root, options.Categories, options.Limit)
	if err != nil {
		return bench.Result{}, err
	}
	if len(samples) == 0 {
		return bench.Result{}, errors.New("the dataset contains no recordings")
	}
	expected := len(samples)
	if options.Limit > 0 {
		// A limited run is a smaller experiment, not a complete cell. The
		// expected count stays the full dataset so the report refuses it.
		full, err := Load(options.Root, options.Categories, 0)
		if err == nil {
			expected = len(full)
		}
	}

	result := bench.Result{
		Suite: "fdb-v1.5", Cell: options.Cell, Provenance: bench.Capture(), Expected: expected,
	}
	for index, sample := range samples {
		if options.Progress != nil {
			options.Progress(fmt.Sprintf("[%d/%d] %s", index+1, len(samples), sample.ID))
		}
		result.Tasks = append(result.Tasks, runSample(ctx, options, sample))
	}
	result.Finish()
	return result, nil
}

func runSample(ctx context.Context, options Options, sample Sample) bench.TaskOutcome {
	outcome := bench.TaskOutcome{
		ID: sample.ID,
		Notes: map[string]string{
			"category": string(sample.Category),
			"context":  sample.ContextText,
			"event":    sample.EventText,
		},
	}
	transcript, err := bench.Play(ctx, bench.SessionConfig{
		Endpoint: options.Endpoint, Token: options.Token, Model: options.Model,
		Instructions: "You are a helpful voice assistant. Answer the user's question.",
		Realtime:     true, Timeout: options.Timeout,
	}, sample.AudioPath)
	if err != nil {
		outcome.Error = err.Error()
		return outcome
	}
	if transcript.Failure != "" {
		outcome.Error = transcript.Failure
		return outcome
	}
	outcome.Completed = true

	// Was the agent actually making sound when the event began? Everything
	// below depends on it, and a task where it was not is a fact about
	// latency rather than about overlap.
	const lookback = 500.0
	before := transcript.AudioBetween(sample.EventStartMS-lookback, sample.EventStartMS)
	speaking := before > 0

	yieldWindow := float64(options.YieldWindow.Milliseconds())
	after := transcript.AudioBetween(sample.EventStartMS, sample.EventStartMS+yieldWindow)
	outcome.Metrics = map[string]float64{
		"agent_audio_before_event_ms": before,
		"agent_audio_after_event_ms":  after,
	}
	outcome.Notes["agent_was_speaking"] = strconv.FormatBool(speaking)

	if !speaking {
		// Nothing to yield or hold. Recording it as a pass would inflate every
		// category; recording it as a failure would blame overlap handling for
		// a latency problem. It is reported as its own thing.
		outcome.Notes["applicable"] = "false"
		outcome.Passed = true
		return outcome
	}
	outcome.Notes["applicable"] = "true"

	if sample.Category.ShouldYield() {
		// Yielding means the audio stops, and the number that says whether it
		// did is how long it kept going. A ratio of audio volumes would
		// conflate "stopped late" with "never stopped", and those are
		// different failures with different causes.
		latency, found := stopLatency(transcript, sample.EventStartMS)
		if !found {
			// No audio at all after the event: it stopped immediately.
			outcome.Metrics["yield_latency_ms"] = 0
			outcome.Passed = true
			return outcome
		}
		outcome.Metrics["yield_latency_ms"] = latency
		outcome.Passed = latency <= yieldWindow
		return outcome
	}
	// Holding means the audio continues.
	holdWindow := float64(options.HoldWindow.Milliseconds())
	held := transcript.AudioBetween(sample.EventStartMS, sample.EventStartMS+holdWindow)
	outcome.Metrics["agent_audio_hold_window_ms"] = held
	outcome.Passed = held > 0
	return outcome
}

// stopLatency is how long the interrupted utterance kept going.
//
// It follows the audio that was already flowing when the event arrived until
// it stops, and stops looking there. Taking the last audio in the whole
// recording would measure when the agent finished answering the *new* question
// instead - a number tens of seconds long that looks like a catastrophic
// failure to yield and is nothing of the kind.
func stopLatency(transcript bench.Transcript, eventMS float64) (float64, bool) {
	// A gap longer than this means the stream stopped rather than paused
	// between frames. Frames arrive at the pacing interval, so anything much
	// larger is a boundary.
	const gapMS = 400.0
	last := -1.0
	for _, moment := range transcript.Moments {
		if moment.Kind != bench.MomentAgentAudio || moment.AtMS < eventMS {
			continue
		}
		if last >= 0 && moment.AtMS-last > gapMS {
			break
		}
		last = moment.AtMS
	}
	if last < 0 {
		return 0, false
	}
	return last - eventMS, true
}

// Breakdown summarises a result by category, because the four conditions want
// opposite behaviour and one number hides that.
func Breakdown(result bench.Result) map[Category]CategorySummary {
	summaries := map[Category]CategorySummary{}
	for _, task := range result.Tasks {
		category := Category(task.Notes["category"])
		summary := summaries[category]
		summary.Total++
		if !task.Completed {
			summary.Failed++
			summaries[category] = summary
			continue
		}
		if task.Notes["applicable"] == "false" {
			summary.NotApplicable++
			summaries[category] = summary
			continue
		}
		summary.Applicable++
		if task.Passed {
			summary.Passed++
		}
		summaries[category] = summary
	}
	for category, summary := range summaries {
		if summary.Applicable > 0 {
			summary.Rate = float64(summary.Passed) / float64(summary.Applicable)
		}
		summaries[category] = summary
	}
	return summaries
}

// CategorySummary is one condition's reading.
//
// NotApplicable is reported rather than folded in: those are recordings where
// the agent was not speaking when the event arrived, which says something
// about latency and nothing about overlap.
type CategorySummary struct {
	Total         int     `json:"total"`
	Failed        int     `json:"failed"`
	NotApplicable int     `json:"not_applicable"`
	Applicable    int     `json:"applicable"`
	Passed        int     `json:"passed"`
	Rate          float64 `json:"rate"`
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
