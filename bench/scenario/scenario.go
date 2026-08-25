// Package scenario is the end-to-end layer for interaction behaviour.
//
// The suites beside it play a recording somebody else made and score the
// outcome of a task. That answers "did the work get done" and says nothing
// about when anybody spoke, which is the entire subject here. A scenario
// instead scripts a conversation - who says what, and at what moment - so that
// what is scored is the shape of the exchange rather than its conclusion.
//
// The behaviour it exists to test is behaviour no existing suite can express.
// Counting out loud while somebody keeps talking, staying quiet through their
// pauses because they asked to be left alone, pressing a key at a recorded
// menu, cutting in when what is being said is wrong: each of those is a claim
// about timing that a transcript comparison cannot make.
package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
)

// Line is one thing somebody says, and when they start saying it.
//
// Speaker names whoever is talking. It exists because the interesting cases
// have three parties - a user, an agent, and a waiter or a phone menu - and
// they reach the agent down one microphone, exactly as they would in a room.
type Line struct {
	Speaker string
	Text    string
	AtMS    int
}

// Check is one claim about what the agent did.
//
// Windows are expressed against lines of the script rather than against the
// clock, because how long a line takes to say is decided by a speech
// synthesiser and moves when the voice does. A check pinned to a millisecond
// would pass or fail on the length of a vowel.
type Check struct {
	Kind CheckKind
	// Line indexes the script. The window runs from that line's start to its
	// end, extended by AfterMS.
	Line int
	// AfterMS extends the window past the end of the line, which is where the
	// interesting part of a pause lives.
	//
	// Negative pulls the window in short of the line's end, which is how
	// interrupting is distinguished from waiting. Without it a check that only
	// asks whether the agent spoke during a long monologue is satisfied by an
	// agent that waited politely until the end - the exact behaviour the
	// interruption is meant to replace.
	AfterMS int
	// Tool is the call that must have happened, for CheckToolCalled.
	Tool string
	// Any is a set of phrases, one of which must appear in what the agent
	// said, for CheckSaid.
	Any  []string
	Note string
}

// CheckKind names what a check asserts.
type CheckKind string

const (
	// CheckSilent asserts the agent produced no audio in a window. It is the
	// check most of these scenarios turn on, because the failure it catches -
	// talking over somebody who asked not to be interrupted - is the one that
	// makes a system unpleasant rather than merely wrong.
	CheckSilent CheckKind = "silent"
	// CheckSpoke asserts the agent produced audio in a window.
	CheckSpoke CheckKind = "spoke"
	// CheckToolCalled asserts a named tool was called.
	CheckToolCalled CheckKind = "tool"
	// CheckSaid asserts the agent said one of a set of phrases, inside a
	// window when one is given. The window is what makes it mean anything: a
	// scenario that asks an agent to count out loud is not satisfied by the
	// word "two" turning up somewhere in half a minute of unrelated talk, and
	// the first version of this suite passed exactly that way.
	//
	// The phrases stay loose. The words are the model's to choose, and a
	// scenario that pins them is testing its vocabulary.
	CheckSaid CheckKind = "said"
	// CheckNotSaid asserts the agent said none of a set of phrases in a
	// window. It catches the failure that looks like success: speaking at the
	// right moment, about the wrong thing.
	CheckNotSaid CheckKind = "not-said"
)

// saidBetween is what the agent said inside a window, or everything it said
// when the check names no line.
func saidBetween(transcript bench.Transcript, line, fromMS, toMS int) string {
	if line < 0 {
		return strings.Join(transcript.AgentTurns(), " ")
	}
	var said []string
	for _, moment := range transcript.Moments {
		if moment.Kind != bench.MomentAgentText || strings.TrimSpace(moment.Text) == "" {
			continue
		}
		if moment.AtMS >= float64(fromMS) && moment.AtMS <= float64(toMS) {
			said = append(said, moment.Text)
		}
	}
	return strings.Join(said, " ")
}

func truncateSaid(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 90 {
		return text[:90] + "…"
	}
	return text
}

// Scenario is one scripted conversation and what must be true of it.
type Scenario struct {
	Name         string
	Note         string
	Instructions string
	Tools        []Tool
	Script       []Line
	Checks       []Check
	// TrailingMS is quiet held after the last line, so that a scenario about
	// staying silent has somewhere to be silent.
	TrailingMS int
}

// Tool is a capability declared to the session.
type Tool struct {
	Name        string
	Description string
	Parameters  []string
}

// Result is one scenario played.
type Result struct {
	Scenario   string           `json:"scenario"`
	Passed     bool             `json:"passed"`
	Failures   []string         `json:"failures,omitempty"`
	Transcript bench.Transcript `json:"transcript"`
}

// Voice synthesises a line of speech as 24 kHz mono samples.
//
// Different speakers get different voices where a backend offers them, because
// a scenario with a waiter in it is not testing anything if the waiter and the
// user are indistinguishable to the recogniser.
type Voice interface {
	Speak(ctx context.Context, speaker, text string) ([]int16, error)
}

// Span is when one line of the script was actually spoken.
type Span struct {
	StartMS, EndMS int
}

// Timeline is where every line of a script landed once it was synthesised.
type Timeline struct {
	Samples []int16
	Spans   []Span
	TotalMS int
}

