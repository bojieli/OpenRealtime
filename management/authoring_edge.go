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

// ValidateCreateDocumentEdgeRequest admits one named connection between exact
// declared nodes in a canonical in-memory .ortg predecessor. The immutable
// predecessor fingerprint is recompiled and checked by AuthoringEngine.
func ValidateCreateDocumentEdgeRequest(input CreateDocumentEdgeRequest) error {
	if err := validateDocument(input.Document, true); err != nil {
		return err
	}
	if input.Document.Lock != nil || len(input.Document.ChannelDepth) != 0 {
		return fmt.Errorf("%w: edge creation does not accept resolution or channel-depth planes", ErrInvalid)
	}
	if !CanonicalDigest(input.ExpectedFingerprint) {
		return fmt.Errorf("%w: edge creation requires a canonical predecessor fingerprint", ErrInvalid)
	}
	limits := editor.DefaultLimits()
	validIdentifier := func(value string) bool {
		return authoringNodeNamePattern.MatchString(value) && len(value) <= limits.MaxIdentifierBytes
	}
	if !validIdentifier(input.Edge) || !validIdentifier(input.From.Node) ||
		!validIdentifier(input.From.Port) || !validIdentifier(input.To.Node) ||
		!validIdentifier(input.To.Port) ||
		(input.Delivery != string(syntax.Lossless) && input.Delivery != string(syntax.Lossy)) {
		return fmt.Errorf("%w: invalid .ortg edge creation", ErrInvalid)
	}
	file, err := syntax.Parse(input.Document.Path, []byte(input.Document.Source))
	if err != nil || syntax.Format(file) != input.Document.Source {
		return fmt.Errorf("%w: edge creation requires canonical parsed .ortg source", ErrInvalid)
	}
	if countSyntaxEdges(file, input.Edge) != 0 {
		return fmt.Errorf("%w: edge creation identity already exists", ErrInvalid)
	}
	declarations := make(map[string]int)
	for _, node := range file.Graph.Nodes() {
		declarations[node.Name]++
	}
	if declarations[input.From.Node] != 1 || declarations[input.To.Node] != 1 {
		return fmt.Errorf("%w: edge creation endpoints are missing or ambiguous", ErrInvalid)
	}
	if _, err := nextAuthoringRevision(input.Document.Revision); err != nil {
		return err
	}
	return nil
}

// ValidateCreateDocumentEdgeResult proves the exact canonical syntax change
// and binds the response to both predecessor and compiler-produced candidate
// fingerprints. Catalog-dependent compilation remains the engine's duty.
func ValidateCreateDocumentEdgeResult(
	input CreateDocumentEdgeRequest, result CreateDocumentEdgeResult,
) error {
	if err := ValidateCreateDocumentEdgeRequest(input); err != nil {
		return err
	}
	if result.Edge != input.Edge || result.PreviousFingerprint != input.ExpectedFingerprint ||
		!CanonicalDigest(result.CandidateFingerprint) ||
		result.CandidateFingerprint == result.PreviousFingerprint ||
		result.Edits.Path != input.Document.Path {
		return fmt.Errorf("%w: edge creation result names another graph or request", ErrConflict)
	}
	digest := sha256.Sum256([]byte(input.Document.Source))
	wantDigest := "sha256:" + hex.EncodeToString(digest[:])
	if result.Edits.SourceDigest != wantDigest || len(result.Edits.Edits) != 1 {
		return fmt.Errorf("%w: edge creation result has another source or edit count", ErrConflict)
	}
	created, err := editor.ApplyEdits([]byte(input.Document.Source), result.Edits)
	if err != nil || len(created) == 0 || len(created) > maxManagedSourceBytes {
		return fmt.Errorf("%w: edge creation edit is invalid or exceeds the source bound", ErrConflict)
	}
	file, _ := syntax.Parse(input.Document.Path, []byte(input.Document.Source))
	file.Graph.Statements = append(file.Graph.Statements, syntax.Statement{Edge: &syntax.Edge{
		Name:     input.Edge,
		From:     syntax.Endpoint{Node: input.From.Node, Port: input.From.Port},
		To:       syntax.Endpoint{Node: input.To.Node, Port: input.To.Port},
		Delivery: syntax.Delivery(input.Delivery),
	}})
	want := []byte(syntax.Format(file))
	if !bytes.Equal(created, want) {
		return fmt.Errorf("%w: edge creation changes more or less than the requested edge", ErrConflict)
	}
	parsed, err := syntax.Parse(input.Document.Path, created)
	if err != nil || !bytes.Equal(created, []byte(syntax.Format(parsed))) {
		return fmt.Errorf("%w: edge creation result is not canonical .ortg", ErrConflict)
	}
	return nil
}

func nextAuthoringRevision(current uint64) (uint64, error) {
	if current == 0 {
		current = 1
	}
	if current == ^uint64(0) {
		return 0, fmt.Errorf("%w: authoring document revision is exhausted", ErrInvalid)
	}
	return current + 1, nil
}
