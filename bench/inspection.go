package bench

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

// ResolutionFromInspection converts one management-plane snapshot into the
// deployment resolution consumed by GraphAttestor. The expected resolution is
// the reviewed contract used to author the benchmark cell: its capabilities
// determine which nodes need a live provider handshake, and its path names
// give observed edge sequences their semantic meaning.
//
// The conversion is intentionally strict and pure. It never promotes Graph IR
// declarations to observed identities, and it never uses correlation IDs as
// selected-path names. Callers may therefore retain the snapshot separately
// without giving mutable inspection data a second interpretation later.
func ResolutionFromInspection(
	graph ir.Graph,
	configuration ArtifactIdentity,
	expected LiveResolution,
	snapshot inspect.Live,
) (LiveResolution, error) {
	if err := graph.Validate(); err != nil {
		return LiveResolution{}, fmt.Errorf("resolve live inspection: graph: %w", err)
	}
	// buildGraphEvidence performs the graph-relative half of expected-contract
	// validation: complete node coverage, exact implementations, and real,
	// continuous Graph IR edges for every semantic required path.
	if _, err := buildGraphEvidence(graph, configuration, expected); err != nil {
		return LiveResolution{}, fmt.Errorf("resolve live inspection: expected resolution: %w", err)
	}
	if err := validateInspectionIdentity(graph, configuration, snapshot); err != nil {
		return LiveResolution{}, fmt.Errorf("resolve live inspection: %w", err)
	}

	required := make(map[string]ElementResolution, len(expected.Elements))
	for _, resolution := range expected.Elements {
		required[resolution.Node] = resolution
	}
	graphNodes := make(map[string]ir.Node, len(graph.Nodes))
	for _, node := range graph.Nodes {
		graphNodes[node.ID] = node
	}
	for nodeID := range snapshot.Nodes {
		if _, found := graphNodes[nodeID]; !found {
			return LiveResolution{}, fmt.Errorf("live snapshot contains extra node %q", nodeID)
		}
	}

	result := LiveResolution{Elements: make([]ElementResolution, 0, len(graph.Nodes))}
	if snapshot.Deployment != nil {
		canonical, err := inspect.CanonicalDeploymentEvidence(*snapshot.Deployment)
		if err != nil {
			return LiveResolution{}, fmt.Errorf("resolve live inspection: deployment evidence: %w", err)
		}
		if err := canonical.ValidateExact(); err != nil {
			return LiveResolution{}, fmt.Errorf("resolve live inspection: deployment evidence: %w", err)
		}
		if expected.Deployment != nil && !reflect.DeepEqual(canonical, *expected.Deployment) {
			return LiveResolution{}, errors.New("resolve live inspection: deployment evidence drifted from the reviewed contract")
		}
		result.Deployment = &canonical
	} else if expected.Deployment != nil {
		return LiveResolution{}, errors.New("resolve live inspection: live snapshot has no deployment evidence")
	}
	for _, graphNode := range graph.Nodes {
		live, found := snapshot.Nodes[graphNode.ID]
		if !found {
			return LiveResolution{}, fmt.Errorf("live snapshot is missing graph node %q", graphNode.ID)
		}
		if live.Error != "" || live.State == "failed" {
			return LiveResolution{}, fmt.Errorf("live graph node %q is %q: %s",
				graphNode.ID, live.State, live.Error)
		}
		if live.Resolution == nil {
			return LiveResolution{}, fmt.Errorf("live graph node %q has no resolution", graphNode.ID)
		}
		resolution, err := convertNodeResolution(
			graphNode, required[graphNode.ID], *live.Resolution,
		)
		if err != nil {
			return LiveResolution{}, fmt.Errorf("live graph node %q: %w", graphNode.ID, err)
		}
		result.Elements = append(result.Elements, resolution)
	}

	paths, err := resolveSemanticPaths(graph, expected.Paths, snapshot)
	if err != nil {
		return LiveResolution{}, fmt.Errorf("resolve live inspection: %w", err)
	}
	result.Paths = paths
	return canonicalResolution(result), nil
}

func validateInspectionIdentity(
	graph ir.Graph, configuration ArtifactIdentity, snapshot inspect.Live,
) error {
	if snapshot.FormatVersion != inspect.LiveFormatVersion {
		return fmt.Errorf("live snapshot format is %d, want %d",
			snapshot.FormatVersion, inspect.LiveFormatVersion)
	}
	if snapshot.GraphID != graph.ID || snapshot.GraphRevision != graph.Revision ||
		snapshot.Fingerprint != graph.Fingerprint {
		return fmt.Errorf("live graph is %s@%d (%s), want %s@%d (%s)",
			snapshot.GraphID, snapshot.GraphRevision, snapshot.Fingerprint,
			graph.ID, graph.Revision, graph.Fingerprint)
	}
	if snapshot.Error != "" {
		return fmt.Errorf("live graph state %q failed: %s", snapshot.State, snapshot.Error)
	}
	switch snapshot.State {
	case "mounted", "running", "closed":
	case "":
		return errors.New("live snapshot has no lifecycle state")
	default:
		return fmt.Errorf("live snapshot has non-attestable lifecycle state %q", snapshot.State)
	}
	if snapshot.Configuration == nil {
		return errors.New("live snapshot has no configuration identity")
	}
	actual, err := convertInspectionArtifact(*snapshot.Configuration, "live configuration")
	if err != nil {
		return err
	}
	if actual != configuration {
		return fmt.Errorf("live configuration is %+v, want %+v", actual, configuration)
	}
	return nil
}

