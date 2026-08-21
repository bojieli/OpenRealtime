package simulation

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bojieli/OpenRealtime/action"
)

// Scenarios returns the built-in motivating scenarios by name.
//
// Each one exists because it fails differently. Support exercises a tool call
// whose argument has to survive being spoken; the interview exercises turn
// alternation over many short exchanges; the debate exercises whether a large
// body of context is usable while a conversation is running rather than only
// in a single-shot answer.
func Scenarios() map[string]func() Scenario {
	return map[string]func() Scenario{
		"support":   SupportCall,
		"interview": Interview,
		"debate":    Debate,
	}
}

// ScenarioNames lists the built-in scenarios in a stable order.
func ScenarioNames() []string { return []string{"support", "interview", "debate"} }

// Lookup returns one built-in scenario.
func Lookup(name string) (Scenario, error) {
	factory, known := Scenarios()[strings.ToLower(strings.TrimSpace(name))]
	if !known {
		return Scenario{}, fmt.Errorf(
			"unknown scenario %q; known scenarios are %s", name, strings.Join(ScenarioNames(), ", "))
	}
	return factory(), nil
}

// orderID is the identifier the customer has to convey by voice.
//
// It is deliberately a mix of letters and digits. Getting it from one side to
// the other means it survives synthesis, the link, and recognition intact, and
// arriving in a tool argument is the only proof of that - a paraphrase would
// hide the failure.
const orderID = "R7742"

