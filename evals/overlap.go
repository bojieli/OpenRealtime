package evals

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/session"
)

// OverlapCases freeze the moment somebody talks over the agent.
//
// The agent is mid-sentence and a second voice arrives. What that voice is
// doing decides whether the agent stops: a person taking the floor should be
// given it at once, and a person saying "mm-hm" should be talked straight
// through, because stopping for a continuer is how an agent becomes impossible
// to hold a conversation with. The barge-in policy makes that call; this is
// the evidence it makes it from, and it was the last model-backed decision in
// the system with no cases of its own.
//
// The two failures cost differently and both are bad. Reading a real
// interruption as a continuer talks over somebody who wanted the floor, and
// they have to say it again louder. Reading a continuer as an interruption
// stops the agent for a noise that meant "go on", which is the failure that
// makes the whole arrangement feel broken - the agent that cannot finish a
// sentence. Neither is recoverable by a later layer, so the cases are weighted
// evenly and both directions are vetoed.
func OverlapCases() []Case {
	over := func(name, heard, note string, accept []Action, forbid ...Action) Case {
		return Case{
			Name: name, Decision: DecisionOverlap, Context: heard,
			Accept: accept, Forbid: forbid, Note: note,
		}
	}
	directed := []Action{ActionOverlapDirected}
	backchannel := []Action{ActionOverlapBackchannel}
	side := []Action{ActionOverlapSide}
	// Ambiguous is a real answer, not a failure: a fragment too short to read
	// is exactly what it is for, and forcing a guess there is worse than
	// saying so.
	unclear := []Action{ActionOverlapAmbiguous, ActionOverlapDirected}

	return []Case{
		// --- taking the floor ---
		over("correction", "no, no, that's not what I asked",
			"a correction is the clearest case of wanting the floor",
			directed, ActionOverlapBackchannel, ActionOverlapSide),
		over("new-question", "wait, how much did you say it costs?",
			"a question needs an answer and cannot wait for the sentence to end",
			directed, ActionOverlapBackchannel, ActionOverlapSide),
		over("stop-request", "stop, stop, go back a second",
			"an explicit request to stop",
			directed, ActionOverlapBackchannel, ActionOverlapSide),
		over("wrong-detail", "that's the wrong account number",
			"a factual correction mid-sentence",
			directed, ActionOverlapBackchannel, ActionOverlapSide),
		over("longer-than-a-continuer", "right, but that doesn't cover the second invoice",
			"opens like a continuer and carries a new subject, which makes it directed",
			directed, ActionOverlapBackchannel, ActionOverlapSide),

		// --- showing they are listening ---
		over("mm-hm", "mm-hm",
			"the case the whole classifier exists for",
			backchannel, ActionOverlapDirected, ActionOverlapSide),
		over("right", "right",
			"a bare continuer with nothing else in it",
			backchannel, ActionOverlapDirected, ActionOverlapSide),
		over("yeah-okay", "yeah, okay",
			"two continuers is still a continuer",
			backchannel, ActionOverlapDirected, ActionOverlapSide),
		over("got-it", "got it",
			"an acknowledgement, not a request",
			backchannel, ActionOverlapDirected, ActionOverlapSide),
		over("uh-huh-sure", "uh-huh, sure",
			"agreement offered while the agent keeps the floor",
			backchannel, ActionOverlapDirected, ActionOverlapSide),

		// --- talking to somebody else ---
		over("to-the-room", "no, not you, I'm on a call",
			"addressed away from the agent in as many words",
			side, ActionOverlapDirected, ActionOverlapBackchannel),
		over("aside-to-person", "Sam, can you shut the door?",
			"a request to a named third party",
			side, ActionOverlapDirected, ActionOverlapBackchannel),
		// Accepts ambiguous as well, and that is the case being specified
		// correctly rather than being weakened. What marks this as addressed
		// elsewhere is that it is shouted, and volume is not in a transcript -
		// so "not enough to tell" is a defensible reading and leads somewhere
		// different, since barge-in treats ambiguous as neither reason to stop
		// nor reason to carry on and lets time decide.
		//
		// Directed is not defensible, and it is what the model answers, three
		// times out of three. Kept as a failing case: stopping mid-sentence to
		// answer something said to somebody in another room is the failure,
		// and it stays visible until it is fixed.
		over("household", "I'll be down in a minute!",
			"called across a room; unreadable from text is fair, answering it is not",
			[]Action{ActionOverlapSide, ActionOverlapAmbiguous},
			ActionOverlapDirected, ActionOverlapBackchannel),

		// --- not enough to tell yet ---
		over("single-syllable", "so",
			"one word that could begin anything",
			unclear, ActionOverlapBackchannel, ActionOverlapSide),
		over("clipped-start", "can you just",
			"cut off before the verb; directed is the safe reading and so is saying so",
			unclear, ActionOverlapBackchannel, ActionOverlapSide),
	}
}

// OverlapRunner asks the classifier what the overlapping speech is.
type OverlapRunner struct {
	Policy interaction.OverlapClassifier
	Label  string

	asked atomic.Uint64
}

func (runner *OverlapRunner) Name() string       { return runner.Label }
func (runner *OverlapRunner) Decision() Decision { return DecisionOverlap }

func (runner *OverlapRunner) Observe(ctx context.Context, item Case) Observation {
	began := time.Now()
	// The classifier caches per revision, so each case needs its own.
	nth := runner.asked.Add(1)
	decision := interaction.Context{
		NowNS: nth * uint64(time.Hour),
		Duplex: session.Snapshot{
			UserSpeaking: true, AgentSpeaking: true, UserSpeechStartedNS: 1,
		},
		Revision: interaction.Revision{ID: nth, StableText: strings.TrimSpace(item.Context)},
	}
	evidence := runner.Policy.Classify(ctx, decision)
	elapsed := time.Since(began)
	action := ActionOverlapAmbiguous
	switch evidence {
	case interaction.OverlapDirected:
		action = ActionOverlapDirected
	case interaction.OverlapBackchannel:
		action = ActionOverlapBackchannel
	case interaction.OverlapSide:
		action = ActionOverlapSide
	}
	return Observation{
		Elapsed: elapsed, Actions: []Action{action}, Text: string(evidence),
	}
}
