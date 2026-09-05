package interaction

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
)

// The two policies here are judgement calls about a live conversation, and
// rule-based versions of them are exactly the brittle keyword-and-threshold
// machinery this project avoids everywhere else. Each is served instead by a
// policy model: a small, fast model with a short prompt and a constrained
// output.
//
// Both degrade cleanly. With no model configured, backchannel falls back to
// off and turn projection to silence-only endpointing. The system runs without
// them; it is simply less alive.

// BackchannelOptions configures the model-backed policy.
type BackchannelOptions struct {
	// Tokens are the continuers the policy may emit, in the language the
	// deployment expects. They are a deployment's choice rather than a
	// model's: what a listener says to show they are still there is
	// language-specific and register-specific, and a model inventing one would
	// be free generation by another name.
	Tokens map[BackchannelChoice]string
	// MinimumInterval bounds how often a continuer may be emitted. Without it
	// an eager model turns attentiveness into interruption.
	MinimumInterval time.Duration
	// MinimumSpeech is how long the user must have been talking before a
	// continuer is worth considering at all.
	MinimumSpeech time.Duration
}

// DefaultBackchannelTokens are the shipped English continuers.
func DefaultBackchannelTokens() map[BackchannelChoice]string {
	return map[BackchannelChoice]string{
		BackchannelAcknowledge: "mm-hm",
		BackchannelAffirm:      "right",
	}
}

type modelBackchannel struct {
	decider Decider
	options BackchannelOptions

	mu      sync.Mutex
	lastNS  uint64
	lastRev uint64
}

// NewModelBackchannel serves the backchannel decision with a policy model.
func NewModelBackchannel(decider Decider, options BackchannelOptions) (Backchannel, error) {
	if decider == nil {
		return nil, fmt.Errorf("a model-backed backchannel policy requires a decider")
	}
	if len(options.Tokens) == 0 {
		options.Tokens = DefaultBackchannelTokens()
	}
	if options.MinimumInterval <= 0 {
		options.MinimumInterval = 3 * time.Second
	}
	if options.MinimumSpeech <= 0 {
		options.MinimumSpeech = 2 * time.Second
	}
	return &modelBackchannel{decider: decider, options: options}, nil
}

func (policy *modelBackchannel) Name() string { return "model:" + policy.decider.Name() }

// Decide asks whether to interject.
//
// The cheap checks come first and most of the time they answer: the user must
// be speaking, the agent must not be, enough speech must have happened, and
// enough time must have passed since the last continuer. Only what survives
// all four is worth a model call, which is what keeps this policy's cost
// proportional to how often it actually does anything.
func (policy *modelBackchannel) Decide(ctx context.Context, decision Context) (BackchannelDecision, error) {
	if !decision.Duplex.UserSpeaking || decision.Duplex.AgentSpeaking {
		return BackchannelDecision{Choice: BackchannelNone, Reason: "not listening to the user"}, nil
	}
	if decision.Revision.Empty() {
		return BackchannelDecision{Choice: BackchannelNone, Reason: "nothing heard yet"}, nil
	}
	started := decision.Duplex.UserSpeechStartedNS
	if started == 0 || decision.NowNS-started < uint64(policy.options.MinimumSpeech.Nanoseconds()) {
		return BackchannelDecision{Choice: BackchannelNone, Reason: "the user has only just started"}, nil
	}
	policy.mu.Lock()
	tooSoon := policy.lastNS != 0 &&
		decision.NowNS-policy.lastNS < uint64(policy.options.MinimumInterval.Nanoseconds())
	sameRevision := policy.lastRev == decision.Revision.ID
	policy.mu.Unlock()
	if tooSoon || sameRevision {
		return BackchannelDecision{Choice: BackchannelNone, Reason: "too soon since the last continuer"}, nil
	}

	options := []string{string(BackchannelNone), string(BackchannelAcknowledge), string(BackchannelAffirm)}
	outcome, err := policy.decider.Decide(ctx, Decision{
		Prompt: "A person is speaking to an agent. Decide whether the agent should say a short " +
			"listener continuer right now - the sort of thing a person says to show they are still " +
			"listening - or stay silent.\n\n" +
			"Answer none unless the speaker has clearly finished a thought and would expect " +
			"acknowledgement. Answer acknowledge for an ordinary continuer. Answer affirm only when " +
			"the speaker said something that calls for agreement.\n\n" +
			"A continuer is for when nothing is owed. Anything that asks for something owes a " +
			"reply, and a noise is the one response worse than none: it takes the moment the reply " +
			"belonged in and gives back nothing. So a question is none, however finished it sounds " +
			"- asking is a finished thought that expects an answer, not acknowledgement, and " +
			"something else decides what the answer is.\n\n" +
			"None as well while they are still assembling what they are saying: mid-clause, " +
			"searching for a word, counting or reading or spelling something out. A continuer " +
			"lands on top of those rather than between them.\n\n" +
			"Most of the time the right answer is none.",
		Options:  options,
		Evidence: "What the person has said so far: " + decision.Revision.Text(),
	})
	if err != nil {
		// A policy model that fails is a policy that is off, not a session
		// that breaks. Silence is always a safe answer here.
		return BackchannelDecision{Choice: BackchannelNone, Reason: "policy model unavailable"}, err
	}
	choice := BackchannelChoice(outcome.Option)
	if choice == BackchannelNone {
		return BackchannelDecision{Choice: BackchannelNone, Reason: "the model chose silence"}, nil
	}
	token, exists := policy.options.Tokens[choice]
	if !exists || strings.TrimSpace(token) == "" {
		return BackchannelDecision{Choice: BackchannelNone, Reason: "no token configured for that choice"}, nil
	}
	policy.mu.Lock()
	policy.lastNS, policy.lastRev = decision.NowNS, decision.Revision.ID
	policy.mu.Unlock()
	return BackchannelDecision{Choice: choice, Token: token, Reason: "policy model chose to interject"}, nil
}

