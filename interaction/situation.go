package interaction

import "strings"

// Act is what an interaction model chooses. It decides what the agent does in
// an instant, never what the agent says: the acts below select machinery that
// already exists rather than introducing any, which is why none of them names
// any content.
type Act string

const (
	// ActStaySilent does nothing: the agent says nothing and starts nothing.
	//
	// The name is load-bearing in a way worth recording. Asked to explain
	// itself on a case it got wrong, a model answered that it had chosen
	// listen "to keep track" of what the speaker was saying - it had
	// understood the task perfectly and read the act as an instruction to pay
	// attention rather than as a decision to say nothing.
	//
	// Renaming it to stay-silent fixed that and broke something else: measured
	// over the same cases, the name only slides a model along a trade between
	// acting when it should and not acting when it should not, leaving how
	// well it tells those apart unchanged. listen 40%/85%, wait 60%/73%,
	// stay-silent 75%/54% on the same model and prompt. It is a threshold, not
	// a quality lever, and it is named listen because that measured best on
	// the model this ships with - not because the word is unambiguous.
	ActStaySilent Act = "listen"
	// ActSpeakThrough speaks while the other party keeps the floor. They have
	// not finished, the agent is not taking over, and they can talk straight
	// through it.
	ActSpeakThrough Act = "speak-through"
	// ActAnswer takes the floor because it is free.
	ActAnswer Act = "answer"
	// ActInterrupt takes the floor from someone who has not offered it.
	ActInterrupt Act = "interrupt"
	// ActCallTool acts without speaking. It is the phase that may act and may
	// not speak, an authority rule the runtime already enforces.
	ActCallTool Act = "call-tool"
	// ActKeepSpeaking carries on through someone else starting.
	ActKeepSpeaking Act = "keep-speaking"
	// ActStopSpeaking yields the floor mid-sentence.
	ActStopSpeaking Act = "stop-speaking"
)

// Situation is everything an interaction model decides from.
//
// It renders as one labelled block rather than a message list, and that is a
// design decision rather than a convenience. Chat roles describe two parties.
// The cases that matter most here have three - a user, an agent, and a waiter
// or a phone menu - and flattening a third speaker into "user" would erase the
// distinction the decision turns on. A block can name whoever is talking.
//
// The fields are ordered by how stable they are, because the block is a cached
// prefix with a volatile tail: instructions and conversation change rarely,
// the instant changes constantly, and putting them the other way round would
// invalidate the cache on every tick.
type Situation struct {
	// Contract is what this deployment told the agent to be and to do.
	//
	// It belongs here for the same reason the conversation does. A decision
	// about when to speak often turns on something only the contract knows -
	// that a date is wrong, that a price is out of range, that a caller is not
	// entitled to what they just asked for - and a model that cannot see it
	// has no way to tell an ordinary sentence from one worth cutting into.
	// Correcting somebody mid-sentence never once worked without this, because
	// the fact being corrected against was never in front of the decision.
	Contract string
	// Pins are standing instructions someone gave out loud, each carrying its
	// own age. They live outside the conversation window because truncating
	// that window must never silently repeal a policy somebody set.
	Pins []string
	// Recent is the rolling conversation window, oldest first.
	Recent []string
	// AgentSpeaking says whether the agent's own voice is playing, and
	// AgentSaying is what it has said so far in that sentence. Together they
	// decide which acts exist: an agent that is not speaking cannot keep
	// speaking, and one already mid-sentence is not choosing whether to start.
	AgentSpeaking bool
	AgentSaying   string
	// Speaker names whoever else is involved, empty when nobody is. Speaking
	// says whether they are talking this instant; Heard is what they have
	// said, whether or not they have finished saying it.
	//
	// Who is speaking and what was heard are separate because collapsing them
	// hides every completed utterance the moment its speaker stops - the model
	// is told a turn ended and never told what the turn said.
	Speaker  string
	Speaking bool
	Heard    string
	// Silence is how long the quiet has lasted, in a unit a model can reason
	// about. It is deliberately finer than the projection cognition gets:
	// three hundred milliseconds is nothing to a reader of the transcript and
	// is the whole question here.
	Silence string
	// SincePrevious is how long between the previous utterance ending and this
	// one starting.
	//
	// A recogniser breaks a sentence wherever the speaker draws breath, so one
	// instruction arrives as several, each capitalised and punctuated into
	// something that looks like a sentence of its own. "Say anything else."
	// reads as an imperative and is the tail of "and don't say anything else"
	// - and the only reliable signal that the two belong together is that
	// nothing happened between them. Without this the decision is being asked
	// whether a turn has ended while being shown neither of the two gaps that
	// would say so.
	SincePrevious string
	// InFlight is work already running. It is what stops the same decision
	// being taken twice.
	InFlight string
	// Seen is the newest thing an observer noticed that nobody said out loud.
	Seen string
	// Tools names what the agent could do without speaking. Offering the
	// call-tool act while naming no tool asks a model to choose something it
	// has no way to know is possible.
	Tools []string
}

