// Package ir defines the immutable, language-neutral graph representation
// consumed by runtime and inspection tooling. Authoring frontends must lower
// through the shared graph compiler rather than constructing runtime objects.
package ir

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const FormatVersion = 1

// BoundaryQueuePrefix reserves the runtime/inspection queue namespace used
// for exported graph boundaries. Internal edge IDs may never use it; without
// this reservation an edge could alias a boundary queue in live evidence.
const BoundaryQueuePrefix = "boundary:"

// Delivery is the bounded-channel behavior when a queue is full.
type Delivery string

const (
	Lossless Delivery = "lossless"
	Lossy    Delivery = "lossy"
)

// BoundaryDirection is relative to the composite graph.
type BoundaryDirection string

const (
	InputBoundary  BoundaryDirection = "input"
	OutputBoundary BoundaryDirection = "output"
)

// Source identifies a frontend location for inspection and diagnostics. It
// is provenance rather than graph semantics and is excluded from fingerprints.
type Source struct {
	Path        string `json:"path,omitempty" yaml:"path,omitempty"`
	StartOffset int    `json:"start_offset,omitempty" yaml:"start_offset,omitempty"`
	EndOffset   int    `json:"end_offset,omitempty" yaml:"end_offset,omitempty"`
	Line        int    `json:"line,omitempty" yaml:"line,omitempty"`
	Column      int    `json:"column,omitempty" yaml:"column,omitempty"`
}

// Graph is a fully elaborated graph. Slices are canonicalized by Freeze; code
// mounting a graph should accept only values for which Validate succeeds.
type Graph struct {
	FormatVersion uint64     `json:"format_version" yaml:"format_version"`
	ID            string     `json:"id" yaml:"id"`
	Revision      uint64     `json:"revision" yaml:"revision"`
	Fingerprint   string     `json:"fingerprint" yaml:"fingerprint"`
	Lineage       []string   `json:"lineage,omitempty" yaml:"lineage,omitempty"`
	Nodes         []Node     `json:"nodes" yaml:"nodes"`
	Edges         []Edge     `json:"edges,omitempty" yaml:"edges,omitempty"`
	Boundaries    []Boundary `json:"boundaries,omitempty" yaml:"boundaries,omitempty"`
	Scopes        []Scope    `json:"scopes,omitempty" yaml:"scopes,omitempty"`
}

// Node is one resolved element instance. Configuration and implementation
// references are non-secret identities; actual values live in separate
// artifacts and are schema-checked before mount.
type Node struct {
	ID                  string                             `json:"id" yaml:"id"`
	Element             element.Identity                   `json:"element" yaml:"element"`
	Implementation      string                             `json:"implementation,omitempty" yaml:"implementation,omitempty"`
	ConfigReference     string                             `json:"config_reference,omitempty" yaml:"config_reference,omitempty"`
	ConfigDigest        string                             `json:"config_digest,omitempty" yaml:"config_digest,omitempty"`
	DeploymentReference string                             `json:"deployment_reference,omitempty" yaml:"deployment_reference,omitempty"`
	DeploymentDigest    string                             `json:"deployment_digest,omitempty" yaml:"deployment_digest,omitempty"`
	Ports               []Port                             `json:"ports" yaml:"ports"`
	Reaction            element.Reaction                   `json:"reaction,omitempty" yaml:"reaction,omitempty"`
	StateSchema         string                             `json:"state_schema,omitempty" yaml:"state_schema,omitempty"`
	StateTransfer       *element.StateTransferCapabilities `json:"state_transfer,omitempty" yaml:"state_transfer,omitempty"`
	ConfigSchema        string                             `json:"config_schema,omitempty" yaml:"config_schema,omitempty"`
	Dependencies        []element.Dependency               `json:"dependencies,omitempty" yaml:"dependencies,omitempty"`
	Effects             []element.Effect                   `json:"effects,omitempty" yaml:"effects,omitempty"`
	Source              *Source                            `json:"source,omitempty" yaml:"source,omitempty"`
}

