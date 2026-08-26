package interaction

import "strings"

// Instruction governs the model that decides what an agent does in an instant
// rather than what it says.
//
// It is not one of the cognition instructions and does not live with them. The
// others are given to a model that has already been asked to respond and is
// choosing words. This one is asked, many times a second, whether to respond
// at all - and the answer is usually no, which is why it has to be cheap
// enough to ask that often and why it must never be tempted to produce content.
//
// The first rule is the load-bearing one. An interaction policy that lives
// only in configuration cannot be changed by the people it governs, and people
// set these policies out loud constantly: wait, let me finish; stop me if I
// get this wrong; tell me the moment it lands; count them as I go. A decision
// that cannot hear those is not a policy, it is a setting.
var Instruction = buildInstruction()

// workedExample is one decision with its answer and the reason for it.
//
// The reason is included because an example that only shows an answer teaches
// the answer; one that shows why teaches the rule, and the rules here are
// about evidence rather than about which words appeared.
type workedExample struct {
	state Situation
	act   Act
	why   string
}

// examples cover every act, and cover the two visual cases as a pair.
//
// The pairing is deliberate and was learned the hard way: an earlier version
// had exactly one example with a visual observation in it and its answer was
// "listen", which taught that seeing something is a reason not to act. Every
// kind of evidence here appears at least once where it justifies acting and
// once where it does not.
func examples() []workedExample {
	return []workedExample{
		{Situation{
			Recent:  []string{"agent: what are you working on?"},
			Speaker: "user", Speaking: true,
			Heard: "so I was thinking maybe we could try the",
		}, ActStaySilent, "Nothing makes acting necessary: the sentence is unfinished and nothing is asking for a response."},

		{Situation{
			Recent: []string{"agent: what are you working on?"},
			Heard:  "what time does the pharmacy close", Silence: "1400ms",
		}, ActAnswer, "They asked a complete question and have stopped for well over a second."},

		{Situation{
			Pins:    []string{"read the running total back to me each time I add something (1m ago)"},
			Recent:  []string{"user: I'm doing the shopping list now"},
			Speaker: "user", Speaking: true,
			Heard: "add milk, and two tins of tomatoes",
		}, ActSpeakThrough, "They asked for a running total and have just added items, and they are still going."},

		{Situation{
			Pins:    []string{"stop me if I quote a price under fifty (3m ago)"},
			Recent:  []string{"user: I'm going to tell you how the call went"},
			Speaker: "user", Speaking: true,
			Heard: "I told them we could do it for forty and they seemed happy so",
		}, ActInterrupt, "They asked to be stopped on a price under fifty and have just quoted forty, so waiting makes it worse."},

		{Situation{
			Recent:  []string{"user: ring them and get me through to a person"},
			Speaker: "other", Speaking: true,
			Heard: "to leave a message press star, to speak to an agent press nine",
			Tools: []string{"press_key(digit) - send a keypad tone on the open call"},
		}, ActCallTool, "The menu just named the option that gets them a person, and talking to a recording achieves nothing."},

		{Situation{
			Recent:        []string{"user: how much was it?"},
			AgentSpeaking: true, AgentSaying: "the total comes to about",
			Speaker: "user", Speaking: true, Heard: "yeah",
		}, ActKeepSpeaking, "Nothing makes acting necessary: an acknowledgement is not a bid for the floor."},

		{Situation{
			Recent:        []string{"user: did you book it?"},
			AgentSpeaking: true, AgentSaying: "I have booked the table for",
			Speaker: "user", Speaking: true, Heard: "hang on, not that one",
		}, ActStopSpeaking, "They are taking the floor to correct something, and continuing would bury it."},

		{Situation{
			Recent: []string{
				"user: Put the meeting in for Thursday.",
				"agent: Done, Thursday it is.",
				"user: and make it.",
			},
			Silence: "550ms", SincePrevious: "410ms",
			Heard: "An hour long.",
		}, ActStaySilent, "Nothing makes acting necessary: four hundred milliseconds after a sentence already answered, this is its tail."},

		{Situation{
			Pins:    []string{"tell me the moment the courier arrives (20m ago)"},
			Recent:  []string{"user: I'll be in the other room"},
			Silence: "6m", Seen: "a van has stopped outside and someone is walking up the path",
		}, ActAnswer, "They asked to be told the moment the courier arrived, and it has just arrived."},

		{Situation{
			Pins:    []string{"tell me when the kettle has boiled (2m ago)"},
			Recent:  []string{"user: I'm going to read for a bit"},
			Silence: "90s", Seen: "the kettle is still heating",
		}, ActStaySilent, "Nothing makes acting necessary: the instruction stands but the kettle has not boiled."},

		{Situation{
			Recent: []string{
				"user: I'm just going to get on with this for a bit",
				"agent: I will be here if you need me",
			},
			Speaker: "someone else in the room",
			Heard:   "did you get the milk on the way in", Silence: "1300ms",
		}, ActStaySilent, "Nothing makes acting necessary: a different voice asked somebody else in the room a question, and nobody has asked the agent to deal with them."},
	}
}

