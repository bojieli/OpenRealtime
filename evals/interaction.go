package evals

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/cognition"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// The acts an interaction model may choose. Each selects machinery that
// already exists rather than introducing any: speak-through and interrupt
// differ from answer only in what happens to the floor, and call-tool is the
// phase that acts and cannot speak, which is an authority rule the runtime
// already enforces.
const (
	ActListen       Action = "listen"
	ActSpeakThrough Action = "speak-through"
	ActAnswer       Action = "answer"
	ActInterrupt    Action = "interrupt"
	ActCallTool     Action = "call-tool"
	ActKeepSpeaking Action = "keep-speaking"
	ActStopSpeaking Action = "stop-speaking"
	ActUnparsed     Action = "unparsed"
)

var interactionActs = []Action{
	ActListen, ActSpeakThrough, ActAnswer, ActInterrupt,
	ActCallTool, ActKeepSpeaking, ActStopSpeaking,
}

// situation is the context an interaction model decides from.
//
// It is rendered as one labelled block rather than a message list, and that is
// a design decision rather than a convenience. Chat roles describe two parties.
// The cases that matter most here have three - a user, an agent, and a waiter
// or a phone menu - and flattening a third speaker into "user" would erase the
// distinction the decision turns on. A block can name whoever is talking.
//
// Who is speaking and what has been heard are separate fields, which they were
// not in the first version of this. Collapsing them hid every completed
// utterance the instant its speaker stopped: the model was told a turn had
// ended and never told what the turn said, and answered "listen" because
// nothing had been put in front of it to answer.
type situation struct {
	// pins are standing instructions someone gave out loud, each carrying its
	// own age. They are outside the conversation window because truncating the
	// window must never silently repeal a policy.
	pins []string
	// recent is the rolling conversation window, oldest first.
	recent []string
	// agentSpeaking says whether the agent's own voice is playing, and
	// agentSaying is what it has said so far in that sentence. This decides
	// which acts are even available: an agent that is not speaking cannot keep
	// speaking, and one that is mid-sentence is not choosing whether to start.
	agentSpeaking bool
	agentSaying   string
	// speaker names whoever else is involved, empty when nobody is. speaking
	// says whether they are talking this instant; heard is what they have said,
	// whether or not they have finished saying it.
	speaker  string
	speaking bool
	heard    string
	silence  string
	// tools names what the agent could actually do without speaking. Offering
	// call-tool while naming no tool asks the model to choose an act it has no
	// way to know is possible, which is how the phone-menu case failed: the
	// key it needed to press was never mentioned to it.
	tools []string
	// inflight is work already running: a tool call, a reasoning pass. It is
	// what stops the model deciding the same thing twice.
	inflight string
	lastSeen string
}

// availableActs is the act set this instant admits.
//
// The runtime knows which acts are meaningful and there is no reason to make
// the model rediscover it. Offering "listen" to an agent that is mid-sentence
// invites an answer that means nothing, and the first measurement of this
// suite produced exactly that: two of the failures were a speaking agent being
// told to "answer".
func (state situation) availableActs() []Action {
	acts := []Action{ActKeepSpeaking, ActStopSpeaking}
	if !state.agentSpeaking {
		acts = []Action{ActListen, ActSpeakThrough, ActAnswer, ActInterrupt}
	}
	if len(state.tools) > 0 {
		acts = append(acts, ActCallTool)
	}
	return acts
}

func (state situation) render() string {
	var block strings.Builder
	if len(state.pins) > 0 {
		block.WriteString("Standing instructions:\n")
		for _, pin := range state.pins {
			block.WriteString("- " + pin + "\n")
		}
		block.WriteString("\n")
	}
	if len(state.recent) > 0 {
		block.WriteString("Recent conversation:\n")
		for _, line := range state.recent {
			block.WriteString(line + "\n")
		}
		block.WriteString("\n")
	}
	block.WriteString("Now:\n")
	if state.agentSpeaking {
		block.WriteString("agent: speaking out loud right now, has said \"" + state.agentSaying + "\" so far\n")
	} else {
		block.WriteString("agent: not speaking\n")
	}
	who := fallback(state.speaker, "user")
	switch {
	case state.speaking:
		block.WriteString(who + ": speaking right now\n")
	case state.heard != "":
		block.WriteString(who + ": stopped speaking " + fallback(state.silence, "0ms") + " ago\n")
	default:
		block.WriteString("nobody else is speaking\n")
	}
	if state.heard != "" {
		block.WriteString("heard from " + who + " so far: \"" + state.heard + "\"\n")
	}
	block.WriteString("silence: " + fallback(state.silence, "0ms") + "\n")
	block.WriteString("work in flight: " + fallback(state.inflight, "nothing") + "\n")
	if len(state.tools) > 0 {
		block.WriteString("tools the agent can use without speaking: " + strings.Join(state.tools, ", ") + "\n")
	}
	if state.lastSeen != "" {
		block.WriteString("last seen: " + state.lastSeen + "\n")
	}
	acts := state.availableActs()
	names := make([]string, len(acts))
	for index, action := range acts {
		names[index] = string(action)
	}
	block.WriteString("\nAvailable acts right now: " + strings.Join(names, ", "))
	return block.String()
}

