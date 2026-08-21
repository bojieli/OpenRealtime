package interaction

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
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
	// Confidence is the threshold a projection must clear to end a turn. A
	// projected endpoint that is wrong cuts the user off, so this is
	// deliberately conservative.
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
			"reply would clearly be welcome now.",
		Options: []string{"continuing", "finished"},
		Evidence: fmt.Sprintf(
			"What the person has said so far: %s\nSilence since they stopped: %d ms",
			decision.Revision.Text(), decision.Revision.SilenceNS/1_000_000),
	})
	result := Projection{Reason: "policy model declined to project"}
	if err == nil && outcome.Option == "finished" && outcome.Confidence >= policy.options.Confidence {
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