// Compose lays the script out on a timeline.
//
// Everything mixes into one channel because that is what a microphone in a
// room produces, and because the agent's ability to tell two speakers apart is
// part of what is under test rather than something the harness should do for
// it. A line that runs longer than its slot simply overlaps the next one,
// which is also what happens in a room.
func Compose(ctx context.Context, voice Voice, item Scenario) (Timeline, error) {
	const rate = 24_000
	total := item.TrailingMS
	spoken := make([][]int16, len(item.Script))
	spans := make([]Span, len(item.Script))
	for index, line := range item.Script {
		samples, err := voice.Speak(ctx, line.Speaker, line.Text)
		if err != nil {
			return Timeline{}, fmt.Errorf("synthesise %q: %w", line.Text, err)
		}
		spoken[index] = samples
		end := line.AtMS + len(samples)*1000/rate
		spans[index] = Span{StartMS: line.AtMS, EndMS: end}
		if end > total {
			total = end
		}
	}
	if item.TrailingMS > 0 {
		total += item.TrailingMS
	}
	mixed := make([]int16, total*rate/1000)
	for index, samples := range spoken {
		offset := item.Script[index].AtMS * rate / 1000
		for position, sample := range samples {
			if offset+position >= len(mixed) {
				break
			}
			// Saturating mix. Two people talking at once are louder than one,
			// and wrapping the sum would turn an overlap into a burst of noise
			// that the recogniser reads as neither of them.
			sum := int32(mixed[offset+position]) + int32(sample)
			switch {
			case sum > 32767:
				sum = 32767
			case sum < -32768:
				sum = -32768
			}
			mixed[offset+position] = int16(sum)
		}
	}
	return Timeline{Samples: mixed, Spans: spans, TotalMS: total}, nil
}

// Play runs one scenario and scores it.
func Play(ctx context.Context, voice Voice, config bench.SessionConfig, item Scenario) (Result, error) {
	timeline, err := Compose(ctx, voice, item)
	if err != nil {
		return Result{Scenario: item.Name}, err
	}
	config.Instructions = item.Instructions
	config.Tools = declare(item.Tools)
	config.Respond = func(name string, arguments json.RawMessage) (json.RawMessage, error) {
		// A scenario is about when the agent acts, not about what the world
		// answers, so every call succeeds with nothing in it.
		return json.RawMessage(`{"ok":true}`), nil
	}
	config.Realtime = true
	if config.TrailingSilence == 0 {
		config.TrailingSilence = time.Duration(item.TrailingMS) * time.Millisecond
	}
	transcript, err := bench.PlaySamples(ctx, config, timeline.Samples)
	if err != nil {
		return Result{Scenario: item.Name, Transcript: transcript}, err
	}
	return Score(item, timeline, transcript), nil
}

// Score applies a scenario's checks to what happened.
func Score(item Scenario, timeline Timeline, transcript bench.Transcript) Result {
	result := Result{Scenario: item.Name, Transcript: transcript, Passed: true}
	for _, check := range item.Checks {
		if failure := apply(check, timeline, transcript); failure != "" {
			result.Passed = false
			result.Failures = append(result.Failures, failure)
		}
	}
	return result
}

// audibleMS is how much agent audio in a window counts as having spoken.
//
// Not zero. A cancelled turn can leave a few milliseconds of a syllable in the
// record, and a check that treats that as speech reports a failure nobody in
// the room would have heard.
const audibleMS = 120

func apply(check Check, timeline Timeline, transcript bench.Transcript) string {
	from, to := 0, timeline.TotalMS
	if check.Line >= 0 && check.Line < len(timeline.Spans) {
		span := timeline.Spans[check.Line]
		from, to = span.StartMS, span.EndMS+check.AfterMS
		if to <= from {
			to = from + 1
		}
	}
	where := fmt.Sprintf("%d-%dms", from, to)
	switch check.Kind {
	case CheckSilent:
		if audio := transcript.AudioBetween(float64(from), float64(to)); audio > audibleMS {
			return fmt.Sprintf("spoke %.0fms during %s, which should have been silent (%s)", audio, where, check.Note)
		}
	case CheckSpoke:
		if audio := transcript.AudioBetween(float64(from), float64(to)); audio <= audibleMS {
			return fmt.Sprintf("said nothing during %s (%s)", where, check.Note)
		}
	case CheckToolCalled:
		for _, called := range transcript.ToolCalls() {
			if called == check.Tool {
				return ""
			}
		}
		return fmt.Sprintf("never called %s (%s)", check.Tool, check.Note)
	case CheckSaid:
		said := strings.ToLower(saidBetween(transcript, check.Line, from, to))
		for _, phrase := range check.Any {
			if strings.Contains(said, strings.ToLower(phrase)) {
				return ""
			}
		}
		return fmt.Sprintf("said %q during %s, none of %v (%s)",
			truncateSaid(said), where, check.Any, check.Note)
	case CheckNotSaid:
		said := strings.ToLower(saidBetween(transcript, check.Line, from, to))
		for _, phrase := range check.Any {
			if strings.Contains(said, strings.ToLower(phrase)) {
				return fmt.Sprintf("said %q during %s, which contains %q (%s)",
					truncateSaid(said), where, phrase, check.Note)
			}
		}
	}
	return ""
}

func declare(tools []Tool) []json.RawMessage {
	declared := make([]json.RawMessage, 0, len(tools))
	for _, tool := range tools {
		properties := map[string]any{}
		for _, parameter := range tool.Parameters {
			properties[parameter] = map[string]string{"type": "string"}
		}
		encoded, err := json.Marshal(map[string]any{
			"type": "function", "name": tool.Name, "description": tool.Description,
			"parameters": map[string]any{"type": "object", "properties": properties},
		})
		if err != nil {
			continue
		}
		declared = append(declared, encoded)
	}
	return declared
}
