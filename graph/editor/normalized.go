package editor

import (
	"bytes"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bojieli/OpenRealtime/graph/manifest"
	"github.com/bojieli/OpenRealtime/graph/syntax"
)

// NormalizedDocument is an immutable canonical YAML/JSON topology snapshot.
// Unlike the .ortg language-service Document, normalized interchange is
// regenerated atomically after a graph mutation instead of preserving source
// trivia or exposing position-sensitive language features.
type NormalizedDocument struct {
	path      string
	source    []byte
	digest    string
	limits    Limits
	document  manifest.Document
	encoding  string
	positions sourcePositions
}

// AnalyzeNormalized admits only the canonical output of the normalized graph
// manifest marshalers. Strict but noncanonical YAML/JSON remains compilable,
// but must be normalized before a visual mutation can replace the document.
func AnalyzeNormalized(path string, source []byte, limits Limits) (*NormalizedDocument, error) {
	resolved, err := normalizeLimits(limits)
	if err != nil {
		return nil, err
	}
	if len(path) == 0 || len(path) > resolved.MaxPathBytes || strings.TrimSpace(path) != path ||
		strings.ContainsAny(path, "\x00\r\n") {
		return nil, fmt.Errorf("normalized editor path exceeds its bound or contains control characters")
	}
	if len(source) == 0 || len(source) > resolved.MaxSourceBytes {
		return nil, fmt.Errorf("normalized editor source has %d bytes; maximum is %d",
			len(source), resolved.MaxSourceBytes)
	}
	extension := strings.ToLower(filepath.Ext(path))
	var file syntax.File
	switch extension {
	case ".yaml", ".yml":
		file, err = manifest.ParseYAML(path, slices.Clone(source))
	case ".json":
		file, err = manifest.ParseJSON(path, slices.Clone(source))
	default:
		return nil, fmt.Errorf("normalized editor requires a .yaml, .yml, or .json topology")
	}
	if err != nil {
		return nil, fmt.Errorf("parse normalized topology: %w", err)
	}
	if len(file.Imports)+len(file.Graph.Statements) > resolved.MaxGraphStatements {
		return nil, fmt.Errorf("normalized editor topology exceeds the statement bound")
	}
	document := manifest.FromSyntax(file)
	canonical, err := marshalNormalized(extension, document)
	if err != nil {
		return nil, fmt.Errorf("format normalized topology: %w", err)
	}
	if !bytes.Equal(source, canonical) {
		return nil, ErrSyntaxUnavailable
	}
	owned := slices.Clone(source)
	return &NormalizedDocument{
		path: path, source: owned, digest: sourceDigest(owned), limits: resolved,
		document: document, encoding: extension, positions: newSourcePositions(owned),
	}, nil
}

func (document *NormalizedDocument) Path() string { return document.path }

func (document *NormalizedDocument) SourceDigest() string { return document.digest }

func (document *NormalizedDocument) Source() []byte { return slices.Clone(document.source) }

// RenameNodeID regenerates a canonical normalized manifest after changing the
// one unambiguous node declaration and every edge/boundary reference to it.
func (document *NormalizedDocument) RenameNodeID(oldName, newName string) (EditSet, error) {
	if document == nil {
		return EditSet{}, ErrSyntaxUnavailable
	}
	valid := func(value string) bool {
		return nodeNamePattern.MatchString(value) && len(value) <= document.limits.MaxIdentifierBytes
	}
	if !valid(oldName) || !valid(newName) {
		return EditSet{}, ErrInvalidRename
	}
	declarations := 0
	for _, node := range document.document.Graph.Nodes {
		if node.ID == oldName {
			declarations++
		}
		if oldName != newName && node.ID == newName {
			return EditSet{}, fmt.Errorf("%w: node %q already exists", ErrRenameCollision, newName)
		}
	}
	if declarations != 1 {
		return EditSet{}, fmt.Errorf("%w: node %q is missing or ambiguous", ErrInvalidRename, oldName)
	}
	if oldName == newName {
		return EditSet{Path: document.path, SourceDigest: document.digest}, nil
	}

	next := cloneManifest(document.document)
	references := 0
	for index := range next.Graph.Nodes {
		if next.Graph.Nodes[index].ID == oldName {
			next.Graph.Nodes[index].ID = newName
			references++
		}
	}
	for index := range next.Graph.Edges {
		if renamed, changed := renameNormalizedEndpoint(next.Graph.Edges[index].From, oldName, newName); changed {
			next.Graph.Edges[index].From = renamed
			references++
		}
		if renamed, changed := renameNormalizedEndpoint(next.Graph.Edges[index].To, oldName, newName); changed {
			next.Graph.Edges[index].To = renamed
			references++
		}
	}
	for index := range next.Graph.Boundaries {
		if renamed, changed := renameNormalizedEndpoint(next.Graph.Boundaries[index].Endpoint, oldName, newName); changed {
			next.Graph.Boundaries[index].Endpoint = renamed
			references++
		}
	}
	if references > document.limits.MaxRenameEdits {
		return EditSet{}, ErrEditLimit
	}
	return document.replace(next)
}

