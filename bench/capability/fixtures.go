package capability

import "time"

// The pilot suite: twenty-four matched pairs, three in each of the eight task
// families.
//
// Every pair is written the same way. A shared prefix sets up a question long
// enough that the agent is still answering it when the branches diverge, and
// each branch then contributes one authored phrase that the other does not
// have. Scoring terms are preregistered here, with the fixture, rather than
// chosen after a run: a scorer written once the outputs are known measures the
// outputs.
//
// The required terms are deliberately disjunctions of synonyms, so the scorer
// rewards the content and not one particular wording, and every content
// expectation carries a floor on how much is said, so that acknowledging the
// correction without acting on it fails.

// midAnswer is how far into the agent's answer a branch's first phrase lands.
// The agent has to be audibly speaking for mid-speech adaptation to be the
// thing under test rather than ordinary turn-taking.
const midAnswer = 3500 * time.Millisecond

func branchAt(prefix *script, into time.Duration) *script {
	return newScript(prefix.ends() + into)
}

func req(groups ...[]string) [][]string { return groups }

// Pilot is the frozen pilot split.
func Pilot() []Pair {
	pairs := []Pair{}
	pairs = append(pairs, semanticCorrection()...)
	pairs = append(pairs, steering()...)
	pairs = append(pairs, prosodic()...)
	pairs = append(pairs, silence()...)
	pairs = append(pairs, addressing()...)
	pairs = append(pairs, proactive()...)
	pairs = append(pairs, concurrent()...)
	pairs = append(pairs, revision()...)
	return pairs
}

// PairByID returns one pilot pair.
func PairByID(id string) (Pair, bool) {
	for _, pair := range Pilot() {
		if pair.ID == id {
			return pair, true
		}
	}
	return Pair{}, false
}

// --------------------------------------------------------------------------
// Semantic correction during speech.
//
// The correction replaces a referent the agent is in the middle of talking
// about. The neutral branch says something equally long that changes nothing,
// so a system that simply restarts on any interruption fails it.

func semanticCorrection() []Pair {
	const travel = "You are a travel assistant in a live voice conversation. Give specific, practical suggestions. Keep each stretch of speech short."
	japan := newScript(0).say("open", SpeakerUser,
		"I'm booking a trip to Japan next month and I want to spend three days in Tokyo")
	japanA := branchAt(japan, midAnswer).say("correct", SpeakerUser, "sorry I meant Kyoto not Tokyo")
	japanB := branchAt(japan, midAnswer).say("agree", SpeakerUser, "yes that is the sort of thing I want")

	const cooking = "You are a cooking assistant in a live voice conversation. Give specific steps. Keep each stretch of speech short."
	sauce := newScript(0).say("open", SpeakerUser,
		"I'm making a tomato pasta sauce tonight and I want to start it off with butter")
	sauceA := branchAt(sauce, midAnswer).say("correct", SpeakerUser, "actually use olive oil instead of butter")
	sauceB := branchAt(sauce, midAnswer).say("agree", SpeakerUser, "yes that is how I usually do it")

	const planning = "You are an assistant helping plan work meetings in a live voice conversation. Keep each stretch of speech short."
	meeting := newScript(0).say("open", SpeakerUser,
		"Can you help me plan the agenda for the team meeting on Tuesday morning")
	meetingA := branchAt(meeting, midAnswer).say("correct", SpeakerUser, "the meeting moved to Thursday afternoon")
	meetingB := branchAt(meeting, midAnswer).say("agree", SpeakerUser, "that ordering works for me")

	return []Pair{{
		ID: "sc-01", Family: FamilySemanticCorrection, Split: SplitPilot, Instructions: travel,
		Prefix: japan.done(),
		Variants: []Variant{{
			ID: "corrected", Label: "Kyoto, not Tokyo", Feedback: "correct", Events: japanA.done(),
			Expect: Expectation{
				After: japanA.lastWord(), Within: 9 * time.Second, MinWords: 12,
				RequireAnyOf: req(
					[]string{"kyoto"},
					[]string{"temple", "shrine", "fushimi", "gion", "arashiyama", "bamboo",
						"kinkaku", "geisha", "higashiyama", "nijo", "philosopher", "kiyomizu", "nishiki"}),
				Forbid: []string{"shibuya", "shinjuku", "asakusa", "harajuku", "akihabara", "tokyo tower", "ginza"},
			},
		}, {
			ID: "unchanged", Label: "neutral acknowledgement", Feedback: "agree", Events: japanB.done(),
			Expect: Expectation{
				After: japanB.lastWord(), Within: 9 * time.Second, MinWords: 12,
				RequireAnyOf: req([]string{"tokyo", "shibuya", "shinjuku", "asakusa", "harajuku",
					"akihabara", "ginza", "tsukiji", "ueno", "meiji"}),
				Forbid: []string{"kyoto"},
			},
		}},
	}, {
		ID: "sc-02", Family: FamilySemanticCorrection, Split: SplitPilot, Instructions: cooking,
		Prefix: sauce.done(),
		Variants: []Variant{{
			ID: "corrected", Label: "olive oil, not butter", Feedback: "correct", Events: sauceA.done(),
			Expect: Expectation{
				After: sauceA.lastWord(), Within: 9 * time.Second, MinWords: 10,
				RequireAnyOf: req(
					[]string{"olive oil", "olive"},
					[]string{"garlic", "onion", "simmer", "heat", "pan", "saute", "sauté",
						"soften", "warm", "tomato", "basil"}),
			},
		}, {
			ID: "unchanged", Label: "neutral acknowledgement", Feedback: "agree", Events: sauceB.done(),
			Expect: Expectation{
				After: sauceB.lastWord(), Within: 9 * time.Second, MinWords: 10,
				RequireAnyOf: req([]string{"butter", "garlic", "onion", "simmer", "pan", "tomato"}),
				Forbid:       []string{"olive oil"},
			},
		}},
	}, {
		ID: "sc-03", Family: FamilySemanticCorrection, Split: SplitPilot, Instructions: planning,
		Prefix: meeting.done(),
		Variants: []Variant{{
			ID: "corrected", Label: "Thursday afternoon, not Tuesday morning", Feedback: "correct",
			Events: meetingA.done(),
			Expect: Expectation{
				After: meetingA.lastWord(), Within: 9 * time.Second, MinWords: 10,
				RequireAnyOf: req([]string{"thursday"}, []string{"afternoon", "agenda", "item", "slot", "start"}),
			},
		}, {
			ID: "unchanged", Label: "neutral acknowledgement", Feedback: "agree", Events: meetingB.done(),
			Expect: Expectation{
				After: meetingB.lastWord(), Within: 9 * time.Second, MinWords: 10,
				RequireAnyOf: req([]string{"agenda", "item", "morning", "tuesday", "minutes", "start"}),
				Forbid:       []string{"thursday"},
			},
		}},
	}}
}