// SupportCall is a customer with a problem and an agent with a tool.
//
// The measurement is not whether the agent is polite. It is whether an
// identifier spoken by one model reaches the other model's tool call
// unmangled, which is the failure mode that makes voice tool-use hard and the
// one a text harness cannot see at all.
func SupportCall() Scenario {
	return Scenario{
		Name: "support",
		Question: "can an identifier spoken by one agent reach the other agent's " +
			"tool call intact, and does the answer come back in speech?",
		Left: Role{
			Name: "customer",
			Instruction: persona("a customer phoning a shop's support line about an order that has not arrived") +
				"Your order number is " + orderID + ". Say it clearly, letter by letter as " +
				spellOut(orderID) + ", whenever it is needed. " +
				"If the agent asks for the number again, repeat it the same way. " +
				"When the agent tells you where the order is, thank them and say goodbye.",
			Opens: true,
			Cue: "The agent has just picked up. Your first line is: " +
				"Hi, my order has not arrived. The order number is " + spellOut(orderID) + ".",
		},
		Right: Role{
			Name: "agent",
			Instruction: persona("a support agent for an online shop") +
				"When the customer gives an order number, call lookup_order with it immediately. " +
				"Write the number down exactly as you heard it, including letters. " +
				"When the result comes back, say the status and the delivery date in one sentence. " +
				"Do not ask for information you already have.",
			Tools: []action.ToolSpec{{
				Name:        "lookup_order",
				Description: "Look up the status and delivery date of an order by its order number.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {"order_id": {"type": "string", "description": "the order number"}},
					"required": ["order_id"]
				}`),
			}},
			Tool: func(name string, arguments json.RawMessage) (json.RawMessage, error) {
				if name != "lookup_order" {
					return nil, fmt.Errorf("no such tool %q", name)
				}
				var decoded struct {
					OrderID string `json:"order_id"`
				}
				_ = json.Unmarshal(arguments, &decoded)
				// The tool answers whatever it is given. Refusing an unknown
				// order would let the scenario pass or fail on the tool's
				// opinion rather than on what the agents managed to convey.
				return json.RawMessage(fmt.Sprintf(
					`{"order_id":%q,"status":"delayed in transit","delivery_date":"Thursday"}`,
					decoded.OrderID)), nil
			},
		},
		MaxTurns: 8,
		Checks: []Check{
			bothSidesSpoke(),
			{
				Name: "tool called",
				Why:  "a support agent that never looks anything up is not doing the job",
				Pass: func(conversation Conversation) (bool, string) {
					for _, call := range conversation.ToolCalls {
						if call.Name == "lookup_order" {
							return true, string(call.Arguments)
						}
					}
					return false, "lookup_order was never called"
				},
			},
			{
				Name: "spoken identifier survived the link",
				Why: "the order number is synthesised, carried as audio, and recognised; " +
					"arriving in the tool argument is the only proof it survived",
				Pass: func(conversation Conversation) (bool, string) {
					for _, call := range conversation.ToolCalls {
						if call.Name != "lookup_order" {
							continue
						}
						var decoded struct {
							OrderID string `json:"order_id"`
						}
						_ = json.Unmarshal(call.Arguments, &decoded)
						normalised := strings.ToUpper(strings.NewReplacer(
							" ", "", "-", "", ".", "", "#", "").Replace(decoded.OrderID))
						if strings.Contains(normalised, orderID) {
							return true, "order_id=" + decoded.OrderID
						}
						return false, fmt.Sprintf(
							"the agent called lookup_order with %q; the customer said %q",
							decoded.OrderID, orderID)
					}
					return false, "lookup_order was never called"
				},
			},
			{
				Name: "the answer was spoken back",
				Why:  "a tool result the customer never hears has not answered anything",
				Pass: func(conversation Conversation) (bool, string) {
					for _, turn := range conversation.TurnsBy("agent") {
						if contains(turn.Text, "thursday") || contains(turn.Text, "delay") {
							return true, turn.Text
						}
					}
					return false, "the agent never spoke the status or the delivery date"
				},
			},
		},
	}
}

// Interview is a technical interview: many short turns, strictly alternating.
//
// It is the turn-taking scenario. A system that answers correctly but talks
// over its counterpart, or waits four seconds before every reply, fails a
// conversation while passing every single-turn benchmark there is.
func Interview() Scenario {
	return Scenario{
		Name:     "interview",
		Question: "do two agents alternate cleanly over many short turns without talking over each other?",
		Left: Role{
			Name: "interviewer",
			Instruction: persona("an interviewer running a technical screen for a backend engineering role") +
				"Ask exactly one short technical question, then stop and wait. " +
				"After each answer, ask one follow-up question about what they just said. " +
				"Never answer your own question and never explain the topic yourself.",
			Opens: true,
			Cue: "The candidate has joined the call. Your first line is: " +
				"Thanks for joining. What is a database index, and why would you add one?",
		},
		Right: Role{
			Name: "candidate",
			Instruction: persona("a backend engineer being interviewed for a job") +
				"Answer the question you were just asked and then stop talking. " +
				"Do not ask questions of your own and do not interview the interviewer.",
		},
		MaxTurns: 8,
		Checks: []Check{
			bothSidesSpoke(),
			{
				Name: "the interviewer asked more than once",
				Why:  "a single question is an exchange, not an interview",
				Pass: func(conversation Conversation) (bool, string) {
					questions := 0
					for _, turn := range conversation.TurnsBy("interviewer") {
						if strings.Contains(turn.Text, "?") {
							questions++
						}
					}
					if questions >= 2 {
						return true, fmt.Sprintf("%d questions", questions)
					}
					return false, fmt.Sprintf("only %d questions were asked", questions)
				},
			},
			{
				Name: "turns alternate",
				Why: "a conversation where one side speaks twice in a row has lost the floor " +
					"somewhere, which is the failure turn-taking exists to prevent",
				Pass: func(conversation Conversation) (bool, string) {
					if len(conversation.Turns) < 4 {
						return false, fmt.Sprintf("only %d turns", len(conversation.Turns))
					}
					repeats := 0
					for index := 1; index < len(conversation.Turns); index++ {
						if conversation.Turns[index].Speaker == conversation.Turns[index-1].Speaker {
							repeats++
						}
					}
					// One repeat is a clarification, not a breakdown.
					if repeats <= 1 {
						return true, fmt.Sprintf("%d turns, %d consecutive", len(conversation.Turns), repeats)
					}
					return false, fmt.Sprintf(
						"%d of %d turns followed the same speaker", repeats, len(conversation.Turns))
				},
			},
			overlapUnder(0.25),
		},
	}
}

// Debate is two agents arguing a technical question over a large shared
// document.
//
// The document is the point. Both sides are given the same long specification
// and asked to argue from it, and the checks look for a fact that appears once,
// deep inside it. Retrieving that fact while a real-time conversation is in
// flight is a different problem from retrieving it in a single-shot answer:
// the context has to be usable at conversational latency, turn after turn.
func Debate() Scenario {
	const needle = "eleven milliseconds"
	document := LargeContext(needle)
	common := persona("an engineer in a live technical review") +
		"Argue from the specification below, cite specific numbers from it, and never " +
		"summarise the whole document.\n\nSpecification under review:\n\n" + document

	return Scenario{
		Name: "debate",
		Question: "can two agents argue from a large shared document at conversational " +
			"latency, and does a fact buried deep in it actually reach speech?",
		Left: Role{
			Name:        "proposer",
			Instruction: common + "\n\nYou argue the design should ship as it is.",
			Opens:       true,
			Cue: "The review has started and it is your turn. Open by stating the p99 " +
				"admission latency from the specification, with its number and unit, and " +
				"say that it is why the design should ship.",
		},
		Right: Role{
			Name: "reviewer",
			Instruction: common + "\n\nYou argue the design is not ready. " +
				"Challenge each claim with a different specific figure from the document.",
		},
		MaxTurns: 6,
		Checks: []Check{
			bothSidesSpoke(),
			{
				Name: "the buried fact reached speech",
				Why: "the figure appears once, deep in the document; saying it out loud is " +
					"proof the context was usable during a live conversation rather than only " +
					"in a single-shot answer",
				Pass: func(conversation Conversation) (bool, string) {
					for _, turn := range conversation.Turns {
						if contains(turn.Text, needle) || contains(turn.Text, "11 millisecond") ||
							contains(turn.Text, "11 ms") {
							return true, turn.Speaker + ": " + turn.Text
						}
					}
					// Recognition can mangle a number even when the model said
					// it, so what each side generated is checked too, and the
					// distinction is reported rather than hidden.
					for speaker, lines := range conversation.Said {
						for _, line := range lines {
							if contains(line, needle) || contains(line, "11 ms") ||
								contains(line, "11 millisecond") {
								return true, "said by " + speaker +
									" but not recognised by the listener: " + line
							}
						}
					}
					return false, "neither side ever produced the figure"
				},
			},
			{
				Name: "both sides argued",
				Why:  "a debate where one side only agrees has not tested anything",
				Pass: func(conversation Conversation) (bool, string) {
					if len(conversation.TurnsBy("proposer")) >= 1 &&
						len(conversation.TurnsBy("reviewer")) >= 1 &&
						len(conversation.Turns) >= 3 {
						return true, fmt.Sprintf("%d turns", len(conversation.Turns))
					}
					return false, fmt.Sprintf("only %d turns", len(conversation.Turns))
				},
			},
			overlapUnder(0.35),
		},
	}
}

// minimumSpeechMS is how much a side must say before it counts as taking part.
//
// Two and a half seconds across a whole conversation is a very low bar, and
// that is the point: it excludes a side that only ever emitted a syllable
// while excluding nothing that was actually a contribution.
const minimumSpeechMS = 2500

// bothSidesSpoke is the check every scenario needs first.
//
// A run where one agent never said anything is not a conversation, and every
// other check on it would be measuring a monologue.
//
// Turn counts alone are not enough, and finding that out cost a scenario that
// reported four passes while one side had contributed half a second of noise:
// its counterpart's every utterance was answered with a grunt, so the turns
// alternated perfectly and the overlap was negligible. Both of those checks
// were true and neither meant anything. Substance is checked here so the rest
// of the checks are measuring a conversation.
func bothSidesSpoke() Check {
	return Check{
		Name: "both sides took part",
		Why: "a side that only emits a syllable makes turn-taking look perfect while " +
			"contributing nothing, and every check downstream then measures a monologue",
		Pass: func(conversation Conversation) (bool, string) {
			detail := ""
			taking := true
			for _, speaker := range []string{conversation.Left, conversation.Right} {
				turns := len(conversation.TurnsBy(speaker))
				speech := conversation.SpeechMS[speaker]
				detail += fmt.Sprintf("%s %d turns %.1f s; ", speaker, turns, speech/1000)
				if turns == 0 || speech < minimumSpeechMS {
					taking = false
				}
			}
			return taking, strings.TrimSuffix(detail, "; ")
		},
	}
}

// overlapUnder bounds how much of the conversation was both sides at once.
//
// Some overlap is correct - that is what barge-in is - so the bound is a
// fraction rather than zero. A system with no overlap at all in a live
// conversation is usually one that is not listening while it speaks.
func overlapUnder(fraction float64) Check {
	return Check{
		Name: fmt.Sprintf("overlap under %.0f%%", fraction*100),
		Why: "some overlap is barge-in working; a lot of it is two systems talking " +
			"past each other with nobody holding the floor",
		Pass: func(conversation Conversation) (bool, string) {
			total := 0.0
			for _, milliseconds := range conversation.SpeechMS {
				total += milliseconds
			}
			if total == 0 {
				return false, "no speech was carried in either direction"
			}
			ratio := conversation.OverlapMS * 2 / total
			detail := fmt.Sprintf("%.0f ms overlap of %.0f ms speech (%.0f%%)",
				conversation.OverlapMS, total, ratio*100)
			return ratio <= fraction, detail
		},
	}
}

// persona is the preamble every role needs.
//
// A session instruction is composed ahead of the phase instruction, which
// frames the model as the voice of a helpful agent. Left alone, both sides of
// a simulated call drift into being the assistant - the customer opens with
// "how can I help you?" - and the conversation measures nothing. Saying what
// the role is not turns out to matter as much as saying what it is.
func persona(role string) string {
	return "You are " + role + ". You are not an assistant and you are not here to help: " +
		"play this part and nothing else. Never offer assistance and never ask how you can help.\n\n" +
		"Speak one or two short sentences at a time, in English, then stop and let the other " +
		"person reply.\n\n"
}

// spellOut renders an identifier the way a person reads one aloud.
//
// A model handed "R7742" often says "arr seventy-seven forty-two", which is
// not what the far side's recogniser will reassemble. Spelling it in the
// instruction makes the spoken form deterministic, so the check measures the
// link rather than the speaker's improvisation.
func spellOut(identifier string) string {
	parts := make([]string, 0, len(identifier))
	for _, character := range identifier {
		parts = append(parts, string(character))
	}
	return strings.Join(parts, "-")
}
