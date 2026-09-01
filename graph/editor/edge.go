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
