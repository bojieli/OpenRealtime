// Package simulation puts two OpenRealtime agents on a live audio link and
// lets them talk to each other.
//
// Every other suite plays a recording at the system and scores the reply.
// That measures a system against a person who is not there, and it can only
// ever measure the half of the conversation the recording is not doing. The
// motivating scenarios for this project - a support call, an interview, a
// technical argument over a large body of context - are not recordings. They
// are two parties taking turns, interrupting, waiting, and deciding when to
// stop, and none of that is observable from one side.
//
// So this connects two sessions ear to mouth. What one agent says is
// synthesised, carried as audio, recognised by the other, and answered. There
// is no text shortcut anywhere in the loop: a turn only becomes a turn if the
// speech survived synthesis, the link, and recognition, which is exactly the
// path a deployment has and exactly the path a text harness cannot test.
//
// The link is paced in real time and always carrying something. A real
// microphone does not stop producing frames when nobody is talking, and an
// endpoint detector that never sees silence never fires - so silence is sent
// as deliberately as speech is.
package simulation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/realtimeclient"
)

const (
	// sampleRate is what both sessions speak and hear. One rate on the whole
	// link means no resampling anywhere, so nothing in the measurement is an
	// artefact of a converter.
	sampleRate = 24000
	// frameSamples is 20 ms. It is the granularity at which the line decides
	// between speech and silence, and it matches what a transport would carry.
	frameSamples = sampleRate / 50
	frameBytes   = frameSamples * 2
	framePeriod  = 20 * time.Millisecond
)

// Role is one side of a conversation.
type Role struct {
	// Name identifies the side in a transcript.
	Name string
	// Instruction is the session instruction, which is where a persona lives.
	Instruction string
	// Tools are declared to this side.
	Tools []action.ToolSpec
	// Tool answers a call. A role with tools and no answerer would hang its
	// own turn, so the runner refuses that combination rather than timing out.
	Tool func(name string, arguments json.RawMessage) (json.RawMessage, error)
	// Opens makes this side speak first. Exactly one side must.
	Opens bool
	// Cue is what an opening side is told to get it started. Empty selects a
	// neutral one. It is delivered to this side alone and never reaches the
	// other, so it is not part of the conversation and never becomes a turn.
	Cue string
}

// Scenario is a motivating scenario written as two roles and what has to
// happen between them for the scenario to have worked.
type Scenario struct {
	Name string
	// Question is what this scenario is meant to answer, in one line.
	Question    string
	Left, Right Role
	// MaxTurns bounds the conversation. Zero selects a default.
	MaxTurns int
	// Budget bounds wall-clock time. A conversation runs in real time, so this
	// is the real constraint.
	Budget time.Duration
	// Settle is how long both sides must be silent before the conversation is
	// judged to have ended naturally.
	Settle time.Duration
	// Checks are what must hold afterwards.
	Checks []Check
}

// Check is one thing that must be true of a finished conversation.
//
// A check states its own reason. A scenario that fails should say what the
// system failed to do, not that assertion four returned false.
type Check struct {
	Name string
	Why  string
	Pass func(Conversation) (bool, string)
}

// Turn is one side speaking, as the other side heard it.
//
// Heard, not said: the text is what the listener's recogniser produced, so a
// turn in this record is proof the audio survived the whole path. A turn the
// speaker generated and the listener never heard does not appear, which is the
// correct behaviour - it did not happen.
type Turn struct {
	Speaker  string  `json:"speaker"`
	Text     string  `json:"text"`
	AtMS     float64 `json:"at_ms"`
	Overlaps bool    `json:"overlaps,omitempty"`
}

// ToolCall is one call a side made during the conversation.
type ToolCall struct {
	Caller    string          `json:"caller"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	AtMS      float64         `json:"at_ms"`
}

// Conversation is the record of one run.
type Conversation struct {
	Scenario string `json:"scenario"`
	Question string `json:"question,omitempty"`
	Left     string `json:"left"`
	Right    string `json:"right"`

	Turns     []Turn     `json:"turns"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// Said is what each side's own model produced, for comparison against what
	// the other side heard. A large gap between them is a recognition problem
	// rather than a reasoning one, and telling those apart matters.
	Said map[string][]string `json:"said"`

	DurationMS float64 `json:"duration_ms"`
	// SpeechMS is how long each side's voice was reaching the other.
	SpeechMS map[string]float64 `json:"speech_ms"`
	// OverlapMS is how long both voices were reaching each other at once.
	OverlapMS float64 `json:"overlap_ms"`
	// EndedBy says why the conversation stopped: settled, turns, budget, or an
	// error. A scenario that ran out of budget is not the same as one that
	// finished, and a report that could not tell them apart would be useless.
	EndedBy string   `json:"ended_by"`
	Errors  []string `json:"errors,omitempty"`

	Results []CheckResult `json:"checks"`
}

