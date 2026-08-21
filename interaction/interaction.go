// Package interaction is the control plane: the decisions about *when*.
//
// Nothing flows through this package. Trigger cadence, speculative pre-start,
// floor ownership, barge-in, commit-versus-cancel are decisions made over
// evidence from perception, cognition, and action, and the borrowing from
// networking is exact rather than metaphorical - the data plane moves things,
// the control plane decides how.
//
// It is a structural claim and not a ranking. Interaction is the most
// important subsystem in this project: a full-duplex model has turn-taking
// trained into its weights, but a cascade and an Omni model have none
// whatsoever, so for two of the four bindings every bit of responsiveness and
// naturalness the system exhibits is manufactured here and nowhere else.
//
// Every row is a named policy with an interface and a shipped default, because
// a policy that cannot be swapped cannot be measured, and measuring and
// replacing policy is what this project is for.
package interaction

import (
	"fmt"
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
	return nil
}

// Report is the policy set as evidence. It is what a measurement cell records
// and what the health endpoint publishes.
type Report struct {
	Trigger        string `json:"trigger"`
	Preparation    string `json:"preparation"`
	Rollout        string `json:"rollout"`
	Floor          string `json:"floor"`
	BargeIn        string `json:"barge_in"`
	Commitment     string `json:"commitment"`
	Repair         string `json:"repair"`
	Backchannel    string `json:"backchannel"`
	TurnProjection string `json:"turn_projection"`
	Overlap        string `json:"overlap"`
	Deferral       string `json:"deferral"`
}

func (policies Policies) Report() Report {
	name := func(policy Named) string {
		if policy == nil {
			return "unset"
		}
		return policy.Name()
	}
	return Report{
		Trigger: name(policies.Trigger), Preparation: name(policies.Preparation),
		Rollout: name(policies.Rollout), Floor: name(policies.Floor),
		BargeIn: name(policies.BargeIn), Commitment: name(policies.Commitment),
		Repair: name(policies.Repair), Backchannel: name(policies.Backchannel),
		TurnProjection: name(policies.TurnProjection), Overlap: name(policies.Overlap),
		Deferral: name(policies.Deferral),
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
}
