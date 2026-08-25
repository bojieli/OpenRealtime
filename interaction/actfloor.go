package interaction

import (
	"context"
	"errors"
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
	// cut into somebody's sentence again.
	//
	// Interrupting is a scarce act and stops being an interruption when it
	// stops being scarce: done twice in five seconds it is not cutting in, it
	// is talking over somebody. The bound is structural rather than something
	// left to the model, because the model is asked afresh on every partial
	// and has no memory of having just done it - consulted sixty-five times in
	// one conversation it answered "interrupt" sixty-five times, each
	// defensible alone and together an agent nobody could speak to.
	//
	// Zero selects four seconds.
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
		options.MinimumBetweenInterruptions = 4 * time.Second
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
	case act == ActInterrupt && floor.recentlyInterrupted(decision.NowNS):
		// Not a refusal of the judgement, a refusal of its repetition. The
		// model is asked again on every partial and cannot remember having
		// just cut in.
		verdict = EndpointDecision{Act: act, Reason: "interrupted too recently to interrupt again"}
	case act == ActAnswer || act == ActInterrupt:
		if act == ActInterrupt {
			floor.markInterrupted(decision.NowNS)
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

func (floor *actFloor) recentlyInterrupted(nowNS uint64) bool {
	floor.mu.Lock()
	defer floor.mu.Unlock()
	if floor.lastInterruptNS == 0 || nowNS < floor.lastInterruptNS {
		return false
	}
	return nowNS-floor.lastInterruptNS < uint64(floor.options.MinimumBetweenInterruptions.Nanoseconds())
}

func (floor *actFloor) markInterrupted(nowNS uint64) {
	floor.mu.Lock()
	defer floor.mu.Unlock()
	floor.lastInterruptNS = nowNS
}