func fallback(value, whenEmpty string) string {
	if strings.TrimSpace(value) == "" {
		return whenEmpty
	}
	return value
}

// InteractionCases freeze the decision the whole redesign exists for.
//
// The suite is built around *paired* cases: two cases with an identical live
// edge, differing only in what someone said earlier, whose correct acts
// differ. A model that ignores the conversation scores 50% on every pair by
// construction, so the pair rate measures the one property a turn-taking
// predicate cannot have - that the policy is something the people in the room
// can change by saying so.
//
// The rest of the families come from behaviour a working system has to show:
// that silence is neither necessary nor sufficient to act, that a visual
// change can be worth speaking about when nobody is talking at all, that work
// already running must not be started twice, and - most of the volume - that
// the ordinary answer is to do nothing.
func InteractionCases() []Case {
	act := func(name string, state situation, note string, accept []Action, forbid ...Action) Case {
		return Case{
			Name: name, Decision: DecisionInteraction, Context: state.render(),
			Accept: accept, Forbid: forbid, Note: note,
		}
	}
	story := []string{
		"user: I'm going to tell you about my afternoon - count the animals out loud as I mention them",
		"agent: okay",
	}
	return []Case{
		// --- paired: a spoken instruction changes the act ---
		act("count-animal-heard", situation{
			pins:    []string{"count the animals out loud as I mention them (2m ago)"},
			recent:  story,
			speaker: "user", speaking: true,
			heard: "it was a warm afternoon and the capybara wandered over",
		}, "the pinned instruction is the only reason to speak here",
			[]Action{ActSpeakThrough}, ActAnswer, ActInterrupt, ActStopSpeaking),

		act("count-animal-heard-unpinned", situation{
			recent:  []string{"user: I'm going to tell you about my afternoon", "agent: okay"},
			speaker: "user", speaking: true,
			heard: "it was a warm afternoon and the capybara wandered over",
		}, "same edge, no instruction: interjecting would be inexplicable",
			[]Action{ActListen}, ActSpeakThrough, ActAnswer, ActInterrupt),

		act("count-pause-no-animal", situation{
			pins:    []string{"count the animals out loud as I mention them (2m ago)"},
			recent:  story,
			silence: "900ms",
			heard:   "it was a warm afternoon and",
		}, "a pause mid-thought during a story someone asked to tell uninterrupted",
			[]Action{ActListen}, ActAnswer, ActInterrupt, ActSpeakThrough),

		act("pause-after-question", situation{
			recent:  []string{"user: I need to check something"},
			silence: "900ms",
			heard:   "can you look up my last order",
		}, "same silence, a finished question, and nothing asking for restraint",
			[]Action{ActAnswer}, ActListen, ActInterrupt),

		// --- paired: an instruction to interject the moment something happens ---
		act("build-finished-pinned", situation{
			pins:    []string{"tell me the moment the build finishes (4m ago)"},
			recent:  []string{"user: I'll keep talking while it runs", "agent: understood"},
			speaker: "user", speaking: true,
			heard:    "so the other thing I wanted to mention was the",
			inflight: "nothing",
			lastSeen: "build finished successfully (0.2s ago)",
		}, "explicitly asked to be told immediately, even mid-sentence",
			[]Action{ActInterrupt, ActSpeakThrough}, ActListen),

		act("build-finished-unpinned", situation{
			recent:  []string{"user: I'll keep talking while it runs", "agent: understood"},
			speaker: "user", speaking: true,
			heard:    "so the other thing I wanted to mention was the",
			lastSeen: "build finished successfully (0.2s ago)",
		}, "same event, nobody asked to be interrupted for it",
			[]Action{ActListen}, ActInterrupt, ActAnswer),

		// --- silence is not a trigger ---
		act("short-pause-mid-sentence", situation{
			recent:  []string{"agent: what can I help with?"},
			silence: "300ms",
			heard:   "I think we should probably",
		}, "300ms mid-clause is breathing, not a turn ending",
			[]Action{ActListen}, ActAnswer, ActInterrupt),

		act("long-silence-complete", situation{
			recent:  []string{"agent: what can I help with?"},
			silence: "1800ms",
			heard:   "I'd like to cancel my subscription",
		}, "complete request, nearly two seconds: waiting longer is a bug",
			[]Action{ActAnswer}, ActListen),

		// --- content is a trigger, with no pause at all ---
		act("restaurant-heard-the-dish", situation{
			recent: []string{
				"user: I want fish, order for me when you hear something good",
				"other: tonight we have three specials",
			},
			speaker: "other", speaking: true,
			heard:   "the second is a duck confit and the third is sea bass with fennel",
			silence: "0ms",
		}, "the awaited thing was said and the speaker has not paused",
			[]Action{ActInterrupt, ActSpeakThrough}, ActListen),

		act("restaurant-nothing-yet", situation{
			recent: []string{
				"user: I want fish, order for me when you hear something good",
				"other: tonight we have three specials",
			},
			speaker: "other", speaking: true,
			heard:   "the first is a ribeye with peppercorn sauce",
			silence: "0ms",
		}, "same shape, nothing worth acting on yet",
			[]Action{ActListen}, ActInterrupt, ActAnswer),

		act("ivr-heard-the-option", situation{
			recent: []string{
				"user: call them and find out where my order is",
				"agent: dialling now",
			},
			speaker: "other", speaking: true,
			heard:   "press 1 for billing, press 2 for order status, press 3 for",
			silence: "0ms",
			tools:   []string{"press_key(digit) - send a keypad tone on the open call"},
		}, "a phone menu: the act is pressing a key, and saying anything is useless",
			[]Action{ActCallTool}, ActAnswer, ActInterrupt, ActSpeakThrough),

		// --- work already in flight must not be started twice ---
		act("ivr-already-pressed", situation{
			recent: []string{
				"user: call them and find out where my order is",
				"agent: dialling now",
			},
			speaker: "other", speaking: true,
			heard:    "press 4 for technical support, press 5 to repeat",
			inflight: "press_key(2) sent 0.3s ago, awaiting result",
			silence:  "0ms",
			tools:    []string{"press_key(digit) - send a keypad tone on the open call"},
		}, "the menu keeps talking; the key is already sent and must not be sent again",
			[]Action{ActListen}, ActCallTool, ActInterrupt),

		// --- visual trigger, nobody speaking ---
		act("posture-slouching", situation{
			pins:     []string{"tell me if I slouch (6m ago)"},
			recent:   []string{"user: I'm going to work for a while"},
			silence:  "45s",
			lastSeen: "the person's back is curved forward over the desk (0.5s ago)",
		}, "nobody is talking: a visual change is the only evidence there is",
			[]Action{ActAnswer, ActSpeakThrough}, ActListen),

		act("posture-upright", situation{
			pins:     []string{"tell me if I slouch (6m ago)"},
			recent:   []string{"user: I'm going to work for a while"},
			silence:  "45s",
			lastSeen: "the person is sitting upright at the desk (0.5s ago)",
		}, "the same instruction with the condition absent",
			[]Action{ActListen}, ActAnswer, ActInterrupt, ActSpeakThrough),

		// --- overlap: the agent is mid-sentence ---
		act("overlap-backchannel", situation{
			recent:        []string{"user: what did you find?"},
			agentSpeaking: true, agentSaying: "I checked the order and it shipped on",
			speaker: "user", speaking: true,
			heard: "mhm",
		}, "an acknowledgement is not a bid for the floor",
			[]Action{ActKeepSpeaking}, ActStopSpeaking, ActAnswer),

		act("overlap-real-interruption", situation{
			recent:        []string{"user: what did you find?"},
			agentSpeaking: true, agentSaying: "I checked the order and it shipped on",
			speaker: "user", speaking: true,
			heard: "no wait that's the wrong order",
		}, "a correction: continuing would talk over something that matters",
			[]Action{ActStopSpeaking}, ActKeepSpeaking, ActListen),

		act("overlap-while-translating", situation{
			pins:          []string{"translate everything he says into English as he goes (3m ago)"},
			recent:        []string{"other: guten Tag", "agent: good afternoon"},
			agentSpeaking: true, agentSaying: "the meeting starts at",
			speaker: "other", speaking: true,
			heard: "und wir treffen uns um drei Uhr",
		}, "the instruction is to speak over him continuously; yielding defeats it",
			[]Action{ActKeepSpeaking}, ActStopSpeaking, ActListen),

		// --- interrupting to correct ---
		act("correct-a-wrong-fact", situation{
			recent: []string{
				"agent: the deadline they gave is the 3rd",
				"user: right, so let me plan this out",
			},
			speaker: "user", speaking: true,
			heard:   "we'll do the review next week and ship it by the 13th which gives us",
			silence: "0ms",
		}, "the plan is built on a date the agent just corrected; later is worse",
			[]Action{ActInterrupt, ActSpeakThrough}, ActListen),

		act("plan-is-consistent", situation{
			recent: []string{
				"agent: the deadline they gave is the 3rd",
				"user: right, so let me plan this out",
			},
			speaker: "user", speaking: true,
			heard:   "we'll do the review this week and ship it by the 3rd which gives us",
			silence: "0ms",
		}, "the same monologue with nothing wrong in it",
			[]Action{ActListen}, ActInterrupt, ActAnswer, ActStopSpeaking),

		// --- restraint: doing nothing is the common answer ---
		act("nothing-happening", situation{
			recent:  []string{"user: thanks", "agent: anytime"},
			silence: "12s",
		}, "an idle line: acting here would be inventing a reason to talk",
			[]Action{ActListen}, ActAnswer, ActSpeakThrough, ActInterrupt),

		act("agent-speaking-alone", situation{
			recent:        []string{"user: tell me what you found"},
			agentSpeaking: true, agentSaying: "the refund was issued on the fourth and",
			silence: "0ms",
		}, "nobody else is talking and nothing is in flight",
			[]Action{ActKeepSpeaking}, ActStopSpeaking, ActAnswer, ActInterrupt),

		act("user-mid-sentence-ordinary", situation{
			recent:  []string{"agent: what can I help with?"},
			speaker: "user", speaking: true,
			heard:   "I was looking at my account earlier and I noticed that the",
			silence: "0ms",
		}, "an ordinary sentence in progress with nothing pinned",
			[]Action{ActListen}, ActAnswer, ActInterrupt, ActSpeakThrough),

		act("deliberating-and-asked", situation{
			recent:   []string{"user: where is my order?", "agent: let me check that"},
			silence:  "4s",
			inflight: "reasoning pass running 4.1s, no result yet",
		}, "the user has heard four seconds of nothing while work runs",
			[]Action{ActSpeakThrough, ActAnswer}, ActCallTool, ActInterrupt),
	}
}

