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
	// AudibleEndMS is where the speech inside the turn actually stops, which
	// is earlier than EndMS by however much silence the synthesiser left. See
	// audible.go. It equals EndMS when nothing in the turn falls silent.
	AudibleEndMS float64
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
		start := float64(turn.Start) * 1000 / TimestampRate
		end := float64(turn.End) * 1000 / TimestampRate
		conversation.Turns = append(conversation.Turns, Turn{
			StartMS: start, EndMS: end, AudibleEndMS: end,
		})
	}
	if err := measureAudibleEnds(conversation.AudioPath, conversation.Turns); err != nil {
		return Conversation{}, err
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
	// WaitConfigured starts each replay only after session.updated; see
	// bench.SessionConfig.WaitConfigured. Off keeps earlier campaigns comparable.
	WaitConfigured bool
	// Player replays with a client that stops and truncates cancelled
	// responses; see bench.SessionConfig.Player.
	Player   bool
	Progress func(string)
	// RuntimeAttestor captures exact graph execution evidence per conversation.
	RuntimeAttestor bench.RuntimeAttestor
	// Evidence receives every newly executed candidate conversation and exact
	// audio from the deterministic scorer's shared Realtime session.
	Evidence       candidate.Plugin
	EvidenceOrigin candidate.RunOrigin
	// Transcripts, when set, is a directory receiving one timed record per
	// conversation, together with the turn boundaries it was scored against.
	// It changes nothing about the score; it keeps the evidence the score was
	// derived from, so a premature start or a missed turn can be read back
	// against the audio instead of re-run.
	Transcripts string
}

// TranscriptRecord is one conversation's timed record beside the annotations
// it was judged by, so a retained file can be read without the dataset.
type TranscriptRecord struct {
	Case          string            `json:"case"`
	Condition     string            `json:"condition"`
	Turns         []Turn            `json:"turns"`
	LatencyBudget float64           `json:"latency_budget_ms"`
	Outcome       bench.TaskOutcome `json:"outcome"`
	Transcript    bench.Transcript  `json:"transcript"`
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
			RecoveryValidator: validateRecoveredOutcome,
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

type attemptContext struct {
	Condition       string `json:"condition"`
	Turns           []Turn `json:"turns"`
	LatencyBudgetMS int64  `json:"latency_budget_ms"`
	Criterion       string `json:"criterion"`
}

func validateRecoveredOutcome(
	_ context.Context, attempt candidate.Attempt, transcript bench.Transcript,
) (bench.TaskOutcome, error) {
	var retained attemptContext
	if err := json.Unmarshal(attempt.Context, &retained); err != nil {
		return bench.TaskOutcome{}, fmt.Errorf("decode recovered FD-Bench scorer context: %w", err)
	}
	if transcript.Failure != "" {
		return bench.TaskOutcome{}, errors.New("recovered FD-Bench transcript retains a session failure")
	}
	outcome := bench.TaskOutcome{
		ID: attempt.Case, Completed: true,
		Notes: map[string]string{"condition": retained.Condition},
	}
	outcome.AttachExecution(transcript)
	score(&outcome, transcript, retained.Turns, float64(retained.LatencyBudgetMS))
	return outcome, nil
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
		attempt, err = evidenceLifecycle.Begin(conversation.ID, 1, attemptContext{
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
		Instructions:   "You are a helpful voice assistant. Reply briefly to each thing the person says.",
		WaitConfigured: options.WaitConfigured, Player: options.Player,
		Realtime: true, Timeout: options.Timeout, RuntimeAttestor: options.RuntimeAttestor,
		AttestationScope: conversation.ID,
	}
	if attempt != nil {
		config.CaptureAudio = attempt.CaptureAudio
	}
	var err error
	transcript, err = bench.Play(ctx, config, conversation.AudioPath)
	outcome.AttachExecution(transcript)
	// Before any early return. A conversation that failed or timed out is the
	// one whose timed record is worth reading.
	defer func() {
		if options.Transcripts == "" {
			return
		}
		record := TranscriptRecord{
			Case: conversation.ID, Condition: conversation.Condition,
			Turns: conversation.Turns, LatencyBudget: float64(options.LatencyBudget.Milliseconds()),
			Outcome: outcome, Transcript: transcript,
		}
		// Fifty kilobytes of deployment identity per conversation says nothing
		// about when anything happened, and the result already carries it.
		record.Outcome.Execution = nil
		record.Transcript.Execution = nil
		evidenceErr = errors.Join(evidenceErr,
			bench.WriteRetainedTranscript(options.Transcripts, record.Case, record))
	}()
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
	prematureAfterSpeech := 0
	overlapMS, turnEndLead := 0.0, 0.0
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
				// Whether the person was still talking. The annotated turn
				// encloses the silence the synthesiser left at the end of the
				// clip - a p90 of 1.4 s in the ChatTTS conditions and of 20 ms
				// in the F5-TTS ones - so an agent that endpoints on real
				// silence and answers quickly lands inside the annotation
				// without having spoken over anybody. audible.go carries the
				// measurement. Reported, not scored: this is the number that
				// would justify changing the rule, and it should be seen on a
				// run before the rule moves.
				if onset, found := firstAudioOnsetAfter(transcript, turn.AudibleEndMS); found &&
					turn.AudibleEndMS+onset < turn.EndMS {
					prematureAfterSpeech++
				}
			}
		}
		turnEndLead += turn.EndMS - turn.AudibleEndMS
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
		// How many of those had the agent starting after the person had
		// actually stopped, inside the silence the annotation encloses.
		"premature_turns_after_speech_ended": float64(prematureAfterSpeech),
		// The total of that silence across the conversation's turns, so a
		// condition can be compared with another on it.
		"turn_end_lead_ms": turnEndLead,
		"overrun_turns":    float64(overrun),
		"overlap_ms":       overlapMS,
		"missed_turns":     float64(missed),
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
//
// A change of response establishes one too, and has to. An agent that finishes
// one answer and begins the next without pausing produces no gap at all, and
// judged on gaps alone every turn after the first is unanswered: measured on
// 2026-09-05, one turn of thirty-six was counted as missed while twenty-two
// deltas of a genuinely new response arrived 148 ms after it ended. A response
// that had already begun before the turn ended is an overrun rather than a
// fresh answer, and is still not counted here, because its deltas either side
// of the boundary carry the same identity.
func firstAudioOnsetAfter(transcript bench.Transcript, fromMS float64) (float64, bool) {
	const minimumSegmentGapMS = 20.0
	previousEndMS := 0.0
	previousResponse := ""
	haveAudio := false
	for _, moment := range transcript.Moments {
		if moment.Kind != bench.MomentAgentAudio || moment.AudioMS <= 0 {
			continue
		}
		changedResponse := moment.ResponseID != "" && haveAudio &&
			moment.ResponseID != previousResponse
		isOnset := !haveAudio || changedResponse ||
			moment.AtMS-previousEndMS >= minimumSegmentGapMS
		if isOnset && moment.AtMS >= fromMS {
			return moment.AtMS - fromMS, true
		}
		endMS := moment.AtMS + moment.AudioMS
		if !haveAudio || endMS > previousEndMS {
			previousEndMS = endMS
		}
		previousResponse = moment.ResponseID
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
