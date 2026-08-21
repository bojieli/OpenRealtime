package interaction

import (
	"fmt"
	"sync"
	"time"
)

// DefaultCadence is the shipped trigger interval and the reference cell's
// level for factor F4.
const DefaultCadence = 200 * time.Millisecond

// Opportunity is one decision point. Open is the only field a caller must
// honour; Reason exists so a trace can say why the opportunity appeared.
type Opportunity struct {
	Open     bool   `json:"open"`
	Reason   string `json:"reason,omitempty"`
	Revision uint64 `json:"revision,omitempty"`
	AtNS     uint64 `json:"at_ns"`
}

// Trigger decides when a decision opportunity opens.
//
// It answers responsiveness, and it is the axis factor F4 varies. A trigger is
// stateful across a session - it remembers when it last fired - so one
// instance belongs to one session.
type Trigger interface {
	Named
	// Next reports whether an opportunity opens now. It is called whenever new
	// evidence arrives and may also be called on a timer; a trigger that only
	// fires on evidence simply never opens on the timer calls.
	Next(Context) Opportunity
	// Interval is the longest a caller should wait before calling Next again
	// with no new evidence. Zero means a trigger needs no timer at all.
	Interval() time.Duration
	// Reset forgets per-turn state at an endpoint.
	Reset()
}

// fixedCadenceTrigger opens an opportunity at a fixed interval while there is
// evidence to act on. It is the reference policy and the one the frozen
// cadence matrices measured.
type fixedCadenceTrigger struct {
	cadence time.Duration

	mu       sync.Mutex
	lastNS   uint64
	lastRev  uint64
	hasFired bool
}

// NewFixedCadenceTrigger opens an opportunity no more often than cadence.
func NewFixedCadenceTrigger(cadence time.Duration) Trigger {
	if cadence <= 0 {
		cadence = DefaultCadence
	}
	return &fixedCadenceTrigger{cadence: cadence}
}

func (trigger *fixedCadenceTrigger) Name() string {
	return fmt.Sprintf("fixed-cadence-%dms", trigger.cadence.Milliseconds())
}

func (trigger *fixedCadenceTrigger) Interval() time.Duration { return trigger.cadence }

func (trigger *fixedCadenceTrigger) Next(context Context) Opportunity {
	if context.Revision.Empty() {
		return Opportunity{AtNS: context.NowNS}
	}
	trigger.mu.Lock()
	defer trigger.mu.Unlock()
	// A final revision is an opportunity whatever the cadence says: the
	// endpoint is not something to be rate limited.
	if context.Revision.Final {
		trigger.lastNS, trigger.lastRev, trigger.hasFired = context.NowNS, context.Revision.ID, true
		return Opportunity{Open: true, Reason: "final revision", Revision: context.Revision.ID, AtNS: context.NowNS}
	}
	if context.Revision.ID == trigger.lastRev {
		return Opportunity{AtNS: context.NowNS}
	}
	elapsed := context.NowNS - trigger.lastNS
	if trigger.hasFired && elapsed < uint64(trigger.cadence.Nanoseconds()) {
		return Opportunity{AtNS: context.NowNS}
	}
	trigger.lastNS, trigger.lastRev, trigger.hasFired = context.NowNS, context.Revision.ID, true
	return Opportunity{Open: true, Reason: "cadence elapsed", Revision: context.Revision.ID, AtNS: context.NowNS}
}

func (trigger *fixedCadenceTrigger) Reset() {
	trigger.mu.Lock()
	defer trigger.mu.Unlock()
	trigger.lastNS, trigger.lastRev, trigger.hasFired = 0, 0, false
}

// revisionTrigger opens an opportunity on every distinct revision. It is the
// most responsive policy and the most expensive; it exists as a measured level
// rather than as a recommendation.
type revisionTrigger struct {
	mu      sync.Mutex
	lastRev uint64
}

func NewRevisionTrigger() Trigger { return &revisionTrigger{} }

func (trigger *revisionTrigger) Name() string            { return "revision" }
func (trigger *revisionTrigger) Interval() time.Duration { return 0 }

func (trigger *revisionTrigger) Next(context Context) Opportunity {
	if context.Revision.Empty() {
		return Opportunity{AtNS: context.NowNS}
	}
	trigger.mu.Lock()
	defer trigger.mu.Unlock()
	if context.Revision.ID == trigger.lastRev {
		return Opportunity{AtNS: context.NowNS}
	}
	trigger.lastRev = context.Revision.ID
	reason := "new revision"
	if context.Revision.Final {
		reason = "final revision"
	}
	return Opportunity{Open: true, Reason: reason, Revision: context.Revision.ID, AtNS: context.NowNS}
}

func (trigger *revisionTrigger) Reset() {
	trigger.mu.Lock()
	defer trigger.mu.Unlock()
	trigger.lastRev = 0
}

// endpointTrigger opens an opportunity only at the endpoint. It is the
// compatibility policy: it is what a turn-based system does, and it is the
// baseline every responsiveness claim is measured against.
type endpointTrigger struct{}

func NewEndpointTrigger() Trigger { return endpointTrigger{} }

func (endpointTrigger) Name() string            { return "endpoint-only" }
func (endpointTrigger) Interval() time.Duration { return 0 }
func (endpointTrigger) Reset()                  {}

func (endpointTrigger) Next(context Context) Opportunity {
	if !context.Revision.Final || context.Revision.Empty() {
		return Opportunity{AtNS: context.NowNS}
	}
	return Opportunity{Open: true, Reason: "endpoint", Revision: context.Revision.ID, AtNS: context.NowNS}
}
