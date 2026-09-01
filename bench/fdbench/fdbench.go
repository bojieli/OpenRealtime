// Package fdbench runs FD-Bench, the endpointing and timing suite.
//
// Each conversation is a recording of a person speaking in several turns, with
// the exact sample offsets of every turn alongside it. Between turns there is
// a gap where a reply belongs. That makes three things measurable without a
// judge model and without any interpretation:
//
//	response latency  how long after a turn ends the agent starts speaking
//	prematurity       whether it started while the person was still talking
//	missed turns      whether it answered at all before the next turn began
//
// Those three are in tension, which is the point of measuring them together. A
// system tuned for latency starts talking over people; one tuned to never
// interrupt waits so long that the conversation stops working. A single number
// would hide the trade, so this reports all three.
package fdbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
)

// TimestampRate is the rate the released .timestamps offsets are expressed
// in. The prepared WAVs are 24 kHz, but the annotations retain the 16 kHz
// sample indices of FD-Bench's source audio. Treating those indices as 24 kHz
// compresses every turn to two thirds of its real position and scores agent
// audio against the wrong speaker turn.
const TimestampRate = 16_000

// Turn is one span of user speech.
type Turn struct {
	StartMS float64
	EndMS   float64
}

// Conversation is one recording and its annotations.
type Conversation struct {
	ID        string
	Condition string
	AudioPath string
	Turns     []Turn
}

type rawTurn struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// Load reads conversations from one or more conditions.
//
// The dataset is partitioned by synthesiser, difficulty, and noise level, and
// those are not interchangeable: a result from the clean set says nothing
// about the 0 dB set. Conditions are therefore selected explicitly rather than
// merged.
func Load(root string, conditions []string, limit int) ([]Conversation, error) {
	if len(conditions) == 0 {
		return nil, errors.New("a condition is required: the dataset's partitions do not merge")
	}
	var conversations []Conversation
	for _, condition := range conditions {
		directory := filepath.Join(root, condition)
		entries, err := os.ReadDir(directory)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", directory, err)
		}
		var names []string
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".wav") {
				names = append(names, strings.TrimSuffix(entry.Name(), ".wav"))
			}
		}
		sort.Strings(names)
		for _, name := range names {
			conversation, err := loadConversation(directory, condition, name)
			if err != nil {
				return nil, err
			}
			conversations = append(conversations, conversation)
			if limit > 0 && len(conversations) >= limit {
				return conversations, nil
			}
		}
	}
	return conversations, nil
}

// Count reports how many conversations a set of conditions contains, which is
// what a cell's expected task count must be.
func Count(root string, conditions []string) (int, error) {
	total := 0
	for _, condition := range conditions {
		entries, err := os.ReadDir(filepath.Join(root, condition))
		if err != nil {
			return 0, err
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".wav") {
				total++
			}
		}
	}
	return total, nil
}

func loadConversation(directory, condition, name string) (Conversation, error) {
	payload, err := os.ReadFile(filepath.Join(directory, name+".timestamps"))
	if err != nil {
		return Conversation{}, fmt.Errorf("read timestamps for %s: %w", name, err)
	}
	var raw []rawTurn
	if err := json.Unmarshal(payload, &raw); err != nil {
		return Conversation{}, fmt.Errorf("decode timestamps for %s: %w", name, err)
	}
	if len(raw) == 0 {
		return Conversation{}, fmt.Errorf("%s has no annotated turns", name)
	}
	conversation := Conversation{
		ID: condition + "/" + name, Condition: condition,
		AudioPath: filepath.Join(directory, name+".wav"),
	}
	for _, turn := range raw {
		conversation.Turns = append(conversation.Turns, Turn{
			StartMS: float64(turn.Start) * 1000 / TimestampRate,
			EndMS:   float64(turn.End) * 1000 / TimestampRate,
		})
	}
	return conversation, nil
}

