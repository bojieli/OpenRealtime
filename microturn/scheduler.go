// Package microturn implements deterministic scheduling and revision-aware planning state.
package microturn

import (
	"errors"
	"fmt"
	"math"
)

const maxOpportunitiesPerObservation = uint64(4_096)

type TriggerReason string

const (
	TriggerFixedInterval      TriggerReason = "fixed_interval"
	TriggerPerceptionRevision TriggerReason = "perception_revision"
	TriggerEndpoint           TriggerReason = "endpoint"
)

type Observation struct {
	AtNS        uint64
	RevisionID  uint64
	HasRevision bool
	Endpoint    bool
}

type Opportunity struct {
	ID               uint64        `json:"id"`
	OpenedNS         uint64        `json:"opened_ns"`
	Reason           TriggerReason `json:"reason"`
	SourceRevisionID uint64        `json:"source_revision_id"`
}

type Scheduler interface {
	Name() string
	Observe(Observation) ([]Opportunity, error)
}

type observationState struct {
	hasObservation bool
	lastNS         uint64
	latestRevision uint64
	endpointSeen   bool
}

func (state *observationState) validate(observation Observation) error {
	if state.endpointSeen {
		return errors.New("scheduler cannot observe events after the endpoint")
	}
	if state.hasObservation && observation.AtNS < state.lastNS {
		return errors.New("scheduler observations must be monotonic")
	}
	if observation.HasRevision {
		if observation.RevisionID == 0 || observation.RevisionID <= state.latestRevision {
			return errors.New("scheduler revision IDs must increase")
		}
	} else if observation.RevisionID != 0 {
		return errors.New("revision ID requires has_revision")
	}
	return nil
}

func (state *observationState) accept(observation Observation) {
	state.hasObservation = true
	state.lastNS = observation.AtNS
	if observation.HasRevision {
		state.latestRevision = observation.RevisionID
	}
	state.endpointSeen = observation.Endpoint
}

type FixedScheduler struct {
	state     observationState
	cadenceNS uint64
	nextNS    uint64
	nextID    uint64
	exhausted bool
}

func NewFixedScheduler(cadenceNS uint64) (*FixedScheduler, error) {
	if cadenceNS == 0 {
		return nil, errors.New("fixed cadence must be positive")
	}
	return &FixedScheduler{cadenceNS: cadenceNS, nextNS: cadenceNS}, nil
}

func (scheduler *FixedScheduler) Name() string {
	return fmt.Sprintf("fixed_%dns", scheduler.cadenceNS)
}

func (scheduler *FixedScheduler) Observe(observation Observation) ([]Opportunity, error) {
	if err := scheduler.state.validate(observation); err != nil {
		return nil, err
	}
	tickCount := uint64(0)
	if !scheduler.exhausted && scheduler.nextNS <= observation.AtNS {
		tickCount = (observation.AtNS-scheduler.nextNS)/scheduler.cadenceNS + 1
	}
	endpointExtra := uint64(0)
	if observation.Endpoint && (tickCount == 0 || scheduler.nextNS+(tickCount-1)*scheduler.cadenceNS != observation.AtNS) {
		endpointExtra = 1
	}
	if tickCount > maxOpportunitiesPerObservation || endpointExtra > maxOpportunitiesPerObservation-tickCount {
		return nil, errors.New("fixed scheduler observation would exceed the bounded opportunity batch")
	}
	var opportunities []Opportunity
	for !scheduler.exhausted && scheduler.nextNS < observation.AtNS {
		opportunities = append(opportunities, scheduler.open(scheduler.nextNS, TriggerFixedInterval))
		scheduler.advance()
	}
	if observation.HasRevision {
		scheduler.state.latestRevision = observation.RevisionID
	}
	for !scheduler.exhausted && scheduler.nextNS == observation.AtNS {
		opportunities = append(opportunities, scheduler.open(scheduler.nextNS, TriggerFixedInterval))
		scheduler.advance()
	}
	if observation.Endpoint && (len(opportunities) == 0 || opportunities[len(opportunities)-1].OpenedNS != observation.AtNS) {
		opportunities = append(opportunities, scheduler.open(observation.AtNS, TriggerEndpoint))
	}
	scheduler.state.accept(observation)
	return opportunities, nil
}

func (scheduler *FixedScheduler) advance() {
	if scheduler.nextNS > math.MaxUint64-scheduler.cadenceNS {
		scheduler.exhausted = true
		return
	}
	scheduler.nextNS += scheduler.cadenceNS
}

func (scheduler *FixedScheduler) open(atNS uint64, reason TriggerReason) Opportunity {
	opportunity := Opportunity{
		ID: scheduler.nextID, OpenedNS: atNS, Reason: reason,
		SourceRevisionID: scheduler.state.latestRevision,
	}
	scheduler.nextID++
	return opportunity
}

type EventScheduler struct {
	state  observationState
	nextID uint64
}

func NewEventScheduler() *EventScheduler { return &EventScheduler{} }

func (scheduler *EventScheduler) Name() string { return "perception_revision" }

func (scheduler *EventScheduler) Observe(observation Observation) ([]Opportunity, error) {
	if err := scheduler.state.validate(observation); err != nil {
		return nil, err
	}
	scheduler.state.accept(observation)
	if !observation.HasRevision && !observation.Endpoint {
		return nil, nil
	}
	reason := TriggerPerceptionRevision
	if observation.Endpoint {
		reason = TriggerEndpoint
	}
	opportunity := Opportunity{
		ID: scheduler.nextID, OpenedNS: observation.AtNS, Reason: reason,
		SourceRevisionID: scheduler.state.latestRevision,
	}
	scheduler.nextID++
	return []Opportunity{opportunity}, nil
}
