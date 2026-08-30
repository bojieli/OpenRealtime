package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
)

const executionRequirementUsage = `usage: openrealtime bench execution graph [flags]

Author a reviewed graph-native benchmark execution requirement that binds exact
Graph IR, values, and expected live deployment resolution. This command never
produces or substitutes for observed per-task execution evidence.`

func runExecutionRequirement(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(executionRequirementUsage)
	}
	switch strings.ToLower(strings.TrimSpace(arguments[0])) {
	case "graph", "graph-native":
		return runGraphExecutionRequirement(arguments[1:], output)
	case "help", "-h", "--help":
		fmt.Fprintln(output, executionRequirementUsage)
		return nil
	default:
		return fmt.Errorf("execution requirement kind must be graph-native, got %q\n%s",
			arguments[0], executionRequirementUsage)
	}
}

func runGraphExecutionRequirement(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime bench execution graph", flag.ContinueOnError)
	flags.SetOutput(output)
	graphPath := flags.String("graph", "", "exact bound Graph IR JSON")
	valuesPath := flags.String("values", "", "exact strict YAML/JSON element-values artifact")
	resolutionPath := flags.String("resolution", "", "strict expected live-resolution JSON")
	configurationID := flags.String("configuration-id", "", "immutable configuration artifact ID; default values://<graph-id>")
	configurationRevision := flags.String("configuration-revision", graphvalues.APIVersion, "immutable configuration schema/revision")
	out := flags.String("out", "", "write the execution-requirement JSON; use - for stdout")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("bench execution graph accepts flags only")
	}
	if strings.TrimSpace(*graphPath) == "" || strings.TrimSpace(*valuesPath) == "" ||
		strings.TrimSpace(*resolutionPath) == "" || strings.TrimSpace(*out) == "" {
		return errors.New("bench execution graph requires -graph, -values, -resolution, and -out")
	}

	graphSource, err := os.ReadFile(*graphPath)
	if err != nil {
		return fmt.Errorf("read bound Graph IR %s: %w", *graphPath, err)
	}
	graph, err := ir.Parse(graphSource)
	if err != nil {
		return fmt.Errorf("parse bound Graph IR %s: %w", *graphPath, err)
	}
	for _, node := range graph.Nodes {
		if node.ConfigReference == "" || node.ConfigDigest == "" {
			return fmt.Errorf("bound Graph IR node %s has no exact configuration reference and digest", node.ID)
		}
	}

	values, err := loadGraphValues(*valuesPath)
	if err != nil {
		return err
	}
	bound, err := graphvalues.Bind(graph, values)
	if err != nil {
		return fmt.Errorf("verify values against bound Graph IR: %w", err)
	}
	if bound.Graph.Fingerprint != graph.Fingerprint {
		return fmt.Errorf(
			"values artifact does not exactly match bound Graph IR: graph fingerprint is %s, values produce %s",
			graph.Fingerprint, bound.Graph.Fingerprint,
		)
	}

	expected, err := bench.ReadExpectedResolution(*resolutionPath)
	if err != nil {
		return err
	}
	identity := strings.TrimSpace(*configurationID)
	if identity == "" {
		identity = "values://" + graph.ID
	}
	configuration := bench.ArtifactIdentity{
		ID: identity, Revision: strings.TrimSpace(*configurationRevision), Digest: bound.Fingerprint,
	}
	requirement, err := bench.RequireGraph(graph, configuration, expected)
	if err != nil {
		return fmt.Errorf("author graph execution requirement: %w", err)
	}
	payload, err := bench.MarshalExecutionRequirement(requirement)
	if err != nil {
		return err
	}
	if *out == "-" {
		_, err = output.Write(payload)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return fmt.Errorf("create execution requirement directory: %w", err)
	}
	if err := atomicWrite(*out, payload); err != nil {
		return err
	}
	fmt.Fprintf(output, "wrote %s  graph=%s@%d  fingerprint=%s  configuration=%s@%s (%s)\n",
		*out, graph.ID, graph.Revision, graph.Fingerprint,
		configuration.ID, configuration.Revision, configuration.Digest)
	return nil
}
