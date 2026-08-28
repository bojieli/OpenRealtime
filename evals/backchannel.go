package evals

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/session"
)

// BackchannelCases freeze the decision to say "mm-hm" while somebody is
// talking.
//
// It is a model-backed decision like the others and was the only one with no
// cases of its own, which is how it stayed unmeasured while everything around
// it was tuned. It deserves them for the same reason the interaction model
// does: it is the other way the agent can make a noise during somebody else's
// turn, and it is governed by nothing the interaction model decides.
//
// Its two failures are not worth the same, and the asymmetry runs the opposite
// way to the interaction model's. A missing continuer costs a little warmth and
// nothing else - the speaker carries on, and nothing they said is lost. A
// continuer in the wrong place lands on top of somebody's sentence, and the
// worst placements are exactly the ones a helpful model reaches for: after a
// question, which needs an answer rather than a noise, and during a silence
// that was asked for. So restraint is the common answer here, and the cases are
// weighted that way deliberately.
func BackchannelCases() []Case {
	said := func(name, heard, note string, accept []Action, forbid ...Action) Case {
		return Case{
			Name: name, Decision: DecisionBackchannel, Context: heard,
			Accept: accept, Forbid: forbid, Note: note,
		}
	}
	quiet := []Action{ActionNoContinuer}
	// Either continuer passes where one is wanted. Which of the two a listener
	// picks is a matter of register, and pinning it would measure taste.
	continuer := []Action{ActionAcknowledge, ActionAffirm}

	return []Case{
		// --- a continuer is what a listener owes here ---
		said("finished-a-thought",
			"So we drove up on the Friday, and the whole place was completely empty.",
			"a complete beat in a story, with more obviously coming",
			continuer, ActionNoContinuer),
		said("mid-story-landmark",
			"I'd been saving for about two years at that point. Two years.",
			"a repetition for emphasis is an invitation to acknowledge",
			continuer, ActionNoContinuer),
		said("agreement-invited",
			"And honestly, after all that, I think they handled it about as well as anyone could have.",
			"a judgement offered for agreement",
			continuer, ActionNoContinuer),

		// --- restraint: a noise here lands on top of somebody ---
		said("mid-clause",
			"So I went to the shop and I was about to",
			"cut off mid-clause; a continuer here is talking over them",
			quiet, ActionAcknowledge, ActionAffirm),
		said("direct-question",
			"So what do you think we should do about the invoice?",
			"a question wants an answer, and mm-hm is the one reply that is worse than none",
			quiet, ActionAcknowledge, ActionAffirm),
		// A request wearing the clothes of a question. This is the residual
		// failure after the prompt learned that questions owe answers: the
		// direct form is refused and the polite one still gets a noise. Kept
		// as a case rather than chased with more wording, because what is
		// wrong is the model's reading of "could you" and naming the phrase
		// would teach the phrase rather than the principle.
		said("polite-request",
			"Could you check whether the Tuesday slot is still free?",
			"a request in question form still owes a reply, not a continuer",
			quiet, ActionAcknowledge, ActionAffirm),
		said("asked-for-silence",
			"Hang on, let me finish this thought before you say anything.",
			"they asked for quiet in as many words",
			quiet, ActionAcknowledge, ActionAffirm),
		said("counting-aloud",
			"Right, so that's one, two, three, four",
			"a list being counted out; a noise between items corrupts it",
			quiet, ActionAcknowledge, ActionAffirm),
		said("dictating-detail",
			"The reference is A as in alpha, four, four, seven, B as in bravo",
			"dictation, where the listener's job is to not make a sound",
			quiet, ActionAcknowledge, ActionAffirm),
		said("reading-aloud",
			"It says here, and I quote, the party of the first part shall",
			"reading something out, mid-sentence",
			quiet, ActionAcknowledge, ActionAffirm),
		said("bare-opening",
			"So",
			"nothing has been said yet to acknowledge",
			quiet, ActionAcknowledge, ActionAffirm),
		said("addressed-elsewhere",
			"No, not you - I was talking to Sam. Sorry, go on.",
			"the speaker is handling somebody else in the room",
			quiet, ActionAcknowledge, ActionAffirm),
		said("thinking-aloud",
			"Let me see, the third, no, the fourth, hang on",
			"searching for a word; a continuer here is pressure, not warmth",
			quiet, ActionAcknowledge, ActionAffirm),
	}
}

// BackchannelRunner asks the policy whether to say a continuer.
//
// The counter matters. The policy refuses to answer twice about one revision,
// and refuses again inside its minimum interval - both are arithmetic the
// runtime wants and neither is what these cases measure. Handing every case the
// same revision and the same clock made the policy answer the first one and
// short-circuit the rest, which reads as a model that never speaks and is
// really a harness that never asked. Every case gets its own revision and a
// clock far enough on that the interval has passed.
type BackchannelRunner struct {
	Policy interaction.Backchannel
	Label  string

	asked atomic.Uint64
}

func (runner *BackchannelRunner) Name() string       { return runner.Label }
func (runner *BackchannelRunner) Decision() Decision { return DecisionBackchannel }

func (runner *BackchannelRunner) Observe(ctx context.Context, item Case) Observation {
	began := time.Now()
	// The runtime's own gates - how long they have been talking, how recently
	// a continuer was emitted - are arithmetic and are not what this measures.
	// The situation is built past them so the case reaches the model.
	nth := runner.asked.Add(1)
	decision := interaction.Context{
		NowNS: nth * uint64(time.Hour),
		Duplex: session.Snapshot{
			UserSpeaking: true, UserSpeechStartedNS: 1,
		},
		Revision: interaction.Revision{ID: nth, StableText: strings.TrimSpace(item.Context)},
	}
	outcome, err := runner.Policy.Decide(ctx, decision)
	elapsed := time.Since(began)
	if err != nil {
		return Observation{Elapsed: elapsed, Err: err}
	}
	action := ActionNoContinuer
	switch outcome.Choice {
	case interaction.BackchannelAcknowledge:
		action = ActionAcknowledge
	case interaction.BackchannelAffirm:
		action = ActionAffirm
	}
	return Observation{
		Elapsed: elapsed, Actions: []Action{action},
		Text: string(outcome.Choice) + " (" + outcome.Reason + ")",
	}
}
