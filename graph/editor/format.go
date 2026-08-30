package editor

import (
	"bytes"

	"github.com/bojieli/OpenRealtime/graph/syntax"
)

// FormatEdits returns one atomic full-document replacement bound to the exact
// source digest. Recovery snapshots deliberately have no formatter edit: a
// formatter must not delete or guess an incomplete statement to make it parse.
func (document *Document) FormatEdits() (EditSet, error) {
	if document == nil || !document.parsed {
		return EditSet{}, ErrFormattingUnavailable
	}
	result := EditSet{
		Path: document.path, SourceDigest: document.digest, Edits: []TextEdit{},
	}
	formatted := []byte(syntax.Format(document.file))
	if bytes.Equal(document.source, formatted) {
		return result, nil
	}
	span, err := sourceSpan(document.positions, 0, len(document.source))
	if err != nil {
		return EditSet{}, err
	}
	result.Edits = []TextEdit{{
		Span: span, OldText: string(document.source), NewText: string(formatted),
	}}
	return result, nil
}