// Port is a resolved descriptor port group. A variadic port has one stable
// lane per edge or exported boundary; lane identities derive from semantic
// connection identities rather than author-facing numeric indexes.
type Port struct {
	Name           string              `json:"name" yaml:"name"`
	Direction      element.Direction   `json:"direction" yaml:"direction"`
	Type           element.Type        `json:"type" yaml:"type"`
	Cardinality    element.Cardinality `json:"cardinality" yaml:"cardinality"`
	Required       bool                `json:"required,omitempty" yaml:"required,omitempty"`
	MinConnections int                 `json:"min_connections,omitempty" yaml:"min_connections,omitempty"`
	LossAllowed    bool                `json:"loss_allowed,omitempty" yaml:"loss_allowed,omitempty"`
	DefaultDepth   int                 `json:"default_depth,omitempty" yaml:"default_depth,omitempty"`
	Lanes          []string            `json:"lanes,omitempty" yaml:"lanes,omitempty"`
}

// Endpoint identifies a concrete port lane. Lane is empty for a singular port.
type Endpoint struct {
	Node string `json:"node" yaml:"node"`
	Port string `json:"port" yaml:"port"`
	Lane string `json:"lane,omitempty" yaml:"lane,omitempty"`
}

func (endpoint Endpoint) String() string {
	result := endpoint.Node + "." + endpoint.Port
	if endpoint.Lane != "" {
		result += "[" + endpoint.Lane + "]"
	}
	return result
}

// Edge is one bounded, typed channel.
type Edge struct {
	ID       string       `json:"id" yaml:"id"`
	From     Endpoint     `json:"from" yaml:"from"`
	To       Endpoint     `json:"to" yaml:"to"`
	Type     element.Type `json:"type" yaml:"type"`
	Delivery Delivery     `json:"delivery" yaml:"delivery"`
	Ordering string       `json:"ordering" yaml:"ordering"`
	Depth    int          `json:"depth" yaml:"depth"`
	Source   *Source      `json:"source,omitempty" yaml:"source,omitempty"`
}

// Boundary exports an internal lane as part of the composite contract.
type Boundary struct {
	Name      string            `json:"name" yaml:"name"`
	Direction BoundaryDirection `json:"direction" yaml:"direction"`
	Endpoint  Endpoint          `json:"endpoint" yaml:"endpoint"`
	Type      element.Type      `json:"type" yaml:"type"`
	Source    *Source           `json:"source,omitempty" yaml:"source,omitempty"`
}

// Scope retains one expanded subgraph instance so inspection can collapse or
// expand hierarchy while runtime execution uses the same flattened channels.
type Scope struct {
	ID         string           `json:"id" yaml:"id"`
	Parent     string           `json:"parent,omitempty" yaml:"parent,omitempty"`
	Composite  element.Identity `json:"composite" yaml:"composite"`
	Nodes      []string         `json:"nodes" yaml:"nodes"`
	Boundaries []ScopeBoundary  `json:"boundaries,omitempty" yaml:"boundaries,omitempty"`
}

type ScopeBoundary struct {
	Name      string            `json:"name" yaml:"name"`
	Direction BoundaryDirection `json:"direction" yaml:"direction"`
	Endpoint  Endpoint          `json:"endpoint" yaml:"endpoint"`
	Type      element.Type      `json:"type" yaml:"type"`
}

// Freeze recursively clones, canonicalizes, validates, and fingerprints a
// graph. The input is never mutated.
func Freeze(graph Graph) (Graph, error) {
	canonical := graph.clone()
	canonical.canonicalize()
	canonical.Fingerprint = ""
	if err := canonical.validateStructure(); err != nil {
		return Graph{}, err
	}
	fingerprint, err := canonical.computeFingerprint()
	if err != nil {
		return Graph{}, err
	}
	canonical.Fingerprint = fingerprint
	return canonical, nil
}

// Validate proves structural consistency and verifies the stored fingerprint.
func (graph Graph) Validate() error {
	canonical := graph.clone()
	canonical.canonicalize()
	if err := canonical.validateStructure(); err != nil {
		return err
	}
	want, err := canonical.computeFingerprint()
	if err != nil {
		return err
	}
	if graph.Fingerprint != want {
		return fmt.Errorf("graph %s fingerprint is %q, want %q", graph.ID, graph.Fingerprint, want)
	}
	return nil
}

// Marshal returns deterministic, newline-terminated JSON.
func (graph Graph) Marshal() ([]byte, error) {
	if err := graph.Validate(); err != nil {
		return nil, err
	}
	canonical := graph.clone()
	canonical.canonicalize()
	payload, err := json.MarshalIndent(canonical, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode graph IR: %w", err)
	}
	return append(payload, '\n'), nil
}

