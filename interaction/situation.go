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
	// ActActSilently engages without being heard.
	//
	// It is about audibility, not about tools. The judgement it carries is
	// that speech would be pointless or unwelcome - a recording that cannot
	// hear it, somebody who asked not to be spoken to - and that the agent
	// should still do something about what is happening. What it then does,
	// press a key or look something up or simply work out where things stand,
	// belongs to the phase that can read the content; deciding that here made
	// this layer reason about which key applied, which is exactly the content
	// judgement it is not allowed to make.
	ActActSilently Act = "act-silently"
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
// Image is a frame handed to a model that can look at one.
//
// Bytes rather than a handle, because the decision is taken on the live
// instant and a handle would have to be resolved inside the hot path anyway.
type Image struct {
	MIMEType string
	Bytes    []byte
}

type Situation struct {
	// TranscriptEvent marks a streaming recogniser observation as a live
	// hypothesis or a settled utterance. Empty is the existing interaction
	// observation space and renders no additional line.
	TranscriptEvent TranscriptEventKind
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
	// Restricted says one of those policies closes off everything it did not
	// ask for. It is kept beside the rendered lines rather than read out of
	// them, because what governs the act set has to be a fact the runtime
	// holds, not a phrase a model has to notice in a list.
	Restricted bool
	// Recent is the rolling conversation window, oldest first.
	Recent []string
	// AgentSpeaking says whether the agent owns an active voice-output
	// lifecycle, from admitted generation through playback. AgentSaying is the
	// prepared or audible text when it is known. Together they decide which
	// acts exist: an agent with no voice output cannot keep speaking, and one
	// already preparing or presenting a response is not choosing whether to
	// start another.
	AgentSpeaking bool
	AgentSaying   string
	// AgentOutputProtected says active output was deliberately authorized by
	// an earlier revision of the transcript stream being decided now. It is a
	// relation computed from typed lifecycle provenance, not a model guess.
	AgentOutputProtected bool
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
	// Seeing is the frame this decision is about, handed to the model as a
	// picture rather than as somebody's description of one.
	//
	// A narrator is a cloud round trip that turns an image into a sentence,
	// and it costs both ways: measured at 1.45 seconds on the critical path of
	// every visual turn - the largest single cost in this system - and
	// whatever the sentence left out is gone. "The build has finished
	// successfully" is a good sentence and it is not the screen.
	//
	// It is only populated where the model deciding can see. A text-only
	// decider gets Seen, which is the description, and the two are
	// alternatives rather than a pair.
	Seeing []Image
	// Silence is how long the quiet has lasted, in a unit a model can reason
	// about. It is deliberately finer than the projection cognition gets:
	// three hundred milliseconds is nothing to a reader of the transcript and
	// is the whole question here.
	Silence string
	// Quiet says nothing has happened for a while and somebody asked to be
	// told about something that happens on its own.
	//
	// Every other decision here is caused by an arrival - words, a frame, a
	// result. An agent whose only inputs are other people's actions cannot
	// honour "tell me if I go quiet for fifteen seconds", because the moment
	// it is about is precisely the one where nothing arrives.
	//
	// It does not weaken the rule that evidence is what makes acting
	// necessary. A policy about time is somebody putting time in front of the
	// agent, and the elapsed silence is then evidence like any other. Without
	// a policy in force this stays false and the quiet decides nothing, which
	// is the inertia the rest of this rests on.
	Quiet bool
	// HeardSince is what has been added since the agent last said anything.
	//
	// Heard grows for as long as the floor holds a turn open, which is exactly
	// as long as somebody keeps talking - so in the case this matters most it
	// is a wall of text with the thing that just happened buried at the end.
	// A model asked "is anything worth acting on" needs to know what is new,
	// not to re-read a monologue it has already decided about forty times.
	HeardSince string
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
	// Tools names what the agent could do without speaking. It is here as a
	// capability rather than as a menu to choose from: acting silently is only
	// a real option if the agent has some way to affect anything without
	// speaking, and offering the act when it has none asks for a decision that
	// cannot be carried out.
	Tools []string
	// AllowedActs narrows the semantic act set to what the composed stack can
	// execute. Empty means the full vocabulary for backwards compatibility.
	// A turn-based speech model, for example, is not offered speak-through
	// unless it also declares concurrent input/output.
	AllowedActs []Act
}

