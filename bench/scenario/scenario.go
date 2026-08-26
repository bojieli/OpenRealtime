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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
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
	// end, extended by AfterMS. Negative means the whole conversation.
	Line int
	// Sight indexes Sees instead, when the moment being checked is something
	// the agent saw. It is one-based so that the zero value keeps meaning
	// "use Line", and it can anchor absolutely where a spoken line cannot:
	// a picture arrives when it is sent, while a sentence takes as long as a
	// synthesiser decides to take.
	Sight int
	// AfterMS extends the window past the end of the line, which is where the
	// interesting part of a pause lives.
	//
	// Negative pulls the window in short of the line's end, which is how
	// interrupting is distinguished from waiting. Without it a check that only
	// asks whether the agent spoke during a long monologue is satisfied by an
	// agent that waited politely until the end - the exact behaviour the
	// interruption is meant to replace.
	AfterMS int
	// FromMS pushes the start of the window past the end of the line, for a
	// check about what happens later rather than about the moment itself.
	//
	// A policy set out loud is answered with a brief "I will" - the voice is
	// told to do exactly that, and the scenarios beside this one depend on it.
	// So a scenario asking whether the agent then waited has to start its
	// window after that answer, or it measures the acknowledgement it asked
	// for.
	FromMS int
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
	// CheckReachedMenu asserts the call ended where it was going.
	//
	// It is the check a fixed recording could not support. Pressing the right
	// key once and pressing it six times sound identical to a script that
	// plays to the end regardless; against a menu that moves, the second lands
	// somewhere with no way back, and the difference is a fact about where the
	// call ended rather than an inference from how many tones went out.
	CheckReachedMenu CheckKind = "reached"
	// CheckAnsweredWithin asserts the agent could be heard within AfterMS of
	// the trigger finishing, in waveform time.
	//
	// It is separate from CheckSpoke because they fail differently and want
	// different windows: speaking at all is a capability, and speaking soon
	// enough is whether anybody would want to use it. A system can pass every
	// other check here and be unbearable.
	CheckAnsweredWithin CheckKind = "answered-within"
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

// Sight is something the agent sees, at a moment, with nobody speaking.
//
// It is a file rather than bytes because the point of these scenarios is that
// a real narrator looks at a real picture: a scenario that handed the runtime
// a description it had written itself would be testing nothing but whether the
// runtime can read its own input back.
type Sight struct {
	AtMS int
	// Path is a PNG on disk.
	Path string
	Note string
}

// Scenario is one scripted conversation and what must be true of it.
type Scenario struct {
	Name         string
	Note         string
	Instructions string
	Tools        []Tool
	Script       []Line
	// Sees are visual events on the same timeline as the speech. A capability
	// that fires with nobody talking cannot be scripted any other way.
	Sees   []Sight
	Checks []Check
	// Menu builds a world that answers, for a scenario where what happens next
	// depends on what the agent did. A phone menu is the case: pressing a key
	// moves the call, and a fixed recording cannot tell an agent that pressed
	// once from one that pressed six times.
	//
	// A constructor rather than a menu, because a scenario is a description
	// and each run needs its own world. Held as a live menu it was built once
	// for the whole suite and every repeat inherited where the last one left
	// the call: one run pressed 1 and reached billing, and the four runs after
	// it pressed 2 correctly and were told 2 was not one of the options,
	// because they were already in billing and billing has no options. Four
	// failures reported against the agent belonged to the harness.
	Menu func() *Menu
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
	Latencies  []Latency        `json:"latencies,omitempty"`
	Transcript bench.Transcript `json:"transcript"`
}

