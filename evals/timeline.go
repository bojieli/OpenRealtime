package evals

import (
	"context"
	"fmt"
	"strings"

	"github.com/bojieli/OpenRealtime/interaction"
)

// A timeline case scores when a decision happens, not only what it is.
//
// A frozen instant cannot catch the failures that matter most about timing. A
// model that endpoints correctly but nine hundred milliseconds late scores
// full marks on every static case and produces a conversation nobody wants to
// have; one that answers during a mid-thought pause scores full marks on the
// instant after the pause ends. Both are visible only against a clock.
//
// Timelines are replayed as a series of instants rather than in real time. The
// model is asked at each one and the answers are read as a sequence, which is
// what makes this cheap enough to run on every change: the wall clock is
// simulated, so a two-second pause costs two model calls rather than two
// seconds.
type Moment struct {
	// AtMS is when this instant occurs, measured from the start of the case.
	AtMS  int
	State interaction.Situation
}

// TimelineCase is one scripted stretch of conversation.
type TimelineCase struct {
	Name    string
	Note    string
	Moments []Moment
	// Expect is the act that should appear, and the window it must appear in.
	// A zero Expect asserts only that nothing in Forbid ever happens.
	Expect      Action
	NotBeforeMS int
	NotAfterMS  int
	// Forbid names acts that must not appear, and ForbidUntilMS bounds when
	// the prohibition applies - zero meaning the whole timeline.
	//
	// The bound matters. A case that forbids answering during a mid-thought
	// pause has nothing to say about the moment after the speaker resumes and
	// completes their sentence; scoring that moment with the same rule marks a
	// model wrong for having waited correctly.
	Forbid        []Action
	ForbidUntilMS int
	// Monotone asserts that once Expect has appeared it never reverts to an
	// act in Forbid. Evidence only accumulates as a pause lengthens, so a
	// decision that flips back is one that was not reading the clock.
	Monotone bool
}

// TimelineOutcome is one timeline replayed.
type TimelineOutcome struct {
	TimelineCase
	Acts      []Action
	FirstAtMS int
	Passed    bool
	Because   string
}

// RunTimeline replays one case and scores when things happened.
func RunTimeline(ctx context.Context, runner Runner, item TimelineCase) TimelineOutcome {
	outcome := TimelineOutcome{TimelineCase: item, FirstAtMS: -1}
	for _, moment := range item.Moments {
		observed := runner.Observe(ctx, Case{
			Name: item.Name, Decision: DecisionInteraction, Context: moment.State.Render(),
		})
		if observed.Err != nil {
			outcome.Because = "provider error: " + observed.Err.Error()
			return outcome
		}
		act := observed.Actions[0]
		outcome.Acts = append(outcome.Acts, act)
		for _, forbidden := range item.Forbid {
			if act != forbidden {
				continue
			}
			// Forbid means two different things, and conflating them scores
			// correct behaviour as a failure.
			//
			// A monotone case forbids going *back*: listening while a pause is
			// still short is exactly right, and only becomes a fault once the
			// turn has already been judged over. Every other case forbids the
			// act outright, within its window.
			if item.Monotone {
				if outcome.FirstAtMS < 0 {
					continue
				}
				outcome.Because = fmt.Sprintf("reverted to %s at %dms after deciding %s at %dms",
					forbidden, moment.AtMS, item.Expect, outcome.FirstAtMS)
				return outcome
			}
			if item.ForbidUntilMS > 0 && moment.AtMS > item.ForbidUntilMS {
				continue
			}
			outcome.Because = fmt.Sprintf("did %s at %dms, which must never happen here", forbidden, moment.AtMS)
			return outcome
		}
		if item.Expect != "" && act == item.Expect && outcome.FirstAtMS < 0 {
			outcome.FirstAtMS = moment.AtMS
			if moment.AtMS < item.NotBeforeMS {
				outcome.Because = fmt.Sprintf("did %s at %dms, which is before %dms",
					item.Expect, moment.AtMS, item.NotBeforeMS)
				return outcome
			}
		}
	}
	switch {
	case item.Expect == "":
		outcome.Passed = true
		outcome.Because = "nothing forbidden happened"
	case outcome.FirstAtMS < 0:
		outcome.Because = fmt.Sprintf("never did %s", item.Expect)
	case item.NotAfterMS > 0 && outcome.FirstAtMS > item.NotAfterMS:
		outcome.Because = fmt.Sprintf("did %s at %dms, later than %dms",
			item.Expect, outcome.FirstAtMS, item.NotAfterMS)
	default:
		outcome.Passed = true
		outcome.Because = fmt.Sprintf("did %s at %dms", item.Expect, outcome.FirstAtMS)
	}
	return outcome
}

