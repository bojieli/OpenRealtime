package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

const benchmarkExecutionFlagHelp = "reviewed execution-requirement JSON; empty preserves the historical unattested cell"
const benchmarkInspectionGraphFlagHelp = "exact bound Graph IR JSON used for authenticated live inspection"

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

// configureSessionBenchmarkAttestor binds one credential value to both the
// Realtime session and its authenticated management-plane inspection. Graph
// artifacts are loaded and reconciled with the reviewed requirement before
// getenv is called, so malformed or mismatched treatments fail before any
// environment, browser, dataset, or protocol work begins.
func configureSessionBenchmarkAttestor(
	requirement bench.ExecutionRequirement,
	inspectionGraphPath string,
	endpoint string,
	tokenEnvironment string,
	getenv func(string) string,
) (bench.RuntimeAttestor, string, error) {
	if err := requirement.Validate(); err != nil {
		return nil, "", fmt.Errorf("invalid benchmark execution requirement: %w", err)
	}
	if requirement.Kind != bench.ExecutionGraphNative &&
		strings.TrimSpace(inspectionGraphPath) != "" {
		return nil, "", errors.New(
			"-inspection-graph requires a graph-native -execution requirement; " +
				"it cannot attest a legacy or unattested cell",
		)
	}

	var prepared *reviewedGraphInspection
	if requirement.Kind == bench.ExecutionGraphNative {
		var err error
		prepared, err = prepareReviewedGraphInspection(requirement, inspectionGraphPath)
		if err != nil {
			return nil, "", err
		}
	}

	var attestor bench.RuntimeAttestor
	if prepared == nil {
		var err error
		attestor, err = sharedDriverAttestor(requirement, nil)
		if err != nil {
			return nil, "", err
		}
	}
	if getenv == nil {
		return nil, "", errors.New("configure benchmark session: nil environment lookup")
	}
	deploymentToken := getenv(tokenEnvironment)

	if prepared != nil {
		client := bench.LiveInspectionClient{
			Endpoint: endpoint, DeploymentToken: deploymentToken,
		}
		resolve, resolverErr := client.Resolver(
			prepared.graph, prepared.configuration, prepared.expected,
		)
		if resolverErr != nil {
			return nil, "", fmt.Errorf("configure authenticated live inspection: %w", resolverErr)
		}
		configured := bench.GraphAttestor{
			Graph: prepared.graph, Configuration: prepared.configuration, Resolve: resolve,
		}
		var err error
		attestor, err = sharedDriverAttestor(requirement, configured)
		if err != nil {
			return nil, "", err
		}
	}
	return attestor, deploymentToken, nil
}

type reviewedGraphInspection struct {
	graph         ir.Graph
	configuration bench.ArtifactIdentity
	expected      bench.LiveResolution
}

// prepareReviewedGraphInspection makes the expanded requirement prove that it
// was derived from this exact bound Graph IR. Rebuilding through RequireGraph
// checks graph identity, element and implementation selection, every node's
// configuration reference/digest, and every ordered required path.
func prepareReviewedGraphInspection(
	requirement bench.ExecutionRequirement, graphPath string,
) (*reviewedGraphInspection, error) {
	if err := requirement.Validate(); err != nil {
		return nil, fmt.Errorf("invalid benchmark execution requirement: %w", err)
	}
	if requirement.Kind != bench.ExecutionGraphNative || requirement.Graph == nil {
		return nil, fmt.Errorf("prepare live inspection: execution kind is %q, want %q",
			requirement.Kind, bench.ExecutionGraphNative)
	}
	if strings.TrimSpace(graphPath) == "" {
		return nil, errors.New(
			"graph-native execution requirement needs -inspection-graph with exact bound Graph IR " +
				"to configure the live runtime attestor",
		)
	}
	source, err := os.ReadFile(graphPath)
	if err != nil {
		return nil, fmt.Errorf("read inspection Graph IR %s: %w", graphPath, err)
	}
	graph, err := ir.Parse(source)
	if err != nil {
		return nil, fmt.Errorf("parse inspection Graph IR %s: %w", graphPath, err)
	}
	actualIdentity := bench.GraphIdentity{
		FormatVersion: graph.FormatVersion,
		ID:            graph.ID,
		Revision:      graph.Revision,
		Fingerprint:   graph.Fingerprint,
	}
	if actualIdentity != requirement.Graph.Graph {
		return nil, fmt.Errorf(
			"inspection graph identity is %+v, reviewed execution requires %+v",
			actualIdentity, requirement.Graph.Graph,
		)
	}

	expected := resolutionFromReviewedGraph(*requirement.Graph)
	reconstructed, err := bench.RequireGraph(graph, requirement.Graph.Configuration, expected)
	if err != nil {
		return nil, fmt.Errorf("reconstruct reviewed graph execution requirement: %w", err)
	}
	reviewedPayload, err := bench.MarshalExecutionRequirement(requirement)
	if err != nil {
		return nil, fmt.Errorf("encode reviewed graph execution requirement: %w", err)
	}
	reconstructedPayload, err := bench.MarshalExecutionRequirement(reconstructed)
	if err != nil {
		return nil, fmt.Errorf("encode reconstructed graph execution requirement: %w", err)
	}
	if !bytes.Equal(reviewedPayload, reconstructedPayload) {
		return nil, errors.New(
			"inspection Graph IR does not reproduce the reviewed node configuration, runtime, " +
				"capability, and required-path evidence",
		)
	}
	return &reviewedGraphInspection{
		graph: graph, configuration: requirement.Graph.Configuration, expected: expected,
	}, nil
}

func resolutionFromReviewedGraph(reviewed bench.GraphEvidence) bench.LiveResolution {
	resolution := bench.LiveResolution{
		Elements: make([]bench.ElementResolution, 0, len(reviewed.Nodes)),
		Paths:    make([]bench.SelectedPath, 0, len(reviewed.Paths)),
	}
	for _, node := range reviewed.Nodes {
		resolution.Elements = append(resolution.Elements, bench.ElementResolution{
			Node: node.Node, Element: node.Element, Implementation: node.Implementation,
			Runtime: node.Runtime, Capabilities: node.Capabilities,
		})
	}
	for _, path := range reviewed.Paths {
		selected := bench.SelectedPath{Name: path.Name, Edges: make([]string, 0, len(path.Edges))}
		for _, edge := range path.Edges {
			selected.Edges = append(selected.Edges, edge.ID)
		}
		resolution.Paths = append(resolution.Paths, selected)
	}
	return resolution
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