// Latency is how long after something happened the agent could be heard.
//
// Measured in waveform time: from the last sample of the thing that triggered
// it to the first sample the agent produced. That is the number a person in
// the room experiences, and it is not the same as the time from the decision -
// a decision taken in thirty milliseconds is still half a second of silence if
// the recogniser, the voice and the synthesiser each take their share.
//
// The arrival of an audio event stands in for the moment it is heard, which is
// exact enough while playback is realtime and worth naming as a proxy rather
// than presenting as a measurement of a loudspeaker.
type Latency struct {
	// After names what triggered it: a line of the script, or something seen.
	After string `json:"after"`
	// EndedMS is when that thing finished, in the playback's own clock.
	EndedMS int `json:"ended_ms"`
	// MS is the wait. Negative means the agent was already speaking, which is
	// not a latency and is recorded rather than hidden.
	MS float64 `json:"ms"`
	// Heard is false when the agent never spoke after it at all.
	Heard bool `json:"heard"`
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
	// Sights is when each visual event was scheduled.
	Sights  []int
	TotalMS int
}

// Compose lays the script out on a timeline.
//
// Everything mixes into one channel because that is what a microphone in a
// room produces, and because the agent's ability to tell two speakers apart is
// part of what is under test rather than something the harness should do for
// it. A line that runs longer than its slot simply overlaps the next one,
// which is also what happens in a room.
// breathMS is the least quiet between two lines from the same speaker.
//
// Long enough that the endpoint gate closes between them at any setting a
// deployment would use, and short enough to still be a breath rather than a
// pause somebody would read as the end of the conversation.
const breathMS = 600