// RemoveEdgeID removes exactly one named or endpoint-derived edge and
// regenerates the canonical normalized document.
func (document *NormalizedDocument) RemoveEdgeID(edgeID string) (EditSet, error) {
	if document == nil {
		return EditSet{}, ErrSyntaxUnavailable
	}
	matches := make([]int, 0, 1)
	for index, edge := range document.document.Graph.Edges {
		if normalizedEdgeIdentity(edge) == edgeID {
			matches = append(matches, index)
		}
	}
	if len(matches) != 1 {
		return EditSet{}, fmt.Errorf("%w: edge %q is missing or ambiguous", ErrInvalidEdgeMutation, edgeID)
	}
	next := cloneManifest(document.document)
	index := matches[0]
	next.Graph.Edges = append(next.Graph.Edges[:index], next.Graph.Edges[index+1:]...)
	return document.replace(next)
}

// CreateEdgeID appends one named normalized edge. Catalog-dependent direction,
// type, arity, and delivery checks remain the compiler's responsibility.
func (document *NormalizedDocument) CreateEdgeID(
	edgeID string, from, to syntax.Endpoint, delivery syntax.Delivery,
) (EditSet, error) {
	if document == nil {
		return EditSet{}, ErrSyntaxUnavailable
	}
	valid := func(value string) bool {
		return nodeNamePattern.MatchString(value) && len(value) <= document.limits.MaxIdentifierBytes
	}
	if !valid(edgeID) || !valid(from.Node) || !valid(from.Port) || !valid(to.Node) || !valid(to.Port) ||
		(delivery != syntax.Lossless && delivery != syntax.Lossy) {
		return EditSet{}, fmt.Errorf("%w: edge declaration is not canonical", ErrInvalidEdgeMutation)
	}
	declarations := make(map[string]int)
	for _, node := range document.document.Graph.Nodes {
		declarations[node.ID]++
	}
	if declarations[from.Node] != 1 || declarations[to.Node] != 1 {
		return EditSet{}, fmt.Errorf("%w: endpoint node is missing or ambiguous", ErrInvalidEdgeMutation)
	}
	for _, edge := range document.document.Graph.Edges {
		if normalizedEdgeIdentity(edge) == edgeID {
			return EditSet{}, fmt.Errorf("%w: edge %q already exists", ErrInvalidEdgeMutation, edgeID)
		}
	}
	next := cloneManifest(document.document)
	next.Graph.Edges = append(next.Graph.Edges, manifest.Edge{
		ID: edgeID, From: from.String(), To: to.String(), Delivery: string(delivery),
	})
	return document.replace(next)
}

func (document *NormalizedDocument) replace(next manifest.Document) (EditSet, error) {
	formatted, err := marshalNormalized(document.encoding, next)
	if err != nil {
		return EditSet{}, err
	}
	span, err := sourceSpan(document.positions, 0, len(document.source))
	if err != nil {
		return EditSet{}, err
	}
	return EditSet{
		Path: document.path, SourceDigest: document.digest,
		Edits: []TextEdit{{Span: span, OldText: string(document.source), NewText: string(formatted)}},
	}, nil
}

func marshalNormalized(extension string, document manifest.Document) ([]byte, error) {
	if extension == ".json" {
		return manifest.MarshalJSON(document)
	}
	return manifest.MarshalYAML(document)
}

func cloneManifest(source manifest.Document) manifest.Document {
	result := source
	result.Graph.Imports = slices.Clone(source.Graph.Imports)
	result.Graph.Nodes = slices.Clone(source.Graph.Nodes)
	result.Graph.Edges = slices.Clone(source.Graph.Edges)
	result.Graph.Boundaries = slices.Clone(source.Graph.Boundaries)
	return result
}

func normalizedEdgeIdentity(edge manifest.Edge) string {
	if edge.ID != "" {
		return edge.ID
	}
	return edge.From + "->" + edge.To
}

func renameNormalizedEndpoint(value, oldName, newName string) (string, bool) {
	node, port, found := strings.Cut(value, ".")
	if !found || node != oldName {
		return value, false
	}
	return newName + "." + port, true
}
