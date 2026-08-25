package interaction

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/session"
)

// ActFloorOptions configures a floor owned by the interaction model.
type ActFloorOptions struct {
	// Timeout bounds one decision. It runs on the audio path, where a verdict
	// that arrives after the moment it was about is worthless.
	Timeout time.Duration
	// SilenceDuration is the endpoint the fallback uses when the model cannot
	// answer, and the point from which Liveness is measured.
	SilenceDuration time.Duration
	// MinimumBetweenInterruptions is how long must pass before the agent may
	// cut into somebody's sentence again, as a backstop.
	//
	// The bound that does the work is not this one. Interrupting the same
	// utterance twice is talking over somebody; interrupting a later one is a
	// fresh decision that may well be right, and a flat timer cannot tell them
	// apart - it blocks the second exactly as readily as the first. So the
	// primary rule is one interruption per stretch of speech, and this is only
	// here to catch a speaker whose every sentence gets cut into.
	//
	// Zero selects five seconds.
	//
	// Two was too short and the reason is the recogniser: it commits and starts
	// a fresh utterance every few seconds, so each fragment of one monologue
	// looks like a new stretch of speech and the prefix test lets it through.
	// Measured, that produced five interruptions in ten seconds, each 2.3
	// seconds after the last and each just clearing the bound. A person cutting
	// into the same monologue five times is not interrupting them.
	MinimumBetweenInterruptions time.Duration
	// Liveness is the longest the model may hold the floor past that point.
	//
	// It exists because the failure it guards against is silent and total: a
	// model that answers "listen" to everything produces an agent that never
	// speaks again, and nothing in the conversation would report it. The bound
	// is generous rather than tight - honouring "don't interrupt me while I
	// think this through" is the entire point, and a bound short enough to be
	// safe against a broken model is short enough to break the feature.
	Liveness time.Duration
}

// NewActFloor gives the turn-taking decision to the interaction model.
//
// The engine floor it replaces decides from silence and a projection, and both
// of those describe acoustics. This one decides from the conversation, which is
// what lets somebody change the policy by saying so.
func NewActFloor(model *InteractionModel, options ActFloorOptions) (Floor, error) {
	if model == nil {
		return nil, errors.New("an act floor requires an interaction model")
	}
	if options.Timeout <= 0 {
		options.Timeout = 150 * time.Millisecond
	}
	if options.SilenceDuration <= 0 {
		options.SilenceDuration = 500 * time.Millisecond
	}
	if options.Liveness <= 0 {
		options.Liveness = 20 * time.Second
	}
	if options.MinimumBetweenInterruptions <= 0 {
		options.MinimumBetweenInterruptions = 5 * time.Second
	}
	return &actFloor{model: model, options: options}, nil
}

type actFloor struct {
	model   *InteractionModel
	options ActFloorOptions

	mu              sync.Mutex
	lastKey         string
	last            EndpointDecision
	lastInterruptNS uint64
	// interruptedHeard is what had been heard when the agent last cut in. A
	// stretch of speech that still begins with it is the same stretch, however
	// much has been added since.
	interruptedHeard string
}

func (floor *actFloor) Name() string      { return "act:" + floor.model.Name() }
func (floor *actFloor) EngineOwned() bool { return true }

