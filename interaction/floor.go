package interaction

import (
	"fmt"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/session"
)

// Holder names who currently owns the turn.
type Holder string

const (
	HolderNobody Holder = "nobody"
	HolderUser   Holder = "user"
	HolderAgent  Holder = "agent"
)

// EndpointDecision answers whether the user has finished.
//
// Projected marks a decision made before silence confirmed it - a turn
// projection saw the end of the turn coming. It is recorded separately because
// a projected endpoint that turns out to be wrong is a different failure from
// a late one, and the two must not be averaged together.
type EndpointDecision struct {
	Ended     bool   `json:"ended"`
	Projected bool   `json:"projected,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// Act names the choice behind the decision, when a floor made one.
	//
	// It carries because the two acts that end a turn end it for opposite
	// reasons. Answering means the floor came free; interrupting means it was
	// taken from somebody still using it, and what to say into a turn taken
	// that way is not what to say into one that was offered.
	Act Act `json:"act,omitempty"`
}

// Floor decides when the user has finished and who holds the turn. It affects
// latency and correctness at once, which is why it is the one interaction
// policy a model is ever allowed to take over.
type Floor interface {
	Named
	// Endpoint reports whether the user's turn has ended.
	Endpoint(Context) EndpointDecision
	// Holder reports who owns the turn given the duplex state.
	Holder(session.Snapshot) Holder
	// EngineOwned reports whether the engine decides endpoints, or whether a
	// model-native signal does. It is factor F5.
	EngineOwned() bool
}

// EngineFloorOptions configures the engine-owned floor.
type EngineFloorOptions struct {
	// SilenceDuration is how much silence ends a turn. Zero selects 500 ms,
	// matching the Realtime default.
	SilenceDuration time.Duration
	// MinimumSpeech guards against a cough ending a turn that never started.
	MinimumSpeech time.Duration
	// Projection, when set, may end a turn before silence confirms it, and
	// may hold one open past the silence threshold.
	Projection TurnProjection
	// ProjectionHold bounds how much extra silence a "still going" projection
	// may buy beyond SilenceDuration. Zero selects one second.
	//
	// It is a maximum rather than a target, and it exists because the failure
	// modes are not symmetric. A projection that wrongly says the person is
	// still going costs this much added latency once; the same projection
	// without a bound costs a turn that never ends, because the model is asked
	// again on every revision and a model that keeps answering "continuing"
	// would hold the floor for as long as it kept saying so.
	ProjectionHold time.Duration
}

type engineFloor struct {
	options EngineFloorOptions
}

// NewEngineFloor keeps endpointing in the engine.
//
// The engine keeps the floor even for models that expect to own it, and
// deliberately: an Omni model leans on VAD, and VAD mis-endpoints on spelled
// identifiers and digit strings - exactly the inputs a tool-using voice agent
// depends on getting right.
func NewEngineFloor(options EngineFloorOptions) Floor {
	if options.SilenceDuration <= 0 {
		options.SilenceDuration = 500 * time.Millisecond
	}
	if options.ProjectionHold <= 0 {
		options.ProjectionHold = time.Second
	}
	return engineFloor{options: options}
}

func (floor engineFloor) Name() string {
	if floor.options.Projection != nil && floor.options.Projection.Name() != "vad-only" {
		return fmt.Sprintf("engine-%dms+%dms+%s", floor.options.SilenceDuration.Milliseconds(),
			floor.options.ProjectionHold.Milliseconds(), floor.options.Projection.Name())
	}
	return fmt.Sprintf("engine-%dms", floor.options.SilenceDuration.Milliseconds())
}

func (floor engineFloor) EngineOwned() bool { return true }

func (floor engineFloor) Endpoint(context Context) EndpointDecision {
	if context.Revision.Final {
		return EndpointDecision{Ended: true, Reason: "perception reported a final revision"}
	}
	if context.Revision.Empty() {
		return EndpointDecision{}
	}
	held := false
	heldReason := ""
	if floor.options.Projection != nil {
		projected := floor.options.Projection.Project(context)
		if projected.Ending {
			return EndpointDecision{Ended: true, Projected: true, Reason: projected.Reason}
		}
		held, heldReason = projected.Continuing, projected.Reason
	}
	if context.Duplex.UserSpeaking {
		return EndpointDecision{}
	}
	if context.Revision.SilenceNS < uint64(floor.options.SilenceDuration.Nanoseconds()) {
		return EndpointDecision{}
	}
	// Silence says the turn is over; the projection may say the person is only
	// pausing. Believe it, but only up to the bound - past that the turn ends
	// whatever the model thinks, because a floor that can be talked out of
	// ending is not a floor.
	if held && context.Revision.SilenceNS < uint64((floor.options.SilenceDuration+floor.options.ProjectionHold).Nanoseconds()) {
		return EndpointDecision{Projected: true, Reason: heldReason}
	}
	return EndpointDecision{Ended: true, Reason: "silence exceeded the endpoint threshold"}
}

func (floor engineFloor) Holder(state session.Snapshot) Holder {
	switch {
	case state.UserSpeaking:
		return HolderUser
	case state.AgentSpeaking:
		return HolderAgent
	default:
		return HolderNobody
	}
}

type modelFloor struct{ name string }

// NewModelFloor hands endpointing to the binding's model. Only a model that
// actually owns its own floor - a full-duplex one - should be given this.
func NewModelFloor(name string) Floor {
	if strings.TrimSpace(name) == "" {
		name = "model"
	}
	return modelFloor{name: name}
}

func (floor modelFloor) Name() string      { return "model-native-" + floor.name }
func (floor modelFloor) EngineOwned() bool { return false }

func (floor modelFloor) Endpoint(context Context) EndpointDecision {
	// The model reports the endpoint through perception; the engine only
	// echoes what it was told rather than second-guessing it.
	if context.Revision.Final {
		return EndpointDecision{Ended: true, Reason: "model reported the end of the turn"}
	}
	return EndpointDecision{}
}

func (floor modelFloor) Holder(state session.Snapshot) Holder {
	switch {
	case state.AgentSpeaking && !state.UserSpeaking:
		return HolderAgent
	case state.UserSpeaking && !state.AgentSpeaking:
		return HolderUser
	case state.UserSpeaking && state.AgentSpeaking:
		// A model that owns its floor keeps it through overlap; deciding
		// otherwise here would be the engine overruling the model it delegated
		// to.
		return HolderAgent
	default:
		return HolderNobody
	}
}

// BargeInDecision says what to do about user speech over agent output.
type BargeInDecision struct {
	Cancel bool   `json:"cancel"`
	Reason string `json:"reason,omitempty"`
}

// BargeInInput is an overlap instant.
type BargeInInput struct {
	Context
	// OverlapNS is how long the overlap has lasted.
	OverlapNS uint64 `json:"overlap_ns"`
	// Evidence is what perception believes the overlapping speech is, when a
	// classifier supplied an opinion. An empty value means unclassified.
	Evidence OverlapEvidence `json:"evidence,omitempty"`
}

// OverlapEvidence is a typed opinion about overlapping speech, supplied by a
// trusted classifier. The runtime never derives it from transcript words.
type OverlapEvidence string

const (
	OverlapDirected    OverlapEvidence = "directed_speech"
	OverlapBackchannel OverlapEvidence = "listener_backchannel"
	OverlapSide        OverlapEvidence = "side_speech"
	OverlapAmbiguous   OverlapEvidence = "ambiguous_speech"
)

// BargeIn decides whether user speech over agent output cancels it.
type BargeIn interface {
	Named
	Decide(BargeInInput) BargeInDecision
}

type immediateBargeIn struct{}

// NewImmediateBargeIn yields the floor as soon as the user speaks. It is the
// shipped default because a system that keeps talking over its user is worse
// than one that stops too eagerly.
func NewImmediateBargeIn() BargeIn { return immediateBargeIn{} }

func (immediateBargeIn) Name() string { return "immediate" }

func (immediateBargeIn) Decide(input BargeInInput) BargeInDecision {
	if !input.Duplex.Overlapping() {
		return BargeInDecision{}
	}
	if input.Evidence == OverlapBackchannel || input.Evidence == OverlapSide {
		return BargeInDecision{Reason: "overlap is not directed at the agent"}
	}
	return BargeInDecision{Cancel: true, Reason: "user speech over agent output"}
}

type sustainedBargeIn struct {
	hold time.Duration
}

// NewSustainedBargeIn requires the overlap to last before yielding, which
// stops a single acknowledgement from cutting the agent off mid-sentence.
func NewSustainedBargeIn(hold time.Duration) BargeIn {
	if hold <= 0 {
		hold = 300 * time.Millisecond
	}
	return sustainedBargeIn{hold: hold}
}

func (policy sustainedBargeIn) Name() string {
	return fmt.Sprintf("sustained-%dms", policy.hold.Milliseconds())
}

func (policy sustainedBargeIn) Decide(input BargeInInput) BargeInDecision {
	if !input.Duplex.Overlapping() {
		return BargeInDecision{}
	}
	if input.Evidence == OverlapBackchannel || input.Evidence == OverlapSide {
		return BargeInDecision{Reason: "overlap is not directed at the agent"}
	}
	// The hold is a maximum, not a minimum. It exists to give a classifier
	// time to say what the overlap is; once something has said "this is
	// directed at you", waiting out the rest of it is just talking over
	// somebody who is already interrupting.
	if input.Evidence == OverlapDirected {
		return BargeInDecision{Cancel: true, Reason: "classified as directed speech"}
	}
	if input.OverlapNS < uint64(policy.hold.Nanoseconds()) {
		return BargeInDecision{Reason: "overlap has not been sustained"}
	}
	return BargeInDecision{Cancel: true, Reason: "sustained user speech over agent output"}
}

type neverBargeIn struct{}

// NewNeverBargeIn finishes the utterance whatever the user does. It exists as
// a control condition and for deployments where the agent's output is a
// recording that must play in full.
func NewNeverBargeIn() BargeIn { return neverBargeIn{} }

func (neverBargeIn) Name() string { return "never" }

func (neverBargeIn) Decide(BargeInInput) BargeInDecision {
	return BargeInDecision{Reason: "barge-in disabled"}
}
