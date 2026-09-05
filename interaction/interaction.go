// Package interaction is the control plane: the decisions about *when*.
//
// Nothing flows through this package. Trigger cadence, speculative pre-start,
// floor ownership, barge-in, commit-versus-cancel are decisions made over
// evidence from perception, cognition, and action, and the borrowing from
// networking is exact rather than metaphorical - the data plane moves things,
// the control plane decides how.
//
// It is a structural claim and not a ranking. Interaction is the most
// important subsystem in this project. It may be implemented by engine
// predicates, an engine policy model, or a native interaction-capable model;
// binding ownership selects among capabilities rather than inferring the
// answer from a cascade, Omni, or duplex label.
//
// Every row is a named policy with an interface and a shipped default, because
// a policy that cannot be swapped cannot be measured, and measuring and
// replacing policy is what this project is for.
package interaction

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/bojieli/OpenRealtime/session"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// Named is implemented by every policy. The name appears in evidence records
// and in the health endpoint, so a measured cell can state which policy set
// produced it.
type Named interface {
	Name() string
}

// Revision is the newest perception evidence available at a decision instant.
//
// It is deliberately a value rather than a reference to the perception
// subsystem: a policy decides *when*, and giving it a live handle to the thing
// it is deciding about would invite it to reach into the data plane.
type Revision struct {
	ID           uint64 `json:"id"`
	StableText   string `json:"stable_text"`
	UnstableText string `json:"unstable_text"`
	Final        bool   `json:"final"`
	// ObservedNS is when perception produced this revision.
	ObservedNS uint64 `json:"observed_ns"`
	// SilenceNS is how long the acoustic gate has seen silence, zero while the
	// user is audible. Endpoint and turn-projection policies read it; nothing
	// else should.
	SilenceNS uint64 `json:"silence_ns,omitempty"`
}

// Text is the complete text of the revision, stable prefix first.
func (revision Revision) Text() string { return revision.StableText + revision.UnstableText }

// Empty reports whether the revision carries no transcript at all.
func (revision Revision) Empty() bool { return strings.TrimSpace(revision.Text()) == "" }

// Policies is the assembled control plane for one session.
//
// Assembling it in one place is what makes the set legible: a deployment can
// print exactly which twelve decisions it is making and which implementation
// makes each of them, and a measurement cell can vary exactly one.
type Policies struct {
	Trigger        Trigger
	Preparation    Preparation
	Rollout        Rollout
	Floor          Floor
	BargeIn        BargeIn
	Commitment     Commitment
	Repair         Repair
	Backchannel    Backchannel
	TurnProjection TurnProjection
	// Overlap says what user speech over agent output is. It only matters
	// with a barge-in policy that waits long enough to ask.
	Overlap  OverlapClassifier
	Deferral Deferral
	// Interaction is the model that replaces Floor, Backchannel,
	// TurnProjection and Overlap with one decision.
	//
	// It is nil by default and, while shadowing, decides nothing: it is asked
	// on every revision and its answer recorded beside what the four
	// predicates actually did. That is deliberate. The four have measured
	// behaviour on four benchmark suites and this does not, so the way to earn
	// the swap is a corpus of disagreements drawn from real recordings rather
	// than an argument that the new shape is better.
	Interaction *InteractionModel
	// TranscriptEvents is the opt-in policy for a streaming recogniser's
	// partial and final transcript events. It is deliberately parallel to
	// Interaction: enabling it must not mutate the tuned observation space or
	// instruction used by the existing interaction path.
	TranscriptEvents *TranscriptEventPolicy
	// ShadowInteraction records each decision taken twice. Non-nil implies
	// shadowing.
	ShadowInteraction func(ShadowDecision)
	// Extraction notices when somebody set an interaction policy out loud.
	//
	// Without it the interaction model is a turn-taking predicate with a
	// larger vocabulary: it can read the conversation, but nothing lifts a
	// policy out of that conversation and keeps it once the window has moved
	// on. Measured over a run of scripted conversations, none of two hundred
	// and sixty decisions carried a standing instruction until this existed.
	Extraction Extractor
}

