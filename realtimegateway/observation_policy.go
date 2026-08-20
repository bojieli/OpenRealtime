package realtimegateway

import (
	"fmt"
	"strings"
)

// ObservationPolicy controls which typed perception revisions may enter the
// canonical trajectory and therefore start authoritative cognition. It is
// independent from PreparationPolicy: private preparation has no speech or
// tool sink, while every observation admitted here may eventually produce both.
type ObservationPolicy string

const (
	// ObservationEndpointOnly admits only the terminal ASR revision. This is the
	// compatibility default and the policy used by the frozen study.
	ObservationEndpointOnly ObservationPolicy = "endpoint-only"
	// ObservationStablePartial admits a changed, non-empty StableText prefix
	// before endpoint and the final transcript when it extends that prefix.
	// Eligibility comes only from the provider's typed stable/unstable boundary.
	ObservationStablePartial ObservationPolicy = "stable-partial"
)

func ParseObservationPolicy(value string) (ObservationPolicy, error) {
	policy := ObservationPolicy(strings.ToLower(strings.TrimSpace(value)))
	switch policy {
	case ObservationEndpointOnly, ObservationStablePartial:
		return policy, nil
	default:
		return "", fmt.Errorf("observation policy must be %q or %q, got %q", ObservationEndpointOnly, ObservationStablePartial, value)
	}
}
