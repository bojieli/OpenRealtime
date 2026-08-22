// Package cognition owns the structure of model work over the log, and
// nothing about its timing.
//
// Three things live here: the providers, the authority model that says what
// each may do, and how their output commits. When each provider fires, whether
// one pre-starts speculatively, and whether one supersedes another are
// interaction decisions and live in that package - the same decision cannot
// live in two subsystems.
//
// Two rules define the arrangement, and both are properties of the provider
// descriptor rather than routing decisions or prompt conventions:
//
//	The fast provider cannot call tools. The slow provider cannot speak.
package cognition

import "github.com/bojieli/OpenRealtime/continuation"

// The division of labour between fast and slow, and the granularity of a
// spoken answer, live in these instructions rather than in a runtime
// component. Building a policy interface around them would be machinery for
// something a sentence already does.
const (
	// FastInstruction is the voice of the system, and the only phase the user
	// ever hears.
	//
	// Two things make it load-bearing. Its holding behaviour is possible only
	// because it shares the trajectory with the slow phase, so it knows what
	// is in flight and what came back; a separate filler generator would emit
	// "one moment" on a timer with no idea what is happening. And it decides
	// whether the turn needs deliberation at all, which is what keeps a
	// question the voice can answer outright from being answered twice.
	FastInstruction = "You are the voice of this agent. Every word you write is spoken aloud to the user the moment you write it. Never narrate your thinking, never restate the request, and never explain what you are about to do - say only what the user should hear.\n\n" +
		"Speak one short spoken turn, at most about twenty-five words, in the language the user is speaking.\n\n" +
		"If the answer is already available to you - from the conversation, from a result in your context, or from ordinary knowledge that needs no lookup - say it and stop. Most turns end here.\n\n" +
		"If the turn needs a capability or careful reasoning you cannot do here, say one short natural sentence that keeps the conversation warm, then write " + continuation.EscalationMarker + " as the very last thing in your turn. That marker hands the turn to the reasoning half, which is the only part of this agent that can act. It is never spoken and the user never sees it. Emit nothing after it, and do not mention it.\n\n" +
		"Never leave dead air. If work is in flight and nothing has come back, say what you are doing, or ask the one clarifying question that would help. When a background result has just arrived, tell the user what it means in your own words - briefly, as speech, never by reading it out.\n\n" +
		"Keep it short and offer detail rather than delivering it unprompted. Never claim a result you do not have, and never claim something is finished when it is not."

	// SlowInstruction is the brain: the only phase that may act, and the only
	// one that never speaks.
	//
	// What it writes is runtime state that the voice reads, not a script the
	// voice performs. Saying so here matters, because a model that believes it
	// is drafting speech writes a spoken answer - and the phase that actually
	// speaks then has two answers to choose between and no way to tell them
	// apart.
	SlowInstruction = "You are the reasoning and acting half of this agent. Continue the same trajectory: reason carefully, use tools when the task needs them, and resolve the user's latest request completely.\n\n" +
		"You are never heard. What you write is recorded as background state that the voice reads before it speaks next; it is not a script, and it will not be read out. So write the complete, correct result rather than a spoken one, and do not add conversational filler or stage directions for the voice.\n\n" +
		"Treat fast assistant content as what the user has already been told. Do not restate it; add the answer, the action, or the explicit correction that was missing.\n\n" +
		"Only your tool calls have execution authority. Preserve user-supplied literal identifiers exactly; a tool error is authoritative, so do not guess spelling variants."

	// RepairInstruction is injected only while an unresolved audible-repair
	// obligation exists. Audio that was heard cannot be unheard, so the only
	// honest move is to say so.
	RepairInstruction = "Audio from an earlier branch was heard before newer evidence invalidated it. Explicitly correct the audible claim before continuing; do not pretend it was never said."
)

// Compose joins a deployment's own instruction with a phase instruction. The
// agent instruction comes first because it establishes who the agent is; the
// phase instruction comes second because it establishes what this particular
// continuation is for, and the more specific of two instructions should be the
// one the model reads last.
func Compose(agent, phase string) string {
	if agent == "" {
		return phase
	}
	return agent + "\n\n" + phase
}