func buildInstruction() string {
	var text strings.Builder
	text.WriteString(
		"You decide what an agent does in this instant. You never choose words and never speak to anyone: " +
			"you pick one act and reply with its name alone, lowercase, nothing else. Choose only from the acts " +
			"listed as available, since the others describe things the agent is not in a position to do.\n\n" +
			"You are shown any standing instructions the people in this conversation gave out loud, the recent " +
			"conversation, and the current instant: what the agent is doing, who else is speaking and what has " +
			"been heard from them so far, how long the silence has lasted, how long a gap there was before " +
			"they started this one, what work is already running, and anything just seen that nobody said " +
			"out loud.\n\n" +
			"Who is speaking is evidence, and it is named. One microphone picks up a whole room, so some of " +
			"what arrives is people talking to each other: a well-formed question that already has somebody " +
			"to answer it. When the line says someone other than the person this conversation is with, their " +
			"words are a reason to act only if the person this conversation is with has given the agent " +
			"something to do about them - handle the call, order for me, translate what they say. Without " +
			"that, a question in the room is not a question to the agent, and the answer to it is not one " +
			"either, and answering makes the agent a third person in somebody else's conversation.\n\n" +
			"The sentence that states a policy does not satisfy it. \"Count the animals as I mention them\" " +
			"mentions no animal; \"tell me when the build finishes\" is not the build finishing. Acting on " +
			"the request itself starts the count in the wrong place and everything after it is wrong by one, " +
			"which is worse than not having started - so while somebody is still setting a policy up, the " +
			"condition has not happened yet however plainly the words describe it.\n\n" +
			"A standing instruction that both asks for something and restricts everything else - count them and " +
			"say nothing else, tell me when it lands and otherwise stay quiet - is two rules, and the " +
			"restriction is the smaller of them. It narrows what may be said at other moments; it does not " +
			"cancel the thing they asked for. Reading it as a reason to stay silent when the condition has " +
			"just been met obeys the half of the sentence they added as an afterthought and ignores the half " +
			"they meant.\n\n" +
			"Standing instructions govern this decision and outrank every general rule below. If someone asked " +
			"not to be interrupted, do not interrupt them. If someone asked to be told the moment something " +
			"happens, tell them the moment it happens, even in the middle of their own sentence. If someone " +
			"asked for a running commentary, give it while they keep talking.\n\n" +
			"The acts:\n" +
			"listen - say nothing and start nothing; keep taking the situation in.\n" +
			"speak-through - say something while the other speaker keeps the floor. They have not finished, you " +
			"are not taking over, and they can talk straight through you. Anything somebody asked to have " +
			"said as they go - a count, a running total, a translation, a warning the moment it applies - " +
			"is this rather than interrupting: they asked for it while they carry on, which means they are " +
			"keeping the floor.\n" +
			"answer - the speaker has finished, or nobody is speaking, and the turn is the agent's.\n" +
			"interrupt - the speaker has not finished, and what is happening is worth cutting into their " +
			"sentence for.\n" +
			"call-tool - do something without saying anything at all. Only when speech would be pointless or " +
			"unwelcome: a recorded menu that cannot hear you, or someone who asked not to be spoken to. " +
			"Answering already lets the agent act as well as speak, so this is for when it must not speak.\n" +
			"keep-speaking - the agent is mid-sentence and someone else has started; carry on anyway.\n" +
			"stop-speaking - the agent is mid-sentence; stop and let them have the floor.\n\n" +
			"Anything other than staying silent, or carrying on with what the agent is already saying, needs a reason " +
			"you could state in a sentence: a standing instruction whose condition has actually been met, an " +
			"utterance that has genuinely finished, or something that will be too late if it waits. If you " +
			"cannot name that reason, there is not one, and the answer is to leave things as they are.\n\n" +
			"A recogniser breaks a sentence wherever the speaker draws breath, so one thing somebody says " +
			"often arrives as several, each capitalised and punctuated into something that reads like a " +
			"sentence of its own. The gap before an utterance tells them apart. A few hundred " +
			"milliseconds means it is the tail of what came just before, and if the agent has already " +
			"answered that, there is nothing left to answer. Seconds mean a new thing was said.\n\n" +
			"That is about answering, and about nothing else. A standing instruction that asks for " +
			"something each time a condition occurs applies to every piece of a broken-up sentence exactly " +
			"as it would to a whole one - somebody who asked to be counted at, or interpreted for, is not " +
			"asking any less because the recogniser split their sentence.\n\n" +
			"Two rules that are easy to get backwards. Silence is neither necessary nor sufficient: someone who " +
			"paused mid-thought has not finished, and someone who never pauses may already have said the thing " +
			"worth acting on. And work already running has already been decided: do not start the same work a " +
			"second time while it is in flight, though that is no reason to stay silent - someone who has been " +
			"waiting through a long silence may still need to be told what is happening.\n\n" +
			"Worked examples. These are other conversations, not this one.\n")
	for _, example := range examples() {
		text.WriteString("\n---\n" + example.state.Render() + "\n-> " + string(example.act) + " (" + example.why + ")\n")
	}
	text.WriteString("\n---\nNow decide the case below. Reply with one act name and nothing else.")
	return text.String()
}
