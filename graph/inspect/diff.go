package inspect

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

// ChangeKind is deliberately structural. A graph diff never claims that a
// change is safe, compatible, hot-swappable, or behavior-preserving; those
// decisions require an explicit reconciliation policy and runtime evidence.
type ChangeKind string

const (
	ChangeAdded    ChangeKind = "added"
	ChangeRemoved  ChangeKind = "removed"
	ChangeModified ChangeKind = "modified"
)

// GraphReference identifies one exact immutable side of a diff.
type GraphReference struct {
	FormatVersion uint64 `json:"format_version"`
	ID            string `json:"id"`
	Revision      uint64 `json:"revision"`
	Fingerprint   string `json:"fingerprint"`
}

// GraphMetadata contains graph-wide semantic fields not represented by a
// node, port, edge, boundary, or scope.
type GraphMetadata struct {
	FormatVersion uint64   `json:"format_version"`
	ID            string   `json:"id"`
	Revision      uint64   `json:"revision"`
	Lineage       []string `json:"lineage,omitempty"`
}

type GraphMetadataChange struct {
	Fields []string      `json:"fields"`
	Before GraphMetadata `json:"before"`
	After  GraphMetadata `json:"after"`
}

// NodeDefinition is a node's semantic selection and lifecycle contract.
// Ports are diffed independently so a caller can highlight the exact port
// that changed. Source provenance is intentionally absent.
type NodeDefinition struct {
	ID              string               `json:"id"`
	Element         element.Identity     `json:"element"`
	Implementation  string               `json:"implementation,omitempty"`
	ConfigReference string               `json:"config_reference,omitempty"`
	ConfigDigest    string               `json:"config_digest,omitempty"`
	Reaction        element.Reaction     `json:"reaction,omitempty"`
	StateSchema     string               `json:"state_schema,omitempty"`
	ConfigSchema    string               `json:"config_schema,omitempty"`
	Dependencies    []element.Dependency `json:"dependencies,omitempty"`
	Effects         []element.Effect     `json:"effects,omitempty"`
}

type NodeChange struct {
	Kind   ChangeKind      `json:"kind"`
	Node   string          `json:"node"`
	Fields []string        `json:"fields,omitempty"`
	Before *NodeDefinition `json:"before,omitempty"`
	After  *NodeDefinition `json:"after,omitempty"`
}

type PortChange struct {
	Kind   ChangeKind `json:"kind"`
	Node   string     `json:"node"`
	Port   string     `json:"port"`
	Fields []string   `json:"fields,omitempty"`
	Before *ir.Port   `json:"before,omitempty"`
	After  *ir.Port   `json:"after,omitempty"`
}

type EdgeChange struct {
	Kind   ChangeKind `json:"kind"`
	Edge   string     `json:"edge"`
	Fields []string   `json:"fields,omitempty"`
	Before *ir.Edge   `json:"before,omitempty"`
	After  *ir.Edge   `json:"after,omitempty"`
}

type BoundaryChange struct {
	Kind     ChangeKind   `json:"kind"`
	Boundary string       `json:"boundary"`
	Fields   []string     `json:"fields,omitempty"`
	Before   *ir.Boundary `json:"before,omitempty"`
	After    *ir.Boundary `json:"after,omitempty"`
}

type ScopeChange struct {
	Kind   ChangeKind `json:"kind"`
	Scope  string     `json:"scope"`
	Fields []string   `json:"fields,omitempty"`
	Before *ir.Scope  `json:"before,omitempty"`
	After  *ir.Scope  `json:"after,omitempty"`
}