func Compose(ctx context.Context, voice Voice, item Scenario) (Timeline, error) {
	const rate = 24_000
	total := item.TrailingMS
	for _, sight := range item.Sees {
		if sight.AtMS > total {
			total = sight.AtMS
		}
	}
	spoken := make([][]int16, len(item.Script))
	spans := make([]Span, len(item.Script))
	// Where the last line finished, whoever said it. The scenarios that are
	// about simultaneous speech are about the agent talking over somebody, or
	// somebody talking over the agent; every script here is a conversation
	// between people taking turns, written seconds apart.
	finished := 0
	spokenYet := false
	for index, line := range item.Script {
		samples, err := voice.Speak(ctx, line.Speaker, line.Text)
		if err != nil {
			return Timeline{}, fmt.Errorf("synthesise %q: %w", line.Text, err)
		}
		spoken[index] = samples
		start := line.AtMS
		// A script says when a line starts, against durations the author heard
		// from whatever synthesiser was in use that day. A slower one runs the
		// lines together, and a story told in three sentences eight seconds
		// apart becomes one utterance twenty-seven seconds long that never
		// reaches an endpoint - measured, the agent heard the first sentence
		// and nothing after it for the rest of the scenario. Across speakers
		// it is worse than a lost endpoint: the caller asking for the call and
		// the recording answering it arrived as one sentence, "and find out
		// where my order has got to Thank you for calling Press one for
		// billing", and the agent pressed a key at the person who asked.
		if spokenYet && finished+breathMS > start {
			start = finished + breathMS
		}
		spans[index] = Span{StartMS: start, EndMS: start + len(samples)*1000/rate}
		finished, spokenYet = spans[index].EndMS, true
		if spans[index].EndMS > total {
			total = spans[index].EndMS
		}
	}
	if item.TrailingMS > 0 {
		total += item.TrailingMS
	}
	mixed := make([]int16, total*rate/1000)
	for index, samples := range spoken {
		offset := spans[index].StartMS * rate / 1000
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
	sights := make([]int, len(item.Sees))
	for index, sight := range item.Sees {
		sights[index] = sight.AtMS
	}
	return Timeline{Samples: mixed, Spans: spans, Sights: sights, TotalMS: total}, nil
}

// Play runs one scenario and scores it.
func Play(ctx context.Context, voice Voice, config bench.SessionConfig, item Scenario) (Result, error) {
	timeline, err := Compose(ctx, voice, item)
	if err != nil {
		return Result{Scenario: item.Name}, err
	}
	config.Instructions = item.Instructions
	config.Tools = declare(item.Tools)
	// A scenario is about when the agent acts, not about what the world
	// answers, so a call succeeds with nothing in it - unless the scenario
	// brought a world that answers, which is what a phone menu is.
	var menu *Menu
	if item.Menu != nil {
		menu = item.Menu()
	}
	config.Respond = func(name string, arguments json.RawMessage) (json.RawMessage, error) {
		if menu != nil {
			return menu.Respond(name, arguments)
		}
		return json.RawMessage(`{"ok":true}`), nil
	}
	config.Realtime = true
	scheduled, err := sights(item.Sees)
	if err != nil {
		return Result{Scenario: item.Name}, err
	}
	config.Scheduled = scheduled
	if config.TrailingSilence == 0 {
		config.TrailingSilence = time.Duration(item.TrailingMS) * time.Millisecond
	}
	transcript, err := bench.PlaySamples(ctx, config, timeline.Samples)
	if err != nil {
		return Result{Scenario: item.Name, Transcript: transcript}, err
	}
	result := score(item, timeline, transcript, menu)
	result.Latencies = latencies(item, timeline, transcript)
	return result, nil
}

// Score applies a scenario's checks to what happened.
func Score(item Scenario, timeline Timeline, transcript bench.Transcript) Result {
	var menu *Menu
	if item.Menu != nil {
		menu = item.Menu()
	}
	return score(item, timeline, transcript, menu)
}

// score is Score against a menu that has already been played, which is the
// only way the menu checks mean anything: Score building its own would score a
// call nobody made.
func score(item Scenario, timeline Timeline, transcript bench.Transcript, menu *Menu) Result {
	result := Result{Scenario: item.Name, Transcript: transcript, Passed: true}
	for _, check := range item.Checks {
		if failure := apply(check, timeline, transcript, menu); failure != "" {
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

func apply(check Check, timeline Timeline, transcript bench.Transcript, menu *Menu) string {
	from, to := 0, timeline.TotalMS
	switch {
	case check.Sight > 0 && check.Sight <= len(timeline.Sights):
		from = timeline.Sights[check.Sight-1]
		to = from + check.AfterMS
		if to <= from {
			to = timeline.TotalMS
		}
	case check.Line >= 0 && check.Line < len(timeline.Spans):
		span := timeline.Spans[check.Line]
		from, to = span.StartMS, span.EndMS+check.AfterMS
		if check.FromMS != 0 {
			from = span.EndMS + check.FromMS
		}
		if to <= from {
			to = from + 1
		}
	}
	// A latency is measured from the moment the trigger stopped, not from the
	// moment it started: what a person waits through is the silence after
	// somebody finishes talking.
	//
	// Unless the answer is an act rather than speech. A key is pressed while
	// the recording is still reading the options - that is the whole of what
	// this scenario is about, and the note on the check says so - so measuring
	// the wait from the end asks the agent to wait for the one thing it is
	// being told not to wait for. Measured, the agent pressed 2 at 14.1
	// seconds, reached order status, and the check reported that it never
	// answered, because the line it was answering ran until 17.9.
	if check.Kind == CheckAnsweredWithin {
		if check.Sight > 0 && check.Sight <= len(timeline.Sights) {
			from = timeline.Sights[check.Sight-1]
		} else if check.Line >= 0 && check.Line < len(timeline.Spans) {
			span := timeline.Spans[check.Line]
			from = span.EndMS
			if check.Tool != "" {
				from = span.StartMS
			}
		}
	}
	where := fmt.Sprintf("%d-%dms", from, to)
	switch check.Kind {
	case CheckSilent:
		// Audio from turns that began in the window, not audio playing in it.
		// The question is whether something here made the agent speak, and a
		// sentence already under way when the window opened is not an answer
		// to it.
		if audio := transcript.AudioStartedBetween(float64(from), float64(to)); audio > audibleMS {
			return fmt.Sprintf("spoke %.0fms during %s, which should have been silent (%s)", audio, where, check.Note)
		}
	case CheckSpoke:
		if audio := transcript.AudioBetween(float64(from), float64(to)); audio <= audibleMS {
			return fmt.Sprintf("said nothing during %s (%s)", where, check.Note)
		}
	case CheckAnsweredWithin:
		// FirstAudioAfter returns the wait, not the moment. Subtracting the
		// offset again turned every late reply into a large negative number
		// and every latency bound into one nothing could fail.
		//
		// When the check names a tool the answer is that tool being called,
		// not the agent speaking. A phone menu is the case: the useful act is
		// silent by design and the scenario says so in the check above this
		// one, so measuring the wait as a wait for speech asks the agent to
		// fail one check or the other. Its own note is about the key - "a menu
		// moves on, and a key pressed after it has is pressed into the next
		// option" - and the key is what gets measured.
		wait, ok := transcript.FirstAudioAfter(float64(from))
		// The deadline is still "not later than AfterMS past the trigger
		// ending". Measuring from the start moves where the clock starts, not
		// when the agent is late.
		deadline := float64(check.AfterMS)
		if check.Tool != "" {
			wait, ok = transcript.FirstToolCallAfter(check.Tool, float64(from))
			if check.Line >= 0 && check.Line < len(timeline.Spans) {
				span := timeline.Spans[check.Line]
				deadline += float64(span.EndMS - span.StartMS)
			}
		}
		if !ok {
			return fmt.Sprintf("never answered after %dms (%s)", from, check.Note)
		}
		if wait > deadline {
			return fmt.Sprintf("answered %.0fms after %dms, later than %.0fms (%s)",
				wait, from, deadline, check.Note)
		}
	case CheckReachedMenu:
		if menu == nil {
			return ""
		}
		if !menu.Reached() {
			return fmt.Sprintf("the call ended at %s after %d presses (%s)",
				menu.Where(), menu.Presses(), check.Note)
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

// sights turns the visual events into protocol sends.
//
// They go as input_image content on a conversation item, which is the shape
// the official clients produce and the one the gateway already accepts. The
// harness does not narrate them: the deployment's own observer looks at the
// picture and writes what it sees, which is the thing under test.
func sights(seen []Sight) ([]bench.ScheduledEvent, error) {
	if len(seen) == 0 {
		return nil, nil
	}
	events := make([]bench.ScheduledEvent, 0, len(seen))
	for _, sight := range seen {
		payload, err := os.ReadFile(sight.Path)
		if err != nil {
			return nil, fmt.Errorf("read the frame for %dms: %w", sight.AtMS, err)
		}
		events = append(events, bench.ScheduledEvent{
			AtMS: sight.AtMS,
			Event: map[string]any{
				"type": "conversation.item.create",
				"item": map[string]any{
					"type": "message", "role": "user",
					"content": []map[string]any{{
						"type":      "input_image",
						"image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(payload),
					}},
				},
			},
		})
	}
	return events, nil
}

// latencies measures the wait after every trigger in a scenario.
//
// Every line and every sight is a candidate, because which of them the agent
// was answering is not something the harness can know - and reporting all of
// them is more honest than guessing at one. A trigger the agent never answered
// is reported as unheard rather than dropped, since a scenario whose latencies
// all look excellent because the slow ones vanished is worse than no numbers.
func latencies(item Scenario, timeline Timeline, transcript bench.Transcript) []Latency {
	measured := make([]Latency, 0, len(timeline.Spans)+len(timeline.Sights))
	measure := func(after string, endedMS int) {
		wait, ok := transcript.FirstAudioAfter(float64(endedMS))
		entry := Latency{After: after, EndedMS: endedMS, Heard: ok, MS: wait}
		measured = append(measured, entry)
	}
	for index, span := range timeline.Spans {
		who := "user"
		if index < len(item.Script) && item.Script[index].Speaker != "" {
			who = item.Script[index].Speaker
		}
		measure(fmt.Sprintf("%s said line %d", who, index), span.EndMS)
	}
	for index, at := range timeline.Sights {
		measure(fmt.Sprintf("saw frame %d", index+1), at)
	}
	return measured
}