// --------------------------------------------------------------------------
// Mid-explanation steering.
//
// Both branches interrupt an explanation with the same kind of phrase and ask
// for opposite things. The topic named in each is explicit, so what counts as
// the right continuation is decided by the fixture rather than by whatever the
// agent happened to be saying.

func steering() []Pair {
	const explainer = "You are a patient technical explainer in a live voice conversation. Explain in order, in short stretches of speech."
	dns := newScript(0).say("open", SpeakerUser,
		"Can you walk me through what actually happens when I type a web address into my browser")
	dnsA := branchAt(dns, midAnswer).say("skip", SpeakerUser,
		"I already know how the name lookup works skip ahead to the connection")
	dnsB := branchAt(dns, midAnswer).say("deepen", SpeakerUser,
		"wait explain the name lookup part in more detail")

	bread := newScript(0).say("open", SpeakerUser,
		"How do I get a sourdough starter going from scratch at home")
	breadA := branchAt(bread, midAnswer).say("skip", SpeakerUser,
		"I already know the feeding part skip ahead to baking the loaf")
	breadB := branchAt(bread, midAnswer).say("deepen", SpeakerUser,
		"hold on explain the feeding schedule in more detail")

	pump := newScript(0).say("open", SpeakerUser,
		"Can you explain how a heat pump actually heats a house in winter")
	pumpA := branchAt(pump, midAnswer).say("skip", SpeakerUser,
		"I understand the refrigerant cycle already move on to what it costs to run")
	pumpB := branchAt(pump, midAnswer).say("deepen", SpeakerUser,
		"go back and explain the refrigerant cycle more slowly")

	return []Pair{{
		ID: "st-01", Family: FamilySteering, Split: SplitPilot, Instructions: explainer,
		Prefix: dns.done(),
		Variants: []Variant{{
			ID: "skip", Label: "I know that part", Feedback: "skip", Events: dnsA.done(),
			Expect: Expectation{
				After: dnsA.lastWord(), Within: 9 * time.Second, MinWords: 12,
				RequireAnyOf: req([]string{"tcp", "handshake", "tls", "https", "connect", "connection",
					"port", "request", "socket", "certificate"}),
				Forbid: []string{"recursive", "root server", "authoritative", "nameserver"},
			},
		}, {
			ID: "deepen", Label: "explain that part", Feedback: "deepen", Events: dnsB.done(),
			Expect: Expectation{
				After: dnsB.lastWord(), Within: 9 * time.Second, MinWords: 12,
				RequireAnyOf: req([]string{"recursive", "resolver", "root", "authoritative", "cache",
					"nameserver", "ttl", "dns", "zone"}),
				Forbid: []string{"handshake", "tls", "certificate"},
			},
		}},
	}, {
		ID: "st-02", Family: FamilySteering, Split: SplitPilot, Instructions: explainer,
		Prefix: bread.done(),
		Variants: []Variant{{
			ID: "skip", Label: "I know that part", Feedback: "skip", Events: breadA.done(),
			Expect: Expectation{
				After: breadA.lastWord(), Within: 9 * time.Second, MinWords: 12,
				RequireAnyOf: req([]string{"oven", "bake", "dutch oven", "score", "steam", "preheat",
					"loaf", "shape", "crust", "proof"}),
				Forbid: []string{"discard", "feeding schedule", "every twelve hours"},
			},
		}, {
			ID: "deepen", Label: "explain that part", Feedback: "deepen", Events: breadB.done(),
			Expect: Expectation{
				After: breadB.lastWord(), Within: 9 * time.Second, MinWords: 12,
				RequireAnyOf: req([]string{"feed", "feeding", "discard", "ratio", "twelve hours",
					"once a day", "grams", "flour and water", "bubbles"}),
				Forbid: []string{"preheat", "dutch oven", "crust"},
			},
		}},
	}, {
		ID: "st-03", Family: FamilySteering, Split: SplitPilot, Instructions: explainer,
		Prefix: pump.done(),
		Variants: []Variant{{
			ID: "skip", Label: "I know that part", Feedback: "skip", Events: pumpA.done(),
			Expect: Expectation{
				After: pumpA.lastWord(), Within: 9 * time.Second, MinWords: 12,
				RequireAnyOf: req([]string{"cost", "electricity", "bill", "efficiency", "cop",
					"kilowatt", "kwh", "cheaper", "price", "running cost", "tariff"}),
				Forbid: []string{"refrigerant", "compressor", "evaporator", "condenser"},
			},
		}, {
			ID: "deepen", Label: "explain that part", Feedback: "deepen", Events: pumpB.done(),
			Expect: Expectation{
				After: pumpB.lastWord(), Within: 9 * time.Second, MinWords: 12,
				RequireAnyOf: req([]string{"refrigerant", "compressor", "evaporator", "condenser",
					"expansion", "boils", "pressure", "coil"}),
				Forbid: []string{"bill", "kwh", "tariff"},
			},
		}},
	}}
}