// AvailableActs is the act set this instant admits.
//
// The runtime knows which acts are meaningful and there is no reason to make a
// model rediscover it. Offering "listen" to an agent that is mid-sentence
// invites an answer that means nothing.
// AllActs is the whole vocabulary.
//
// The list was written out by hand wherever one was needed - validation, the
// available set, dispatch - and a vocabulary that exists only as repeated
// literals cannot be checked against anything. An act added to the constants
// and missed in one of those places is not a compile error; it is an act that
// silently means nothing wherever it was missed, which has happened twice in
// the dispatch path.
//
// Order is the order they are declared, and callers that show acts to a model
// rely on it being stable.
func AllActs() []Act {
	return []Act{
		ActStaySilent, ActSpeakThrough, ActAnswer, ActInterrupt,
		ActActSilently, ActKeepSpeaking, ActStopSpeaking,
	}
}

func (state Situation) AvailableActs() []Act {
	acts := []Act{ActKeepSpeaking, ActStopSpeaking}
	if !state.AgentSpeaking {
		acts = []Act{ActStaySilent, ActSpeakThrough, ActInterrupt}
		// Answering means the floor is free. While somebody is audibly using
		// it, that is not a judgement to offer: taking a floor still in use is
		// what interrupt is for, and speaking without taking it is
		// speak-through. Offered anyway, a model reaches for answer whenever
		// something is worth saying and the runtime then refuses it, so the
		// thing worth saying is never said at all.
		// The same judgement, made by the person instead of the runtime. Told
		// to say nothing but the thing they asked for, an ordinary reply is
		// not an act they have left available - and the thing they did ask
		// for is still sayable, through speak-through, while speech is live.
		//
		// A final transcript is the one bounded recovery point when the live
		// act missed its moment. Its event-specific policy can distinguish a
		// required count/translation from unrelated end-of-story silence, so
		// leave answer available there. Empty TranscriptEvent keeps the tuned
		// legacy observation space exactly as before.
		// A restriction such as "tell me when the build finishes and say
		// nothing else" removes ordinary answers, not the one answer the
		// restriction itself reserves. A final transcript is one typed proof
		// that the reserved condition may have arrived; a visual observation
		// and a standing-policy quiet timer are the two non-speech proofs. In
		// all three cases the policy model still chooses between silence and
		// answer from the exact evidence. Withdrawing answer here would leave
		// silence as the only executable option precisely when the promised
		// condition needs deciding.
		reservedConditionEvidence := state.TranscriptEvent == TranscriptFinal ||
			state.Seen != "" || len(state.Seeing) > 0 || state.Quiet
		if !state.Speaking && (!state.Restricted || reservedConditionEvidence) {
			acts = append(acts, ActAnswer)
		}
	}
	if len(state.Tools) > 0 {
		acts = append(acts, ActActSilently)
	}
	if len(state.AllowedActs) > 0 {
		allowed := make(map[Act]struct{}, len(state.AllowedActs))
		for _, act := range state.AllowedActs {
			allowed[act] = struct{}{}
		}
		filtered := acts[:0]
		for _, act := range acts {
			if _, ok := allowed[act]; ok {
				filtered = append(filtered, act)
			}
		}
		acts = filtered
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
	if state.TranscriptEvent != "" {
		block.WriteString("transcript event: " + string(state.TranscriptEvent) + "\n")
	}
	if state.AgentSpeaking {
		if strings.TrimSpace(state.AgentSaying) == "" {
			block.WriteString("agent: voice output is active and still being prepared\n")
		} else {
			block.WriteString("agent: voice output is queued or audible, saying \"" + state.AgentSaying + "\"\n")
		}
		if state.AgentOutputProtected {
			block.WriteString("agent output was deliberately triggered by an earlier revision of this same transcript stream\n")
		}
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
	// Always said, and never by omission. Leaving the line out when there is
	// nothing new and again when everything is new renders two opposite
	// situations identically, and a model reading the absence has to guess
	// which one it is in. Told to count animals as they were mentioned, it
	// guessed "nothing new" every time and counted none of them.
	if state.Heard != "" {
		switch trimmed := strings.TrimSpace(state.HeardSince); {
		case trimmed == "":
			block.WriteString("nothing has been said since the agent last spoke\n")
		case trimmed == strings.TrimSpace(state.Heard):
			block.WriteString("all of this is new since the agent last spoke\n")
		default:
			block.WriteString("new since the agent last spoke: \"" + trimmed + "\"\n")
		}
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