// ShadowDecision is one instant, decided twice.
type ShadowDecision struct {
	NowNS uint64 `json:"now_ns"`
	// Situation is the block the interaction model was shown, verbatim. A
	// disagreement is only diagnosable next to what was actually in front of
	// the model, and reconstructing it afterwards reconstructs a different
	// moment.
	Situation string `json:"situation"`
	Act       string `json:"act"`
	// Predicates is what the shipped policies did at the same instant.
	Predicates map[string]string `json:"predicates"`
	Agreed     bool              `json:"agreed"`
	ElapsedNS  uint64            `json:"elapsed_ns"`
	Error      string            `json:"error,omitempty"`
}

// Defaults returns the shipped policy set: fixed 200 ms triggering, no
// speculative preparation, fast then slow with slow silent, an engine-owned
// floor, immediate barge-in, commit-on-complete, audible repair, no policy
// models, and deferral by duplex state.
//
// Preparation defaults to off because it is the one policy here that spends
// something. Every other default is a decision about when work already
// happening should happen; continuous preparation starts a continuation per
// changed revision, most of which the endpoint will contradict and discard.
// That is a real capability and a real token cost, so it is a deployment's
// choice rather than the shape a session takes by not saying anything.
func Defaults() Policies {
	return Policies{
		Trigger:        NewFixedCadenceTrigger(DefaultCadence),
		Preparation:    NewEndpointPreparation(),
		Rollout:        NewFastThenSlowRollout(RolloutOptions{}),
		Floor:          NewEngineFloor(EngineFloorOptions{}),
		BargeIn:        NewImmediateBargeIn(),
		Commitment:     NewCompleteCommitment(),
		Repair:         NewAudibleRepair(),
		Backchannel:    NoBackchannel{},
		TurnProjection: VADOnlyProjection{},
		Overlap:        UnclassifiedOverlap{},
		Deferral:       NewDuplexDeferral(DeferralOptions{}),
	}
}

// Validate reports missing policies. A nil policy is a configuration error
// rather than something to substitute silently: a runtime that quietly filled
// in a default would report a policy set it was not running.
func (policies Policies) Validate() error {
	missing := make([]string, 0, 10)
	check := func(name string, value any) {
		if value == nil {
			missing = append(missing, name)
		}
	}
	check("trigger", policies.Trigger)
	check("preparation", policies.Preparation)
	check("rollout", policies.Rollout)
	check("floor", policies.Floor)
	check("barge_in", policies.BargeIn)
	check("commitment", policies.Commitment)
	check("repair", policies.Repair)
	check("backchannel", policies.Backchannel)
	check("turn_projection", policies.TurnProjection)
	check("overlap", policies.Overlap)
	check("deferral", policies.Deferral)
	if len(missing) > 0 {
		return fmt.Errorf("interaction policies are unset: %s", strings.Join(missing, ", "))
	}
	return policies.validateProjection()
}

// validateProjection refuses a turn projection the floor does not consult.
//
// Turn projection reaches the conversation only through the floor: it is the
// floor that asks whether the turn is ending early or still going. A policy
// set can therefore name a projection while its floor never calls it, and it
// would report a model projection, flip the evidence capabilities, and project
// nothing. The serve command has always rebuilt the floor when it installs
// one, but every other composer had to remember to; make the disagreement a
// validation error so none of them can forget.
//
// The VAD-only projection is the null decision and needs no floor support: a
// floor with no projection at all and the shipped default are the same policy.
func (policies Policies) validateProjection() error {
	if isVADOnly(policies.TurnProjection) {
		return nil
	}
	floor, ok := policies.Floor.(ProjectingFloor)
	if !ok {
		return fmt.Errorf("interaction policies: turn_projection %q is set but floor %q does not consult a projection; "+
			"build the floor with EngineFloorOptions.Projection", policies.TurnProjection.Name(), policies.Floor.Name())
	}
	consulted := floor.Projection()
	if !sameProjection(consulted, policies.TurnProjection) {
		consultedName := "none"
		if consulted != nil {
			consultedName = consulted.Name()
		}
		return fmt.Errorf("interaction policies: turn_projection %q is set but floor %q consults %s; "+
			"the floor must be built around the projection the set reports",
			policies.TurnProjection.Name(), policies.Floor.Name(), consultedName)
	}
	return nil
}

