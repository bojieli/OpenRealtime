package evals

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// The acts an interaction model may choose, derived from the runtime's own
// vocabulary rather than restated here. Restating them once already produced a
// silent divergence, and a scoring layer that disagrees with the thing it
// scores about what an answer means is worse than no scoring layer.
const (
	ActStaySilent   = Action(interaction.ActStaySilent)
	ActSpeakThrough = Action(interaction.ActSpeakThrough)
	ActAnswer       = Action(interaction.ActAnswer)
	ActInterrupt    = Action(interaction.ActInterrupt)
	ActCallTool     = Action(interaction.ActCallTool)
	ActKeepSpeaking = Action(interaction.ActKeepSpeaking)
	ActStopSpeaking = Action(interaction.ActStopSpeaking)
	ActUnparsed     = Action("unparsed")
)

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
// interactionStates keeps the structure behind each case. Declaring the cases
// twice - once rendered for one runner and once structured for the other -
// would guarantee they eventually disagree about what is being measured.
var interactionStates = map[string]interaction.Situation{}

func InteractionCases() []Case {
	act := func(name string, state interaction.Situation, note string, accept []Action, forbid ...Action) Case {
		interactionStates[name] = state
		return Case{
			Name: name, Decision: DecisionInteraction, Context: state.Render(),
			Accept: accept, Forbid: forbid, Note: note,
		}
	}
	story := []string{
		"user: I'm going to tell you about my afternoon - count the animals out loud as I mention them",
		"agent: okay",
	}
	return []Case{
		// --- paired: a spoken instruction changes the act ---
		act("count-with-a-nothing-else-clause", interaction.Situation{
			Pins: []string{"count the animals out loud as I mention them and say nothing else (18s ago)"},
			Recent: []string{
				"user: I'm going to tell you about my afternoon, count the animals out loud and say nothing else.",
				"agent: I will count each animal as you mention them.",
				"user: It was a warm afternoon and a capybara wandered over.",
				"agent: One.",
			},
			Speaker: "user", Speaking: true,
			SincePrevious: "2200ms",
			Heard:         "Then, a heron.",
		}, "taken verbatim from a run where this answered listen: the restriction narrows what else may "+
			"be said and does not cancel the count",
			[]Action{ActSpeakThrough, ActInterrupt}, ActStaySilent),

		act("count-animal-heard", interaction.Situation{
			Pins:    []string{"count the animals out loud as I mention them (2m ago)"},
			Recent:  story,
			Speaker: "user", Speaking: true,
			Heard: "it was a warm afternoon and the capybara wandered over",
		}, "the pinned instruction is the only reason to speak here",
			[]Action{ActSpeakThrough}, ActAnswer, ActInterrupt, ActStopSpeaking),

		act("count-a-fragment-of-the-story", interaction.Situation{
			Pins:    []string{"count the animals out loud as I mention them (2m ago)"},
			Recent:  []string{"user: I was walking by the river when a", "agent: one"},
			Speaker: "user", Speaking: true,
			SincePrevious: "380ms",
			Heard:         "And then a heron landed.",
		}, "a broken-up sentence does not suspend a standing instruction; they are not asking any less "+
			"because the recogniser split it",
			[]Action{ActSpeakThrough, ActInterrupt}, ActStaySilent),

		act("count-animal-heard-unpinned", interaction.Situation{
			Recent:  []string{"user: I'm going to tell you about my afternoon", "agent: okay"},
			Speaker: "user", Speaking: true,
			Heard: "it was a warm afternoon and the capybara wandered over",
		}, "same edge, no instruction: interjecting would be inexplicable",
			[]Action{ActStaySilent}, ActSpeakThrough, ActAnswer, ActInterrupt),

		act("count-pause-no-animal", interaction.Situation{
			Pins:    []string{"count the animals out loud as I mention them (2m ago)"},
			Recent:  story,
			Silence: "900ms",
			Heard:   "it was a warm afternoon and",
		}, "a pause mid-thought during a story someone asked to tell uninterrupted",
			[]Action{ActStaySilent}, ActAnswer, ActInterrupt, ActSpeakThrough),

		act("count-pause-complete-sentence", interaction.Situation{
			Pins:    []string{"count the animals out loud as I mention them and say nothing else (2m ago)"},
			Recent:  story,
			Silence: "900ms",
			Heard:   "it was a warm afternoon and I was walking along by the river",
		}, "the hard half of the pair: a finished sentence, a real pause, and a policy that still says nothing",
			[]Action{ActStaySilent}, ActAnswer, ActInterrupt, ActSpeakThrough),

		act("already-agreed-to-this", interaction.Situation{
			Recent: []string{
				"user: I'm going to read for a bit.",
				"agent: Enjoy your reading.",
				"user: Tell me the moment the build.",
				"agent: I will.",
				"user: finishes. and don't.",
			},
			Silence:       "600ms",
			SincePrevious: "480ms",
			Heard:         "Say anything else.",
		}, "a recogniser splits a sentence where the speaker breathes, so one instruction arrives as "+
			"several; the agent has already agreed and agreeing again sounds like it forgot",
			[]Action{ActStaySilent}, ActAnswer, ActInterrupt, ActSpeakThrough),

		act("the-same-words-after-a-real-gap", interaction.Situation{
			Recent: []string{
				"user: Tell me the moment the build finishes.",
				"agent: I will let you know as soon as it does.",
			},
			Silence:       "700ms",
			SincePrevious: "34s",
			Heard:         "Actually, also tell me if any of the tests fail.",
		}, "half a minute later the same shape is a new instruction, not the tail of the old one",
			[]Action{ActAnswer}, ActStaySilent),

		act("pause-after-question", interaction.Situation{
			Recent:  []string{"user: I need to check something"},
			Silence: "900ms",
			Heard:   "can you look up my last order",
		}, "same silence, a finished question, and nothing asking for restraint",
			[]Action{ActAnswer}, ActStaySilent, ActInterrupt),

		// --- paired: an instruction to interject the moment something happens ---
		act("build-finished-pinned", interaction.Situation{
			Pins:    []string{"tell me the moment the build finishes (4m ago)"},
			Recent:  []string{"user: I'll keep talking while it runs", "agent: understood"},
			Speaker: "user", Speaking: true,
			Heard:    "so the other thing I wanted to mention was the",
			InFlight: "nothing",
			Seen:     "build finished successfully (0.2s ago)",
		}, "explicitly asked to be told immediately, even mid-sentence",
			[]Action{ActInterrupt, ActSpeakThrough}, ActStaySilent),

		act("build-finished-unpinned", interaction.Situation{
			Recent:  []string{"user: I'll keep talking while it runs", "agent: understood"},
			Speaker: "user", Speaking: true,
			Heard: "so the other thing I wanted to mention was the",
			Seen:  "build finished successfully (0.2s ago)",
		}, "same event, nobody asked to be interrupted for it",
			[]Action{ActStaySilent}, ActInterrupt, ActAnswer),

		// --- silence is not a trigger ---
		act("short-pause-mid-sentence", interaction.Situation{
			Recent:  []string{"agent: what can I help with?"},
			Silence: "300ms",
			Heard:   "I think we should probably",
		}, "300ms mid-clause is breathing, not a turn ending",
			[]Action{ActStaySilent}, ActAnswer, ActInterrupt),

		act("long-silence-complete", interaction.Situation{
			Recent:  []string{"agent: what can I help with?"},
			Silence: "1800ms",
			Heard:   "I'd like to cancel my subscription",
		}, "complete request, nearly two seconds: waiting longer is a bug",
			[]Action{ActAnswer}, ActStaySilent),

		// --- content is a trigger, with no pause at all ---
		act("restaurant-heard-the-dish", interaction.Situation{
			Recent: []string{
				"user: I want fish, order for me when you hear something good",
				"other: tonight we have three specials",
			},
			Speaker: "other", Speaking: true,
			Heard:   "the second is a duck confit and the third is sea bass with fennel",
			Silence: "0ms",
		}, "the awaited thing was said and the speaker has not paused",
			[]Action{ActInterrupt, ActSpeakThrough}, ActStaySilent),

		act("restaurant-nothing-yet", interaction.Situation{
			Recent: []string{
				"user: I want fish, order for me when you hear something good",
				"other: tonight we have three specials",
			},
			Speaker: "other", Speaking: true,
			Heard:   "the first is a ribeye with peppercorn sauce",
			Silence: "0ms",
		}, "same shape, nothing worth acting on yet",
			[]Action{ActStaySilent}, ActInterrupt, ActAnswer),

		act("ivr-heard-the-option", interaction.Situation{
			Recent: []string{
				"user: call them and find out where my order is",
				"agent: dialling now",
			},
			Speaker: "other", Speaking: true,
			Heard:   "press 1 for billing, press 2 for order status, press 3 for",
			Silence: "0ms",
			Tools:   []string{"press_key(digit) - send a keypad tone on the open call"},
		}, "a phone menu: the act is pressing a key, and saying anything is useless",
			[]Action{ActCallTool}, ActAnswer, ActInterrupt, ActSpeakThrough),

		// --- work already in flight must not be started twice ---
		act("ivr-already-pressed", interaction.Situation{
			Recent: []string{
				"user: call them and find out where my order is",
				"agent: dialling now",
			},
			Speaker: "other", Speaking: true,
			Heard:    "press 4 for technical support, press 5 to repeat",
			InFlight: "press_key(2) sent 0.3s ago, awaiting result",
			Silence:  "0ms",
			Tools:    []string{"press_key(digit) - send a keypad tone on the open call"},
		}, "the menu keeps talking; the key is already sent and must not be sent again",
			[]Action{ActStaySilent}, ActCallTool, ActInterrupt),

		// --- visual trigger, nobody speaking ---
		act("posture-slouching", interaction.Situation{
			Pins:    []string{"tell me if I slouch (6m ago)"},
			Recent:  []string{"user: I'm going to work for a while"},
			Silence: "45s",
			Seen:    "the person's back is curved forward over the desk (0.5s ago)",
		}, "nobody is talking: a visual change is the only evidence there is",
			[]Action{ActAnswer, ActSpeakThrough}, ActStaySilent),

		act("posture-upright", interaction.Situation{
			Pins:    []string{"tell me if I slouch (6m ago)"},
			Recent:  []string{"user: I'm going to work for a while"},
			Silence: "45s",
			Seen:    "the person is sitting upright at the desk (0.5s ago)",
		}, "the same instruction with the condition absent",
			[]Action{ActStaySilent}, ActAnswer, ActInterrupt, ActSpeakThrough),

		// --- overlap: the agent is mid-sentence ---
		act("overlap-backchannel", interaction.Situation{
			Recent:        []string{"user: what did you find?"},
			AgentSpeaking: true, AgentSaying: "I checked the order and it shipped on",
			Speaker: "user", Speaking: true,
			Heard: "mhm",
		}, "an acknowledgement is not a bid for the floor",
			[]Action{ActKeepSpeaking}, ActStopSpeaking, ActAnswer),

		act("overlap-real-interruption", interaction.Situation{
			Recent:        []string{"user: what did you find?"},
			AgentSpeaking: true, AgentSaying: "I checked the order and it shipped on",
			Speaker: "user", Speaking: true,
			Heard: "no wait that's the wrong order",
		}, "a correction: continuing would talk over something that matters",
			[]Action{ActStopSpeaking}, ActKeepSpeaking, ActStaySilent),

		act("overlap-while-translating", interaction.Situation{
			Pins:          []string{"translate everything he says into English as he goes (3m ago)"},
			Recent:        []string{"other: guten Tag", "agent: good afternoon"},
			AgentSpeaking: true, AgentSaying: "the meeting starts at",
			Speaker: "other", Speaking: true,
			Heard: "und wir treffen uns um drei Uhr",
		}, "the instruction is to speak over him continuously; yielding defeats it",
			[]Action{ActKeepSpeaking}, ActStopSpeaking, ActStaySilent),

		// --- interrupting to correct ---
		act("correct-against-the-contract", interaction.Situation{
			Contract: "You help plan projects. The deadline the client gave is the 3rd of the month. " +
				"Correct the person immediately if they say a date that contradicts it.",
			Recent:  []string{"user: right, so let me plan this out"},
			Speaker: "user", Speaking: true,
			Heard:   "we'll do the review next week and ship it by the 13th which gives us",
			Silence: "0ms",
		}, "the fact being corrected against is in the contract, not in anything anybody said",
			[]Action{ActInterrupt, ActSpeakThrough}, ActStaySilent),

		act("contract-consistent", interaction.Situation{
			Contract: "You help plan projects. The deadline the client gave is the 3rd of the month. " +
				"Correct the person immediately if they say a date that contradicts it.",
			Recent:  []string{"user: right, so let me plan this out"},
			Speaker: "user", Speaking: true,
			Heard:   "we'll do the review this week and ship it by the 3rd which gives us",
			Silence: "0ms",
		}, "the same contract with nothing contradicting it",
			[]Action{ActStaySilent}, ActInterrupt, ActAnswer),

		act("correct-a-wrong-fact", interaction.Situation{
			Recent: []string{
				"agent: the deadline they gave is the 3rd",
				"user: right, so let me plan this out",
			},
			Speaker: "user", Speaking: true,
			Heard:   "we'll do the review next week and ship it by the 13th which gives us",
			Silence: "0ms",
		}, "the plan is built on a date the agent just corrected; later is worse",
			[]Action{ActInterrupt, ActSpeakThrough}, ActStaySilent),

		act("plan-is-consistent", interaction.Situation{
			Recent: []string{
				"agent: the deadline they gave is the 3rd",
				"user: right, so let me plan this out",
			},
			Speaker: "user", Speaking: true,
			Heard:   "we'll do the review this week and ship it by the 3rd which gives us",
			Silence: "0ms",
		}, "the same monologue with nothing wrong in it",
			[]Action{ActStaySilent}, ActInterrupt, ActAnswer, ActStopSpeaking),

		// --- restraint: doing nothing is the common answer ---
		act("nothing-happening", interaction.Situation{
			Recent:  []string{"user: thanks", "agent: anytime"},
			Silence: "12s",
		}, "an idle line: acting here would be inventing a reason to talk",
			[]Action{ActStaySilent}, ActAnswer, ActSpeakThrough, ActInterrupt),

		act("agent-speaking-alone", interaction.Situation{
			Recent:        []string{"user: tell me what you found"},
			AgentSpeaking: true, AgentSaying: "the refund was issued on the fourth and",
			Silence: "0ms",
		}, "nobody else is talking and nothing is in flight",
			[]Action{ActKeepSpeaking}, ActStopSpeaking, ActAnswer, ActInterrupt),

		act("user-mid-sentence-ordinary", interaction.Situation{
			Recent:  []string{"agent: what can I help with?"},
			Speaker: "user", Speaking: true,
			Heard:   "I was looking at my account earlier and I noticed that the",
			Silence: "0ms",
		}, "an ordinary sentence in progress with nothing pinned",
			[]Action{ActStaySilent}, ActAnswer, ActInterrupt, ActSpeakThrough),

		act("deliberating-and-asked", interaction.Situation{
			Recent:   []string{"user: where is my order?", "agent: let me check that"},
			Silence:  "4s",
			InFlight: "reasoning pass running 4.1s, no result yet",
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
			Instruction: interaction.Instruction,
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

// PolicyInteractionRunner measures the decision the way the runtime takes it.
//
// A boundary measured under different settings is a different boundary, and
// these two paths differ in ways that matter: the shipping path constrains
// decoding to the enumerated acts, allows four tokens, and turns reasoning off
// at the endpoint. Measuring free generation instead would score a prompt
// nobody runs.
type PolicyInteractionRunner struct {
	Model *interaction.InteractionModel
	Label string
	// Situations recovers the state a case was built from. Cases carry their
	// rendered block so that what is asserted is what a model was shown, and
	// this runner needs the structure back to ask for the right act set.
	Situations map[string]interaction.Situation
}

func (runner PolicyInteractionRunner) Name() string       { return runner.Label }
func (runner PolicyInteractionRunner) Decision() Decision { return DecisionInteraction }

func (runner PolicyInteractionRunner) Observe(ctx context.Context, item Case) Observation {
	started := time.Now()
	// Repeated runs distinguish their cases by suffixing the name, and the
	// structure behind a case belongs to the case rather than to the run.
	name := item.Name
	if hash := strings.LastIndexByte(name, '#'); hash > 0 {
		name = name[:hash]
	}
	state, known := runner.Situations[name]
	if !known {
		return Observation{
			Actions: []Action{ActUnparsed},
			Err:     fmt.Errorf("no situation recorded for case %q", name),
		}
	}
	act, outcome, err := runner.Model.Decide(ctx, state)
	return Observation{
		Actions: []Action{Action(act)}, Text: outcome.Option,
		Elapsed: time.Since(started), Err: err,
	}
}

// InteractionSituations exposes the state behind each case by name, so a
// runner that needs the structure rather than the rendered block can recover
// it without the cases being declared twice.
func InteractionSituations() map[string]interaction.Situation {
	states := make(map[string]interaction.Situation, len(interactionStates))
	for name, state := range interactionStates {
		states[name] = state
	}
	return states
}