// Parse decodes strict JSON and verifies its fingerprint.
func Parse(source []byte) (Graph, error) {
	if err := strictjson.Validate(source); err != nil {
		return Graph{}, fmt.Errorf("decode graph IR: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	var graph Graph
	if err := decoder.Decode(&graph); err != nil {
		return Graph{}, fmt.Errorf("decode graph IR: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return Graph{}, errors.New("decode graph IR: trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return Graph{}, fmt.Errorf("decode graph IR trailing data: %w", err)
	}
	if err := graph.Validate(); err != nil {
		return Graph{}, err
	}
	graph.canonicalize()
	return graph, nil
}

func (graph Graph) computeFingerprint() (string, error) {
	semantic := graph.clone()
	semantic.Fingerprint = ""
	for index := range semantic.Nodes {
		semantic.Nodes[index].Source = nil
	}
	for index := range semantic.Edges {
		semantic.Edges[index].Source = nil
	}
	for index := range semantic.Boundaries {
		semantic.Boundaries[index].Source = nil
	}
	semantic.canonicalize()
	payload, err := json.Marshal(semantic)
	if err != nil {
		return "", fmt.Errorf("fingerprint graph IR: %w", err)
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func (graph *Graph) canonicalize() {
	sort.Strings(graph.Lineage)
	sort.Slice(graph.Nodes, func(left, right int) bool { return graph.Nodes[left].ID < graph.Nodes[right].ID })
	for nodeIndex := range graph.Nodes {
		node := &graph.Nodes[nodeIndex]
		sort.Slice(node.Ports, func(left, right int) bool { return node.Ports[left].Name < node.Ports[right].Name })
		for portIndex := range node.Ports {
			sort.Strings(node.Ports[portIndex].Lanes)
		}
		sort.Strings(node.Reaction.Triggers)
		sort.Strings(node.Reaction.SampledState)
		sort.Strings(node.Reaction.Interrupts)
		sort.Strings(node.Reaction.Outcomes)
		sort.Slice(node.Dependencies, func(left, right int) bool {
			return node.Dependencies[left].Name < node.Dependencies[right].Name
		})
		sort.Slice(node.Effects, func(left, right int) bool {
			return node.Effects[left].Name < node.Effects[right].Name
		})
	}
	sort.Slice(graph.Edges, func(left, right int) bool { return graph.Edges[left].ID < graph.Edges[right].ID })
	sort.Slice(graph.Boundaries, func(left, right int) bool {
		if graph.Boundaries[left].Direction != graph.Boundaries[right].Direction {
			return graph.Boundaries[left].Direction < graph.Boundaries[right].Direction
		}
		return graph.Boundaries[left].Name < graph.Boundaries[right].Name
	})
	sort.Slice(graph.Scopes, func(left, right int) bool { return graph.Scopes[left].ID < graph.Scopes[right].ID })
	for index := range graph.Scopes {
		sort.Strings(graph.Scopes[index].Nodes)
		sort.Slice(graph.Scopes[index].Boundaries, func(left, right int) bool {
			if graph.Scopes[index].Boundaries[left].Direction != graph.Scopes[index].Boundaries[right].Direction {
				return graph.Scopes[index].Boundaries[left].Direction < graph.Scopes[index].Boundaries[right].Direction
			}
			return graph.Scopes[index].Boundaries[left].Name < graph.Scopes[index].Boundaries[right].Name
		})
	}
}

func (graph Graph) clone() Graph {
	result := graph
	result.Lineage = slices.Clone(graph.Lineage)
	result.Nodes = slices.Clone(graph.Nodes)
	for index := range result.Nodes {
		node := &result.Nodes[index]
		node.Ports = slices.Clone(node.Ports)
		for portIndex := range node.Ports {
			node.Ports[portIndex].Type = node.Ports[portIndex].Type.Clone()
			node.Ports[portIndex].Lanes = slices.Clone(node.Ports[portIndex].Lanes)
		}
		node.Reaction.Triggers = slices.Clone(node.Reaction.Triggers)
		node.Reaction.SampledState = slices.Clone(node.Reaction.SampledState)
		node.Reaction.Interrupts = slices.Clone(node.Reaction.Interrupts)
		node.Reaction.Outcomes = slices.Clone(node.Reaction.Outcomes)
		node.StateTransfer = node.StateTransfer.Clone()
		node.Dependencies = slices.Clone(node.Dependencies)
		node.Effects = slices.Clone(node.Effects)
		if node.Source != nil {
			copy := *node.Source
			node.Source = &copy
		}
	}
	result.Edges = slices.Clone(graph.Edges)
	for index := range result.Edges {
		result.Edges[index].Type = result.Edges[index].Type.Clone()
		if result.Edges[index].Source != nil {
			copy := *result.Edges[index].Source
			result.Edges[index].Source = &copy
		}
	}
	result.Boundaries = slices.Clone(graph.Boundaries)
	for index := range result.Boundaries {
		result.Boundaries[index].Type = result.Boundaries[index].Type.Clone()
		if result.Boundaries[index].Source != nil {
			copy := *result.Boundaries[index].Source
			result.Boundaries[index].Source = &copy
		}
	}
	result.Scopes = slices.Clone(graph.Scopes)
	for index := range result.Scopes {
		result.Scopes[index].Nodes = slices.Clone(result.Scopes[index].Nodes)
		result.Scopes[index].Boundaries = slices.Clone(result.Scopes[index].Boundaries)
		for boundaryIndex := range result.Scopes[index].Boundaries {
			boundary := &result.Scopes[index].Boundaries[boundaryIndex]
			boundary.Type = boundary.Type.Clone()
		}
	}
	return result
}

func (graph Graph) validateStructure() error {
	if graph.FormatVersion != FormatVersion {
		return fmt.Errorf("graph IR format %d is unsupported; want %d", graph.FormatVersion, FormatVersion)
	}
	if strings.TrimSpace(graph.ID) == "" {
		return errors.New("graph IR has an empty ID")
	}
	if graph.Revision == 0 {
		return fmt.Errorf("graph %s revision must be positive", graph.ID)
	}
	if len(graph.Nodes) == 0 {
		return fmt.Errorf("graph %s has no nodes", graph.ID)
	}

	type portKey struct{ node, port string }
	nodes := make(map[string]Node, len(graph.Nodes))
	ports := make(map[portKey]Port)
	for _, node := range graph.Nodes {
		if node.ID == "" {
			return fmt.Errorf("graph %s has a node with an empty ID", graph.ID)
		}
		if _, duplicate := nodes[node.ID]; duplicate {
			return fmt.Errorf("graph %s repeats node %q", graph.ID, node.ID)
		}
		if err := element.ValidateIdentity(node.Element); err != nil {
			return fmt.Errorf("graph %s node %s: %w", graph.ID, node.ID, err)
		}
		if (node.ConfigReference == "") != (node.ConfigDigest == "") {
			return fmt.Errorf("graph %s node %s config reference and digest must be present together",
				graph.ID, node.ID)
		}
		if node.ConfigDigest != "" {
			if strings.TrimSpace(node.ConfigReference) != node.ConfigReference {
				return fmt.Errorf("graph %s node %s config reference has surrounding whitespace",
					graph.ID, node.ID)
			}
			if !strings.HasPrefix(node.ConfigDigest, "sha256:") ||
				len(node.ConfigDigest) != len("sha256:")+sha256.Size*2 {
				return fmt.Errorf("graph %s node %s has invalid config digest %q",
					graph.ID, node.ID, node.ConfigDigest)
			}
			if _, err := hex.DecodeString(strings.TrimPrefix(node.ConfigDigest, "sha256:")); err != nil {
				return fmt.Errorf("graph %s node %s has invalid config digest: %w", graph.ID, node.ID, err)
			}
		}
		if (node.DeploymentReference == "") != (node.DeploymentDigest == "") {
			return fmt.Errorf("graph %s node %s deployment reference and digest must be present together",
				graph.ID, node.ID)
		}
		if node.DeploymentDigest != "" {
			if strings.TrimSpace(node.DeploymentReference) != node.DeploymentReference {
				return fmt.Errorf("graph %s node %s deployment reference has surrounding whitespace",
					graph.ID, node.ID)
			}
			if !strings.HasPrefix(node.DeploymentDigest, "sha256:") ||
				len(node.DeploymentDigest) != len("sha256:")+sha256.Size*2 {
				return fmt.Errorf("graph %s node %s has invalid deployment digest %q",
					graph.ID, node.ID, node.DeploymentDigest)
			}
			if _, err := hex.DecodeString(strings.TrimPrefix(node.DeploymentDigest, "sha256:")); err != nil {
				return fmt.Errorf("graph %s node %s has invalid deployment digest: %w", graph.ID, node.ID, err)
			}
		}
		if node.StateTransfer != nil {
			if err := node.StateTransfer.Validate(); err != nil {
				return fmt.Errorf("graph %s node %s: %w", graph.ID, node.ID, err)
			}
			if node.StateSchema == "" {
				return fmt.Errorf("graph %s node %s state transfer requires a state schema", graph.ID, node.ID)
			}
		}
		nodes[node.ID] = node
		if len(node.Ports) == 0 {
			return fmt.Errorf("graph %s node %s has no ports", graph.ID, node.ID)
		}
		for _, port := range node.Ports {
			key := portKey{node: node.ID, port: port.Name}
			if _, duplicate := ports[key]; duplicate {
				return fmt.Errorf("graph %s node %s repeats port %q", graph.ID, node.ID, port.Name)
			}
			if err := port.Type.ValidateConcretePort(); err != nil {
				return fmt.Errorf("graph %s node %s port %s: %w", graph.ID, node.ID, port.Name, err)
			}
			switch port.Direction {
			case element.Input, element.Output:
			default:
				return fmt.Errorf("graph %s node %s port %s has invalid direction %q",
					graph.ID, node.ID, port.Name, port.Direction)
			}
			switch port.Cardinality {
			case element.One:
				if len(port.Lanes) != 0 {
					return fmt.Errorf("graph %s singular port %s.%s declares lanes", graph.ID, node.ID, port.Name)
				}
			case element.Variadic:
				seenLanes := make(map[string]struct{}, len(port.Lanes))
				for _, lane := range port.Lanes {
					if lane == "" {
						return fmt.Errorf("graph %s variadic port %s.%s has an empty lane", graph.ID, node.ID, port.Name)
					}
					if _, duplicate := seenLanes[lane]; duplicate {
						return fmt.Errorf("graph %s variadic port %s.%s repeats lane %q", graph.ID, node.ID, port.Name, lane)
					}
					seenLanes[lane] = struct{}{}
				}
			default:
				return fmt.Errorf("graph %s node %s port %s has invalid cardinality %q",
					graph.ID, node.ID, port.Name, port.Cardinality)
			}
			ports[key] = port
		}
		portByName := make(map[string]Port, len(node.Ports))
		for _, port := range node.Ports {
			portByName[port.Name] = port
		}
		for _, group := range []struct {
			label     string
			names     []string
			direction element.Direction
		}{
			{label: "trigger", names: node.Reaction.Triggers, direction: element.Input},
			{label: "sampled state", names: node.Reaction.SampledState, direction: element.Input},
			{label: "interrupt", names: node.Reaction.Interrupts, direction: element.Input},
			{label: "outcome", names: node.Reaction.Outcomes, direction: element.Output},
		} {
			for _, name := range group.names {
				port, found := portByName[name]
				if !found {
					return fmt.Errorf("graph %s node %s reaction names unknown %s port %q",
						graph.ID, node.ID, group.label, name)
				}
				if port.Direction != group.direction {
					return fmt.Errorf("graph %s node %s reaction %s port %s is %s, want %s",
						graph.ID, node.ID, group.label, name, port.Direction, group.direction)
				}
			}
		}
	}

	connections := make(map[string]string)
	connectionCounts := make(map[portKey]int, len(ports))
	validateEndpoint := func(endpoint Endpoint, want element.Direction, owner string) (Port, error) {
		group := portKey{node: endpoint.Node, port: endpoint.Port}
		port, found := ports[group]
		if !found {
			return Port{}, fmt.Errorf("graph %s %s references unknown port %s.%s",
				graph.ID, owner, endpoint.Node, endpoint.Port)
		}
		if port.Direction != want {
			return Port{}, fmt.Errorf("graph %s %s uses %s port %s as %s",
				graph.ID, owner, port.Direction, endpoint.String(), want)
		}
		if port.Cardinality == element.One && endpoint.Lane != "" {
			return Port{}, fmt.Errorf("graph %s %s selects lane %q on singular port %s.%s",
				graph.ID, owner, endpoint.Lane, endpoint.Node, endpoint.Port)
		}
		if port.Cardinality == element.Variadic && !slices.Contains(port.Lanes, endpoint.Lane) {
			return Port{}, fmt.Errorf("graph %s %s selects unknown lane %q on %s.%s",
				graph.ID, owner, endpoint.Lane, endpoint.Node, endpoint.Port)
		}
		key := endpoint.String()
		if previous, duplicate := connections[key]; duplicate {
			return Port{}, fmt.Errorf("graph %s port lane %s is used by both %s and %s",
				graph.ID, key, previous, owner)
		}
		connections[key] = owner
		connectionCounts[group]++
		return port, nil
	}

	edgeIDs := make(map[string]struct{}, len(graph.Edges))
	for _, edge := range graph.Edges {
		if edge.ID == "" {
			return fmt.Errorf("graph %s has an edge with an empty ID", graph.ID)
		}
		if strings.HasPrefix(edge.ID, BoundaryQueuePrefix) {
			return fmt.Errorf("graph %s edge %q uses reserved boundary queue prefix %q",
				graph.ID, edge.ID, BoundaryQueuePrefix)
		}
		if _, duplicate := edgeIDs[edge.ID]; duplicate {
			return fmt.Errorf("graph %s repeats edge %q", graph.ID, edge.ID)
		}
		edgeIDs[edge.ID] = struct{}{}
		from, err := validateEndpoint(edge.From, element.Output, "edge "+edge.ID)
		if err != nil {
			return err
		}
		to, err := validateEndpoint(edge.To, element.Input, "edge "+edge.ID)
		if err != nil {
			return err
		}
		if !edge.Type.Equal(from.Type) || !edge.Type.Equal(to.Type) {
			return fmt.Errorf("graph %s edge %s type %s disagrees with endpoint types %s and %s",
				graph.ID, edge.ID, edge.Type.String(), from.Type.String(), to.Type.String())
		}
		switch edge.Delivery {
		case Lossless:
		case Lossy:
			if !from.LossAllowed || !to.LossAllowed {
				return fmt.Errorf("graph %s edge %s is lossy but an endpoint forbids loss", graph.ID, edge.ID)
			}
		default:
			return fmt.Errorf("graph %s edge %s has invalid delivery %q", graph.ID, edge.ID, edge.Delivery)
		}
		if edge.Ordering != "fifo" {
			return fmt.Errorf("graph %s edge %s has unsupported ordering %q", graph.ID, edge.ID, edge.Ordering)
		}
		if edge.Depth <= 0 {
			return fmt.Errorf("graph %s edge %s has unbounded or invalid depth %d", graph.ID, edge.ID, edge.Depth)
		}
	}

	boundaryNames := make(map[string]struct{}, len(graph.Boundaries))
	for _, boundary := range graph.Boundaries {
		if boundary.Name == "" {
			return fmt.Errorf("graph %s has a boundary with an empty name", graph.ID)
		}
		if _, duplicate := boundaryNames[boundary.Name]; duplicate {
			return fmt.Errorf("graph %s repeats boundary %q", graph.ID, boundary.Name)
		}
		boundaryNames[boundary.Name] = struct{}{}
		var want element.Direction
		switch boundary.Direction {
		case InputBoundary:
			want = element.Input
		case OutputBoundary:
			want = element.Output
		default:
			return fmt.Errorf("graph %s boundary %s has invalid direction %q",
				graph.ID, boundary.Name, boundary.Direction)
		}
		port, err := validateEndpoint(boundary.Endpoint, want, "boundary "+boundary.Name)
		if err != nil {
			return err
		}
		if !boundary.Type.Equal(port.Type) {
			return fmt.Errorf("graph %s boundary %s type %s disagrees with %s",
				graph.ID, boundary.Name, boundary.Type.String(), port.Type.String())
		}
	}

	for key, port := range ports {
		minimum := port.MinConnections
		if port.Required && minimum < 1 {
			minimum = 1
		}
		if connectionCounts[key] < minimum {
			return fmt.Errorf("graph %s port %s.%s requires at least %d connection(s), got %d",
				graph.ID, key.node, key.port, minimum, connectionCounts[key])
		}
	}
	scopes := make(map[string]Scope, len(graph.Scopes))
	for _, scope := range graph.Scopes {
		if len(scope.Nodes) == 0 {
			return fmt.Errorf("graph %s scope %s contains no nodes", graph.ID, scope.ID)
		}
		if scope.ID == "" {
			return fmt.Errorf("graph %s has a subgraph scope with an empty ID", graph.ID)
		}
		if _, duplicate := scopes[scope.ID]; duplicate {
			return fmt.Errorf("graph %s repeats subgraph scope %q", graph.ID, scope.ID)
		}
		if err := element.ValidateIdentity(scope.Composite); err != nil {
			return fmt.Errorf("graph %s scope %s: %w", graph.ID, scope.ID, err)
		}
		scopes[scope.ID] = scope
		seenNodes := make(map[string]struct{}, len(scope.Nodes))
		for _, node := range scope.Nodes {
			if _, found := nodes[node]; !found {
				return fmt.Errorf("graph %s scope %s names unknown node %q", graph.ID, scope.ID, node)
			}
			if _, duplicate := seenNodes[node]; duplicate {
				return fmt.Errorf("graph %s scope %s repeats node %q", graph.ID, scope.ID, node)
			}
			seenNodes[node] = struct{}{}
		}
		boundaryNames := make(map[string]struct{}, len(scope.Boundaries))
		for _, boundary := range scope.Boundaries {
			if _, duplicate := boundaryNames[boundary.Name]; duplicate || boundary.Name == "" {
				return fmt.Errorf("graph %s scope %s has duplicate or empty boundary %q",
					graph.ID, scope.ID, boundary.Name)
			}
			boundaryNames[boundary.Name] = struct{}{}
			port, found := ports[portKey{node: boundary.Endpoint.Node, port: boundary.Endpoint.Port}]
			if !found {
				return fmt.Errorf("graph %s scope %s boundary %s names unknown endpoint %s",
					graph.ID, scope.ID, boundary.Name, boundary.Endpoint.String())
			}
			want := element.Input
			if boundary.Direction == OutputBoundary {
				want = element.Output
			} else if boundary.Direction != InputBoundary {
				return fmt.Errorf("graph %s scope %s boundary %s has invalid direction %q",
					graph.ID, scope.ID, boundary.Name, boundary.Direction)
			}
			if port.Direction != want || !port.Type.Equal(boundary.Type) {
				return fmt.Errorf("graph %s scope %s boundary %s disagrees with endpoint %s",
					graph.ID, scope.ID, boundary.Name, boundary.Endpoint.String())
			}
			if port.Cardinality == element.One && boundary.Endpoint.Lane != "" {
				return fmt.Errorf("graph %s scope %s boundary %s selects a lane on singular port %s",
					graph.ID, scope.ID, boundary.Name, boundary.Endpoint.String())
			}
			if port.Cardinality == element.Variadic && boundary.Endpoint.Lane != "" &&
				!slices.Contains(port.Lanes, boundary.Endpoint.Lane) {
				return fmt.Errorf("graph %s scope %s boundary %s selects unknown lane %q on %s.%s",
					graph.ID, scope.ID, boundary.Name, boundary.Endpoint.Lane,
					boundary.Endpoint.Node, boundary.Endpoint.Port)
			}
		}
	}
	for _, scope := range graph.Scopes {
		if scope.Parent != "" {
			if _, found := scopes[scope.Parent]; !found {
				return fmt.Errorf("graph %s scope %s names unknown parent scope %q", graph.ID, scope.ID, scope.Parent)
			}
			if scope.Parent == scope.ID {
				return fmt.Errorf("graph %s scope %s is its own parent", graph.ID, scope.ID)
			}
		}
	}
	for _, scope := range graph.Scopes {
		seenParents := map[string]struct{}{scope.ID: {}}
		parent := scope.Parent
		for parent != "" {
			if _, cycle := seenParents[parent]; cycle {
				return fmt.Errorf("graph %s subgraph scope parent cycle reaches %s", graph.ID, parent)
			}
			seenParents[parent] = struct{}{}
			parent = scopes[parent].Parent
		}
		if scope.Parent != "" {
			parentNodes := make(map[string]struct{}, len(scopes[scope.Parent].Nodes))
			for _, node := range scopes[scope.Parent].Nodes {
				parentNodes[node] = struct{}{}
			}
			for _, node := range scope.Nodes {
				if _, contained := parentNodes[node]; !contained {
					return fmt.Errorf("graph %s child scope %s node %s is absent from parent scope %s",
						graph.ID, scope.ID, node, scope.Parent)
				}
			}
		}
	}
	return nil
}