// --------------------------------------------------------------------------
// Prosodic acknowledgement.
//
// The two branches contain the same word. Everything that distinguishes them
// is in the cue, so a text-only cell cannot do better than pick one branch and
// be wrong on the other - which is the measurement, not a defect of the pair.

func prosodic() []Pair {
	const explainer = "You are a patient technical explainer in a live voice conversation. Explain in short stretches of speech, and check in when the other person sounds unsure."
	network := newScript(0).say("open", SpeakerUser,
		"I'm setting up a home network and I want to put the router down in the basement")
	networkA := branchAt(network, midAnswer).cue("react", SpeakerUser, "okay",
		"flat, falling pitch: a settled acknowledgement, not a question")
	networkB := branchAt(network, midAnswer).cue("react", SpeakerUser, "okay",
		"sharply rising pitch, drawn out: doubtful, questioning")

	const ops = "You are an operations assistant in a live voice conversation. Keep each stretch of speech short, and check in when the other person sounds unsure."
	backups := newScript(0).say("open", SpeakerUser,
		"I'm thinking of moving our nightly backups to run at midnight instead of noon")
	backupsA := branchAt(backups, midAnswer).cue("react", SpeakerUser, "sure",
		"falling pitch, brisk: agreement")
	backupsB := branchAt(backups, midAnswer).cue("react", SpeakerUser, "sure",
		"rising pitch, hesitant and drawn out: doubt")

	launch := newScript(0).say("open", SpeakerUser,
		"We should probably move the product launch to the first week of December")
	launchA := branchAt(launch, midAnswer).cue("react", SpeakerUser, "right",
		"falling pitch: following along, agreeing")
	launchB := branchAt(launch, midAnswer).cue("react", SpeakerUser, "right",
		"rising pitch with a held vowel: unconvinced, questioning")

	continuation := func(after string, terms []string) Expectation {
		return Expectation{
			After: after, Within: 9 * time.Second, MinWords: 10,
			RequireAnyOf: req(terms),
			Forbid: []string{"which part", "what part", "did i lose you", "does that make sense",
				"are you with me", "shall i go over"},
		}
	}
	checkIn := func(after string) Expectation {
		return Expectation{
			After: after, Within: 9 * time.Second, MinWords: 5,
			RequireAnyOf: req([]string{"which part", "what part", "not sure", "unsure", "do you want",
				"would you like", "shall i", "should i", "go over", "explain", "any of that",
				"does that", "are you", "sounded", "hesit", "a question"}),
		}
	}
	return []Pair{{
		ID: "pr-01", Family: FamilyProsodic, Split: SplitPilot, Instructions: explainer,
		Prefix: network.done(),
		Variants: []Variant{{
			ID: "affirmative", Label: "\"okay\" falling: keep going", Feedback: "react",
			Events: networkA.done(),
			Expect: continuation(networkA.lastWord(), []string{"router", "signal", "wifi", "wi-fi",
				"ethernet", "cable", "coverage", "floor", "concrete", "antenna", "access point", "mesh"}),
		}, {
			ID: "questioning", Label: "\"okay?\" rising: check in", Feedback: "react",
			Events: networkB.done(), Expect: checkIn(networkB.lastWord()),
		}},
	}, {
		ID: "pr-02", Family: FamilyProsodic, Split: SplitPilot, Instructions: ops,
		Prefix: backups.done(),
		Variants: []Variant{{
			ID: "affirmative", Label: "\"sure\" falling: keep going", Feedback: "react",
			Events: backupsA.done(),
			Expect: continuation(backupsA.lastWord(), []string{"backup", "midnight", "window", "load",
				"restore", "retention", "snapshot", "job", "overnight", "schedule"}),
		}, {
			ID: "questioning", Label: "\"sure?\" rising: check in", Feedback: "react",
			Events: backupsB.done(), Expect: checkIn(backupsB.lastWord()),
		}},
	}, {
		ID: "pr-03", Family: FamilyProsodic, Split: SplitPilot, Instructions: ops,
		Prefix: launch.done(),
		Variants: []Variant{{
			ID: "affirmative", Label: "\"right\" falling: keep going", Feedback: "react",
			Events: launchA.done(),
			Expect: continuation(launchA.lastWord(), []string{"launch", "december", "week", "marketing",
				"freeze", "release", "timeline", "date", "holiday", "team"}),
		}, {
			ID: "questioning", Label: "\"right?\" rising: check in", Feedback: "react",
			Events: launchB.done(), Expect: checkIn(launchB.lastWord()),
		}},
	}}
}