// sameProjection is true when the floor consults the projection the set
// reports. Names are the reporting granularity, so they must agree; when both
// are handles, the floor must also hold the very same one, since two model
// projections over one decider can still differ in their thresholds.
func sameProjection(consulted, reported TurnProjection) bool {
	if consulted == nil || reported == nil {
		return false
	}
	if consulted.Name() != reported.Name() {
		return false
	}
	left, right := reflect.ValueOf(consulted), reflect.ValueOf(reported)
	if left.Kind() == reflect.Ptr && right.Kind() == reflect.Ptr {
		return left.Pointer() == right.Pointer()
	}
	return true
}

func isVADOnly(projection TurnProjection) bool {
	if projection == nil {
		return true
	}
	_, ok := projection.(VADOnlyProjection)
	return ok || projection.Name() == "vad-only"
}

// Report is the policy set as evidence. It is what a measurement cell records
// and what the health endpoint publishes.
type Report struct {
	Trigger          string `json:"trigger"`
	Preparation      string `json:"preparation"`
	Rollout          string `json:"rollout"`
	Floor            string `json:"floor"`
	BargeIn          string `json:"barge_in"`
	Commitment       string `json:"commitment"`
	Repair           string `json:"repair"`
	Backchannel      string `json:"backchannel"`
	TurnProjection   string `json:"turn_projection"`
	Overlap          string `json:"overlap"`
	Deferral         string `json:"deferral"`
	Interaction      string `json:"interaction"`
	TranscriptEvents string `json:"transcript_events"`
	Extraction       string `json:"extraction"`
}

func (policies Policies) Report() Report {
	name := func(policy Named) string {
		if policy == nil {
			return "unset"
		}
		return policy.Name()
	}
	interactionName := "unset"
	if policies.Interaction != nil {
		interactionName = policies.Interaction.Name()
	}
	transcriptEventsName := "unset"
	if policies.TranscriptEvents != nil {
		transcriptEventsName = policies.TranscriptEvents.Name()
	}
	return Report{
		Trigger: name(policies.Trigger), Preparation: name(policies.Preparation),
		Rollout: name(policies.Rollout), Floor: name(policies.Floor),
		BargeIn: name(policies.BargeIn), Commitment: name(policies.Commitment),
		Repair: name(policies.Repair), Backchannel: name(policies.Backchannel),
		TurnProjection: name(policies.TurnProjection), Overlap: name(policies.Overlap),
		Deferral: name(policies.Deferral), Interaction: interactionName,
		TranscriptEvents: transcriptEventsName,
		Extraction:       name(policies.Extraction),
	}
}

// Context is what every policy may read at a decision instant. Policies get a
// copy, never a handle: a control-plane decision must not be able to mutate
// the state it is deciding about.
type Context struct {
	NowNS  uint64
	Duplex session.Snapshot
	// Revision is the newest perception evidence, zero-valued when none exists.
	Revision Revision
	// Phase names which cognition provider the decision concerns, where that
	// is meaningful. It is empty for decisions that concern neither.
	Phase trajectory.Phase
	// Situation is the conversation and the instant together, for policies
	// that need both.
	//
	// Its absence was the reason no policy here could be changed by the people
	// it governed. Everything above describes acoustics and a partial
	// transcript, so a policy set out loud - let me finish, tell me the moment
	// it lands, count them as I go - was not merely ignored: there was no path
	// by which it could arrive. It is a pointer because most decisions do not
	// need it and assembling it is not free.
	Situation *Situation
}