// Endpoint asks the interaction model whether the turn is over.
//
// Only two acts end it. Answering takes a floor that is free; interrupting
// takes one that is not, and both mean the agent is about to speak into the
// trajectory as it stands. Every other act leaves the turn open, including the
// ones that produce speech - speaking through somebody is precisely speech
// that does not end their turn.
func (floor *actFloor) Endpoint(decision Context) EndpointDecision {
	silence := decision.Revision.SilenceNS
	beyond := uint64((floor.options.SilenceDuration + floor.options.Liveness).Nanoseconds())
	if silence >= beyond {
		return EndpointDecision{Ended: true, Reason: "the floor was held past the liveness bound"}
	}
	if decision.Situation == nil {
		// Without the conversation there is nothing this floor can do that the
		// silence rule does not already do better.
		if silence >= uint64(floor.options.SilenceDuration.Nanoseconds()) {
			return EndpointDecision{Ended: true, Reason: "silence exceeded the endpoint threshold"}
		}
		return EndpointDecision{}
	}
	if decision.Revision.Empty() {
		return EndpointDecision{}
	}
	// Cached on the whole situation, not on the revision.
	//
	// The revision is the wrong key and the difference is not subtle: while
	// somebody holds a pause the transcript stops changing and the silence
	// does not, which is the entire question. Worse, the last call before the
	// pause is made while they are still audible, where answering is refused -
	// so a revision-keyed cache serves that refusal back for as long as the
	// pause lasts and the turn never ends at all.
	//
	// The situation renders everything the answer depends on, so an identical
	// string is an identical question and reusing the answer is free rather
	// than a guess.
	key := decision.Situation.Render()
	floor.mu.Lock()
	if floor.lastKey == key {
		cached := floor.last
		floor.mu.Unlock()
		return cached
	}
	floor.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), floor.options.Timeout)
	defer cancel()
	act, _, err := floor.model.Decide(ctx, *decision.Situation)
	verdict := EndpointDecision{}
	switch {
	case err != nil:
		// A decision that could not be taken falls back to the rule rather
		// than to a guess, which keeps a dead policy model from either muting
		// the agent or making it interrupt.
		if silence >= uint64(floor.options.SilenceDuration.Nanoseconds()) {
			verdict = EndpointDecision{Ended: true, Reason: "interaction model unavailable; silence exceeded the threshold"}
		}
	case act == ActAnswer && decision.Duplex.UserSpeaking:
		// Answering means the speaker has finished and the floor is free.
		// While they are still audible that is not a judgement the model is
		// entitled to make, it is a fact it has contradicted, and acting on it
		// cuts the utterance mid-word: the recogniser is handed a fragment,
		// returns something that is not what anybody said, and the agent
		// answers that instead. Taking a floor somebody still holds is what
		// interrupt is for, and the model has to say so.
		verdict = EndpointDecision{Act: act, Reason: "answering was chosen while the speaker was still audible"}
	case act == ActInterrupt && floor.alreadyInterrupted(decision.NowNS, decision.Situation.Heard):
		// Not a refusal of the judgement, a refusal of its repetition. The
		// model is asked again on every partial and cannot remember having
		// just cut in.
		verdict = EndpointDecision{Act: act, Reason: "interrupted too recently to interrupt again"}
	case act == ActAnswer || act == ActInterrupt:
		if act == ActInterrupt {
			floor.markInterrupted(decision.NowNS, decision.Situation.Heard)
		}
		verdict = EndpointDecision{Ended: true, Projected: true, Act: act, Reason: "the interaction model chose " + string(act)}
	default:
		verdict = EndpointDecision{Act: act, Reason: "the interaction model chose " + string(act)}
	}
	floor.mu.Lock()
	floor.lastKey, floor.last = key, verdict
	floor.mu.Unlock()
	return verdict
}

// Holder reports who has the floor from the duplex state, which is a fact
// rather than a judgement and needs no model.
func (floor *actFloor) Holder(state session.Snapshot) Holder {
	switch {
	case state.UserSpeaking:
		return HolderUser
	case state.AgentSpeaking:
		return HolderAgent
	default:
		return HolderNobody
	}
}

// alreadyInterrupted reports whether cutting in now would be cutting into the
// same thing twice.
//
// The model is asked afresh on every partial and cannot remember having just
// done it, so each of sixty-five interruptions in one conversation was
// defensible alone. What makes them wrong is the sequence, which is a property
// nothing in a single decision can see.
func (floor *actFloor) alreadyInterrupted(nowNS uint64, heard string) bool {
	floor.mu.Lock()
	defer floor.mu.Unlock()
	if floor.lastInterruptNS == 0 {
		return false
	}
	// Still the same stretch of speech: it has only grown since.
	if floor.interruptedHeard != "" && strings.HasPrefix(heard, floor.interruptedHeard) {
		return true
	}
	if nowNS < floor.lastInterruptNS {
		return false
	}
	return nowNS-floor.lastInterruptNS < uint64(floor.options.MinimumBetweenInterruptions.Nanoseconds())
}

func (floor *actFloor) markInterrupted(nowNS uint64, heard string) {
	floor.mu.Lock()
	defer floor.mu.Unlock()
	floor.lastInterruptNS, floor.interruptedHeard = nowNS, heard
}
