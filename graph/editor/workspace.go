package editor

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/graph/syntax"
)

var (
	ErrWorkspaceLimit     = errors.New("editor workspace exceeds its bound")
	ErrWorkspaceIdentity  = errors.New("editor workspace document identity is invalid")
	ErrWorkspaceAmbiguous = errors.New("editor workspace navigation target is ambiguous")
)

// WorkspaceLimits bounds an explicitly supplied in-memory document set. It
// does not authorize discovery, loading, or mutation of any path. Zero fields
// select defaults.
type WorkspaceLimits struct {
	MaxDocuments        int `json:"max_documents"`
	MaxTotalSourceBytes int `json:"max_total_source_bytes"`
	MaxImports          int `json:"max_imports"`
	MaxPathBytes        int `json:"max_path_bytes"`
}

// WorkspaceDocument binds one immutable analysis snapshot to the caller's
// exact protocol identity. Snapshot is deliberately not a serialized source
// or loading capability.
type WorkspaceDocument struct {
	Identity LSPDocumentIdentity `json:"identity"`
	Snapshot *Document           `json:"-"`
}

// WorkspaceDefinitionKind identifies which workspace-owned source symbol was
// resolved without conflating it with a descriptor or local node definition.
type WorkspaceDefinitionKind string

const (
	WorkspaceImportDefinition   WorkspaceDefinitionKind = "import"
	WorkspaceSubgraphDefinition WorkspaceDefinitionKind = "subgraph"
	WorkspaceBoundaryDefinition WorkspaceDefinitionKind = "subgraph_boundary"
)

// WorkspaceDefinition retains both source and target snapshot identities even
// though the final standards-defined LocationLink cannot carry target version
// or digest fields. An adapter must revalidate the workspace lease before it
// projects this value onto the wire.
type WorkspaceDefinition struct {
	WorkspaceDigest      string                  `json:"workspace_digest"`
	Kind                 WorkspaceDefinitionKind `json:"kind"`
	OriginSelectionRange LSPRange                `json:"originSelectionRange"`
	TargetDocument       LSPDocumentIdentity     `json:"target_document"`
	TargetRange          LSPRange                `json:"targetRange"`
	TargetSelectionRange LSPRange                `json:"targetSelectionRange"`
}

type workspaceImport struct {
	path          string
	explicitAlias string
	targetPath    string
	pathSpan      syntax.Span
	aliasSpan     *syntax.Span
}

type workspaceTarget struct {
	full      LSPRange
	selection LSPRange
}

type workspaceIndexedDocument struct {
	identity   LSPDocumentIdentity
	snapshot   *Document
	indexable  bool
	imports    []workspaceImport
	graph      workspaceTarget
	boundaries map[string][]workspaceTarget
}

// WorkspaceIndex is an immutable index over only the documents supplied to
// NewWorkspaceIndex. Relative imports are resolved as identifiers inside that
// closed set; no path is opened and no missing document is discovered.
type WorkspaceIndex struct {
	limits      WorkspaceLimits
	fingerprint string
	documents   []LSPDocumentIdentity
	byURI       map[string]*workspaceIndexedDocument
	byPath      map[string]*workspaceIndexedDocument
}