// ProjectionOptions configures the model-backed turn projection.
type ProjectionOptions struct {
	// MinimumSilence is how much silence must have accumulated before a
	// projection is even considered. Projecting into the middle of a word is
	// worse than waiting.
	MinimumSilence time.Duration
	// Confidence is the threshold a projection must clear to end a turn early.
	// A projected endpoint that is wrong cuts the user off, so this is
	// deliberately conservative.
	//
	// There is deliberately no matching threshold for the other answer. The
	// two failures are not symmetric and the comment above says so: ending a
	// turn early cuts a person off mid-sentence, and holding one open costs
	// latency that -projection-hold already bounds. Requiring the same
	// evidence for both spends the cheap failure to avoid the expensive one,
	// which is backwards - and in practice it discarded the answer that keeps
	// people from being interrupted while they searched for a word.
	Confidence float64
}

type modelProjection struct {
	decider Decider
	options ProjectionOptions

	mu      sync.Mutex
	lastRev uint64
	last    Projection
}

// NewModelProjection serves turn projection with a policy model.
func NewModelProjection(decider Decider, options ProjectionOptions) (TurnProjection, error) {
	if decider == nil {
		return nil, fmt.Errorf("a model-backed projection policy requires a decider")
	}
	if options.MinimumSilence <= 0 {
		options.MinimumSilence = 120 * time.Millisecond
	}
	if options.Confidence <= 0 {
		options.Confidence = 0.7
	}
	return &modelProjection{decider: decider, options: options}, nil
}

func (policy *modelProjection) Name() string { return "model:" + policy.decider.Name() }