// GraphDiff is a deterministic, exact structural comparison. Collection
// order is stable, before/after values are recursively independent, and no
// field assigns a compatibility or safety interpretation.
type GraphDiff struct {
	Before     GraphReference       `json:"before"`
	After      GraphReference       `json:"after"`
	Metadata   *GraphMetadataChange `json:"metadata,omitempty"`
	Nodes      []NodeChange         `json:"nodes,omitempty"`
	Ports      []PortChange         `json:"ports,omitempty"`
	Edges      []EdgeChange         `json:"edges,omitempty"`
	Boundaries []BoundaryChange     `json:"boundaries,omitempty"`
	Scopes     []ScopeChange        `json:"scopes,omitempty"`
}

func (diff GraphDiff) Empty() bool {
	return diff.Metadata == nil && len(diff.Nodes) == 0 && len(diff.Ports) == 0 &&
		len(diff.Edges) == 0 && len(diff.Boundaries) == 0 && len(diff.Scopes) == 0
}

// DiffGraphs validates both exact frozen Graph IR values before comparing
// them. A stale fingerprint is therefore a refusal, not a misleading diff.
func DiffGraphs(before, after ir.Graph) (GraphDiff, error) {
	if err := before.Validate(); err != nil {
		return GraphDiff{}, fmt.Errorf("diff graph before: %w", err)
	}
	if err := after.Validate(); err != nil {
		return GraphDiff{}, fmt.Errorf("diff graph after: %w", err)
	}
	diff := GraphDiff{Before: graphReference(before), After: graphReference(after)}

	beforeMetadata := graphMetadata(before)
	afterMetadata := graphMetadata(after)
	if fields := metadataFields(beforeMetadata, afterMetadata); len(fields) > 0 {
		diff.Metadata = &GraphMetadataChange{
			Fields: fields, Before: beforeMetadata, After: afterMetadata,
		}
	}

	beforeNodes := make(map[string]ir.Node, len(before.Nodes))
	afterNodes := make(map[string]ir.Node, len(after.Nodes))
	for _, node := range before.Nodes {
		beforeNodes[node.ID] = node
	}
	for _, node := range after.Nodes {
		afterNodes[node.ID] = node
	}
	for _, id := range sortedUnionKeys(beforeNodes, afterNodes) {
		left, hadLeft := beforeNodes[id]
		right, hadRight := afterNodes[id]
		switch {
		case !hadLeft:
			afterDefinition := nodeDefinition(right)
			diff.Nodes = append(diff.Nodes, NodeChange{
				Kind: ChangeAdded, Node: id, After: &afterDefinition,
			})
		case !hadRight:
			beforeDefinition := nodeDefinition(left)
			diff.Nodes = append(diff.Nodes, NodeChange{
				Kind: ChangeRemoved, Node: id, Before: &beforeDefinition,
			})
		default:
			beforeDefinition := nodeDefinition(left)
			afterDefinition := nodeDefinition(right)
			if fields := nodeFields(beforeDefinition, afterDefinition); len(fields) > 0 {
				diff.Nodes = append(diff.Nodes, NodeChange{
					Kind: ChangeModified, Node: id, Fields: fields,
					Before: &beforeDefinition, After: &afterDefinition,
				})
			}
		}
	}

	diff.Ports = diffPorts(beforeNodes, afterNodes)
	diff.Edges = diffEdges(before.Edges, after.Edges)
	diff.Boundaries = diffBoundaries(before.Boundaries, after.Boundaries)
	diff.Scopes = diffScopes(before.Scopes, after.Scopes)
	return diff, nil
}

func graphReference(graph ir.Graph) GraphReference {
	return GraphReference{
		FormatVersion: graph.FormatVersion, ID: graph.ID,
		Revision: graph.Revision, Fingerprint: graph.Fingerprint,
	}
}

func graphMetadata(graph ir.Graph) GraphMetadata {
	lineage := slices.Clone(graph.Lineage)
	if len(lineage) == 0 {
		lineage = nil
	}
	return GraphMetadata{
		FormatVersion: graph.FormatVersion, ID: graph.ID,
		Revision: graph.Revision, Lineage: lineage,
	}
}