// NewWorkspaceIndex validates and fingerprints a closed immutable population.
func NewWorkspaceIndex(
	documents []WorkspaceDocument, configured WorkspaceLimits,
) (*WorkspaceIndex, error) {
	limits, err := normalizeWorkspaceLimits(configured)
	if err != nil {
		return nil, err
	}
	if len(documents) > limits.MaxDocuments {
		return nil, fmt.Errorf("%w: %d documents; maximum is %d",
			ErrWorkspaceLimit, len(documents), limits.MaxDocuments)
	}
	index := &WorkspaceIndex{
		limits: limits, byURI: make(map[string]*workspaceIndexedDocument, len(documents)),
		byPath: make(map[string]*workspaceIndexedDocument, len(documents)),
	}
	totalBytes, totalImports := 0, 0
	for position, input := range documents {
		if input.Snapshot == nil {
			return nil, fmt.Errorf("%w: document %d has no snapshot", ErrWorkspaceIdentity, position)
		}
		if err := input.Snapshot.validateLSPDocumentIdentity(input.Identity); err != nil {
			return nil, fmt.Errorf("%w: document %d: %v", ErrWorkspaceIdentity, position, err)
		}
		if len(input.Identity.Path) > limits.MaxPathBytes || !utf8.ValidString(input.Identity.Path) {
			return nil, fmt.Errorf("%w: document %d path exceeds its bound",
				ErrWorkspaceIdentity, position)
		}
		if sourceDigest(input.Snapshot.source) != input.Identity.SourceDigest {
			return nil, fmt.Errorf("%w: document %d source no longer matches its digest",
				ErrWorkspaceIdentity, position)
		}
		if input.Snapshot.file.Path != "" && input.Snapshot.file.Path != input.Identity.Path {
			return nil, fmt.Errorf("%w: document %d syntax path does not match its identity",
				ErrWorkspaceIdentity, position)
		}
		if _, duplicate := index.byURI[input.Identity.URI]; duplicate {
			return nil, fmt.Errorf("%w: duplicate URI %q", ErrWorkspaceAmbiguous, input.Identity.URI)
		}
		if _, duplicate := index.byPath[input.Identity.Path]; duplicate {
			return nil, fmt.Errorf("%w: duplicate path %q", ErrWorkspaceAmbiguous, input.Identity.Path)
		}
		if len(input.Snapshot.source) > limits.MaxTotalSourceBytes-totalBytes {
			return nil, fmt.Errorf("%w: total source bytes exceed %d",
				ErrWorkspaceLimit, limits.MaxTotalSourceBytes)
		}
		totalBytes += len(input.Snapshot.source)
		imports := len(input.Snapshot.file.Imports)
		if imports > limits.MaxImports-totalImports {
			return nil, fmt.Errorf("%w: total imports exceed %d", ErrWorkspaceLimit, limits.MaxImports)
		}
		totalImports += imports
		owned := &workspaceIndexedDocument{
			identity: input.Identity, snapshot: input.Snapshot,
			boundaries: make(map[string][]workspaceTarget),
		}
		index.byURI[input.Identity.URI] = owned
		index.byPath[input.Identity.Path] = owned
	}

	keys := make([]string, 0, len(index.byURI))
	for uri := range index.byURI {
		keys = append(keys, uri)
	}
	sort.Strings(keys)
	for _, uri := range keys {
		document := index.byURI[uri]
		if !document.snapshot.parsed || !document.snapshot.canonical {
			continue
		}
		if err := index.indexCanonicalDocument(document); err != nil {
			return nil, err
		}
		document.indexable = true
	}
	index.documents = make([]LSPDocumentIdentity, 0, len(index.byURI))
	for _, uri := range keys {
		index.documents = append(index.documents, index.byURI[uri].identity)
	}
	encoded, err := json.Marshal(index.documents)
	if err != nil {
		return nil, fmt.Errorf("fingerprint editor workspace: %w", err)
	}
	index.fingerprint = sourceDigest(encoded)
	return index, nil
}

func normalizeWorkspaceLimits(value WorkspaceLimits) (WorkspaceLimits, error) {
	defaults := WorkspaceLimits{
		MaxDocuments: 128, MaxTotalSourceBytes: 128 << 20,
		MaxImports: 8192, MaxPathBytes: 64 << 10,
	}
	fields := []struct {
		name string
		set  *int
		def  int
		hard int
	}{
		{"max_documents", &value.MaxDocuments, defaults.MaxDocuments, 4096},
		{"max_total_source_bytes", &value.MaxTotalSourceBytes, defaults.MaxTotalSourceBytes, 1 << 30},
		{"max_imports", &value.MaxImports, defaults.MaxImports, 1 << 20},
		{"max_path_bytes", &value.MaxPathBytes, defaults.MaxPathBytes, 64 << 10},
	}
	for _, field := range fields {
		if *field.set == 0 {
			*field.set = field.def
		}
		if *field.set < 1 || *field.set > field.hard {
			return WorkspaceLimits{}, fmt.Errorf(
				"editor workspace %s must be between 1 and %d", field.name, field.hard,
			)
		}
	}
	return value, nil
}