// Project anticipates the end of a turn.
//
// It sees only what was available at the instant of the decision: the partial
// transcript so far and how long the silence has lasted. That constraint is
// the whole point. A projection model prompted or trained on hindsight - on
// the final transcript, on what the user said next - produces a judgement that
// cannot be reproduced online, which is the standard trap for a learned
// endpointer and the reason this contract passes a value rather than a handle
// to the perception subsystem.
func (policy *modelProjection) Project(decision Context) Projection {
	if decision.Revision.Empty() || decision.Revision.Final {
		return Projection{Reason: "nothing to project"}
	}
	if decision.Duplex.UserSpeaking {
		return Projection{Reason: "the user is still audible"}
	}
	if decision.Revision.SilenceNS < uint64(policy.options.MinimumSilence.Nanoseconds()) {
		return Projection{Reason: "too little silence to judge"}
	}
	policy.mu.Lock()
	if policy.lastRev == decision.Revision.ID {
		cached := policy.last
		policy.mu.Unlock()
		return cached
	}
	policy.mu.Unlock()

	// Projection runs on the audio path, which cannot block: a decision that
	// arrives after the silence threshold has passed anyway is worthless. The
	// bound is the caller's context.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	outcome, err := policy.decider.Decide(ctx, Decision{
		Prompt: "A person is speaking to an agent. Decide whether they have finished their turn " +
			"and are waiting for a reply, or whether they are pausing mid-thought and will " +
			"continue.\n\n" +
			"Answer continuing when the utterance is grammatically or semantically incomplete, " +
			"when it ends on a filler, or when it trails off mid-list. Answer finished only when a " +
			"reply would clearly be welcome now.\n\n" +
			"Both answers cost something. Continuing keeps the agent silent a moment longer, " +
			"which is wasted time if the person really had finished. Finished lets the agent " +
			"start talking, which cuts the person off if they had not. Judge only the words " +
			"below and the pause so far; do not assume a long pause means the turn ended, " +
			"because a person searching for a word pauses exactly like a person who has stopped.",
		Options: []string{"continuing", "finished"},
		Evidence: fmt.Sprintf(
			"What the person has said so far: %s\nSilence since they stopped: %d ms",
			decision.Revision.Text(), decision.Revision.SilenceNS/1_000_000),
	})
	result := Projection{Reason: "policy model declined to project"}
	if err == nil && outcome.Option == "continuing" {
		result = Projection{
			Continuing: true, Confidence: outcome.Confidence,
			Reason: "policy model judged the turn unfinished",
		}
	}
	if err == nil && outcome.Option == "finished" && outcome.Sure(policy.options.Confidence) {
		result = Projection{
			Ending: true, Confidence: outcome.Confidence,
			Reason: "policy model projected the end of the turn",
		}
	}
	policy.mu.Lock()
	policy.lastRev, policy.last = decision.Revision.ID, result
	policy.mu.Unlock()
	return result
}

// OverlapClassifier says what overlapping user speech is.
//
// Barge-in already takes typed evidence, and until now nothing produced it:
// every overlap arrived unclassified and the immediate policy treated all of
// it as the user taking the floor. That is the safe failure - stopping when
// somebody says "mm-hm" is annoying, talking over a real interruption is
// worse - but it is a failure, and the overlap suite measures exactly how
// often it happens.
//
// Classification needs words, and words arrive a few hundred milliseconds
// after the sound does. So this is only useful with a barge-in policy that
// waits: the sustained policy holds the floor briefly, and this decides what
// to do with that time.
type OverlapClassifier interface {
	Named
	Classify(context.Context, Context) OverlapEvidence
}

// UnclassifiedOverlap is the rule fallback: say nothing, and let barge-in
// treat the overlap as directed speech.
type UnclassifiedOverlap struct{}

func (UnclassifiedOverlap) Name() string { return "unclassified" }

func (UnclassifiedOverlap) Classify(context.Context, Context) OverlapEvidence { return "" }

type modelOverlapClassifier struct {
	decider Decider

	mu      sync.Mutex
	lastRev uint64
	last    OverlapEvidence
}

// NewModelOverlapClassifier classifies overlap with a policy model.
func NewModelOverlapClassifier(decider Decider) (OverlapClassifier, error) {
	if decider == nil {
		return nil, fmt.Errorf("an overlap classifier requires a decider")
	}
	return &modelOverlapClassifier{decider: decider}, nil
}

func (policy *modelOverlapClassifier) Name() string { return "model:" + policy.decider.Name() }

