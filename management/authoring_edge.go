package management

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/bojieli/OpenRealtime/graph/editor"
	"github.com/bojieli/OpenRealtime/graph/syntax"
)

// ValidateRemoveDocumentEdgeRequest admits one unique edge from canonical,
// in-memory .ortg source. Resolution, channel overrides, filesystem roots, and
// deployment state are deliberately outside this pure syntax mutation.
func ValidateRemoveDocumentEdgeRequest(input RemoveDocumentEdgeRequest) error {
	if err := validateDocument(input.Document, true); err != nil {
		return err
	}
	if input.Document.Lock != nil || len(input.Document.ChannelDepth) != 0 {
		return fmt.Errorf("%w: edge removal does not accept resolution or channel-depth planes", ErrInvalid)
	}
	maximum := 4*editor.DefaultLimits().MaxIdentifierBytes + 4
	if input.Edge == "" || input.Edge != strings.TrimSpace(input.Edge) ||
		len(input.Edge) > maximum || strings.ContainsAny(input.Edge, "\x00\r\n") {
		return fmt.Errorf("%w: invalid .ortg edge identity", ErrInvalid)
	}
	file, err := syntax.Parse(input.Document.Path, []byte(input.Document.Source))
	if err != nil || syntax.Format(file) != input.Document.Source {
		return fmt.Errorf("%w: edge removal requires canonical parsed .ortg source", ErrInvalid)
	}
	if countSyntaxEdges(file, input.Edge) != 1 {
		return fmt.Errorf("%w: edge removal source does not contain exactly one edge %q", ErrInvalid, input.Edge)
	}
	return nil
}

// ValidateRemoveDocumentEdgeResult independently proves that the returned edit
// removes exactly the selected edge statement and its attached comments while
// preserving the canonical formatting of every remaining syntax statement.
func ValidateRemoveDocumentEdgeResult(
	input RemoveDocumentEdgeRequest, result RemoveDocumentEdgeResult,
) error {
	if err := ValidateRemoveDocumentEdgeRequest(input); err != nil {
		return err
	}
	if result.Edge != input.Edge || result.Edits.Path != input.Document.Path {
		return fmt.Errorf("%w: edge removal result names another request", ErrConflict)
	}
	digest := sha256.Sum256([]byte(input.Document.Source))
	wantDigest := "sha256:" + hex.EncodeToString(digest[:])
	if result.Edits.SourceDigest != wantDigest || len(result.Edits.Edits) != 1 {
		return fmt.Errorf("%w: edge removal result has another source or edit count", ErrConflict)
	}

	removed, err := editor.ApplyEdits([]byte(input.Document.Source), result.Edits)
	if err != nil || len(removed) == 0 || len(removed) > maxManagedSourceBytes {
		return fmt.Errorf("%w: edge removal edit is invalid or exceeds the source bound", ErrConflict)
	}
	file, _ := syntax.Parse(input.Document.Path, []byte(input.Document.Source))
	if removeSyntaxEdge(&file, input.Edge) != 1 {
		return fmt.Errorf("%w: edge removal source identity changed", ErrConflict)
	}
	want := []byte(syntax.Format(file))
	if !bytes.Equal(removed, want) {
		return fmt.Errorf("%w: edge removal changes more or less than the selected edge", ErrConflict)
	}
	parsed, err := syntax.Parse(input.Document.Path, removed)
	if err != nil || !bytes.Equal(removed, []byte(syntax.Format(parsed))) {
		return fmt.Errorf("%w: edge removal result is not canonical .ortg", ErrConflict)
	}
	return nil
}

func countSyntaxEdges(file syntax.File, identity string) int {
	count := 0
	for _, statement := range file.Graph.Statements {
		if statement.Edge != nil && statement.Edge.Identity() == identity {
			count++
		}
	}
	return count
}

func removeSyntaxEdge(file *syntax.File, identity string) int {
	if file == nil {
		return 0
	}
	statements := slices.Clone(file.Graph.Statements)
	kept := statements[:0]
	removed := 0
	for _, statement := range statements {
		if statement.Edge != nil && statement.Edge.Identity() == identity {
			removed++
			continue
		}
		kept = append(kept, statement)
	}
	file.Graph.Statements = kept
	return removed
}