// DefaultWorkspaceLimits returns immutable standalone index bounds. An LSP
// adapter derives larger zero-valued aggregate bounds when its configured
// document or source limits require them.
func DefaultWorkspaceLimits() WorkspaceLimits {
	value, _ := normalizeWorkspaceLimits(WorkspaceLimits{})
	return value
}

// Fingerprint returns the digest of the sorted path/URI/version/source-digest
// population from which this index was built.
func (index *WorkspaceIndex) Fingerprint() string {
	if index == nil {
		return ""
	}
	return index.fingerprint
}

// Documents returns independent identities in deterministic URI order.
func (index *WorkspaceIndex) Documents() []LSPDocumentIdentity {
	if index == nil {
		return nil
	}
	return append([]LSPDocumentIdentity(nil), index.documents...)
}

// Definition resolves import declarations, imported graph references, and
// imported boundary-port references. handled is true whenever the workspace
// namespace owns the symbol, including a missing or invalid target; callers
// must not fall back to a descriptor namespace in that case.
func (index *WorkspaceIndex) Definition(
	identity LSPDocumentIdentity, position LSPPosition,
) (result WorkspaceDefinition, handled bool, err error) {
	if index == nil {
		return WorkspaceDefinition{}, false, errors.New("editor workspace index is nil")
	}
	document := index.byURI[identity.URI]
	if document == nil || document.identity != identity {
		return WorkspaceDefinition{}, false, ErrStalePosition
	}
	if !document.indexable {
		return WorkspaceDefinition{}, false, ErrSyntaxUnavailable
	}
	offset, err := document.snapshot.offsetAtLSPPosition(position)
	if err != nil {
		return WorkspaceDefinition{}, false, err
	}
	for _, imported := range document.imports {
		origin := imported.pathSpan
		if imported.aliasSpan != nil && spanContainsOffset(*imported.aliasSpan, offset) {
			origin = *imported.aliasSpan
		} else if !spanContainsOffset(imported.pathSpan, offset) {
			continue
		}
		target := index.byPath[imported.targetPath]
		if target == nil || !target.indexable {
			return WorkspaceDefinition{}, true, ErrDefinitionMissing
		}
		definition, err := index.definition(
			document, origin, target, WorkspaceImportDefinition, target.graph,
		)
		return definition, true, err
	}

	cursor, err := document.snapshot.Cursor(offset)
	if err != nil {
		return WorkspaceDefinition{}, false, err
	}
	symbol, err := document.snapshot.symbolAt(cursor)
	if err != nil {
		if errors.Is(err, ErrNoSymbol) {
			return WorkspaceDefinition{}, false, nil
		}
		return WorkspaceDefinition{}, false, err
	}
	elementReference := ""
	switch symbol.kind {
	case SymbolElement:
		elementReference = symbol.element
	case SymbolPortReference:
		record, found := document.snapshot.nodes[symbol.node]
		if !found || record.ambiguous {
			return WorkspaceDefinition{}, false, nil
		}
		elementReference = record.element
	default:
		return WorkspaceDefinition{}, false, nil
	}
	imported, target, claimed, err := index.importedTarget(document, elementReference)
	if err != nil || !claimed {
		return WorkspaceDefinition{}, claimed, err
	}
	if target == nil || !target.indexable {
		return WorkspaceDefinition{}, true, ErrDefinitionMissing
	}
	if symbol.kind == SymbolElement {
		definition, err := index.definition(
			document, symbol.span, target, WorkspaceSubgraphDefinition, target.graph,
		)
		return definition, true, err
	}
	targets := target.boundaries[symbol.port]
	if len(targets) == 0 {
		return WorkspaceDefinition{}, true, ErrDefinitionMissing
	}
	if len(targets) != 1 {
		return WorkspaceDefinition{}, true, fmt.Errorf(
			"%w: imported graph %q repeats boundary %q",
			ErrWorkspaceAmbiguous, imported.path, symbol.port,
		)
	}
	definition, err := index.definition(
		document, symbol.span, target, WorkspaceBoundaryDefinition, targets[0],
	)
	return definition, true, err
}

