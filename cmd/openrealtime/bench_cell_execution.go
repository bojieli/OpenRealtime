package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bojieli/OpenRealtime/bench"
)

const benchmarkExecutionFlagHelp = "reviewed execution-requirement JSON; empty preserves the historical unattested cell"

// attachBenchmarkExecution keeps the reviewed execution contract independent
// from the flags that describe a benchmark's treatment. Omission is a strict
// compatibility path: it does not alter the historical cell identity.
func attachBenchmarkExecution(cell *bench.Cell, path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if cell == nil {
		return errors.New("attach benchmark execution: nil cell")
	}
	requirement, err := bench.ReadExecutionRequirement(path)
	if err != nil {
		return err
	}
	cell.Execution = requirement
	return nil
}

// sharedDriverAttestor selects the evidence source available to suites that
// own their Realtime protocol session. A legacy requirement can be proven from
// the negotiated status itself. A graph-native requirement cannot: it needs a
// deployment-backed live resolver, which callers must configure explicitly.
func sharedDriverAttestor(
	requirement bench.ExecutionRequirement, configured bench.RuntimeAttestor,
) (bench.RuntimeAttestor, error) {
	if err := requirement.Validate(); err != nil {
		return nil, fmt.Errorf("invalid benchmark execution requirement: %w", err)
	}
	if !requirement.Required() {
		return configured, nil
	}
	if configured != nil {
		return configured, nil
	}
	switch requirement.Kind {
	case bench.ExecutionLegacy:
		return bench.LegacyStatusAttestor{}, nil
	case bench.ExecutionGraphNative:
		return nil, errors.New(
			"graph-native execution requirement needs a configured live runtime attestor; " +
				"declared Graph IR and launch flags are not execution evidence",
		)
	default:
		return nil, fmt.Errorf("unsupported benchmark execution kind %q", requirement.Kind)
	}
}

// requireExternalExecutionSource fails closed for suites whose subprocess owns
// the protocol sessions. The generic CLI currently has neither cell-wide
// observed evidence nor a task-scoped external inspector to pass through.
func requireExternalExecutionSource(requirement bench.ExecutionRequirement, configured bool) error {
	if err := requirement.Validate(); err != nil {
		return fmt.Errorf("invalid benchmark execution requirement: %w", err)
	}
	if !requirement.Required() || configured {
		return nil
	}
	return fmt.Errorf(
		"%s execution requirement needs an external task attestor or observed evidence source; "+
			"the generic CLI has neither configured", requirement.Kind,
	)
}