// Classify decides what the overlapping speech is.
//
// It answers from what has been heard so far and nothing else, for the same
// reason turn projection does: a classifier that needed the rest of the
// utterance could not run while the utterance was happening, which is the only
// time its answer is worth anything.
func (policy *modelOverlapClassifier) Classify(ctx context.Context, decision Context) OverlapEvidence {
	if decision.Revision.Empty() {
		return ""
	}
	policy.mu.Lock()
	if policy.lastRev == decision.Revision.ID {
		cached := policy.last
		policy.mu.Unlock()
		return cached
	}
	policy.mu.Unlock()

	request := Decision{
		Prompt: "An agent is speaking. The person has started speaking over it. Decide what they are doing.\n\n" +
			"Answer directed_speech when they are addressing the agent - interrupting, correcting, or asking " +
			"something new. Answer listener_backchannel for a short continuer that shows they are listening, " +
			"such as mm-hm, right, yeah, or okay, with nothing else in it. Answer side_speech when they are " +
			"clearly talking to somebody else. Answer ambiguous_speech when there is not enough to tell.\n\n" +
			"Who they are talking to is the first question, not the last. Speech aimed at somebody " +
			"else is side speech however it is phrased - a request or a question put to another " +
			"person is still put to another person, and answering it would be joining a " +
			"conversation nobody invited the agent into. A name, role, or title used as a vocative, " +
			"especially at the start of the utterance, identifies its recipient; a question beginning " +
			"'Officer,' is side speech unless the agent contract explicitly identifies this agent as " +
			"that officer. The explicit identity in the agent contract takes precedence over the " +
			"vocative rule. Also look for a reply to something the " +
			"agent did not say, or a bare declarative observation unrelated to what the agent is " +
			"saying. Topical unrelatedness alone is not another addressee: people explicitly change " +
			"the subject while continuing to address the same agent.\n\n" +
			"Listener backchannel is a strict closed class. The entire utterance must be only one " +
			"or two acknowledgement or continuer tokens such as mm-hm, uh-huh, yes, yeah, right, " +
			"or okay. It has no subject, verb, proposition, new topic, request, or question. Never " +
			"call I know, I think, it is, it is starting, that is, or another subject-plus-verb " +
			"fragment a backchannel.\n\n" +
			"Decide from the conversational function already established, not whether the sentence " +
			"is grammatically complete. A turn-repair, floor-taking, or topic-shift opener such as " +
			"actually, wait, no, sorry, hold on, hang on, just a minute, hold that thought, let's, " +
			"by the way, before I forget, or you know what is directed speech unless an explicit " +
			"addressee shows it is aimed elsewhere. A first-person proposal or request such as " +
			"let's switch, can we, let me, or tell me is likewise addressed to the current " +
			"interlocutor unless the words identify somebody else. A first-person conversational " +
			"response such as 'I know' or 'I think so' also addresses the current interlocutor and " +
			"takes the floor; it is directed speech, not side speech, unless an explicit addressee " +
			"shows otherwise. A contrastive continuation " +
			"beginning with but, or an acknowledgement followed by but, claims the floor to " +
			"disagree or redirect; oh but, yeah but, and okay but are directed rather than " +
			"backchannels unless they name another addressee. " +
			"A question opener such as what, how, why, when, where, who, can, could, would, should, " +
			"do, did, is, or are is directed as soon as it appears. A fragment is ambiguous only " +
			"while it has established neither its addressee nor its conversational function.",
		Options: []string{
			string(OverlapDirected), string(OverlapBackchannel),
			string(OverlapSide), string(OverlapAmbiguous),
		},
		Evidence: overlapDecisionEvidence(decision),
	}
	outcome, err := policy.decider.Decide(ctx, request)
	evidence := OverlapEvidence("")
	if err == nil {
		evidence = OverlapEvidence(outcome.Option)
	}
	if evidence == OverlapBackchannel {
		evidence = policy.validateOverlapBackchannel(ctx, decision, request.Evidence)
	}
	policy.mu.Lock()
	policy.lastRev, policy.last = decision.Revision.ID, evidence
	policy.mu.Unlock()
	return evidence
}

const (
	overlapValidBackchannel   = "valid_backchannel"
	overlapInvalidBackchannel = "not_backchannel"
)

