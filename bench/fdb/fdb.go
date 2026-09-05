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
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
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
	// Repeat is how many times each recording runs. Zero and one both mean
	// once, and leave every identifier exactly as a single run produces them.
	//
	// One attempt cannot classify a recording here. Two runs of the same forty
	// recordings against the same executable disagreed on five of them - a
	// recording moved from fail to pass, another from pass to fail, three
	// between failing and having nothing to overlap - while both runs reported
	// the same number of passes, so the category total looked settled while a
	// seventh of what it summed had moved. Every per-recording claim this suite
	// has ever made was sampled once. Repeats are how a defect is told from
	// noise, and Stability is how the answer is read.
	Repeat int
	// YieldWindow is how long the agent has to stop after an interruption
	// before it counts as not having stopped. Zero selects 1 s.
	YieldWindow time.Duration
	// HoldWindow is how long after a non-interruption event the agent is
	// expected to still be speaking. Zero selects 1 s.
	HoldWindow time.Duration
	Timeout    time.Duration
	Progress   func(string)
	// RuntimeAttestor captures exact graph execution evidence for each sample.
	RuntimeAttestor bench.RuntimeAttestor
	// Evidence receives only new candidate attempts and exact audio captured
	// from the same Realtime session used by the deterministic scorer.
	Evidence candidate.Plugin
	// EvidenceOrigin must explicitly identify a production or hermetic shared
	// endpoint whenever Evidence is configured.
	EvidenceOrigin candidate.RunOrigin
}

// Run executes the suite.
func Run(ctx context.Context, options Options) (bench.Result, error) {
	if ctx == nil {
		return bench.Result{}, errors.New("FDB evaluation requires a context")
	}
	if options.Evidence != nil {
		if err := options.EvidenceOrigin.Validate(); err != nil {
			return bench.Result{}, fmt.Errorf("FDB candidate evidence origin: %w", err)
		}
	}
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
	repeat := max(1, options.Repeat)
	expected := len(samples) * repeat
	if options.Limit > 0 {
		// A limited run is a smaller experiment, not a complete cell. The
		// expected count stays the full dataset so the report refuses it.
		full, err := Load(options.Root, options.Categories, 0)
		if err == nil {
			expected = len(full) * repeat
		}
	}

	result := bench.Result{
		Suite: "fdb-v1.5", Cell: options.Cell, Provenance: bench.Capture(), Expected: expected,
	}
	var evidenceLifecycle *candidate.Lifecycle
	if options.Evidence != nil {
		evidenceLifecycle, err = candidate.NewLifecycle(candidate.LifecycleConfig{
			Context: ctx, Plugin: options.Evidence, Suite: result.Suite,
			Cell: result.Cell, Provenance: result.Provenance, Origin: options.EvidenceOrigin,
			RecoveryValidator: validateRecoveredOutcome,
		})
		if err != nil {
			return bench.Result{}, fmt.Errorf("create FDB candidate evidence lifecycle: %w", err)
		}
		result.Provenance = evidenceLifecycle.Provenance()
	}
	finish := func(runErr error) (bench.Result, error) {
		result.Finish()
		if evidenceLifecycle == nil {
			return result, runErr
		}
		return result, errors.Join(runErr, evidenceLifecycle.Finish(result))
	}
	var runErr error
	// Trial-major rather than repeating each recording back to back: a second
	// attempt immediately after the first shares whatever the first left warm,
	// and the point of repeating is to sample the same recording under
	// conditions as independent as the harness can make them. It also means an
	// interrupted run has covered every recording once rather than the first
	// few exhaustively.
	for trial := 1; trial <= repeat; trial++ {
		for index, sample := range samples {
			caseID := sample.ID
			if repeat > 1 {
				caseID = fmt.Sprintf("%s#%d", sample.ID, trial)
			}
			if options.Progress != nil {
				options.Progress(fmt.Sprintf("[%d/%d] %s",
					(trial-1)*len(samples)+index+1, len(samples)*repeat, caseID))
			}
			outcome, evidenceErr := runSample(ctx, options, sample, caseID, trial, evidenceLifecycle)
			result.Tasks = append(result.Tasks, outcome)
			runErr = errors.Join(runErr, evidenceErr)
		}
	}
	return finish(runErr)
}

