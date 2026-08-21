package interaction

import (
	"fmt"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/trajectory"
)

// PreparationDecision says whether to speculatively start work before the
// endpoint, and which phases to start.
//
// Preparation is private by construction: prepared work has no speech sink and
// no tool authority until it is adopted at a real safe point. That is what
// makes it safe to be wrong about, and it is why this is a policy about
// latency rather than about correctness.
type PreparationDecision struct {
	Start  bool               `json:"start"`
	Phases []trajectory.Phase `json:"phases,omitempty"`
	Reason string             `json:"reason,omitempty"`
}

// Preparation decides whether to speculatively pre-start cognition before the
// user has finished speaking. It affects perceived latency and nothing else.
type Preparation interface {
	Named
	Prepare(Context) PreparationDecision
	Reset()
}

// noPreparation waits for the endpoint. It is the control condition.
type noPreparation struct{}

// NewEndpointPreparation returns the policy that never speculates.
func NewEndpointPreparation() Preparation { return noPreparation{} }

func (noPreparation) Name() string                        { return "endpoint-only" }
func (noPreparation) Reset()                              {}
func (noPreparation) Prepare(Context) PreparationDecision { return PreparationDecision{} }

// continuousPreparation starts latest-wins speculative work on every changed
// revision, optionally pacing the slow phase so a long revision stream cannot
// launch a slow continuation per keystroke.
type continuousPreparation struct {
	slowPace time.Duration

	mu           sync.Mutex
	lastRevision uint64
	lastSlowNS   uint64
	slowStarted  bool
}

// NewContinuousPreparation speculates on every changed revision. A positive
// slowPace bounds how often the slow phase may be launched speculatively; zero
// leaves it unbounded, which is the reference configuration.
func NewContinuousPreparation(slowPace time.Duration) Preparation {
	if slowPace < 0 {
		slowPace = 0
	}
	return &continuousPreparation{slowPace: slowPace}
}

func (preparation *continuousPreparation) Name() string {
	if preparation.slowPace <= 0 {
		return "continuous"
	}
	return fmt.Sprintf("continuous-slow-paced-%dms", preparation.slowPace.Milliseconds())
}

func (preparation *continuousPreparation) Prepare(context Context) PreparationDecision {
	if context.Revision.Empty() {
		return PreparationDecision{}
	}
	preparation.mu.Lock()
	defer preparation.mu.Unlock()
	if context.Revision.ID == preparation.lastRevision {
		return PreparationDecision{}
	}
	preparation.lastRevision = context.Revision.ID
	phases := []trajectory.Phase{trajectory.PhaseFast}
	reason := "changed revision"
	if preparation.slowPace <= 0 ||
		!preparation.slowStarted ||
		context.NowNS-preparation.lastSlowNS >= uint64(preparation.slowPace.Nanoseconds()) {
		phases = append(phases, trajectory.PhaseSlow)
		preparation.lastSlowNS, preparation.slowStarted = context.NowNS, true
	} else {
		reason = "changed revision, slow paced out"
	}
	return PreparationDecision{Start: true, Phases: phases, Reason: reason}
}

func (preparation *continuousPreparation) Reset() {
	preparation.mu.Lock()
	defer preparation.mu.Unlock()
	preparation.lastRevision, preparation.lastSlowNS, preparation.slowStarted = 0, 0, false
}