func (index *WorkspaceIndex) definition(
	source *workspaceIndexedDocument,
	origin syntax.Span,
	target *workspaceIndexedDocument,
	kind WorkspaceDefinitionKind,
	location workspaceTarget,
) (WorkspaceDefinition, error) {
	originRange, err := source.snapshot.lspRange(origin)
	if err != nil {
		return WorkspaceDefinition{}, err
	}
	return WorkspaceDefinition{
		WorkspaceDigest: index.fingerprint, Kind: kind,
		OriginSelectionRange: originRange, TargetDocument: target.identity,
		TargetRange: location.full, TargetSelectionRange: location.selection,
	}, nil
}

func (index *WorkspaceIndex) importedTarget(
	source *workspaceIndexedDocument, elementReference string,
) (*workspaceImport, *workspaceIndexedDocument, bool, error) {
	alias, graphName, found := strings.Cut(elementReference, ".")
	if !found || alias == "" || graphName == "" {
		return nil, nil, false, nil
	}
	var (
		match       *workspaceImport
		matchTarget *workspaceIndexedDocument
		claimed     bool
		aliasCount  int
	)
	for position := range source.imports {
		imported := &source.imports[position]
		target := index.byPath[imported.targetPath]
		resolvedAlias := imported.explicitAlias
		if resolvedAlias == "" && target != nil && target.snapshot.parsed {
			resolvedAlias = target.snapshot.file.Graph.Name
		}
		if resolvedAlias != alias {
			continue
		}
		claimed = true
		aliasCount++
		if aliasCount > 1 {
			return nil, nil, true, fmt.Errorf(
				"%w: import alias %q resolves more than once", ErrWorkspaceAmbiguous, alias,
			)
		}
		if target == nil || !target.indexable || graphName != target.snapshot.file.Graph.Name {
			continue
		}
		match, matchTarget = imported, target
	}
	if match == nil {
		if claimed {
			return nil, nil, true, ErrDefinitionMissing
		}
		return nil, nil, false, nil
	}
	return match, matchTarget, true, nil
}

func (index *WorkspaceIndex) indexCanonicalDocument(document *workspaceIndexedDocument) error {
	snapshot := document.snapshot
	if string(snapshot.source) != syntax.Format(snapshot.file) {
		return fmt.Errorf("%w: canonical document %q changed source", ErrWorkspaceIdentity, document.identity.Path)
	}
	graphStart := snapshot.file.Graph.Span.Start.Offset
	graphNameStart := graphStart + len("graph ")
	graphNameEnd := graphNameStart + len(snapshot.file.Graph.Name)
	if graphStart < 0 || graphNameEnd > len(snapshot.source) ||
		string(snapshot.source[graphStart:graphNameEnd]) != "graph "+snapshot.file.Graph.Name {
		return fmt.Errorf("%w: graph name span in %q is invalid", ErrWorkspaceIdentity, document.identity.Path)
	}
	graphSelection, err := sourceSpan(snapshot.positions, graphNameStart, graphNameEnd)
	if err != nil {
		return err
	}
	document.graph, err = workspaceProjection(snapshot, snapshot.file.Graph.Span, graphSelection)
	if err != nil {
		return err
	}

	for _, declaration := range snapshot.file.Imports {
		start, end := declaration.Span.Start.Offset, declaration.Span.End.Offset
		quoted := strconv.Quote(declaration.Path)
		want := "import " + quoted
		if declaration.Alias != "" {
			want += " as " + declaration.Alias
		}
		want += ";"
		if start < 0 || end > len(snapshot.source) || start > end ||
			string(snapshot.source[start:end]) != want {
			return fmt.Errorf("%w: import span in %q is invalid", ErrWorkspaceIdentity, document.identity.Path)
		}
		pathStart := start + len("import ")
		pathSpan, err := sourceSpan(snapshot.positions, pathStart, pathStart+len(quoted))
		if err != nil {
			return err
		}
		var aliasSpan *syntax.Span
		if declaration.Alias != "" {
			aliasStart := pathStart + len(quoted) + len(" as ")
			value, err := sourceSpan(snapshot.positions, aliasStart, aliasStart+len(declaration.Alias))
			if err != nil {
				return err
			}
			aliasSpan = &value
		}
		targetPath, err := resolveWorkspaceImportPath(
			document.identity.Path, declaration.Path, index.limits.MaxPathBytes,
		)
		if err != nil {
			return err
		}
		document.imports = append(document.imports, workspaceImport{
			path: declaration.Path, explicitAlias: declaration.Alias,
			targetPath: targetPath, pathSpan: pathSpan, aliasSpan: aliasSpan,
		})
	}
	for _, boundary := range snapshot.file.Graph.Boundaries() {
		start, end := boundary.Span.Start.Offset, boundary.Span.End.Offset
		want := string(boundary.Direction) + " " + boundary.Name + " = " + boundary.Endpoint.String() + ";"
		if start < 0 || end > len(snapshot.source) || start > end ||
			string(snapshot.source[start:end]) != want {
			return fmt.Errorf("%w: boundary span in %q is invalid", ErrWorkspaceIdentity, document.identity.Path)
		}
		nameStart := start + len(string(boundary.Direction)) + 1
		selection, err := sourceSpan(snapshot.positions, nameStart, nameStart+len(boundary.Name))
		if err != nil {
			return err
		}
		projection, err := workspaceProjection(snapshot, boundary.Span, selection)
		if err != nil {
			return err
		}
		document.boundaries[boundary.Name] = append(document.boundaries[boundary.Name], projection)
	}
	return nil
}

