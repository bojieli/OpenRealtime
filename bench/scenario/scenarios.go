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
				{Speaker: "user", AtMS: 9000, Text: "It was a warm afternoon and I was walking along by the river."},
				{Speaker: "user", AtMS: 17000, Text: "A capybara wandered over and sat down next to me."},
				{Speaker: "user", AtMS: 25000, Text: "Then a heron landed on the far bank and stared at us."},
			},
			TrailingMS: 4000,
			Checks: []Check{
				{Kind: CheckSilent, Line: 1, AfterMS: 3000,
					Note: "no animal was mentioned and the pause afterwards is not an invitation"},
				{Kind: CheckSpoke, Line: 2, AfterMS: 3000, Note: "a capybara is an animal and they asked to be told"},
				{Kind: CheckSpoke, Line: 3, AfterMS: 3000, Note: "a heron is the second one"},
				{Kind: CheckSaid, Line: 2, AfterMS: 3000, Any: []string{"one", "1"},
					Note: "counting means saying the count, at the moment the animal is mentioned"},
				{Kind: CheckSaid, Line: 3, AfterMS: 3000, Any: []string{"two", "2"},
					Note: "and the second animal is two"},
				{Kind: CheckNotSaid, Line: 2, AfterMS: 3000, Any: []string{"?"},
					Note: "they asked to be counted at, not interviewed"},
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
				{Kind: CheckSaid, Any: []string{"paris"}, Line: -1, Note: "and the answer should be right"},
			},
		},
	}
}