func nodeDefinition(node ir.Node) NodeDefinition {
	reaction := node.Reaction
	reaction.Triggers = normalizedStrings(reaction.Triggers)
	reaction.SampledState = normalizedStrings(reaction.SampledState)
	reaction.Interrupts = normalizedStrings(reaction.Interrupts)
	reaction.Outcomes = normalizedStrings(reaction.Outcomes)
	dependencies := slices.Clone(node.Dependencies)
	if len(dependencies) == 0 {
		dependencies = nil
	}
	effects := slices.Clone(node.Effects)
	if len(effects) == 0 {
		effects = nil
	}
	return NodeDefinition{
		ID: node.ID, Element: node.Element, Implementation: node.Implementation,
		ConfigReference: node.ConfigReference, ConfigDigest: node.ConfigDigest,
		Reaction: reaction, StateSchema: node.StateSchema, ConfigSchema: node.ConfigSchema,
		Dependencies: dependencies, Effects: effects,
	}
}

func normalizedStrings(values []string) []string {
	result := slices.Clone(values)
	if len(result) == 0 {
		return nil
	}
	sort.Strings(result)
	return result
}

func metadataFields(before, after GraphMetadata) []string {
	var fields []string
	addField(&fields, "format_version", before.FormatVersion, after.FormatVersion)
	addField(&fields, "id", before.ID, after.ID)
	addField(&fields, "revision", before.Revision, after.Revision)
	addField(&fields, "lineage", before.Lineage, after.Lineage)
	return fields
}

func nodeFields(before, after NodeDefinition) []string {
	var fields []string
	addField(&fields, "element", before.Element, after.Element)
	addField(&fields, "implementation", before.Implementation, after.Implementation)
	addField(&fields, "config_reference", before.ConfigReference, after.ConfigReference)
	addField(&fields, "config_digest", before.ConfigDigest, after.ConfigDigest)
	addField(&fields, "reaction", before.Reaction, after.Reaction)
	addField(&fields, "state_schema", before.StateSchema, after.StateSchema)
	addField(&fields, "config_schema", before.ConfigSchema, after.ConfigSchema)
	addField(&fields, "dependencies", before.Dependencies, after.Dependencies)
	addField(&fields, "effects", before.Effects, after.Effects)
	return fields
}

func portFields(before, after ir.Port) []string {
	var fields []string
	addField(&fields, "direction", before.Direction, after.Direction)
	addField(&fields, "type", before.Type, after.Type)
	addField(&fields, "cardinality", before.Cardinality, after.Cardinality)
	addField(&fields, "required", before.Required, after.Required)
	addField(&fields, "min_connections", before.MinConnections, after.MinConnections)
	addField(&fields, "loss_allowed", before.LossAllowed, after.LossAllowed)
	addField(&fields, "default_depth", before.DefaultDepth, after.DefaultDepth)
	addField(&fields, "lanes", normalizedStrings(before.Lanes), normalizedStrings(after.Lanes))
	return fields
}

func edgeFields(before, after ir.Edge) []string {
	var fields []string
	addField(&fields, "from", before.From, after.From)
	addField(&fields, "to", before.To, after.To)
	addField(&fields, "type", before.Type, after.Type)
	addField(&fields, "delivery", before.Delivery, after.Delivery)
	addField(&fields, "ordering", before.Ordering, after.Ordering)
	addField(&fields, "depth", before.Depth, after.Depth)
	return fields
}

func boundaryFields(before, after ir.Boundary) []string {
	var fields []string
	addField(&fields, "direction", before.Direction, after.Direction)
	addField(&fields, "endpoint", before.Endpoint, after.Endpoint)
	addField(&fields, "type", before.Type, after.Type)
	return fields
}

func scopeFields(before, after ir.Scope) []string {
	var fields []string
	addField(&fields, "parent", before.Parent, after.Parent)
	addField(&fields, "composite", before.Composite, after.Composite)
	addField(&fields, "nodes", before.Nodes, after.Nodes)
	addField(&fields, "boundaries", before.Boundaries, after.Boundaries)
	return fields
}

