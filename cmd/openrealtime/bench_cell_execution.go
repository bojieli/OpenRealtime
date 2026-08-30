package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

const benchmarkExecutionFlagHelp = "reviewed graph-native execution-requirement JSON; empty is diagnostic-only and never reportable"
const benchmarkInspectionGraphFlagHelp = "exact bound Graph IR JSON used for authenticated live inspection"

// attachBenchmarkExecution keeps the reviewed execution contract independent
// from the flags that describe a benchmark's treatment. Omission is permitted
// only for diagnostic callers; release publication refuses unattested cells.
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

// sharedDriverAttestor selects the graph-native evidence source available to
// suites that own their Realtime protocol session. A graph requirement needs a
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
	return nil, errors.New(
		"graph-native execution requirement needs a configured live runtime attestor; " +
			"declared Graph IR and launch flags are not execution evidence",
	)
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
				"it cannot attest an unattested diagnostic cell",
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
	if reviewed.Deployment != nil {
		deployment := reviewed.Deployment.Clone()
		resolution.Deployment = &deployment
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

// captureExternalExecutionEvidence opens one ordinary protocol session
// immediately before a subprocess-owned suite. The resulting pathless proof
// is cell-wide evidence only: it is valid for an external client whose exact
// per-task routes are unavailable, but it must never be relabelled as
// task-scoped evidence or used when the inspector reports selected paths.
func captureExternalExecutionEvidence(
	ctx context.Context,
	requirement bench.ExecutionRequirement,
	inspectionGraphPath string,
	endpoint string,
	tokenEnvironment string,
	model string,
	getenv func(string) string,
) (*bench.ExecutionEvidence, error) {
	if err := requirement.Validate(); err != nil {
		return nil, fmt.Errorf("capture external execution evidence: %w", err)
	}
	if ctx == nil {
		return nil, errors.New("capture external execution evidence: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if !requirement.Required() {
		if strings.TrimSpace(inspectionGraphPath) != "" {
			return nil, errors.New(
				"capture external execution evidence: inspection graph requires an execution requirement",
			)
		}
		return nil, nil
	}
	attestor, deploymentToken, err := configureSessionBenchmarkAttestor(
		requirement, inspectionGraphPath, endpoint, tokenEnvironment, getenv,
	)
	if err != nil {
		return nil, err
	}
	if attestor == nil {
		return nil, errors.New("capture external execution evidence: required cell has no attestor")
	}
	transcript, err := bench.PlaySamples(ctx, bench.SessionConfig{
		Endpoint: endpoint,
		Token:    deploymentToken,
		Model:    model,
		Timeout:  20 * time.Second,
		Quiet:    true,
		// This is an evidence-only session. It sends no user media and waits
		// only long enough to negotiate session.update and final inspection.
		TrailingSilence:        time.Millisecond,
		PostPlaybackQuiet:      2 * time.Second,
		CaptureRuntimeEvidence: true,
		RuntimeAttestor:        attestor,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("capture external execution evidence: %w", err)
	}
	if transcript.ExecutionError != "" {
		return nil, fmt.Errorf("capture external execution evidence: %s", transcript.ExecutionError)
	}
	if transcript.Execution == nil {
		return nil, errors.New("capture external execution evidence: endpoint emitted no execution proof")
	}
	evidence := transcript.Execution.Clone()
	if evidence.Scope != "" {
		return nil, errors.New("capture external execution evidence: cell-wide proof has a task scope")
	}
	if evidence.Graph != nil && len(evidence.Graph.Paths) > 0 {
		return nil, errors.New(
			"capture external execution evidence: selected paths require a task-scoped external inspector",
		)
	}
	if err := requirement.Match(&evidence); err != nil {
		return nil, fmt.Errorf("capture external execution evidence: %w", err)
	}
	return &evidence, nil
}