// --------------------------------------------------------------------------
// Silence and hesitation.
//
// The two branches say the same words. In one the speaker stops in the middle
// of the sentence and the right move is to keep waiting; in the other the
// sentence is finished and the right move is to answer. A system that always
// waits fails one branch and a system that always answers fails the other, so
// neither policy can score the pair by being uniform.

func silence() []Pair {
	waitFor := 2 * time.Second

	const booking = "You are taking a restaurant booking in a live voice conversation. Let the caller finish. Keep each stretch of speech short."
	dinner := newScript(0).say("open", SpeakerUser, "hi I'm calling about dinner tomorrow evening")
	dinnerA := branchAt(dinner, 5*time.Second).say("request", SpeakerUser, "I'd like to book a table for")
	dinnerAFeedback := dinnerA.lastWord()
	dinnerA.gap(2500*time.Millisecond).say("finish", SpeakerUser, "six people on Friday")
	dinnerB := branchAt(dinner, 5*time.Second).say("request", SpeakerUser,
		"I'd like to book a table for six people on Friday")

	const clinic = "You are a clinic receptionist in a live voice conversation. Let the caller finish. Keep each stretch of speech short."
	appointment := newScript(0).say("open", SpeakerUser, "I need to move my appointment")
	appointmentA := branchAt(appointment, 5*time.Second).say("request", SpeakerUser,
		"it's on the fifteenth at the moment but I need")
	appointmentAFeedback := appointmentA.lastWord()
	appointmentA.gap(2500*time.Millisecond).say("finish", SpeakerUser, "to push it to the following week")
	appointmentB := branchAt(appointment, 5*time.Second).say("request", SpeakerUser,
		"it's on the fifteenth at the moment but I need to push it to the following week")

	const support = "You are a laptop support agent in a live voice conversation. Let the caller finish. Keep each stretch of speech short."
	laptop := newScript(0).say("open", SpeakerUser, "my laptop has been acting up since yesterday")
	laptopA := branchAt(laptop, 5*time.Second).say("request", SpeakerUser,
		"the screen goes completely black whenever I")
	laptopAFeedback := laptopA.lastWord()
	laptopA.gap(2500*time.Millisecond).say("finish", SpeakerUser, "close the lid and open it again")
	laptopB := branchAt(laptop, 5*time.Second).say("request", SpeakerUser,
		"the screen goes completely black whenever I close the lid and open it again")

	return []Pair{{
		ID: "si-01", Family: FamilySilence, Split: SplitPilot, Instructions: booking,
		Prefix: dinner.done(),
		Variants: []Variant{{
			ID: "mid-sentence", Label: "pause inside the sentence: wait", Feedback: "request",
			Events: dinnerA.done(),
			Expect: Expectation{After: dinnerAFeedback, Within: waitFor, Silent: true},
		}, {
			ID: "sentence-finished", Label: "pause after the sentence: answer", Feedback: "request",
			Events: dinnerB.done(),
			Expect: Expectation{
				After: dinnerB.lastWord(), Within: 6 * time.Second, MinWords: 6,
				RequireAnyOf: req([]string{"friday", "six", "table", "book", "reserve", "time",
					"name", "party", "evening"}),
			},
		}},
	}, {
		ID: "si-02", Family: FamilySilence, Split: SplitPilot, Instructions: clinic,
		Prefix: appointment.done(),
		Variants: []Variant{{
			ID: "mid-sentence", Label: "pause inside the sentence: wait", Feedback: "request",
			Events: appointmentA.done(),
			Expect: Expectation{After: appointmentAFeedback, Within: waitFor, Silent: true},
		}, {
			ID: "sentence-finished", Label: "pause after the sentence: answer", Feedback: "request",
			Events: appointmentB.done(),
			Expect: Expectation{
				After: appointmentB.lastWord(), Within: 6 * time.Second, MinWords: 6,
				RequireAnyOf: req([]string{"week", "fifteenth", "move", "resched", "appointment",
					"twenty", "day", "slot", "available"}),
			},
		}},
	}, {
		ID: "si-03", Family: FamilySilence, Split: SplitPilot, Instructions: support,
		Prefix: laptop.done(),
		Variants: []Variant{{
			ID: "mid-sentence", Label: "pause inside the sentence: wait", Feedback: "request",
			Events: laptopA.done(),
			Expect: Expectation{After: laptopAFeedback, Within: waitFor, Silent: true},
		}, {
			ID: "sentence-finished", Label: "pause after the sentence: answer", Feedback: "request",
			Events: laptopB.done(),
			Expect: Expectation{
				After: laptopB.lastWord(), Within: 6 * time.Second, MinWords: 6,
				RequireAnyOf: req([]string{"lid", "sleep", "display", "driver", "screen", "settings",
					"power", "close", "wake"}),
			},
		}},
	}}
}

