// Package duplex implements explicit turn, overlap, and interruption policy.
package duplex

import (
	"errors"
	"math"
)

type TurnState string

const (
	StateListening               TurnState = "LISTENING"
	StateUserSpeaking            TurnState = "USER_SPEAKING"
	StateSystemPreparing         TurnState = "SYSTEM_PREPARING"
	StateSystemSpeaking          TurnState = "SYSTEM_SPEAKING"
	StateOverlapUserInterruption TurnState = "OVERLAP_USER_INTERRUPTION"
	StateOverlapBackchannel      TurnState = "OVERLAP_BACKCHANNEL"
	StateRepairing               TurnState = "REPAIRING"
)

type EvidenceKind string

const (
	EvidenceDirectedSpeech EvidenceKind = "directed_speech"
	EvidenceBackchannel    EvidenceKind = "listener_backchannel"
	EvidenceSideSpeech     EvidenceKind = "side_speech"
	EvidenceAmbiguous      EvidenceKind = "ambiguous_speech"
)

type Evidence struct {
	Kind       EvidenceKind `json:"kind"`
	Confidence float64      `json:"confidence"`
	AtNS       uint64       `json:"at_ns"`
}

type Decision string

const (
	DecisionContinue        Decision = "continue"
	DecisionYield           Decision = "yield"
	DecisionNoteBackchannel Decision = "note_backchannel"
	DecisionIgnoreSide      Decision = "ignore_side_speech"
)

type Policy struct {
	DirectedThreshold float64 `json:"directed_threshold"`
	BackchannelFloor  float64 `json:"backchannel_floor"`
}

func DefaultPolicy() Policy {
	return Policy{DirectedThreshold: 0.65, BackchannelFloor: 0.55}
}

func (policy Policy) Decide(state TurnState, evidence Evidence) (Decision, error) {
	if err := policy.validate(); err != nil {
		return "", err
	}
	if math.IsNaN(evidence.Confidence) || math.IsInf(evidence.Confidence, 0) || evidence.Confidence < 0 || evidence.Confidence > 1 {
		return "", errors.New("overlap confidence must be finite and within [0,1]")
	}
	if state != StateSystemSpeaking && state != StateOverlapBackchannel {
		return DecisionContinue, nil
	}
	switch evidence.Kind {
	case EvidenceDirectedSpeech:
		if evidence.Confidence >= policy.DirectedThreshold {
			return DecisionYield, nil
		}
		return DecisionContinue, nil
	case EvidenceBackchannel:
		if evidence.Confidence >= policy.BackchannelFloor {
			return DecisionNoteBackchannel, nil
		}
		return DecisionContinue, nil
	case EvidenceSideSpeech:
		return DecisionIgnoreSide, nil
	case EvidenceAmbiguous:
		return DecisionContinue, nil
	default:
		return "", errors.New("unknown overlap evidence kind")
	}
}

func (policy Policy) validate() error {
	if math.IsNaN(policy.DirectedThreshold) || math.IsNaN(policy.BackchannelFloor) ||
		policy.DirectedThreshold < 0 || policy.DirectedThreshold > 1 ||
		policy.BackchannelFloor < 0 || policy.BackchannelFloor > 1 {
		return errors.New("duplex policy thresholds must be within [0,1]")
	}
	return nil
}

type StateMachine struct {
	state TurnState
}

func NewStateMachine(initial TurnState) (*StateMachine, error) {
	if !knownState(initial) {
		return nil, errors.New("unknown initial turn state")
	}
	return &StateMachine{state: initial}, nil
}

func (machine *StateMachine) State() TurnState { return machine.state }

func (machine *StateMachine) Transition(next TurnState) error {
	if !allowedTransition(machine.state, next) {
		return errors.New("invalid turn-state transition")
	}
	machine.state = next
	return nil
}

func knownState(state TurnState) bool {
	switch state {
	case StateListening, StateUserSpeaking, StateSystemPreparing, StateSystemSpeaking,
		StateOverlapUserInterruption, StateOverlapBackchannel, StateRepairing:
		return true
	default:
		return false
	}
}

func allowedTransition(from, to TurnState) bool {
	switch from {
	case StateListening:
		return to == StateUserSpeaking || to == StateSystemPreparing
	case StateUserSpeaking:
		return to == StateListening || to == StateSystemPreparing
	case StateSystemPreparing:
		return to == StateSystemSpeaking || to == StateListening
	case StateSystemSpeaking:
		return to == StateListening || to == StateOverlapUserInterruption || to == StateOverlapBackchannel || to == StateRepairing
	case StateOverlapUserInterruption:
		return to == StateUserSpeaking || to == StateListening
	case StateOverlapBackchannel:
		return to == StateSystemSpeaking || to == StateOverlapUserInterruption
	case StateRepairing:
		return to == StateSystemSpeaking || to == StateListening
	default:
		return false
	}
}