type attemptContext struct {
	Category      Category `json:"category"`
	ContextText   string   `json:"context_text"`
	EventText     string   `json:"event_text"`
	EventStartMS  float64  `json:"event_start_ms"`
	EventEndMS    float64  `json:"event_end_ms"`
	ShouldYield   bool     `json:"should_yield"`
	YieldWindowMS int64    `json:"yield_window_ms"`
	HoldWindowMS  int64    `json:"hold_window_ms"`
}

func validateRecoveredOutcome(
	_ context.Context, attempt candidate.Attempt, transcript bench.Transcript,
) (bench.TaskOutcome, error) {
	var retained attemptContext
	if err := json.Unmarshal(attempt.Context, &retained); err != nil {
		return bench.TaskOutcome{}, fmt.Errorf("decode recovered FDB scorer context: %w", err)
	}
	if transcript.Failure != "" {
		return bench.TaskOutcome{}, errors.New("recovered FDB transcript retains a session failure")
	}
	outcome := bench.TaskOutcome{
		ID: attempt.Case,
		Notes: map[string]string{
			"category": string(retained.Category),
			"context":  retained.ContextText,
			"event":    retained.EventText,
		},
		Completed: true,
	}
	outcome.AttachExecution(transcript)
	scoreOutcome(&outcome, transcript, retained)
	return outcome, nil
}

func runSample(
	ctx context.Context, options Options, sample Sample, caseID string, trial int,
	evidenceLifecycle *candidate.Lifecycle,
) (outcome bench.TaskOutcome, evidenceErr error) {
	outcome = bench.TaskOutcome{
		ID: caseID,
		Notes: map[string]string{
			"category": string(sample.Category),
			"context":  sample.ContextText,
			"event":    sample.EventText,
		},
	}
	var transcript bench.Transcript
	var attempt *candidate.ActiveAttempt
	if evidenceLifecycle != nil {
		var err error
		attempt, err = evidenceLifecycle.Begin(
			sample.ID, trial, attemptContext{
				Category: sample.Category, ContextText: sample.ContextText, EventText: sample.EventText,
				EventStartMS: sample.EventStartMS, EventEndMS: sample.EventEndMS,
				ShouldYield:   sample.Category.ShouldYield(),
				YieldWindowMS: options.YieldWindow.Milliseconds(), HoldWindowMS: options.HoldWindow.Milliseconds(),
			},
		)
		if err != nil {
			outcome.Error = err.Error()
			return outcome, err
		}
		recovered, found, err := attempt.Recovered()
		if err != nil {
			outcome.Error = err.Error()
			return outcome, err
		}
		if found {
			return recovered.Outcome, nil
		}
		defer func() {
			evidenceErr = errors.Join(evidenceErr, attempt.Complete(outcome, transcript))
		}()
	}
	config := bench.SessionConfig{
		Endpoint: options.Endpoint, Token: options.Token, Model: options.Model,
		Instructions: "You are a helpful voice assistant. Answer the user's question.",
		Realtime:     true, Timeout: options.Timeout, RuntimeAttestor: options.RuntimeAttestor,
		AttestationScope: sample.ID,
	}
	if attempt != nil {
		config.CaptureAudio = attempt.CaptureAudio
	}
	var err error
	transcript, err = bench.Play(ctx, config, sample.AudioPath)
	outcome.AttachExecution(transcript)
	if err != nil {
		outcome.Error = err.Error()
		return outcome, evidenceErr
	}
	if transcript.Failure != "" {
		outcome.Error = transcript.Failure
		return outcome, evidenceErr
	}
	outcome.Completed = true
	scoreOutcome(&outcome, transcript, attemptContext{
		Category: sample.Category, ContextText: sample.ContextText, EventText: sample.EventText,
		EventStartMS: sample.EventStartMS, EventEndMS: sample.EventEndMS,
		ShouldYield:   sample.Category.ShouldYield(),
		YieldWindowMS: options.YieldWindow.Milliseconds(), HoldWindowMS: options.HoldWindow.Milliseconds(),
	})
	return outcome, evidenceErr
}