// --------------------------------------------------------------------------
// Addressing and overlap.
//
// One branch is addressed to the agent and the other to somebody else in the
// room, at the same moment and at the same volume in the transcript. The cue
// carries the acoustic evidence that distinguishes them - turned away, further
// from the microphone - so this family also asks what a single undiarised
// transcript can do.

func addressing() []Pair {
	const counter = "You are taking a food order in a live voice conversation. Keep each stretch of speech short. Only act on what is said to you."
	order := newScript(0).say("open", SpeakerUser,
		"can I get the mushroom burger with fries and a lemonade please")
	orderA := branchAt(order, midAnswer).cue("aside", SpeakerUser, "make that without onions",
		"close to the microphone, addressed to you")
	orderB := branchAt(order, midAnswer).cue("aside", "another person", "hey can you pass me the salt",
		"a different voice, further away and turned aside: not addressed to you")

	const directions = "You are giving directions in a live voice conversation. Keep each stretch of speech short. Only act on what is said to you."
	route := newScript(0).say("open", SpeakerUser,
		"how do I get from the station to the art museum from here")
	routeA := branchAt(route, midAnswer).cue("aside", SpeakerUser, "wait I'm driving not walking",
		"close to the microphone, addressed to you")
	routeB := branchAt(route, midAnswer).cue("aside", "another person", "could you turn the radio down",
		"a different voice, further away and turned aside: not addressed to you")

	const diary = "You are reading out a calendar in a live voice conversation. Keep each stretch of speech short. Only act on what is said to you."
	calendar := newScript(0).say("open", SpeakerUser,
		"what have I got on this Thursday afternoon and evening")
	calendarA := branchAt(calendar, midAnswer).cue("aside", SpeakerUser, "move the dentist to next week",
		"close to the microphone, addressed to you")
	calendarB := branchAt(calendar, midAnswer).cue("aside", "another person", "mum where are my football boots",
		"a child's voice from across the room: not addressed to you")

	return []Pair{{
		ID: "ad-01", Family: FamilyAddressing, Split: SplitPilot, Instructions: counter,
		Prefix: order.done(),
		Variants: []Variant{{
			ID: "to-agent", Label: "correction addressed to the agent", Feedback: "aside",
			Events: orderA.done(),
			Expect: Expectation{
				After: orderA.lastWord(), Within: 8 * time.Second, MinWords: 5,
				RequireAnyOf: req([]string{"onion"}),
				Forbid:       []string{"salt"},
			},
		}, {
			ID: "to-other", Label: "spoken to somebody else", Feedback: "aside", Events: orderB.done(),
			Expect: Expectation{
				After: orderB.lastWord(), Within: 8 * time.Second, MinWords: 5,
				RequireAnyOf: req([]string{"burger", "fries", "lemonade", "order", "anything else",
					"total", "ready", "drink", "mushroom"}),
				Forbid: []string{"salt"},
			},
		}},
	}, {
		ID: "ad-02", Family: FamilyAddressing, Split: SplitPilot, Instructions: directions,
		Prefix: route.done(),
		Variants: []Variant{{
			ID: "to-agent", Label: "correction addressed to the agent", Feedback: "aside",
			Events: routeA.done(),
			Expect: Expectation{
				After: routeA.lastWord(), Within: 8 * time.Second, MinWords: 6,
				RequireAnyOf: req([]string{"driv", "car", "park", "road", "lane", "traffic", "junction"}),
				Forbid:       []string{"radio"},
			},
		}, {
			ID: "to-other", Label: "spoken to somebody else", Feedback: "aside", Events: routeB.done(),
			Expect: Expectation{
				After: routeB.lastWord(), Within: 8 * time.Second, MinWords: 6,
				RequireAnyOf: req([]string{"walk", "left", "right", "street", "minutes", "cross",
					"straight", "museum", "station", "corner"}),
				Forbid: []string{"radio"},
			},
		}},
	}, {
		ID: "ad-03", Family: FamilyAddressing, Split: SplitPilot, Instructions: diary,
		Prefix: calendar.done(),
		Variants: []Variant{{
			ID: "to-agent", Label: "correction addressed to the agent", Feedback: "aside",
			Events: calendarA.done(),
			Expect: Expectation{
				After: calendarA.lastWord(), Within: 8 * time.Second, MinWords: 5,
				RequireAnyOf: req([]string{"dentist"}, []string{"next week", "moved", "move", "resched", "shift"}),
				Forbid:       []string{"boots", "football"},
			},
		}, {
			ID: "to-other", Label: "spoken to somebody else", Feedback: "aside", Events: calendarB.done(),
			Expect: Expectation{
				After: calendarB.lastWord(), Within: 8 * time.Second, MinWords: 5,
				RequireAnyOf: req([]string{"thursday", "afternoon", "evening", "dentist", "meeting",
					"calendar", "free", "booked", "o'clock"}),
				Forbid: []string{"boots", "football"},
			},
		}},
	}}
}

