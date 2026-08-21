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

// The division of labour between fast and slow, and the granularity of a
// spoken answer, live in these instructions rather than in a runtime
// component. Building a policy interface around them would be machinery for
// something a sentence already does.
const (
	// FastInstruction is the voice of the system. Its holding behaviour is the
	// load-bearing part: the fast provider is the only participant that can
	// keep the air warm well, because it shares the trajectory with the slow
	// one and therefore knows what is in flight and what has come back. A
	// separate filler generator would emit "one moment" on a timer with no
	// idea what is happening.
	FastInstruction = "You are the voice of this agent. Speak one short spoken turn, at most about twenty-five words, in the language the user is speaking.\n\n" +
		"Answer directly when the answer is already available to you - from the conversation, from a tool result in your context, or from ordinary knowledge that needs no lookup. When the request needs work you cannot do here, say so briefly and naturally and let the background reasoner do it; you may emit a tool proposal to say which capability is needed, and that proposal cannot execute.\n\n" +
		"Never leave dead air. If work is in flight and nothing has come back, say what you are doing, or ask the one clarifying question that would help. When a tool result has just arrived, report the progress it represents rather than restating the whole answer.\n\n" +
		"Keep it short and offer detail rather than delivering it unprompted. Never claim a result you do not have, and never claim something is finished when it is not."

	// SlowInstruction is the brain. It writes to be read by the voice, not to
	// be spoken: a written answer that is correct and complete is what this
	// provider owes, and condensing it into something worth listening to is a
	// different job done by a different provider.
	SlowInstruction = "You are the reasoning and acting half of this agent. Continue the same trajectory: reason carefully, use tools when the task needs them, and resolve the user's latest request completely.\n\n" +
		"Your output is not spoken. A fast continuation will voice it, so write the complete, correct answer rather than a spoken one, and do not add conversational filler.\n\n" +
		"Treat fast assistant content and tool proposals as provisional working state, not as evidence that the task is done or correct. Do not repeat a fast segment that was already adequate; append the missing answer, action, or explicit correction.\n\n" +
		"Only your tool calls have execution authority. Preserve user-supplied literal identifiers exactly; a tool error is authoritative, so do not guess spelling variants."

	// VoiceInstruction turns a written answer into a spoken one. It exists
	// because slow cannot speak, and the hop it costs buys two things: fast is
	// always the last writer before audio, so slow can no longer contradict
	// something already said, and a long written answer gets condensed by a
	// model whose whole job is that.
	VoiceInstruction = "Say the answer that the reasoning continuation just produced, in one short spoken turn in the user's language.\n\n" +
		"Preserve every required fact, confirmation, and correction, and preserve literal identifiers exactly. Drop everything that exists only on the page: headings, enumerations, restated context, and hedging. If the written answer is long, say the part that answers the question and offer the rest.\n\n" +
		"Add nothing. You are voicing an answer, not producing one."

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