func workspaceProjection(
	document *Document, full syntax.Span, selection syntax.Span,
) (workspaceTarget, error) {
	fullRange, err := document.lspRange(full)
	if err != nil {
		return workspaceTarget{}, err
	}
	selectionRange, err := document.lspRange(selection)
	if err != nil {
		return workspaceTarget{}, err
	}
	return workspaceTarget{full: fullRange, selection: selectionRange}, nil
}

func resolveWorkspaceImportPath(importerPath, importPath string, maximum int) (string, error) {
	if importPath == "" || importPath != strings.TrimSpace(importPath) ||
		!utf8.ValidString(importPath) || strings.ContainsAny(importPath, "\x00\r\n") {
		return "", fmt.Errorf("%w: import path is invalid", ErrWorkspaceIdentity)
	}
	base, baseErr := url.ParseRequestURI(importerPath)
	if baseErr == nil && base.Scheme != "" {
		if base.Opaque != "" || base.User != nil || base.Fragment != "" || base.RawQuery != "" {
			return "", fmt.Errorf("%w: importer URI cannot resolve relative paths",
				ErrWorkspaceIdentity)
		}
		reference, err := url.Parse(importPath)
		if err != nil || reference.IsAbs() || reference.Host != "" || reference.User != nil ||
			reference.RawQuery != "" || reference.Fragment != "" || reference.Opaque != "" {
			return "", fmt.Errorf("%w: import %q is not a relative URI path",
				ErrWorkspaceIdentity, importPath)
		}
		resolved := base.ResolveReference(reference)
		if resolved.Scheme != base.Scheme || resolved.User != nil || resolved.Fragment != "" ||
			resolved.RawQuery != "" {
			return "", fmt.Errorf("%w: import %q changes URI authority",
				ErrWorkspaceIdentity, importPath)
		}
		value := resolved.String()
		if len(value) > maximum {
			return "", fmt.Errorf("%w: resolved import path exceeds %d bytes", ErrWorkspaceLimit, maximum)
		}
		return value, nil
	}
	if strings.ContainsRune(importPath, '\\') {
		return "", fmt.Errorf("%w: logical import paths must use forward slashes", ErrWorkspaceIdentity)
	}
	importedURL, err := url.Parse(importPath)
	if err != nil || importedURL.Scheme != "" || importedURL.Host != "" || importedURL.User != nil ||
		importedURL.RawQuery != "" || importedURL.Fragment != "" || importedURL.Opaque != "" {
		return "", fmt.Errorf("%w: logical import path is invalid", ErrWorkspaceIdentity)
	}
	value := ""
	if path.IsAbs(importPath) {
		value = path.Clean(importPath)
	} else {
		value = path.Clean(path.Join(path.Dir(importerPath), importPath))
	}
	if value == "." || len(value) > maximum {
		return "", fmt.Errorf("%w: resolved import path is invalid or exceeds %d bytes",
			ErrWorkspaceLimit, maximum)
	}
	return value, nil
}

func spanContainsOffset(span syntax.Span, offset int) bool {
	return offset >= span.Start.Offset && offset <= span.End.Offset
}