func convertNodeResolution(
	node ir.Node, required ElementResolution, live inspect.NodeResolution,
) (ElementResolution, error) {
	if live.Element != node.Element {
		return ElementResolution{}, fmt.Errorf("element is %+v, want %+v", live.Element, node.Element)
	}
	if live.Implementation != node.Implementation {
		return ElementResolution{}, fmt.Errorf("implementation is %q, want %q",
			live.Implementation, node.Implementation)
	}
	switch live.RuntimeEvidence {
	case inspect.EvidenceRegistered, inspect.EvidenceLive:
	case inspect.EvidenceDeclared:
		return ElementResolution{}, errors.New("runtime identity is declaration-only")
	default:
		return ElementResolution{}, fmt.Errorf("runtime identity has invalid evidence grade %q",
			live.RuntimeEvidence)
	}
	runtime, err := convertInspectionArtifact(live.Runtime, "runtime")
	if err != nil {
		return ElementResolution{}, err
	}

	needsCapabilities := len(required.Capabilities) > 0
	if needsCapabilities && live.CapabilitiesEvidence != inspect.EvidenceLive {
		return ElementResolution{}, fmt.Errorf(
			"required capabilities have %q evidence, want %q",
			live.CapabilitiesEvidence, inspect.EvidenceLive,
		)
	}
	if len(live.Capabilities) > 0 && live.CapabilitiesEvidence != inspect.EvidenceLive {
		return ElementResolution{}, fmt.Errorf(
			"reported capabilities have %q evidence, want %q",
			live.CapabilitiesEvidence, inspect.EvidenceLive,
		)
	}
	switch live.CapabilitiesEvidence {
	case "", inspect.EvidenceDeclared, inspect.EvidenceRegistered, inspect.EvidenceLive:
	default:
		return ElementResolution{}, fmt.Errorf("capabilities have invalid evidence grade %q",
			live.CapabilitiesEvidence)
	}

	var capabilities []CapabilityIdentity
	if live.CapabilitiesEvidence == inspect.EvidenceLive {
		canonical, err := inspect.CanonicalCapabilities(live.Capabilities)
		if err != nil {
			return ElementResolution{}, fmt.Errorf("capabilities: %w", err)
		}
		if len(canonical) > 0 {
			capabilities = make([]CapabilityIdentity, 0, len(canonical))
		}
		for _, capability := range canonical {
			converted := CapabilityIdentity{Name: capability.Name, Contract: capability.Contract}
			converted.Provider, err = convertInspectionArtifact(capability.Provider,
				"capability "+capability.Name+" provider")
			if err != nil {
				return ElementResolution{}, err
			}
			if capability.Adapter != nil {
				adapter, convertErr := convertInspectionArtifact(*capability.Adapter,
					"capability "+capability.Name+" adapter")
				if convertErr != nil {
					return ElementResolution{}, convertErr
				}
				converted.Adapter = &adapter
			}
			capabilities = append(capabilities, converted)
		}
	}
	return ElementResolution{
		Node: node.ID, Element: node.Element, Implementation: node.Implementation,
		Runtime: runtime, Capabilities: capabilities,
	}, nil
}

func convertInspectionArtifact(source inspect.ArtifactIdentity, label string) (ArtifactIdentity, error) {
	if err := source.Validate(); err != nil {
		return ArtifactIdentity{}, fmt.Errorf("%s: %w", label, err)
	}
	result := ArtifactIdentity{ID: source.ID, Revision: source.Revision, Digest: source.Digest}
	if err := result.validateExact(label); err != nil {
		return ArtifactIdentity{}, err
	}
	return result, nil
}

func resolveSemanticPaths(
	graph ir.Graph, required []SelectedPath, snapshot inspect.Live,
) ([]SelectedPath, error) {
	if len(required) == 0 {
		return nil, nil
	}
	if snapshot.TraceDropped > 0 {
		return nil, fmt.Errorf("route trace dropped %d record(s)", snapshot.TraceDropped)
	}
	edges := make(map[string]struct{}, len(graph.Edges))
	for _, edge := range graph.Edges {
		edges[edge.ID] = struct{}{}
	}
	flowNames := make([]string, 0, len(snapshot.Flows))
	for name := range snapshot.Flows {
		flowNames = append(flowNames, name)
	}
	sort.Strings(flowNames)
	flows := make([][]string, 0, len(flowNames))
	for _, name := range flowNames {
		flow := snapshot.Flows[name]
		if flow.Truncated {
			return nil, fmt.Errorf("route flow %q is truncated", name)
		}
		if flow.Correlation == "" || flow.Correlation != name {
			return nil, fmt.Errorf("route flow %q has inconsistent correlation %q", name, flow.Correlation)
		}
		filtered := make([]string, 0, len(flow.Edges))
		for _, edge := range flow.Edges {
			if strings.HasPrefix(edge, "boundary:") {
				continue
			}
			if _, found := edges[edge]; !found {
				return nil, fmt.Errorf("route flow %q contains unknown graph edge %q", name, edge)
			}
			filtered = append(filtered, edge)
		}
		flows = append(flows, filtered)
	}

	result := make([]SelectedPath, 0, len(required))
	for _, path := range required {
		matched := false
		for _, flow := range flows {
			if orderedSubsequence(flow, path.Edges) {
				matched = true
				break
			}
		}
		if !matched {
			return nil, fmt.Errorf("required semantic path %q was not observed", path.Name)
		}
		// Retain the reviewed semantic name and exact required edge sequence.
		// A correlation ID only proves that these traversals belonged together.
		result = append(result, SelectedPath{Name: path.Name, Edges: append([]string(nil), path.Edges...)})
	}
	return result, nil
}

func orderedSubsequence(observed, required []string) bool {
	if len(required) == 0 {
		return false
	}
	next := 0
	for _, edge := range observed {
		if edge == required[next] {
			next++
			if next == len(required) {
				return true
			}
		}
	}
	return false
}