// CheckResult is one check's verdict.
type CheckResult struct {
	Name   string `json:"name"`
	Why    string `json:"why"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// Passed reports whether every check held.
func (conversation Conversation) Passed() bool {
	if len(conversation.Results) == 0 {
		return false
	}
	for _, result := range conversation.Results {
		if !result.Passed {
			return false
		}
	}
	return true
}

// TurnsBy returns the turns one side spoke.
func (conversation Conversation) TurnsBy(speaker string) []Turn {
	var turns []Turn
	for _, turn := range conversation.Turns {
		if turn.Speaker == speaker {
			turns = append(turns, turn)
		}
	}
	return turns
}

// Config points the runner at an endpoint.
type Config struct {
	Endpoint string
	Token    string
	Model    string
	// Logf receives progress. A conversation runs in real time, so a runner
	// that says nothing for two minutes looks identical to one that hung.
	Logf func(format string, args ...any)
}

// Run holds one conversation and judges it.
func Run(ctx context.Context, config Config, scenario Scenario) (Conversation, error) {
	if strings.TrimSpace(config.Endpoint) == "" {
		return Conversation{}, errors.New("a simulation needs an endpoint")
	}
	if config.Logf == nil {
		config.Logf = func(string, ...any) {}
	}
	if scenario.MaxTurns <= 0 {
		scenario.MaxTurns = 12
	}
	if scenario.Budget <= 0 {
		scenario.Budget = 3 * time.Minute
	}
	if scenario.Settle <= 0 {
		scenario.Settle = 12 * time.Second
	}
	if scenario.Left.Opens == scenario.Right.Opens {
		return Conversation{}, errors.New("exactly one side must open the conversation")
	}
	for _, role := range []Role{scenario.Left, scenario.Right} {
		if len(role.Tools) > 0 && role.Tool == nil {
			return Conversation{}, fmt.Errorf(
				"%s declares tools but cannot answer a call; an unanswered call hangs the turn",
				role.Name)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, scenario.Budget)
	defer cancel()

	record := &record{
		started: time.Now(),
		said:    map[string][]string{},
		speech:  map[string]float64{},
	}

	left, err := connect(ctx, config, scenario.Left, record)
	if err != nil {
		return Conversation{}, fmt.Errorf("connect %s: %w", scenario.Left.Name, err)
	}
	defer left.close()
	right, err := connect(ctx, config, scenario.Right, record)
	if err != nil {
		return Conversation{}, fmt.Errorf("connect %s: %w", scenario.Right.Name, err)
	}
	defer right.close()

	// Ear to mouth: what one side says is what the other hears.
	left.peer, right.peer = right, left

	go left.read(ctx, config)
	go right.read(ctx, config)

	link := newLink(left, right, record)
	go link.run(ctx)

	opener := left
	if scenario.Right.Opens {
		opener = right
	}
	config.Logf("%s opens", opener.role.Name)
	if err := opener.open(ctx, opener.role.Cue); err != nil {
		return Conversation{}, fmt.Errorf("open the conversation: %w", err)
	}

	endedBy := wait(ctx, record, scenario, config.Logf)
	link.stop()

	conversation := record.snapshot()
	conversation.Scenario = scenario.Name
	conversation.Question = scenario.Question
	conversation.Left, conversation.Right = scenario.Left.Name, scenario.Right.Name
	conversation.EndedBy = endedBy
	conversation.Results = judge(scenario, conversation)
	return conversation, nil
}

// wait blocks until the conversation is over and says why.
func wait(
	ctx context.Context, record *record, scenario Scenario, logf func(string, ...any),
) string {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	reported := 0
	for {
		select {
		case <-ctx.Done():
			return "budget"
		case <-ticker.C:
			turns, lastActivity, bothSpoke := record.progress()
			if turns > reported {
				for _, turn := range record.turnsFrom(reported) {
					logf("  %-9s %s", turn.Speaker+":", turn.Text)
				}
				reported = turns
			}
			if turns >= scenario.MaxTurns {
				return "turns"
			}
			// Settling only counts once both sides have spoken. A silence
			// before the second side has said anything is a failure to start,
			// not a conversation that finished.
			if bothSpoke && time.Since(lastActivity) >= scenario.Settle {
				return "settled"
			}
		}
	}
}

// judge runs the scenario's checks.
func judge(scenario Scenario, conversation Conversation) []CheckResult {
	results := make([]CheckResult, 0, len(scenario.Checks))
	for _, check := range scenario.Checks {
		passed, detail := check.Pass(conversation)
		results = append(results, CheckResult{
			Name: check.Name, Why: check.Why, Passed: passed, Detail: detail,
		})
	}
	return results
}

// record accumulates what happened, from both sides at once.
type record struct {
	mu      sync.Mutex
	started time.Time

	turns     []Turn
	toolCalls []ToolCall
	said      map[string][]string
	speech    map[string]float64
	overlapMS float64
	errors    []string

	lastActivity time.Time
	spoke        map[string]bool
}

func (record *record) at() float64 { return float64(time.Since(record.started).Milliseconds()) }

func (record *record) heard(speaker, text string, overlapping bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	record.turns = append(record.turns, Turn{
		Speaker: speaker, Text: text, AtMS: record.at(), Overlaps: overlapping,
	})
	if record.spoke == nil {
		record.spoke = map[string]bool{}
	}
	record.spoke[speaker] = true
	record.lastActivity = time.Now()
}

func (record *record) spokenBy(speaker, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	record.said[speaker] = append(record.said[speaker], text)
	record.lastActivity = time.Now()
}

func (record *record) tool(caller, name string, arguments json.RawMessage) {
	record.mu.Lock()
	defer record.mu.Unlock()
	record.toolCalls = append(record.toolCalls, ToolCall{
		Caller: caller, Name: name, Arguments: arguments, AtMS: record.at(),
	})
	record.lastActivity = time.Now()
}

func (record *record) failed(message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	record.errors = append(record.errors, message)
}

// carried notes one 20 ms frame of somebody's voice arriving somewhere.
func (record *record) carried(speaker string, overlapping bool) {
	record.mu.Lock()
	defer record.mu.Unlock()
	record.speech[speaker] += float64(framePeriod.Milliseconds())
	if overlapping {
		// Counted once for the pair rather than once per side, so the number
		// reads as "how long they were talking over each other".
		record.overlapMS += float64(framePeriod.Milliseconds()) / 2
	}
	record.lastActivity = time.Now()
}

func (record *record) progress() (turns int, lastActivity time.Time, bothSpoke bool) {
	record.mu.Lock()
	defer record.mu.Unlock()
	if record.lastActivity.IsZero() {
		lastActivity = record.started
	} else {
		lastActivity = record.lastActivity
	}
	return len(record.turns), lastActivity, len(record.spoke) >= 2
}

func (record *record) turnsFrom(index int) []Turn {
	record.mu.Lock()
	defer record.mu.Unlock()
	if index >= len(record.turns) {
		return nil
	}
	return append([]Turn(nil), record.turns[index:]...)
}

func (record *record) snapshot() Conversation {
	record.mu.Lock()
	defer record.mu.Unlock()
	said := map[string][]string{}
	for speaker, lines := range record.said {
		said[speaker] = append([]string(nil), lines...)
	}
	speech := map[string]float64{}
	for speaker, milliseconds := range record.speech {
		speech[speaker] = milliseconds
	}
	return Conversation{
		Turns:      append([]Turn(nil), record.turns...),
		ToolCalls:  append([]ToolCall(nil), record.toolCalls...),
		Said:       said,
		SpeechMS:   speech,
		OverlapMS:  record.overlapMS,
		DurationMS: record.at(),
		Errors:     append([]string(nil), record.errors...),
	}
}

func encodeFrame(samples []int16) string {
	payload := make([]byte, len(samples)*2)
	for index, sample := range samples {
		payload[index*2] = byte(uint16(sample))
		payload[index*2+1] = byte(uint16(sample) >> 8)
	}
	return base64.StdEncoding.EncodeToString(payload)
}

func decodeSamples(encoded string) []int16 {
	payload, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(payload) < 2 {
		return nil
	}
	samples := make([]int16, len(payload)/2)
	for index := range samples {
		samples[index] = int16(uint16(payload[index*2]) | uint16(payload[index*2+1])<<8)
	}
	return samples
}

var _ = realtimeclient.Event{}
