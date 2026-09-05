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

		// The pair a counting policy needs, and deliberately not about the
		// animal in any scenario: one example with an animal in it and the
		// answer "stay silent" teaches that the animal is the reason to wait.
		{Situation{
			Pins:    []string{"count the birds out loud as I mention them (40s ago)"},
			Recent:  []string{"user: I was down by the harbour"},
			Speaker: "user", Speaking: true,
			Heard:      "a cormorant was standing on the wall drying its",
			HeardSince: "a cormorant was standing on the wall drying its",
		}, ActSpeakThrough, "They asked to be counted at as they go, a bird has just been named, and they are still talking - so it is now, over them, and not when the sentence ends."},

		{Situation{
			Pins:    []string{"count the birds out loud as I mention them (40s ago)"},
			Recent:  []string{"user: a cormorant was standing on the wall", "agent: one"},
			Speaker: "user", Speaking: true,
			Heard: "a cormorant was standing on the wall drying its wings", HeardSince: "drying its wings",
		}, ActStaySilent, "Nothing makes acting necessary: the cormorant was counted already, and what is new since is the rest of the same sentence with no bird in it."},

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
		}, ActActSilently, "A recording is talking and cannot hear a reply, so anything said now is wasted while something is worth doing about what it just offered."},

		{Situation{
			Recent:  []string{"agent: what can I do for you?"},
			Speaker: "user", Heard: "ring them and get me through to a person", Silence: "900ms",
			Tools: []string{"press_key(digit) - send a keypad tone on the open call"},
		}, ActAnswer, "They have finished asking and are waiting on a reply, and it is a person who will hear it, so the turn is the agent's and it should be audible."},

		{Situation{
			Recent:  []string{"user: ring them and get me through to a person"},
			Speaker: "someone else in the room", Speaking: true,
			Heard: "thank you for calling",
			Tools: []string{"press_key(digit) - send a keypad tone on the open call"},
		}, ActStaySilent, "Nothing makes acting necessary: the recording has answered but has not offered anything yet, and a key pressed now is a key chosen at random."},

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

		{Situation{
			Recent:        []string{"user: what is the UV index today"},
			AgentSpeaking: true, AgentSaying: "The UV index today is 6, which is high",
			Speaker: "someone else in the room", Heard: "I'm going to take a nap",
			Silence: "200ms",
		}, ActKeepSpeaking, "Somebody else in the room said something to nobody in particular while the agent was mid-sentence. It is not a correction, a question, or a request, and stopping would abandon the answer the person actually asked for."},

		{Situation{
			Recent:        []string{"user: read me the deployment checklist"},
			AgentSpeaking: true, AgentSaying: "First, confirm the migration ran on the thirteenth",
			Speaker: "user", Heard: "wait, no, the third", Silence: "150ms",
		}, ActStopSpeaking, "The person this conversation is with is correcting what the agent is saying as it says it. Carrying on would keep asserting the thing they just told it was wrong."},
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
			"keeping the floor. As they go means now, on the piece that has just been said - a count given " +
			"once they have finished the sentence is a summary, and the thing they asked for was the " +
			"commentary.\n" +
			"answer - the speaker has finished, or nobody is speaking, and the turn is the agent's.\n" +
			"interrupt - the speaker has not finished, and what is happening is worth cutting into their " +
			"sentence for.\n" +
			"act-silently - engage without being heard. Only when speech would be pointless or unwelcome: a " +
			"recorded menu that cannot hear you, or someone who asked not to be spoken to. Answering already " +
			"lets the agent act as well as speak, so this is for when it must not speak. What it does once it " +
			"engages - press a key, look something up, or work out where things stand - is not yours to " +
			"decide and not something to reason about here. Only whether something has happened that is " +
			"worth engaging over, and that saying it out loud would be wasted.\n" +
			"keep-speaking - the agent is mid-sentence and someone else has started; carry on anyway.\n" +
			"stop-speaking - the agent is mid-sentence; stop and let them have the floor.\n\n" +
			"When those two are the choice, the agent is already speaking and the question is only whether " +
			"to abandon what it is saying. Stopping is not the safe answer: the person asked for the thing " +
			"being said, and an agent that stops for every voice it hears finishes nothing. Carry on unless " +
			"the words are directed at the agent and change what it should be doing - a correction of what " +
			"it is saying, a different request, or an explicit taking of the floor such as wait, hold on, or " +
			"actually. Speech from somebody else in the room, a remark addressed to nobody, and a listener's " +
			"acknowledgement are all reasons to carry on, and the same evidence about who is speaking that " +
			"governs answering governs this: a voice that is not talking to the agent does not take its " +
			"floor. Judge it on what has actually been heard so far, never on where the sentence might be " +
			"going.\n\n" +
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
			"something each time a condition occurs is not asking any less because the recogniser split the " +
			"sentence: the pieces are still what was said, and a condition that occurs in a later piece has " +
			"occurred. But it is the condition that fires it, not the arrival of more text. Judge it against " +
			"what is new since the agent last spoke. Partials grow by repeating everything heard so far, so " +
			"the same words arrive again and again inside a longer line, and something already counted, " +
			"translated or warned about has not happened twice because it was heard twice. If what is new " +
			"since the agent last spoke does not contain the thing they asked to be told about, the " +
			"condition has not occurred again. That line is always shown, and it says one of three " +
			"things: nothing has been said since the agent last spoke, or all of what was heard is new, " +
			"or it names the part that is.\n\n" +
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