// validateOverlapBackchannel enforces the semantic contract of the
// listener_backchannel label. Enumerated decoding proves only that a provider
// returned a member of the output vocabulary; it does not prove that the
// evidence satisfies that member's meaning. In particular, a model can still
// label a short proposition as a continuer even when the prompt forbids it.
//
// More than two lexical tokens is mechanically outside the declared closed
// class. Shorter evidence remains language- and register-dependent, so a
// second, deliberately binary policy decision validates it rather than baking
// an English keyword list into the runtime. Anything rejected is classified
// again with the impossible label removed. Provider failure stays
// unclassified and therefore follows the graph's explicit bounded fallback.
func (policy *modelOverlapClassifier) validateOverlapBackchannel(
	ctx context.Context, decision Context, evidence string,
) OverlapEvidence {
	valid := overlapBackchannelLexicalShape(decision.Revision.Text())
	if valid {
		outcome, err := policy.decider.Decide(ctx, Decision{
			Prompt:   "Validate the lexical content of a proposed listener backchannel. The overlap classifier has already proposed that the person is listening while the agent keeps speaking. A valid backchannel consists entirely of one or two acknowledgement or continuer tokens meaning keep going, I am listening, such as mhm, uh-huh, yes, yeah, right, okay, or their equivalents in the speaker's language. No additional conversational content is allowed. The words come from live speech recognition: punctuation and capitalization are not independent evidence of a question. A trailing question mark alone must not turn a pure continuer into a request for information. For example, 'Right?', 'Yeah?', 'Okay?', and '嗯?' can be acknowledgements just like their unpunctuated forms. Reject a question only when the words or conversational context actually express a question or repair request. Reject subject-plus-verb propositions, observations, disagreement, new requests, topic shifts, and floor-taking markers. Invalid examples include 'I know', 'I think', 'it is starting', 'yeah but', 'actually', 'wait', 'why?', 'is that right?', and 'by the way'. Decide whether the overlapping person's exact words contain only a pure acknowledgement or continuer, without inventing an additional intention from ASR punctuation.",
			Options:  []string{overlapValidBackchannel, overlapInvalidBackchannel},
			Evidence: evidence,
		})
		if err != nil || outcome.Option == "" {
			return ""
		}
		if outcome.Option == overlapValidBackchannel {
			return OverlapBackchannel
		}
	}
	outcome, err := policy.decider.Decide(ctx, Decision{
		Prompt: "The overlapping person's words are not a pure listener backchannel. Classify " +
			"the remaining conversational function. Answer directed_speech when they address the " +
			"agent to interrupt, correct, ask, disagree, redirect, or take the floor. Answer " +
			"side_speech when evidence shows they address somebody else, including by a name, role, " +
			"or title used as a vocative, or make an unrelated " +
			"room observation rather than address the agent. Answer ambiguous_speech when neither " +
			"the addressee nor function is established. An explicit assistant identity in the agent " +
			"contract takes precedence over the vocative rule. A first-person conversational response " +
			"such as 'I know' or 'I think so' is directed_speech absent evidence of another addressee; " +
			"do not treat it as an unrelated room observation merely because it is a proposition.",
		Options: []string{
			string(OverlapDirected), string(OverlapSide), string(OverlapAmbiguous),
		},
		Evidence: evidence,
	})
	if err != nil {
		return ""
	}
	return OverlapEvidence(outcome.Option)
}

func overlapBackchannelLexicalShape(text string) bool {
	tokens := strings.FieldsFunc(text, func(value rune) bool {
		return !unicode.IsLetter(value) && !unicode.IsNumber(value)
	})
	return len(tokens) > 0 && len(tokens) <= 2
}

func overlapDecisionEvidence(decision Context) string {
	var evidence strings.Builder
	if decision.Situation != nil {
		if contract := strings.TrimSpace(decision.Situation.Contract); contract != "" {
			evidence.WriteString("Agent contract: ")
			evidence.WriteString(contract)
			evidence.WriteByte('\n')
		}
		if agent := strings.TrimSpace(decision.Situation.AgentSaying); agent != "" {
			evidence.WriteString("What the agent is saying now: ")
			evidence.WriteString(agent)
			evidence.WriteByte('\n')
		}
	}
	evidence.WriteString("What the overlapping person has said so far: ")
	evidence.WriteString(decision.Revision.Text())
	return evidence.String()
}
