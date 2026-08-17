// Package translation provides deterministic simultaneous-translation policies
// and provider-neutral evaluation records.
package translation

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
)

type PolicyKind string

const (
	PolicyEndpointed      PolicyKind = "endpointed"
	PolicyStableIncrement PolicyKind = "stable_incremental"
	PolicyAggressive      PolicyKind = "aggressive_incremental"
)

type Segment struct {
	EndMS            uint64
	SourceDelta      string
	TargetDelta      string
	EarlyTargetDelta string
}

type Emission struct {
	SegmentIndex uint64 `json:"segment_index"`
	SourceEndNS  uint64 `json:"source_end_ns"`
	EmittedAtNS  uint64 `json:"emitted_at_ns"`
	SourceDelta  string `json:"source_delta"`
	TargetDelta  string `json:"target_delta"`
	Correct      bool   `json:"correct"`
}

type Evaluation struct {
	Policy           PolicyKind `json:"policy"`
	Emissions        []Emission `json:"emissions"`
	MeanLagNS        uint64     `json:"mean_lag_ns"`
	CompletionLagNS  uint64     `json:"completion_lag_ns"`
	QualityScore     uint64     `json:"quality_score"`
	FailureCount     uint64     `json:"failure_count"`
	ComputeUnits     uint64     `json:"compute_units"`
	AppendOnlyOutput bool       `json:"append_only_output"`
	EngineFrameMS    uint64     `json:"engine_frame_ms"`
}

type Policy struct {
	Kind                   PolicyKind
	BaseDelayNS            uint64
	JitterNS               uint64
	ComputeUnitsPerSegment uint64
}

func DefaultPolicies() []Policy {
	return []Policy{
		{Kind: PolicyEndpointed, BaseDelayNS: 180_000_000, JitterNS: 30_000_000, ComputeUnitsPerSegment: 18},
		{Kind: PolicyStableIncrement, BaseDelayNS: 80_000_000, JitterNS: 20_000_000, ComputeUnitsPerSegment: 22},
		{Kind: PolicyAggressive, BaseDelayNS: 30_000_000, JitterNS: 15_000_000, ComputeUnitsPerSegment: 10},
	}
}

// Evaluate schedules append-only transcript deltas against 200 ms engine-frame
// boundaries. Quality is exact authored-segment accuracy, not a language-model
// or corpus metric.
func (policy Policy) Evaluate(segments []Segment, seed uint64) (Evaluation, error) {
	if err := policy.validate(); err != nil {
		return Evaluation{}, err
	}
	if err := validateSegments(segments); err != nil {
		return Evaluation{}, err
	}
	result := Evaluation{
		Policy: policy.Kind, Emissions: make([]Emission, 0, len(segments)),
		AppendOnlyOutput: true, EngineFrameMS: 200,
	}
	finalEndNS, err := millisecondsToNanoseconds(segments[len(segments)-1].EndMS)
	if err != nil {
		return Evaluation{}, err
	}
	endpointedDelay := sampleDelay(policy.BaseDelayNS, policy.JitterNS, seed, 0)
	var lagTotal uint64
	for index, segment := range segments {
		sourceEndNS, err := millisecondsToNanoseconds(segment.EndMS)
		if err != nil {
			return Evaluation{}, err
		}
		delay := sampleDelay(policy.BaseDelayNS, policy.JitterNS, seed, uint64(index))
		atNS := sourceEndNS
		target := segment.TargetDelta
		switch policy.Kind {
		case PolicyEndpointed:
			atNS = finalEndNS
			if endpointedDelay > math.MaxUint64-atNS || uint64(index) > math.MaxUint64-atNS-endpointedDelay {
				return Evaluation{}, errors.New("endpointed emission timestamp overflows")
			}
			atNS += endpointedDelay + uint64(index)
		case PolicyStableIncrement:
			if delay > math.MaxUint64-atNS {
				return Evaluation{}, errors.New("stable emission timestamp overflows")
			}
			atNS += delay
		case PolicyAggressive:
			if delay > math.MaxUint64-atNS {
				return Evaluation{}, errors.New("aggressive emission timestamp overflows")
			}
			atNS += delay
			target = segment.EarlyTargetDelta
		default:
			return Evaluation{}, fmt.Errorf("unsupported translation policy %q", policy.Kind)
		}
		lag := atNS - sourceEndNS
		if lag > math.MaxUint64-lagTotal {
			return Evaluation{}, errors.New("translation lag total overflows")
		}
		lagTotal += lag
		correct := target == segment.TargetDelta
		if !correct {
			result.FailureCount++
		}
		result.Emissions = append(result.Emissions, Emission{
			SegmentIndex: uint64(index), SourceEndNS: sourceEndNS, EmittedAtNS: atNS,
			SourceDelta: segment.SourceDelta, TargetDelta: target, Correct: correct,
		})
	}
	result.MeanLagNS = lagTotal / uint64(len(segments))
	last := result.Emissions[len(result.Emissions)-1]
	result.CompletionLagNS = last.EmittedAtNS - finalEndNS
	result.QualityScore = uint64(len(segments)-int(result.FailureCount)) * 100 / uint64(len(segments))
	if policy.ComputeUnitsPerSegment > math.MaxUint64/uint64(len(segments)) {
		return Evaluation{}, errors.New("translation compute accounting overflows")
	}
	result.ComputeUnits = policy.ComputeUnitsPerSegment * uint64(len(segments))
	return result, nil
}

func (policy Policy) validate() error {
	switch policy.Kind {
	case PolicyEndpointed, PolicyStableIncrement, PolicyAggressive:
	default:
		return fmt.Errorf("unsupported translation policy %q", policy.Kind)
	}
	if policy.BaseDelayNS < policy.JitterNS || policy.ComputeUnitsPerSegment == 0 {
		return errors.New("translation policy requires nonnegative bounded delay and compute")
	}
	return nil
}

func validateSegments(segments []Segment) error {
	if len(segments) == 0 {
		return errors.New("translation evaluation requires segments")
	}
	for index, segment := range segments {
		if segment.EndMS == 0 || segment.EndMS%200 != 0 || segment.SourceDelta == "" || segment.TargetDelta == "" || segment.EarlyTargetDelta == "" {
			return fmt.Errorf("translation segment %d is incomplete or off-frame", index)
		}
		if index > 0 && segment.EndMS <= segments[index-1].EndMS {
			return errors.New("translation segments must be strictly ordered")
		}
	}
	return nil
}

func millisecondsToNanoseconds(value uint64) (uint64, error) {
	if value > math.MaxUint64/1_000_000 {
		return 0, errors.New("millisecond timestamp overflows nanoseconds")
	}
	return value * 1_000_000, nil
}

func sampleDelay(base, jitter, seed, stream uint64) uint64 {
	if jitter == 0 {
		return base
	}
	random := rand.New(rand.NewPCG(seed^0x9e3779b97f4a7c15, stream^0x243f6a8885a308d3))
	return base - jitter + random.Uint64N(jitter*2+1)
}
