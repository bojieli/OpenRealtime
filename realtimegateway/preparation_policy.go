package realtimegateway

import (
	"fmt"
	"strings"
)

// PreparationPolicy controls when the canonical fast/slow trajectory may
// begin. It changes temporal availability only: both policies use the same
// providers, prompts, tool authority, event loop, and committed trajectory.
type PreparationPolicy string

const (
	// PreparationContinuous permits private latest-wins work on typed ASR
	// revisions before the final endpoint observation is committed.
	PreparationContinuous PreparationPolicy = "continuous"
	// PreparationEndpointOnly begins cognition only after the final ASR
	// endpoint observation has been committed to the canonical trajectory.
	PreparationEndpointOnly PreparationPolicy = "endpoint-only"
)

func ParsePreparationPolicy(value string) (PreparationPolicy, error) {
	policy := PreparationPolicy(strings.ToLower(strings.TrimSpace(value)))
	switch policy {
	case PreparationContinuous, PreparationEndpointOnly:
		return policy, nil
	default:
		return "", fmt.Errorf("preparation policy must be %q or %q, got %q", PreparationContinuous, PreparationEndpointOnly, value)
	}
}