// FormatTimelines renders a set of replayed timelines, failures first.
func FormatTimelines(outcomes []TimelineOutcome) string {
	var builder strings.Builder
	passed := 0
	var failures, passes []TimelineOutcome
	for _, outcome := range outcomes {
		if outcome.Passed {
			passed++
			passes = append(passes, outcome)
			continue
		}
		failures = append(failures, outcome)
	}
	for _, outcome := range append(failures, passes...) {
		mark := "FAIL"
		if outcome.Passed {
			mark = "ok  "
		}
		fmt.Fprintf(&builder, "  %s %-28s %s\n", mark, outcome.Name, outcome.Because)
		if !outcome.Passed {
			fmt.Fprintf(&builder, "       %v\n", outcome.Acts)
		}
	}
	fmt.Fprintf(&builder, "\n  timelines %d/%d\n", passed, len(outcomes))
	return builder.String()
}

// TimelineCases script the behaviour that only shows up against a clock.
func TimelineCases() []TimelineCase {
	ms := func(value int) string { return fmt.Sprintf("%dms", value) }
	return []TimelineCase{
		{
			Name: "endpoint-after-a-finished-question",
			Note: "a complete question, then growing silence: too eager is rude, too slow is worse",
			Moments: []Moment{
				{200, interaction.Situation{Speaker: "user", Speaking: true, Heard: "what time does the pharmacy"}},
				{400, interaction.Situation{Speaker: "user", Speaking: true, Heard: "what time does the pharmacy close"}},
				{600, interaction.Situation{Heard: "what time does the pharmacy close", Silence: ms(200)}},
				{900, interaction.Situation{Heard: "what time does the pharmacy close", Silence: ms(500)}},
				{1200, interaction.Situation{Heard: "what time does the pharmacy close", Silence: ms(800)}},
				{1700, interaction.Situation{Heard: "what time does the pharmacy close", Silence: ms(1300)}},
				{2400, interaction.Situation{Heard: "what time does the pharmacy close", Silence: ms(2000)}},
			},
			Expect: ActAnswer, NotBeforeMS: 400, NotAfterMS: 1700,
		},
		{
			Name: "a pause mid-thought is not a turn ending",
			Note: "the speaker stops for most of a second and then carries on",
			Moments: []Moment{
				{200, interaction.Situation{Speaker: "user", Speaking: true, Heard: "I think we should probably"}},
				{500, interaction.Situation{Heard: "I think we should probably", Silence: ms(200)}},
				{800, interaction.Situation{Heard: "I think we should probably", Silence: ms(500)}},
				{1100, interaction.Situation{Heard: "I think we should probably", Silence: ms(800)}},
				{1400, interaction.Situation{Speaker: "user", Speaking: true, Heard: "I think we should probably go with the second option"}},
			},
			Forbid: []Action{ActAnswer, ActInterrupt}, ForbidUntilMS: 1100,
		},
		{
			Name: "an endpoint decision is not taken back",
			Note: "evidence only accumulates as a pause lengthens; flipping back means the clock was not read",
			Moments: []Moment{
				{300, interaction.Situation{Heard: "I'd like to cancel my subscription", Silence: ms(300)}},
				{700, interaction.Situation{Heard: "I'd like to cancel my subscription", Silence: ms(700)}},
				{1200, interaction.Situation{Heard: "I'd like to cancel my subscription", Silence: ms(1200)}},
				{1900, interaction.Situation{Heard: "I'd like to cancel my subscription", Silence: ms(1900)}},
				{2900, interaction.Situation{Heard: "I'd like to cancel my subscription", Silence: ms(2900)}},
				{4200, interaction.Situation{Heard: "I'd like to cancel my subscription", Silence: ms(4200)}},
			},
			Expect: ActAnswer, NotAfterMS: 1900,
			Forbid: []Action{ActStaySilent}, Monotone: true,
		},
		{
			Name: "no yielding before there is anything to yield to",
			Note: "onset alone carries no information; the recogniser has not produced a word yet",
			Moments: []Moment{
				{0, interaction.Situation{
					AgentSpeaking: true, AgentSaying: "I checked the order and it shipped on",
					Speaker: "user", Speaking: true,
				}},
				{120, interaction.Situation{
					AgentSpeaking: true, AgentSaying: "I checked the order and it shipped on the",
					Speaker: "user", Speaking: true,
				}},
			},
			Forbid: []Action{ActStopSpeaking},
		},
		{
			Name: "yield once the words arrive",
			Note: "the same overlap, now with a transcript that says it is a correction",
			Moments: []Moment{
				{0, interaction.Situation{
					AgentSpeaking: true, AgentSaying: "I checked the order and it shipped on",
					Speaker: "user", Speaking: true,
				}},
				{260, interaction.Situation{
					AgentSpeaking: true, AgentSaying: "I checked the order and it shipped on the",
					Speaker: "user", Speaking: true, Heard: "no wait that's the wrong",
				}},
				{460, interaction.Situation{
					AgentSpeaking: true, AgentSaying: "I checked the order and it shipped on the",
					Speaker: "user", Speaking: true, Heard: "no wait that's the wrong order",
				}},
			},
			Expect: ActStopSpeaking, NotBeforeMS: 260, NotAfterMS: 460,
		},
		{
			Name: "an acknowledgement never takes the floor",
			Note: "the whole of a backchannel, start to finish",
			Moments: []Moment{
				{0, interaction.Situation{
					AgentSpeaking: true, AgentSaying: "the refund went out on the",
					Speaker: "user", Speaking: true,
				}},
				{240, interaction.Situation{
					AgentSpeaking: true, AgentSaying: "the refund went out on the fourth",
					Speaker: "user", Speaking: true, Heard: "mhm",
				}},
				{500, interaction.Situation{
					AgentSpeaking: true, AgentSaying: "the refund went out on the fourth and should",
					Speaker: "user", Speaking: true, Heard: "mhm right",
				}},
			},
			Forbid: []Action{ActStopSpeaking},
		},
		{
			Name: "a standing instruction fires while they keep talking",
			Note: "the counting demo: the trigger word arrives with no pause anywhere near it",
			Moments: []Moment{
				{300, interaction.Situation{
					Pins:    []string{"count the animals out loud as I mention them (2m ago)"},
					Recent:  []string{"user: I'm going to tell you about my afternoon - count the animals out loud as I mention them", "agent: okay"},
					Speaker: "user", Speaking: true, Heard: "it was a warm afternoon and I was walking by the",
				}},
				{700, interaction.Situation{
					Pins:    []string{"count the animals out loud as I mention them (2m ago)"},
					Recent:  []string{"user: I'm going to tell you about my afternoon - count the animals out loud as I mention them", "agent: okay"},
					Speaker: "user", Speaking: true, Heard: "it was a warm afternoon and I was walking by the river when a capybara",
				}},
				{1000, interaction.Situation{
					Pins:    []string{"count the animals out loud as I mention them (2m ago)"},
					Recent:  []string{"user: I'm going to tell you about my afternoon - count the animals out loud as I mention them", "agent: okay"},
					Speaker: "user", Speaking: true, Heard: "it was a warm afternoon and I was walking by the river when a capybara wandered over",
				}},
			},
			Expect: ActSpeakThrough, NotBeforeMS: 700, NotAfterMS: 1000,
			Forbid: []Action{ActAnswer, ActInterrupt},
		},
	}
}
