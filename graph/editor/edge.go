package editor

import (
	"fmt"
	"slices"

	"github.com/bojieli/OpenRealtime/graph/syntax"
)

// RemoveEdgeID returns one atomic full-document replacement that removes the
// unique canonical .ortg edge whose syntax identity equals edgeID. It does not
// compile the intermediate result: removing an edge may intentionally expose a
// required-port diagnostic for the caller to resolve in a later edit.
func (document *Document) RemoveEdgeID(edgeID string) (EditSet, error) {
	if document == nil || !document.parsed || !document.canonical {
		return EditSet{}, ErrSyntaxUnavailable
	}
	if edgeID == "" || len(edgeID) > 4*document.limits.MaxIdentifierBytes+4 {
		return EditSet{}, fmt.Errorf("%w: edge identity is empty or exceeds its bound", ErrInvalidEdgeMutation)
	}

	file := document.file
	file.Graph.Statements = slices.Clone(document.file.Graph.Statements)
	matched := -1
	for index, statement := range file.Graph.Statements {
		if statement.Edge == nil || statement.Edge.Identity() != edgeID {
			continue
		}
		if matched >= 0 {
			return EditSet{}, fmt.Errorf("%w: edge %q is ambiguous", ErrInvalidEdgeMutation, edgeID)
		}
		matched = index
	}
	if matched < 0 {
		return EditSet{}, fmt.Errorf("%w: edge %q is missing", ErrInvalidEdgeMutation, edgeID)
	}
	file.Graph.Statements = slices.Delete(file.Graph.Statements, matched, matched+1)
	span, err := sourceSpan(document.positions, 0, len(document.source))
	if err != nil {
		return EditSet{}, err
	}
	return EditSet{
		Path: document.path, SourceDigest: document.digest,
		Edits: []TextEdit{{
			Span: span, OldText: string(document.source), NewText: syntax.Format(file),
		}},
	}, nil
}

// CreateEdgeID returns one atomic full-document replacement that appends a
// named canonical edge. Semantic connection checks remain the compiler's job;
// this syntax operation only admits unambiguous declared nodes and bounded
// identifiers from an immutable canonical snapshot.
func (document *Document) CreateEdgeID(
	edgeID string, from, to syntax.Endpoint, delivery syntax.Delivery,
) (EditSet, error) {
	if document == nil || !document.parsed || !document.canonical {
		return EditSet{}, ErrSyntaxUnavailable
	}
	validIdentifier := func(value string) bool {
		return nodeNamePattern.MatchString(value) && len(value) <= document.limits.MaxIdentifierBytes
	}
	if !validIdentifier(edgeID) || !validIdentifier(from.Node) || !validIdentifier(from.Port) ||
		!validIdentifier(to.Node) || !validIdentifier(to.Port) ||
		(delivery != syntax.Lossless && delivery != syntax.Lossy) {
		return EditSet{}, fmt.Errorf("%w: edge declaration is not canonical", ErrInvalidEdgeMutation)
	}
	for _, endpoint := range []syntax.Endpoint{from, to} {
		record, found := document.nodes[endpoint.Node]
		if !found || record.ambiguous {
			return EditSet{}, fmt.Errorf("%w: endpoint node %q is missing or ambiguous",
				ErrInvalidEdgeMutation, endpoint.Node)
		}
	}
	for _, existing := range document.file.Graph.Edges() {
		if existing.Identity() == edgeID {
			return EditSet{}, fmt.Errorf("%w: edge %q already exists", ErrInvalidEdgeMutation, edgeID)
		}
	}

	file := document.file
	file.Graph.Statements = append(slices.Clone(document.file.Graph.Statements), syntax.Statement{
		Edge: &syntax.Edge{Name: edgeID, From: from, To: to, Delivery: delivery},
	})
	span, err := sourceSpan(document.positions, 0, len(document.source))
	if err != nil {
		return EditSet{}, err
	}
	return EditSet{
		Path: document.path, SourceDigest: document.digest,
		Edits: []TextEdit{{
			Span: span, OldText: string(document.source), NewText: syntax.Format(file),
		}},
	}, nil
}