func scoreOutcome(outcome *bench.TaskOutcome, transcript bench.Transcript, retained attemptContext) {

	// Was the agent actually making sound when the event began? Everything
	// below depends on it, and a task where it was not is a fact about
	// latency rather than about overlap.
	const lookback = 500.0
	before := transcript.AudioBetween(retained.EventStartMS-lookback, retained.EventStartMS)
	// Whether the agent was speaking when the event began is not the same
	// question as whether it had spoken recently, and the half-second lookback
	// answers the second one. An agent that finishes a short answer three
	// hundred milliseconds before the overlap arrives is judged to have been
	// speaking, then asked to hold through an overlap it has nothing left to
	// hold through, and fails for having been quick. Four of the first forty
	// background-speech recordings were exactly that on 2026-09-05, all of them
	// one-line commands - turn off all smart plugs, start a new audiobook -
	// whose answers are over in a second. So the decision asks whether the
	// audio actually reached the event, while the half-second total stays as
	// the reported metric because it is what says how much was being said.
	contact := transcript.AudioBetween(retained.EventStartMS-contactMS, retained.EventStartMS)
	speaking := contact >= audibleMS

	yieldWindow := float64(retained.YieldWindowMS)
	after := transcript.AudioBetween(retained.EventStartMS, retained.EventStartMS+yieldWindow)
	outcome.Metrics = map[string]float64{
		"agent_audio_before_event_ms": before,
		"agent_audio_at_event_ms":     contact,
		"agent_audio_after_event_ms":  after,
	}
	outcome.Notes["agent_was_speaking"] = strconv.FormatBool(speaking)

	if !speaking {
		// Nothing to yield or hold. Recording it as a pass would inflate every
		// category; recording it as a failure would blame overlap handling for
		// a latency problem. It is reported as its own thing.
		outcome.Notes["applicable"] = "false"
		outcome.Applicability = bench.NotApplicable
		outcome.Passed = false
		return
	}
	outcome.Notes["applicable"] = "true"
	outcome.Applicability = bench.Applicable

	if retained.ShouldYield {
		// Yielding means the audio stops, and the number that says whether it
		// did is how long it kept going. A ratio of audio volumes would
		// conflate "stopped late" with "never stopped", and those are
		// different failures with different causes.
		latency, found := stopLatency(transcript, retained.EventStartMS)
		if !found {
			// No audio at all after the event: it stopped immediately.
			outcome.Metrics["yield_latency_ms"] = 0
			outcome.Passed = true
			return
		}
		outcome.Metrics["yield_latency_ms"] = latency
		outcome.Passed = latency <= yieldWindow
		return
	}
	// Holding means the audio continues.
	holdWindow := float64(retained.HoldWindowMS)
	held := transcript.AudioBetween(retained.EventStartMS, retained.EventStartMS+holdWindow)
	outcome.Metrics["agent_audio_hold_window_ms"] = held
	outcome.Passed = held >= audibleMS
}

// audibleMS is the least agent audio that counts as the agent speaking.
//
// The measurement is a duration in milliseconds and the tests either side of
// it used to ask for more than zero, which one sample of 24 kHz audio -
// 0.0417 ms - satisfies. A recording whose answer ended a single sample inside
// the lookback window was therefore judged applicable, asked to hold through an
// overlap it had already finished, and failed for it; two recordings in the
// 2026-09-05 FDB v1.5 campaign were exactly that. Twenty milliseconds is one
// packet at the rate this engine works in, which is the smallest quantity of
// speech anyone could hear and the smallest the pipeline can deliver.
const audibleMS = 20.0

// contactMS is how close to the event the agent's audio must reach for the
// agent to count as speaking when it arrived.
//
// It is jitter tolerance and nothing more: one hundred milliseconds is a few
// packets, enough that a scheduling gap between two deltas does not read as
// the end of an utterance, and short enough that an answer which genuinely
// finished before the overlap is recognised as having finished.
const contactMS = 100.0

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
		if task.Applicability == bench.NotApplicable ||
			task.Applicability == "" && task.Notes["applicable"] == "false" {
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