// actPattern finds an act name as a whole token. A model that reasons before
// answering mentions several; the last one is its conclusion.
var actPattern = regexp.MustCompile(`(?i)\b(listen|speak-through|answer|interrupt|call-tool|keep-speaking|stop-speaking)\b`)

var thinkPattern = regexp.MustCompile(`(?is)<think>.*?</think>`)

// ParseAct reads an act out of whatever a model returned.
//
// It is deliberately forgiving about surrounding text and deliberately strict
// about the vocabulary: an unrecognised answer becomes ActUnparsed rather than
// a guess, because a decision layer that silently invents an act when the
// model said something else is one that cannot be measured.
func ParseAct(text string) Action {
	text = thinkPattern.ReplaceAllString(text, " ")
	found := actPattern.FindAllString(text, -1)
	if len(found) == 0 {
		return ActUnparsed
	}
	return Action(strings.ToLower(found[len(found)-1]))
}

// InteractionRunner asks one model what the agent should do in an instant.
type InteractionRunner struct {
	Provider continuation.Provider
	Label    string
}

func (runner InteractionRunner) Name() string       { return runner.Label }
func (runner InteractionRunner) Decision() Decision { return DecisionInteraction }

func (runner InteractionRunner) Observe(ctx context.Context, item Case) Observation {
	started := time.Now()
	request := continuation.Request{
		Descriptor: runner.Provider.Descriptor(),
		Invocation: continuation.Invocation{
			Instruction: cognition.InteractionInstruction,
			// The act is one word. The budget is generous enough that a model
			// which insists on reasoning is measured on its conclusion rather
			// than truncated into an unparsed answer, which would confuse "got
			// it wrong" with "was cut off".
			MaxOutputTokens: 512,
		},
		Trajectory: trajectory.Snapshot{Items: []trajectory.Item{{
			ID: "situation", Kind: trajectory.KindObservation, MonotonicNS: 1,
			Content: item.Context, Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
		}}},
	}
	var said strings.Builder
	_, err := runner.Provider.Continue(ctx, request, func(event continuation.Event) error {
		if event.Kind == continuation.EventAssistantDelta {
			said.WriteString(event.Text)
		}
		return nil
	})
	text, _ := continuation.StripMarkers(said.String())
	return Observation{
		Actions: []Action{ParseAct(text)},
		Text:    strings.TrimSpace(text), Elapsed: time.Since(started), Err: err,
	}
}