// AvailableActs is the act set this instant admits.
//
// The runtime knows which acts are meaningful and there is no reason to make a
// model rediscover it. Offering "listen" to an agent that is mid-sentence
// invites an answer that means nothing.
func (state Situation) AvailableActs() []Act {
	acts := []Act{ActKeepSpeaking, ActStopSpeaking}
	if !state.AgentSpeaking {
		acts = []Act{ActStaySilent, ActSpeakThrough, ActAnswer, ActInterrupt}
	}
	if len(state.Tools) > 0 {
		acts = append(acts, ActCallTool)
	}
	return acts
}

// Render writes the block a model reads. Worked examples in the instruction go
// through this same function, so an example and a real decision are the same
// shape - a model that has to translate between two formats of the same thing
// is spending capacity on the translation.
func (state Situation) Render() string {
	var block strings.Builder
	if trimmed := strings.TrimSpace(state.Contract); trimmed != "" {
		block.WriteString("What this agent is for:\n" + trimmed + "\n\n")
	}
	if len(state.Pins) > 0 {
		block.WriteString("Standing instructions:\n")
		for _, pin := range state.Pins {
			block.WriteString("- " + pin + "\n")
		}
		block.WriteString("\n")
	}
	if len(state.Recent) > 0 {
		block.WriteString("Recent conversation:\n")
		for _, line := range state.Recent {
			block.WriteString(line + "\n")
		}
		block.WriteString("\n")
	}
	block.WriteString("Now:\n")
	if state.AgentSpeaking {
		block.WriteString("agent: speaking out loud right now, has said \"" + state.AgentSaying + "\" so far\n")
	} else {
		block.WriteString("agent: not speaking\n")
	}
	who := orElse(state.Speaker, "user")
	switch {
	case state.Speaking:
		block.WriteString(who + ": speaking right now\n")
	case state.Heard != "":
		block.WriteString(who + ": stopped speaking " + orElse(state.Silence, "0ms") + " ago\n")
	default:
		block.WriteString("nobody else is speaking\n")
	}
	if state.Heard != "" {
		block.WriteString("heard from " + who + " so far: \"" + state.Heard + "\"\n")
	}
	if state.Seen != "" {
		block.WriteString("just seen: " + state.Seen + "\n")
	}
	block.WriteString("silence: " + orElse(state.Silence, "0ms") + "\n")
	if trimmed := strings.TrimSpace(state.SincePrevious); trimmed != "" {
		block.WriteString("gap before this utterance: " + trimmed + "\n")
	}
	block.WriteString("work in flight: " + orElse(state.InFlight, "nothing") + "\n")
	if len(state.Tools) > 0 {
		block.WriteString("tools the agent can use without speaking: " + strings.Join(state.Tools, ", ") + "\n")
	}
	acts := state.AvailableActs()
	names := make([]string, len(acts))
	for index, act := range acts {
		names[index] = string(act)
	}
	block.WriteString("\nAvailable acts right now: " + strings.Join(names, ", "))
	return block.String()
}

func orElse(value, whenEmpty string) string {
	if strings.TrimSpace(value) == "" {
		return whenEmpty
	}
	return value
}
