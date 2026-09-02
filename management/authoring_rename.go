package management

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"

	"github.com/bojieli/OpenRealtime/graph/editor"
	"github.com/bojieli/OpenRealtime/graph/syntax"
)

var authoringNodeNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// ValidateRenameDocumentRequest admits only a canonical, in-memory .ortg or
// normalized YAML/JSON document and one unambiguous node-identifier
// replacement. Values, locks, filesystem roots, and deployment data are
// deliberately outside this pure operation.
func ValidateRenameDocumentRequest(input RenameDocumentRequest) error {
	if err := validateDocument(input.Document, false); err != nil {
		return err
	}
	if input.Document.Lock != nil || len(input.Document.ChannelDepth) != 0 {
		return fmt.Errorf("%w: node rename does not accept resolution or channel-depth planes", ErrInvalid)
	}
	limits := editor.DefaultLimits()
	if !authoringNodeNamePattern.MatchString(input.Node) ||
		!authoringNodeNamePattern.MatchString(input.NewName) ||
		len(input.Node) > limits.MaxIdentifierBytes || len(input.NewName) > limits.MaxIdentifierBytes {
		return fmt.Errorf("%w: invalid topology node rename", ErrInvalid)
	}
	file, err := canonicalAuthoringTopology(input.Document)
	if err != nil {
		return fmt.Errorf("%w: node rename requires canonical parsed topology source", ErrInvalid)
	}
	found := 0
	for _, statement := range file.Graph.Statements {
		if statement.Node == nil {
			continue
		}
		if statement.Node.Name == input.Node {
			found++
		}
		if input.NewName != input.Node && statement.Node.Name == input.NewName {
			return fmt.Errorf("%w: node rename target already exists", ErrInvalid)
		}
	}
	if found != 1 {
		return fmt.Errorf("%w: node rename source has %d declarations for %q", ErrInvalid, found, input.Node)
	}
	return nil
}

// ValidateRenameDocumentResult independently proves that the returned edits
// perform exactly the requested graph-wide node rename and no other source
// mutation. It does not require an element catalog because node declarations,
// edge endpoints, and graph boundaries are syntax-level facts.
func ValidateRenameDocumentResult(input RenameDocumentRequest, result RenameDocumentResult) error {
	if err := ValidateRenameDocumentRequest(input); err != nil {
		return err
	}
	if result.Node != input.Node || result.NewName != input.NewName ||
		result.Edits.Path != input.Document.Path {
		return fmt.Errorf("%w: node rename result names another request", ErrConflict)
	}
	digest := sha256.Sum256([]byte(input.Document.Source))
	wantDigest := "sha256:" + hex.EncodeToString(digest[:])
	if result.Edits.SourceDigest != wantDigest {
		return fmt.Errorf("%w: node rename result names another source", ErrConflict)
	}
	if len(result.Edits.Edits) > editor.DefaultLimits().MaxRenameEdits {
		return fmt.Errorf("%w: node rename result exceeds the edit bound", ErrConflict)
	}
	if input.Node == input.NewName && len(result.Edits.Edits) != 0 {
		return fmt.Errorf("%w: no-op node rename returned edits", ErrConflict)
	}
	if input.Node != input.NewName && len(result.Edits.Edits) == 0 {
		return fmt.Errorf("%w: node rename returned no edits", ErrConflict)
	}

	file, _ := canonicalAuthoringTopology(input.Document)
	references := renameSyntaxNode(&file, input.Node, input.NewName)
	if references > editor.DefaultLimits().MaxRenameEdits {
		return fmt.Errorf("%w: node rename reference count exceeds its bound", ErrConflict)
	}
	want, err := formatAuthoringTopology(input.Document.Path, file)
	if err != nil {
		return fmt.Errorf("%w: format expected node rename", ErrConflict)
	}
	if authoringTopologyIsORTG(input.Document.Path) {
		for _, edit := range result.Edits.Edits {
			if edit.OldText != input.Node || edit.NewText != input.NewName {
				return fmt.Errorf("%w: node rename result contains another text mutation", ErrConflict)
			}
		}
		if input.Node != input.NewName && len(result.Edits.Edits) != references {
			return fmt.Errorf("%w: node rename result covers %d of %d references",
				ErrConflict, len(result.Edits.Edits), references)
		}
	} else if input.Node != input.NewName {
		if len(result.Edits.Edits) != 1 || result.Edits.Edits[0].OldText != input.Document.Source ||
			result.Edits.Edits[0].NewText != string(want) {
			return fmt.Errorf("%w: normalized node rename is not one exact document replacement", ErrConflict)
		}
	}
	renamed, err := editor.ApplyEdits([]byte(input.Document.Source), result.Edits)
	if err != nil || len(renamed) == 0 || len(renamed) > maxManagedSourceBytes {
		return fmt.Errorf("%w: node rename edits are invalid or exceed the source bound", ErrConflict)
	}
	if !bytes.Equal(renamed, want) {
		return fmt.Errorf("%w: node rename result changes more or less than the selected node", ErrConflict)
	}
	updated := input.Document
	updated.Source = string(renamed)
	if _, err := canonicalAuthoringTopology(updated); err != nil {
		return fmt.Errorf("%w: node rename result is not canonical topology", ErrConflict)
	}
	return nil
}

func renameSyntaxNode(file *syntax.File, oldName, newName string) int {
	if file == nil || oldName == newName {
		return 0
	}
	references := 0
	for index := range file.Graph.Statements {
		statement := &file.Graph.Statements[index]
		if statement.Node != nil && statement.Node.Name == oldName {
			statement.Node.Name = newName
			references++
		}
		if statement.Edge != nil {
			if statement.Edge.From.Node == oldName {
				statement.Edge.From.Node = newName
				references++
			}
			if statement.Edge.To.Node == oldName {
				statement.Edge.To.Node = newName
				references++
			}
		}
		if statement.Boundary != nil && statement.Boundary.Endpoint.Node == oldName {
			statement.Boundary.Endpoint.Node = newName
			references++
		}
	}
	return references
}