// Options configures a run.
type Options struct {
	Root       string
	Conditions []string
	Endpoint   string
	Token      string
	Model      string
	Cell       bench.Cell
	Limit      int
	// LatencyBudget is how long after a turn ends a reply may take before it
	// counts as late. Zero selects 2 s, which is roughly where a person starts
	// wondering whether the line dropped.
	LatencyBudget time.Duration
	Timeout       time.Duration
	Progress      func(string)
	// RuntimeAttestor captures exact graph execution evidence per conversation.
	RuntimeAttestor bench.RuntimeAttestor
	// Evidence receives every newly executed candidate conversation and exact
	// audio from the deterministic scorer's shared Realtime session.
	Evidence       candidate.Plugin
	EvidenceOrigin candidate.RunOrigin
}

// Run executes the suite.
func Run(ctx context.Context, options Options) (bench.Result, error) {
	if ctx == nil {
		return bench.Result{}, errors.New("FD-Bench evaluation requires a context")
	}
	if options.Evidence != nil {
		if err := options.EvidenceOrigin.Validate(); err != nil {
			return bench.Result{}, fmt.Errorf("FD-Bench candidate evidence origin: %w", err)
		}
	}
	if options.LatencyBudget <= 0 {
		options.LatencyBudget = 2 * time.Second
	}
	if options.Timeout <= 0 {
		options.Timeout = 5 * time.Minute
	}
	conversations, err := Load(options.Root, options.Conditions, options.Limit)
	if err != nil {
		return bench.Result{}, err
	}
	if len(conversations) == 0 {
		return bench.Result{}, errors.New("the selected conditions contain no conversations")
	}
	expected, err := Count(options.Root, options.Conditions)
	if err != nil {
		return bench.Result{}, err
	}

	result := bench.Result{
		Suite: "fd-bench", Cell: options.Cell, Provenance: bench.Capture(), Expected: expected,
	}
	var evidenceLifecycle *candidate.Lifecycle
	if options.Evidence != nil {
		evidenceLifecycle, err = candidate.NewLifecycle(candidate.LifecycleConfig{
			Context: ctx, Plugin: options.Evidence, Suite: result.Suite,
			Cell: result.Cell, Provenance: result.Provenance, Origin: options.EvidenceOrigin,
		})
		if err != nil {
			return bench.Result{}, fmt.Errorf("create FD-Bench candidate evidence lifecycle: %w", err)
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
	for index, conversation := range conversations {
		if options.Progress != nil {
			options.Progress(fmt.Sprintf("[%d/%d] %s", index+1, len(conversations), conversation.ID))
		}
		outcome, evidenceErr := runConversation(ctx, options, conversation, evidenceLifecycle)
		result.Tasks = append(result.Tasks, outcome)
		runErr = errors.Join(runErr, evidenceErr)
	}
	return finish(runErr)
}

func runConversation(
	ctx context.Context, options Options, conversation Conversation,
	evidenceLifecycle *candidate.Lifecycle,
) (outcome bench.TaskOutcome, evidenceErr error) {
	outcome = bench.TaskOutcome{
		ID:    conversation.ID,
		Notes: map[string]string{"condition": conversation.Condition},
	}
	var transcript bench.Transcript
	var attempt *candidate.ActiveAttempt
	if evidenceLifecycle != nil {
		var err error
		attempt, err = evidenceLifecycle.Begin(conversation.ID, 1, struct {
			Condition       string `json:"condition"`
			Turns           []Turn `json:"turns"`
			LatencyBudgetMS int64  `json:"latency_budget_ms"`
			Criterion       string `json:"criterion"`
		}{
			Condition: conversation.Condition, Turns: conversation.Turns,
			LatencyBudgetMS: options.LatencyBudget.Milliseconds(),
			Criterion:       "measure response latency, premature starts, overruns, and missed annotated turns",
		})
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
		Instructions: "You are a helpful voice assistant. Reply briefly to each thing the person says.",
		Realtime:     true, Timeout: options.Timeout, RuntimeAttestor: options.RuntimeAttestor,
		AttestationScope: conversation.ID,
	}
	if attempt != nil {
		config.CaptureAudio = attempt.CaptureAudio
	}
	var err error
	transcript, err = bench.Play(ctx, config, conversation.AudioPath)
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
	score(&outcome, transcript, conversation.Turns, float64(options.LatencyBudget.Milliseconds()))
	return outcome, evidenceErr
}

// score reduces one played conversation to its reading.
//
// It is separated from playing the conversation because the reading is the
// part that has to be right and the part that can be checked without a server:
// three numbers in tension - latency, whether the agent began over the top of
// the person, and whether it answered at all - and no single number that could
// stand for them.
func score(outcome *bench.TaskOutcome, transcript bench.Transcript, turns []Turn, budget float64) {
	var latencies []float64
	answered, premature, overrun, missed := 0, 0, 0, 0
	overlapMS := 0.0
	for index, turn := range turns {
		// Audio during a turn is the agent and the person talking at once, and
		// there are two quite different reasons for it. Either the agent was
		// already speaking when the person started - an answer running past
		// the gap, which barge-in should cut short - or it began speaking
		// while the person was mid-turn, which is a genuine endpointing
		// failure. Counting them together would hide which one a deployment
		// has.
		during := transcript.AudioBetween(turn.StartMS, turn.EndMS)
		if during > 0 {
			overlapMS += during
			const lookback = 200.0
			if transcript.AudioBetween(turn.StartMS-lookback, turn.StartMS) > 0 {
				overrun++
			} else {
				premature++
			}
		}
		// The window for a reply closes when the next turn begins: after that
		// the person has moved on, and a reply is not a late answer to the
		// previous thing, it is an interruption of the next.
		windowEnd := turn.EndMS + budget
		if index+1 < len(turns) {
			windowEnd = min(windowEnd, turns[index+1].StartMS)
		}
		// A reply begins at a new audio segment. A chunk from an answer that
		// was already playing across this user turn is an overrun, not a fresh
		// answer with near-zero latency. Counting its first post-turn chunk here
		// would let one uninterrupted monologue satisfy every later turn.
		latency, found := firstAudioOnsetAfter(transcript, turn.EndMS)
		if found && turn.EndMS+latency <= windowEnd {
			answered++
			latencies = append(latencies, latency)
			continue
		}
		missed++
	}

	outcome.Metrics = map[string]float64{
		"turns":    float64(len(turns)),
		"answered": float64(answered),
		// Premature and overrun are reported apart because they have different
		// causes and different fixes: one is an endpointing failure and the
		// other is what barge-in exists for. Computing the distinction and
		// then reporting one number would hide which one a deployment has,
		// which is the thing this separation was made for.
		"premature_turns": float64(premature),
		"overrun_turns":   float64(overrun),
		"overlap_ms":      overlapMS,
		"missed_turns":    float64(missed),
	}
	if len(latencies) > 0 {
		distribution := bench.Summarise(latencies)
		outcome.Metrics["response_latency_ms"] = distribution.P50
		outcome.Metrics["response_latency_p95_ms"] = distribution.P95
	}
	// A conversation passes when every turn got an answer and none of them was
	// begun over the top of the person still speaking. An overrun is not
	// counted against it: an answer that runs into the next turn and is then
	// cut short is what barge-in is for, and it is measured separately.
	outcome.Passed = missed == 0 && premature == 0
}

// firstAudioOnsetAfter returns the wait to the next distinct agent-audio
// segment. Session output is retained as a sequence of playout chunks, so
// adjacent chunks belong to the same audible segment even when a user turn
// ends between them. The recorder can leave sub-frame scheduling jitter
// between otherwise continuous chunks; only a gap of at least one 20 ms PCM
// packet establishes a new onset.
func firstAudioOnsetAfter(transcript bench.Transcript, fromMS float64) (float64, bool) {
	const minimumSegmentGapMS = 20.0
	previousEndMS := 0.0
	haveAudio := false
	for _, moment := range transcript.Moments {
		if moment.Kind != bench.MomentAgentAudio || moment.AudioMS <= 0 {
			continue
		}
		isOnset := !haveAudio || moment.AtMS-previousEndMS >= minimumSegmentGapMS
		if isOnset && moment.AtMS >= fromMS {
			return moment.AtMS - fromMS, true
		}
		endMS := moment.AtMS + moment.AudioMS
		if !haveAudio || endMS > previousEndMS {
			previousEndMS = endMS
		}
		haveAudio = true
	}
	return 0, false
}

// Conditions lists what a dataset root contains, so a run can name its
// partition rather than guessing at one.
func Conditions(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var conditions []string
	for _, entry := range entries {
		if entry.IsDir() {
			conditions = append(conditions, entry.Name())
		}
	}
	sort.Strings(conditions)
	return conditions, nil
}