// --------------------------------------------------------------------------
// Proactive semantic action.
//
// One branch contains an error the agent should correct as it hears it. The
// other contains the same error, explicitly licensed. The pair measures the
// false-intervention rate directly: a system that corrects everything passes
// one branch and fails the other.

func proactive() []Pair {
	const writing = "You are helping edit writing in a live voice conversation. Keep each stretch of speech short. Point out factual mistakes as you hear them, unless the writer says the fact is deliberate."
	blog := newScript(0).say("open", SpeakerUser,
		"I'm tightening up the opening paragraph of my travel blog post can you help")
	blogA := branchAt(blog, midAnswer).say("claim", SpeakerUser,
		"it opens with Lisbon the capital of Spain has the best pastries anywhere")
	blogB := branchAt(blog, midAnswer).say("claim", SpeakerUser,
		"in my alternate history novel Lisbon is the capital of Spain and that is deliberate")

	const figures = "You are checking figures in a live voice conversation. Keep each stretch of speech short. Point out numbers that do not add up, unless the speaker says they are deliberate."
	slide := newScript(0).say("open", SpeakerUser,
		"let me read you the numbers on this slide before I present it")
	slideA := branchAt(slide, midAnswer).say("claim", SpeakerUser,
		"second quarter revenue was four million up from six million in the first quarter")
	slideB := branchAt(slide, midAnswer).say("claim", SpeakerUser,
		"for the deliberately wrong example slide I wrote four million up from six million")

	const lab = "You are reviewing lab safety text in a live voice conversation. Keep each stretch of speech short. Point out unsafe instructions as you hear them, unless the writer says the error is deliberate."
	handout := newScript(0).say("open", SpeakerUser,
		"let me read you the safety note I drafted for the lab handout")
	handoutA := branchAt(handout, midAnswer).say("claim", SpeakerUser,
		"it says for an acid spill rinse the area and then add more acid to neutralise it")
	handoutB := branchAt(handout, midAnswer).say("claim", SpeakerUser,
		"for the spot the mistake exercise I deliberately wrote add more acid to neutralise it")

	return []Pair{{
		ID: "pa-01", Family: FamilyProactive, Split: SplitPilot, Instructions: writing,
		Prefix: blog.done(),
		Variants: []Variant{{
			ID: "should-correct", Label: "an error stated as fact", Feedback: "claim", Events: blogA.done(),
			Expect: Expectation{
				After: blogA.lastWord(), Within: 8 * time.Second, MinWords: 6,
				RequireAnyOf: req([]string{"portugal"}),
			},
		}, {
			ID: "licensed", Label: "the same error, explicitly deliberate", Feedback: "claim",
			Events: blogB.done(),
			Expect: Expectation{
				After: blogB.lastWord(), Within: 8 * time.Second, MinWords: 6,
				RequireAnyOf: req([]string{"alternate", "novel", "world", "story", "reader", "opening",
					"line", "sentence", "tighten", "fiction"}),
				Forbid: []string{"portugal", "actually the capital", "that's incorrect", "that is incorrect"},
			},
		}},
	}, {
		ID: "pa-02", Family: FamilyProactive, Split: SplitPilot, Instructions: figures,
		Prefix: slide.done(),
		Variants: []Variant{{
			ID: "should-correct", Label: "figures that contradict themselves", Feedback: "claim",
			Events: slideA.done(),
			Expect: Expectation{
				After: slideA.lastWord(), Within: 8 * time.Second, MinWords: 6,
				RequireAnyOf: req([]string{"down", "not up", "decrease", "declin", "lower", "fell",
					"contradic", "doesn't add", "does not add", "discrepan", "drop"}),
			},
		}, {
			ID: "licensed", Label: "the same figures, explicitly deliberate", Feedback: "claim",
			Events: slideB.done(),
			Expect: Expectation{
				After: slideB.lastWord(), Within: 8 * time.Second, MinWords: 6,
				RequireAnyOf: req([]string{"deliberate", "example", "wrong on purpose", "label",
					"clear", "obvious", "spot", "audience", "slide"}),
				Forbid: []string{"doesn't add up", "does not add up", "that's a mistake", "that is a mistake"},
			},
		}},
	}, {
		ID: "pa-03", Family: FamilyProactive, Split: SplitPilot, Instructions: lab,
		Prefix: handout.done(),
		Variants: []Variant{{
			ID: "should-correct", Label: "an unsafe instruction stated as guidance", Feedback: "claim",
			Events: handoutA.done(),
			Expect: Expectation{
				After: handoutA.lastWord(), Within: 8 * time.Second, MinWords: 6,
				RequireAnyOf: req([]string{"don't", "do not", "never", "unsafe", "dangerous", "base",
					"bicarbonate", "soda", "wrong", "incorrect", "should not"}),
			},
		}, {
			ID: "licensed", Label: "the same instruction, explicitly deliberate", Feedback: "claim",
			Events: handoutB.done(),
			Expect: Expectation{
				After: handoutB.lastWord(), Within: 8 * time.Second, MinWords: 6,
				RequireAnyOf: req([]string{"exercise", "spot", "mistake", "student", "obvious",
					"subtle", "answer", "quiz", "works", "good"}),
			},
		}},
	}}
}