func addField[T any](fields *[]string, name string, before, after T) {
	if !sameJSON(before, after) {
		*fields = append(*fields, name)
	}
}

func sameJSON(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func diffPorts(beforeNodes, afterNodes map[string]ir.Node) []PortChange {
	type key struct{ node, port string }
	before := map[key]ir.Port{}
	after := map[key]ir.Port{}
	for _, node := range beforeNodes {
		for _, port := range node.Ports {
			before[key{node: node.ID, port: port.Name}] = canonicalPort(port)
		}
	}
	for _, node := range afterNodes {
		for _, port := range node.Ports {
			after[key{node: node.ID, port: port.Name}] = canonicalPort(port)
		}
	}
	keys := make([]key, 0, len(before)+len(after))
	seen := map[key]struct{}{}
	for item := range before {
		seen[item] = struct{}{}
		keys = append(keys, item)
	}
	for item := range after {
		if _, found := seen[item]; !found {
			keys = append(keys, item)
		}
	}
	sort.Slice(keys, func(left, right int) bool {
		if keys[left].node != keys[right].node {
			return keys[left].node < keys[right].node
		}
		return keys[left].port < keys[right].port
	})
	result := make([]PortChange, 0)
	for _, item := range keys {
		left, hadLeft := before[item]
		right, hadRight := after[item]
		switch {
		case !hadLeft:
			copy := clonePort(right)
			result = append(result, PortChange{Kind: ChangeAdded, Node: item.node, Port: item.port, After: &copy})
		case !hadRight:
			copy := clonePort(left)
			result = append(result, PortChange{Kind: ChangeRemoved, Node: item.node, Port: item.port, Before: &copy})
		default:
			if fields := portFields(left, right); len(fields) > 0 {
				beforeCopy, afterCopy := clonePort(left), clonePort(right)
				result = append(result, PortChange{
					Kind: ChangeModified, Node: item.node, Port: item.port, Fields: fields,
					Before: &beforeCopy, After: &afterCopy,
				})
			}
		}
	}
	return result
}

func canonicalPort(port ir.Port) ir.Port {
	result := clonePort(port)
	result.Lanes = normalizedStrings(result.Lanes)
	return result
}

func clonePort(port ir.Port) ir.Port {
	result := port
	result.Type = port.Type.Clone()
	result.Lanes = slices.Clone(port.Lanes)
	return result
}

func diffEdges(beforeEdges, afterEdges []ir.Edge) []EdgeChange {
	before := make(map[string]ir.Edge, len(beforeEdges))
	after := make(map[string]ir.Edge, len(afterEdges))
	for _, edge := range beforeEdges {
		before[edge.ID] = cloneEdgeWithoutSource(edge)
	}
	for _, edge := range afterEdges {
		after[edge.ID] = cloneEdgeWithoutSource(edge)
	}
	result := make([]EdgeChange, 0)
	for _, id := range sortedUnionKeys(before, after) {
		left, hadLeft := before[id]
		right, hadRight := after[id]
		switch {
		case !hadLeft:
			copy := cloneEdgeWithoutSource(right)
			result = append(result, EdgeChange{Kind: ChangeAdded, Edge: id, After: &copy})
		case !hadRight:
			copy := cloneEdgeWithoutSource(left)
			result = append(result, EdgeChange{Kind: ChangeRemoved, Edge: id, Before: &copy})
		default:
			if fields := edgeFields(left, right); len(fields) > 0 {
				beforeCopy, afterCopy := cloneEdgeWithoutSource(left), cloneEdgeWithoutSource(right)
				result = append(result, EdgeChange{
					Kind: ChangeModified, Edge: id, Fields: fields,
					Before: &beforeCopy, After: &afterCopy,
				})
			}
		}
	}
	return result
}

func cloneEdgeWithoutSource(edge ir.Edge) ir.Edge {
	result := edge
	result.Type = edge.Type.Clone()
	result.Source = nil
	return result
}

func diffBoundaries(beforeBoundaries, afterBoundaries []ir.Boundary) []BoundaryChange {
	before := make(map[string]ir.Boundary, len(beforeBoundaries))
	after := make(map[string]ir.Boundary, len(afterBoundaries))
	for _, boundary := range beforeBoundaries {
		before[boundary.Name] = cloneBoundaryWithoutSource(boundary)
	}
	for _, boundary := range afterBoundaries {
		after[boundary.Name] = cloneBoundaryWithoutSource(boundary)
	}
	result := make([]BoundaryChange, 0)
	for _, name := range sortedUnionKeys(before, after) {
		left, hadLeft := before[name]
		right, hadRight := after[name]
		switch {
		case !hadLeft:
			copy := cloneBoundaryWithoutSource(right)
			result = append(result, BoundaryChange{Kind: ChangeAdded, Boundary: name, After: &copy})
		case !hadRight:
			copy := cloneBoundaryWithoutSource(left)
			result = append(result, BoundaryChange{Kind: ChangeRemoved, Boundary: name, Before: &copy})
		default:
			if fields := boundaryFields(left, right); len(fields) > 0 {
				beforeCopy := cloneBoundaryWithoutSource(left)
				afterCopy := cloneBoundaryWithoutSource(right)
				result = append(result, BoundaryChange{
					Kind: ChangeModified, Boundary: name, Fields: fields,
					Before: &beforeCopy, After: &afterCopy,
				})
			}
		}
	}
	return result
}

func cloneBoundaryWithoutSource(boundary ir.Boundary) ir.Boundary {
	result := boundary
	result.Type = boundary.Type.Clone()
	result.Source = nil
	return result
}

func diffScopes(beforeScopes, afterScopes []ir.Scope) []ScopeChange {
	before := make(map[string]ir.Scope, len(beforeScopes))
	after := make(map[string]ir.Scope, len(afterScopes))
	for _, scope := range beforeScopes {
		before[scope.ID] = cloneScope(scope)
	}
	for _, scope := range afterScopes {
		after[scope.ID] = cloneScope(scope)
	}
	result := make([]ScopeChange, 0)
	for _, id := range sortedUnionKeys(before, after) {
		left, hadLeft := before[id]
		right, hadRight := after[id]
		switch {
		case !hadLeft:
			copy := cloneScope(right)
			result = append(result, ScopeChange{Kind: ChangeAdded, Scope: id, After: &copy})
		case !hadRight:
			copy := cloneScope(left)
			result = append(result, ScopeChange{Kind: ChangeRemoved, Scope: id, Before: &copy})
		default:
			if fields := scopeFields(left, right); len(fields) > 0 {
				beforeCopy, afterCopy := cloneScope(left), cloneScope(right)
				result = append(result, ScopeChange{
					Kind: ChangeModified, Scope: id, Fields: fields,
					Before: &beforeCopy, After: &afterCopy,
				})
			}
		}
	}
	return result
}

func cloneScope(scope ir.Scope) ir.Scope {
	result := scope
	result.Nodes = slices.Clone(scope.Nodes)
	result.Boundaries = slices.Clone(scope.Boundaries)
	if len(result.Boundaries) == 0 {
		result.Boundaries = nil
	}
	for index := range result.Boundaries {
		result.Boundaries[index].Type = result.Boundaries[index].Type.Clone()
	}
	return result
}

func sortedUnionKeys[T any](left, right map[string]T) []string {
	seen := make(map[string]struct{}, len(left)+len(right))
	result := make([]string, 0, len(left)+len(right))
	for key := range left {
		seen[key] = struct{}{}
		result = append(result, key)
	}
	for key := range right {
		if _, found := seen[key]; !found {
			result = append(result, key)
		}
	}
	sort.Strings(result)
	return result
}
