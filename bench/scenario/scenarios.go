package scenario

// Suite is the scripted behaviour an interaction model exists to produce.
//
// Every scenario here is a claim that could not be made by any other suite in
// this repository, because every one of them is about when somebody speaks
// rather than about what they conclude. They come from two places: the
// capabilities a full-duplex model was demonstrated doing, and the cases that
// turn up in production the moment an agent has to hold a phone call.
//
// The control scenario is not padding. Most of these ask the agent to stay
// quiet, and a system that had simply stopped talking would pass all of them.
func Suite() []Scenario {
	return []Scenario{
		{
			Name:         "count-as-they-go",
			Note:         "a policy set out loud, honoured while the speaker keeps talking and ignored in their pauses",
			Instructions: "You are a voice assistant. Follow the instructions the person gives you about when to speak.",
			Script: []Line{
				{Speaker: "user", AtMS: 0, Text: "I'm going to tell you about my afternoon. Count the animals out loud as I mention them, and say nothing else."},
				// The gap after the instruction is deliberate. A person who sets a
				// policy waits for the "okay" before starting, and measured
				// response latency here is about two seconds - so a story that
				// began two seconds after the instruction ended guaranteed the
				// acknowledgement would land inside the first line's window and
				// be scored as counting the wrong thing.
				{Speaker: "user", AtMS: 13000, Text: "It was a warm afternoon and I was walking along by the river."},
				{Speaker: "user", AtMS: 21000, Text: "A capybara wandered over and sat down next to me."},
				{Speaker: "user", AtMS: 29000, Text: "Then a heron landed on the far bank and stared at us."},
			},
			TrailingMS: 4000,
			Checks: []Check{
				{Kind: CheckSilent, Line: 1, AfterMS: 3000,
					Note: "no animal was mentioned and the pause afterwards is not an invitation"},
				// Two claims, kept apart. Whether it counted the right animal is a
				// capability; how soon is a latency, and folding them into one
				// window scored a run that counted both correctly as a failure
				// for being three hundred milliseconds late.
				{Kind: CheckSaid, Line: 2, AfterMS: 5000, Any: []string{"one", "1"},
					Note: "counting means saying the count, for the animal that was mentioned"},
				{Kind: CheckSaid, Line: 3, AfterMS: 5000, Any: []string{"two", "2"},
					Note: "and the second animal is two"},
				{Kind: CheckNotSaid, Line: 2, AfterMS: 5000, Any: []string{"?"},
					Note: "they asked to be counted at, not interviewed"},
				{Kind: CheckAnsweredWithin, Line: 2, AfterMS: 5000,
					Note: "counting as somebody goes is a claim about timing, so it is measured as one"},
			},
		},
		{
			Name:         "asked not to be interrupted",
			Note:         "the same silence, held for the opposite reason: they said so",
			Instructions: "You are a voice assistant. Follow the instructions the person gives you about when to speak.",
			Script: []Line{
				{Speaker: "user", AtMS: 0, Text: "I want to think out loud for a minute. Please don't interrupt me, and don't answer until I ask you to."},
				{Speaker: "user", AtMS: 9000, Text: "So the first option would be to rewrite the whole thing from scratch."},
				{Speaker: "user", AtMS: 18000, Text: "But that would take most of a month and I'm not sure it's worth it."},
			},
			TrailingMS: 5000,
			Checks: []Check{
				{Kind: CheckSilent, Line: 1, AfterMS: 4000, Note: "a long pause is still not permission"},
				{Kind: CheckSilent, Line: 2, AfterMS: 4000, Note: "they have not asked yet"},
			},
		},
		{
			Name: "a recorded menu",
			Note: "the useful act is silent, and talking to a recording achieves nothing",
			Instructions: "You are calling a company's support line on behalf of the user. " +
				"When a recorded menu offers an option that matches what the user wants, press that key.",
			Tools: []Tool{{
				Name: "press_key", Description: "Send a keypad tone on the open call.",
				Parameters: []string{"digit"},
			}},
			Script: []Line{
				{Speaker: "user", AtMS: 0, Text: "Call them and find out where my order has got to."},
				{Speaker: "other", AtMS: 7000, Text: "Thank you for calling. Press one for billing. Press two for order status. Press three for technical support. Press four to repeat these options."},
			},
			TrailingMS: 6000,
			Checks: []Check{
				{Kind: CheckToolCalled, Tool: "press_key", Line: -1,
					Note: "the menu named the option the user asked for"},
				{Kind: CheckSilent, Line: 1, AfterMS: 500,
					Note: "a recording cannot hear you, so speaking over it is wasted and covers the menu"},
				{Kind: CheckAnsweredWithin, Line: 1, AfterMS: 4000, Tool: "press_key",
					Note: "a menu moves on, and a key pressed after it has is pressed into the next option"},
			},
		},
		{
			Name: "cutting in on something wrong",
			Note: "waiting until they finish makes the correction useless, which is why interrupting exists",
			Instructions: "You are a voice assistant helping plan a project. The deadline the client gave is " +
				"the third of the month. If the person says a date that contradicts that, correct them " +
				"immediately, even in the middle of their sentence.",
			Script: []Line{
				{Speaker: "user", AtMS: 0, Text: "Right, so let me plan this out. We'll do the design review next week, and then ship it by the thirteenth, which gives us plenty of time to get the documentation finished and send everything over to their team for sign off."},
			},
			TrailingMS: 5000,
			Checks: []Check{
				{Kind: CheckSpoke, Line: 0, AfterMS: -3000,
					Note: "cutting in means before they finish; waiting politely for the end is the behaviour this replaces"},
				{Kind: CheckSaid, Line: 0, AfterMS: 2000, Any: []string{"third", "3rd", "the 3"},
					Note: "and it has to be the correction; interrupting with an unrelated question is worse than waiting"},
			},
		},
		{
			Name: "ordering from a waiter",
			Note: "a third party lists options and the moment worth acting on passes if you wait for a pause",
			Instructions: "You are helping the user in a restaurant. They have told you what they want. " +
				"When the waiter says something that matches it, say so straight away - the waiter " +
				"will move on to the next item and the moment will be gone.",
			Script: []Line{
				{Speaker: "user", AtMS: 0, Text: "I want fish tonight. Order for me when you hear something that fits."},
				{Speaker: "other", AtMS: 7000, Text: "Good evening. Tonight we have three specials. The first is a ribeye steak with peppercorn sauce. The second is a duck confit with cherries. The third is a sea bass with fennel and new potatoes."},
			},
			TrailingMS: 6000,
			Checks: []Check{
				{Kind: CheckSpoke, Line: 1, AfterMS: 2000,
					Note: "the dish that fits was named and the waiter kept going"},
				{Kind: CheckSaid, Line: 1, AfterMS: 4000, Any: []string{"sea bass", "bass", "fish"},
					Note: "and it has to be about the right dish, not just a noise at the right time"},
			},
		},
		{
			Name: "translating as they speak",
			Note: "the simultaneous-speech case: a policy never to yield the floor, and speech that " +
				"does not end anybody's turn",
			Instructions: "You are interpreting for the user. Do exactly what they ask you to do.",
			// Mandarin rather than a European language, because the recogniser
			// this suite runs against covers Chinese, English, Cantonese,
			// Japanese and Korean and nothing else. Asked to interpret German
			// it returned the right sounds spelled as English words, and the
			// agent faithfully translated the nonsense - which measures the
			// recogniser's language coverage rather than anything about
			// interaction. Live interpreting is bounded by what the recogniser
			// can hear, and that bound belongs in the scenario rather than in
			// its result.
			Script: []Line{
				{Speaker: "user", AtMS: 0, Text: "My colleague only speaks Mandarin. Translate everything he says into English as he goes, and don't wait for him to finish."},
				{Speaker: "other", AtMS: 13000, Text: "你好，很高兴见到你。"},
				{Speaker: "other", AtMS: 20000, Text: "我们明天下午三点在办公室见面。"},
			},
			TrailingMS: 6000,
			Checks: []Check{
				{Kind: CheckSpoke, Line: 1, AfterMS: 4000, Note: "the first sentence should be carried over"},
				{Kind: CheckSaid, Line: 1, AfterMS: 5000,
					Any:  []string{"good day", "good afternoon", "hello", "pleased", "nice to meet", "meet you"},
					Note: "and carried over into English rather than commented on"},
				{Kind: CheckSaid, Line: 2, AfterMS: 5000,
					Any:  []string{"three", "3", "tomorrow", "office"},
					Note: "the second sentence too, which means it never stopped to hand the floor back"},
			},
		},
		{
			Name: "waiting out a silence they asked for",
			Note: "the trigger is the clock: nothing is said and nothing is seen, and the moment " +
				"arrives because time passed",
			Instructions: "You are a voice assistant. Follow the instructions the person gives you " +
				"about when to speak.",
			Script: []Line{
				{Speaker: "user", AtMS: 0, Text: "I'm going to read something now, so I'll be quiet for a while. If I haven't said anything for about fifteen seconds, ask whether I'm still there."},
			},
			// Long enough for the condition to arrive and for a premature
			// answer to be distinguishable from a punctual one.
			TrailingMS: 30000,
			Checks: []Check{
				// Every other scenario here is triggered by speech or a frame.
				// This one has neither after the instruction, so the only
				// thing that can move the agent is the silence lengthening -
				// which is the one input the interaction model is given on
				// every decision and has never been asked to act on.
				// From five seconds, because saying "I will" is what the
				// voice is told to do when somebody sets a policy, and the
				// scenarios beside this one depend on it happening.
				{Kind: CheckSilent, Line: 0, FromMS: 5000, AfterMS: 12000,
					Note: "they said fifteen seconds; speaking at eight is not waiting"},
				{Kind: CheckSaid, Line: 0, AfterMS: 26000,
					Any:  []string{"still there", "still with", "everything all right", "you there", "all right"},
					Note: "and when it does arrive it is the thing they asked for"},
			},
		},
		{
			Name: "somebody else's conversation",
			Note: "the complement of the waiter: a third party talking near the microphone who is " +
				"not addressing the agent at all",
			Instructions: "You are a voice assistant helping the user with their work. " +
				"Answer them when they speak to you.",
			Script: []Line{
				{Speaker: "user", AtMS: 0, Text: "I'm just going to get on with this for a bit."},
				// Directed at each other, in the room, about nothing the agent
				// has anything to do with. Every word of it is a well-formed
				// request to somebody.
				{Speaker: "other", AtMS: 8000, Text: "Did you get the milk on the way in? I looked in the fridge and there wasn't any."},
				{Speaker: "other", AtMS: 16000, Text: "No, I forgot again. Can you put it on the list for tomorrow?"},
			},
			TrailingMS: 6000,
			Checks: []Check{
				{Kind: CheckSilent, Line: 1, AfterMS: 4000,
					Note: "a question in the room is not a question to the agent"},
				{Kind: CheckSilent, Line: 2, AfterMS: 4000,
					Note: "and neither is the answer to it"},
			},
		},
		{
			Name:         "an acknowledgement is not an interruption",
			Note:         "the agent is mid-sentence and somebody says mhm; stopping would be wrong",
			Instructions: "You are a helpful voice assistant. When asked what you found, give the detail you have.",
			Script: []Line{
				{Speaker: "user", AtMS: 0, Text: "Tell me everything you know about the refund process, in as much detail as you can manage."},
				{Speaker: "user", AtMS: 9000, Text: "Mhm."},
				{Speaker: "user", AtMS: 11500, Text: "Right, yeah."},
			},
			TrailingMS: 6000,
			Checks: []Check{
				{Kind: CheckSpoke, Line: 0, AfterMS: 5000, Note: "they asked for detail"},
				{Kind: CheckNotSaid, Line: 2, AfterMS: 4000,
					Any:  []string{"what would you like", "how can I help", "anything else", "sorry"},
					Note: "an acknowledgement is not a new question and must not restart the turn",
				},
			},
		},
		{
			Name: "telling them what it saw",
			Note: "the visual case: nobody is speaking at all, and what makes the moment is something " +
				"the agent saw",
			Instructions: "You are watching the user's screen while they work. Follow the instructions " +
				"they give you about when to speak.",
			Script: []Line{
				{Speaker: "user", AtMS: 0, Text: "I'm going to read for a bit. Tell me the moment the build finishes, and don't say anything else."},
			},
			Sees: []Sight{
				{AtMS: 7000, Path: "bench/scenario/testdata/build-running.png",
					Note: "the condition has not happened; nothing here is worth speaking about"},
				{AtMS: 16000, Path: "bench/scenario/testdata/build-finished.png",
					Note: "the condition they asked to be told about"},
			},
			TrailingMS: 18000,
			Checks: []Check{
				// Four seconds. The frame is narrated by a vision model before
				// it is an observation at all, and measured it lands 1.8s after
				// the frame - so this is a real bound rather than a generous
				// one. An earlier version allowed fifteen, which turned out to
				// be measuring a duplicate acknowledgement rather than latency.
				{Kind: CheckSaid, Sight: 2, AfterMS: 4000,
					Any:  []string{"finished", "done", "built", "passed", "complete"},
					Note: "after seeing it finish, not when acknowledging that they would watch for it"},
				{Kind: CheckAnsweredWithin, Sight: 2, AfterMS: 3000,
					Note: "\"the moment it finishes\" is a claim about latency, so it is measured as one"},
				{Kind: CheckSilent, Sight: 1, AfterMS: 7000,
					Note: "the first screen shows it still running, which is not the moment they asked for"},
			},
		},
		{
			Name: "an ordinary question",
			Note: "the control: most of this suite asks for silence, and a system that had simply " +
				"stopped talking would pass all of it",
			Instructions: "You are a helpful voice assistant. Answer briefly.",
			Script: []Line{
				{Speaker: "user", AtMS: 0, Text: "What's the capital of France?"},
			},
			TrailingMS: 6000,
			Checks: []Check{
				{Kind: CheckSpoke, Line: 0, AfterMS: 4000, Note: "a finished question with nothing standing in the way"},
				{Kind: CheckAnsweredWithin, Line: 0, AfterMS: 2500,
					Note: "waveform time: from the last sample of their question to the first of the answer"},
				{Kind: CheckSaid, Any: []string{"paris"}, Line: -1, Note: "and the answer should be right"},
			},
		},
	}
}