// --------------------------------------------------------------------------
// Concurrent task.
//
// The agent is already producing content on its own schedule when the
// instruction changes. Getting this right means the next thing it says is
// different, not that it stops and says it understood.

func concurrent() []Pair {
	const counting = "You are helping in a live voice conversation. When asked to count or translate out loud, keep doing it in short stretches until told otherwise."
	count := newScript(0).say("open", SpeakerUser,
		"please count out loud from one slowly while I look up my reservation number")
	countA := branchAt(count, midAnswer).say("change", SpeakerUser, "skip ahead to twenty")
	countB := branchAt(count, midAnswer).say("change", SpeakerUser, "keep going at that pace")

	translate := newScript(0).say("open", SpeakerUser,
		"I'll read out phrases and I want you to translate each one into French right after")
	translateA := branchAt(translate, midAnswer).say("change", SpeakerUser,
		"make it Spanish instead the phrase is good morning")
	translateB := branchAt(translate, midAnswer).say("change", SpeakerUser,
		"the first phrase is good morning")

	total := newScript(0).say("open", SpeakerUser,
		"I'll say some numbers and I want you to say the running total out loud")
	totalA := branchAt(total, midAnswer).say("change", SpeakerUser,
		"three then seven then reset the total and start again from four")
	totalB := branchAt(total, midAnswer).say("change", SpeakerUser, "three then seven")

	return []Pair{{
		ID: "co-01", Family: FamilyConcurrent, Split: SplitPilot, Instructions: counting,
		Prefix: count.done(),
		Variants: []Variant{{
			ID: "jump", Label: "skip ahead to twenty", Feedback: "change", Events: countA.done(),
			Expect: Expectation{
				After: countA.lastWord(), Within: 8 * time.Second, MinWords: 2,
				RequireAnyOf: req([]string{"twenty", "20"}),
			},
		}, {
			ID: "continue", Label: "keep going", Feedback: "change", Events: countB.done(),
			Expect: Expectation{
				After: countB.lastWord(), Within: 8 * time.Second, MinWords: 2,
				RequireAnyOf: req([]string{"four", "five", "six", "seven", "eight", "nine", "ten",
					"eleven", "twelve"}),
				Forbid: []string{"twenty"},
			},
		}},
	}, {
		ID: "co-02", Family: FamilyConcurrent, Split: SplitPilot, Instructions: counting,
		Prefix: translate.done(),
		Variants: []Variant{{
			ID: "switch-language", Label: "Spanish instead of French", Feedback: "change",
			Events: translateA.done(),
			Expect: Expectation{
				After: translateA.lastWord(), Within: 8 * time.Second, MinWords: 1,
				RequireAnyOf: req([]string{"buenos días", "buenos dias", "buenos"}),
				Forbid:       []string{"bonjour"},
			},
		}, {
			ID: "as-instructed", Label: "French, as first agreed", Feedback: "change",
			Events: translateB.done(),
			Expect: Expectation{
				After: translateB.lastWord(), Within: 8 * time.Second, MinWords: 1,
				RequireAnyOf: req([]string{"bonjour", "bon matin"}),
				Forbid:       []string{"buenos"},
			},
		}},
	}, {
		ID: "co-03", Family: FamilyConcurrent, Split: SplitPilot, Instructions: counting,
		Prefix: total.done(),
		Variants: []Variant{{
			ID: "reset", Label: "reset the running total", Feedback: "change", Events: totalA.done(),
			Expect: Expectation{
				After: totalA.lastWord(), Within: 8 * time.Second, MinWords: 1,
				RequireAnyOf: req([]string{"four", "4"}),
			},
		}, {
			ID: "accumulate", Label: "keep the running total", Feedback: "change", Events: totalB.done(),
			Expect: Expectation{
				After: totalB.lastWord(), Within: 8 * time.Second, MinWords: 1,
				RequireAnyOf: req([]string{"ten", "10"}),
			},
		}},
	}}
}

// --------------------------------------------------------------------------
// Output revision.
//
// Both branches carry the same new constraint. They differ only in when it
// arrives: early enough to change what has not been said, or late enough that
// something already spoken has to be repaired out loud. Revising pending words
// and repairing spoken ones are different acts, and the pair separates them.

func revision() []Pair {
	const host = "You are helping plan a dinner party in a live voice conversation. Offer suggestions one at a time in short stretches of speech."
	const early, late = 1500 * time.Millisecond, 7500 * time.Millisecond

	dessert := newScript(0).say("open", SpeakerUser,
		"can you suggest three desserts I could make for the dinner party tomorrow")
	dessertA := branchAt(dessert, early).say("constrain", SpeakerUser, "oh and nothing with dairy")
	dessertB := branchAt(dessert, late).say("constrain", SpeakerUser, "oh and nothing with dairy")

	const commute = "You are recommending podcasts in a live voice conversation. Offer them one at a time in short stretches of speech."
	pod := newScript(0).say("open", SpeakerUser,
		"give me three podcast recommendations for my commute this week")
	podA := branchAt(pod, early).say("constrain", SpeakerUser, "nothing longer than thirty minutes please")
	podB := branchAt(pod, late).say("constrain", SpeakerUser, "nothing longer than thirty minutes please")

	const offsite = "You are helping plan a team offsite in a live voice conversation. Offer options one at a time in short stretches of speech."
	cities := newScript(0).say("open", SpeakerUser,
		"list three cities we could hold the team offsite in this autumn")
	citiesA := branchAt(cities, early).say("constrain", SpeakerUser,
		"it has to have a direct flight from Denver")
	citiesB := branchAt(cities, late).say("constrain", SpeakerUser,
		"it has to have a direct flight from Denver")

	repair := []string{"scratch that", "i mentioned", "i said", "earlier", "instead of", "replace",
		"the second one", "the first one", "swap", "forget", "ignore", "take back", "rule out",
		"drop the", "that one out", "correction"}

	return []Pair{{
		ID: "ov-01", Family: FamilyRevision, Split: SplitPilot, Instructions: host,
		Prefix: dessert.done(),
		Variants: []Variant{{
			ID: "before-playing", Label: "constraint arrives before the options are spoken",
			Feedback: "constrain", Events: dessertA.done(),
			Expect: Expectation{
				After: dessertA.lastWord(), Within: 10 * time.Second, MinWords: 8,
				RequireAnyOf: req([]string{"sorbet", "fruit", "dairy free", "dairy-free", "vegan",
					"without dairy", "no dairy", "coconut", "olive oil cake", "meringue", "poach"}),
				Forbid: []string{"cheesecake", "panna cotta", "ice cream", "custard", "cream"},
			},
		}, {
			ID: "after-playing", Label: "constraint arrives once they have been spoken",
			Feedback: "constrain", Events: dessertB.done(),
			Expect: Expectation{
				After: dessertB.lastWord(), Within: 10 * time.Second, MinWords: 8,
				RequireAnyOf: req(repair, []string{"sorbet", "fruit", "dairy free", "dairy-free",
					"vegan", "without dairy", "no dairy", "coconut", "meringue", "poach"}),
			},
		}},
	}, {
		ID: "ov-02", Family: FamilyRevision, Split: SplitPilot, Instructions: commute,
		Prefix: pod.done(),
		Variants: []Variant{{
			ID: "before-playing", Label: "constraint arrives before the options are spoken",
			Feedback: "constrain", Events: podA.done(),
			Expect: Expectation{
				After: podA.lastWord(), Within: 10 * time.Second, MinWords: 8,
				RequireAnyOf: req([]string{"thirty", "30", "short", "under", "twenty", "fifteen", "brief"}),
			},
		}, {
			ID: "after-playing", Label: "constraint arrives once they have been spoken",
			Feedback: "constrain", Events: podB.done(),
			Expect: Expectation{
				After: podB.lastWord(), Within: 10 * time.Second, MinWords: 8,
				RequireAnyOf: req(repair, []string{"thirty", "30", "short", "under", "twenty",
					"fifteen", "brief"}),
			},
		}},
	}, {
		ID: "ov-03", Family: FamilyRevision, Split: SplitPilot, Instructions: offsite,
		Prefix: cities.done(),
		Variants: []Variant{{
			ID: "before-playing", Label: "constraint arrives before the options are spoken",
			Feedback: "constrain", Events: citiesA.done(),
			Expect: Expectation{
				After: citiesA.lastWord(), Within: 10 * time.Second, MinWords: 8,
				RequireAnyOf: req([]string{"direct", "nonstop", "non-stop", "denver", "flight", "fly"}),
			},
		}, {
			ID: "after-playing", Label: "constraint arrives once they have been spoken",
			Feedback: "constrain", Events: citiesB.done(),
			Expect: Expectation{
				After: citiesB.lastWord(), Within: 10 * time.Second, MinWords: 8,
				RequireAnyOf: req(repair, []string{"direct", "nonstop", "non-stop", "denver", "flight", "fly"}),
			},
		}},
	}}
}
